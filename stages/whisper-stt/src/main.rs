mod probe;
mod scoring;

use branchkit_stage_sdk::events::{
    AudioChunk, AudioFormat, AudioStart, AudioStop, Capability, Transcript, event_type,
};
use branchkit_stage_sdk::stage::{
    self, AudioConsumer, AudioCtx, Chunk, CreditPolicy, Flow, InitialGrant, Result,
};
use branchkit_stage_sdk::stage_log;
use branchkit_stage_sdk::wire::Event;
use scoring::ScoredToken;
use whisper_rs::{FullParams, SamplingStrategy, WhisperContext, WhisperContextParameters};

// Shallow queues — an STT engine wants small buffers. `OnSessionStart` grants
// the initial window per session, which also resets the cadence counter.
const CREDIT: CreditPolicy = CreditPolicy {
    initial: 8,
    every: 4,
    grant: 4,
    when: InitialGrant::OnSessionStart,
};

/// The vocabulary the dictation plugin pushes as configuration: words whose
/// tokens the decoder is nudged toward, as the macOS stage does.
const BIAS_EVENT: &str = "ext.branchkit.dictation_bias";
/// Added to each boosted token's logit at every decoding step. The macOS
/// stage's value, where it was measured: large enough to fix a misspelt name,
/// small enough that the only changes it produced were corrections.
const BIAS_BOOST: f32 = 2.0;
/// The most audio one session buffers: 11 minutes at 16 kHz. The voice
/// plugin stops a dictation at 10 (its dictationLimit); the extra minute
/// covers the audio between its timer and the first chunk. Past it the END
/// is dropped, never the start, as in the macOS stage.
const MAX_SAMPLES: usize = 16_000 * 60 * 11;

#[tokio::main(flavor = "current_thread")]
async fn main() {
    // `probe`: the offline re-decode the platform's recognition.redecode runs
    // (probe.rs). It exits when done.
    if std::env::args().nth(1).as_deref() == Some("probe") {
        std::process::exit(probe::run());
    }
    stage::run(run()).await
}

async fn run() -> Result {
    let args = parse_stage_args();
    let model_path = resolve_model(args.model.as_deref())?;
    stage_log::info(&format!("model: {}", model_path.display()));

    let ctx = WhisperContext::new_with_params(
        &model_path.to_string_lossy(),
        WhisperContextParameters::default(),
    )
    .map_err(|e| format!("failed to load model {}: {e}", model_path.display()))?;

    // Persistent: the model loads once and serves every hold, as on macOS.
    // Per-run reloaded it at the start of each dictation.
    let cap = Capability::new("stt", "whisper")
        .audio_format(AudioFormat::PCM_16K_MONO)
        .persistent()
        .accepts_config([BIAS_EVENT])
        .emits(["transcript", "flow_credit"]);

    let mut stt = Whisper {
        ctx,
        args,
        session_id: None,
        pcm_i16: Vec::new(),
        marks: Vec::new(),
        capped: false,
        bias: Bias::default(),
    };

    stage::serve_audio_consumer(cap, CREDIT, &mut stt).await
}

struct Whisper {
    ctx: WhisperContext,
    args: StageArgs,
    session_id: Option<String>,
    pcm_i16: Vec<i16>,
    /// Per chunk: (samples buffered once it was appended, its shared-clock
    /// timestamp), for cutting the buffer at an AudioStop's `cutoff_ms`.
    marks: Vec<(usize, u64)>,
    /// Whether this session reached MAX_SAMPLES (said once, when it does).
    capped: bool,
    bias: Bias,
}

/// The bias vocabulary and the token ids it resolves to. The ids are derived
/// lazily, at transcription, from this model's tokenizer.
#[derive(Default)]
struct Bias {
    words: Vec<String>,
    tokens: Vec<i32>,
    stale: bool,
}

impl Whisper {
    /// Resolve the bias words to token ids: every piece of " word" that is an
    /// ordinary text token carrying a letter or digit. Punctuation pieces
    /// are left out ("git-bug" must not boost "-" everywhere), as on macOS.
    fn refresh_bias_tokens(&mut self) {
        if !self.bias.stale {
            return;
        }
        self.bias.stale = false;
        let eot = self.ctx.token_eot();
        let mut ids: Vec<i32> = Vec::new();
        for word in &self.bias.words {
            let Ok(toks) = self.ctx.tokenize(&format!(" {word}"), 64) else {
                continue;
            };
            for t in toks {
                let wordlike = self
                    .ctx
                    .token_to_str(t)
                    .is_ok_and(|s| s.chars().any(char::is_alphanumeric));
                if t < eot && wordlike && !ids.contains(&t) {
                    ids.push(t);
                }
            }
        }
        stage_log::info(&format!(
            "bias: {} words -> {} boosted tokens",
            self.bias.words.len(),
            ids.len()
        ));
        self.bias.tokens = ids;
    }
}

/// whisper.cpp's logits filter: adds the boost to each bias token. `user_data`
/// is the `Bias` the transcription borrowed, alive for the whole `full` call.
unsafe extern "C" fn boost_bias_tokens(
    _ctx: *mut whisper_rs::WhisperSysContext,
    _state: *mut whisper_rs::WhisperSysState,
    _tokens: *const whisper_rs::WhisperTokenData,
    _n_tokens: std::os::raw::c_int,
    logits: *mut f32,
    user_data: *mut std::ffi::c_void,
) {
    if logits.is_null() || user_data.is_null() {
        return;
    }
    // SAFETY: user_data is `&Bias` set by `transcribe`, which outlives the
    // decode; logits has n_vocab entries and every id is below eot < n_vocab.
    let bias = unsafe { &*(user_data as *const Bias) };
    for &t in &bias.tokens {
        unsafe { *logits.add(t as usize) += BIAS_BOOST };
    }
}

impl AudioConsumer for Whisper {
    async fn on_audio_start(&mut self, ev: AudioStart, _ctx: &mut AudioCtx) -> Result {
        if ev.format != AudioFormat::PCM_16K_MONO {
            return Err(format!("unsupported format: {:?}", ev.format).into());
        }
        stage_log::set_session(&ev.session_id);
        self.session_id = Some(ev.session_id);
        self.pcm_i16.clear();
        self.marks.clear();
        self.capped = false;
        Ok(())
    }

    async fn on_audio_chunk(
        &mut self,
        ev: AudioChunk,
        payload: &[u8],
        _ctx: &mut AudioCtx,
    ) -> Result<Chunk> {
        let sid = self
            .session_id
            .as_ref()
            .ok_or("audio_chunk before audio_start")?;
        if ev.session_id != *sid {
            return Ok(Chunk::Dropped);
        }

        // Accumulate PCM samples from payload (i16 little-endian), up to
        // MAX_SAMPLES.
        let room = MAX_SAMPLES - self.pcm_i16.len();
        if room == 0 {
            if !self.capped {
                self.capped = true;
                stage_log::warn(&format!(
                    "buffer full at {} s: later audio is not transcribed",
                    MAX_SAMPLES / 16_000
                ));
            }
            return Ok(Chunk::Counted);
        }
        for pair in payload.chunks_exact(2).take(room) {
            self.pcm_i16.push(i16::from_le_bytes([pair[0], pair[1]]));
        }
        self.marks.push((self.pcm_i16.len(), ev.timestamp_ms));
        Ok(Chunk::Counted)
    }

    async fn on_audio_stop(&mut self, ev: AudioStop, ctx: &mut AudioCtx) -> Result<Flow> {
        let sid = self
            .session_id
            .take()
            .ok_or("audio_stop before audio_start")?;

        // A stop phrase's audio is cut here, at its onset, so the model never
        // hears it (the same cut the macOS stage makes).
        let keep = scoring::samples_before_cutoff(&self.marks, self.pcm_i16.len(), ev.cutoff_ms);
        if keep < self.pcm_i16.len() {
            stage_log::info(&format!(
                "cutoff {:?}ms: dropping {} buffered samples",
                ev.cutoff_ms,
                self.pcm_i16.len() - keep
            ));
            self.pcm_i16.truncate(keep);
        }

        let transcript = self.transcribe(sid)?;
        ctx.emit(event_type::TRANSCRIPT, &transcript).await?;

        stage_log::clear_session();
        // Persistent: the next hold is another session on this process.
        Ok(Flow::Continue)
    }

    async fn on_other(&mut self, ev: Event, _ctx: &mut AudioCtx) -> Result<Flow> {
        if ev.event_type == BIAS_EVENT {
            // Replaces rather than merges: the sender owns the whole list.
            let words: Vec<String> = ev
                .data
                .get("words")
                .and_then(|w| serde_json::from_value(w.clone()).ok())
                .unwrap_or_default();
            if words != self.bias.words {
                stage_log::info(&format!("bias vocabulary: {} words", words.len()));
                self.bias.words = words;
                self.bias.stale = true;
            }
        }
        Ok(Flow::Continue)
    }
}

impl Whisper {
    fn transcribe(&mut self, session_id: String) -> Result<Transcript> {
        self.refresh_bias_tokens();
        // Convert i16 → f32 for whisper
        let pcm_f32: Vec<f32> = self.pcm_i16.iter().map(|&s| s as f32 / 32768.0).collect();
        let bias = (!self.bias.tokens.is_empty()).then_some(&self.bias);
        let (text, tokens) = decode(
            &self.ctx,
            &pcm_f32,
            &self.args.language,
            self.args.threads,
            bias,
        )?;
        let word_scores = scoring::word_scores(&tokens, &text);
        let confidence = scoring::confidence(&tokens, word_scores.as_deref());

        Ok(Transcript {
            session_id,
            text,
            partial: false,
            is_final: true,
            confidence,
            word_onsets_ms: None,
            word_scores,
        })
    }
}

/// One dictation decode, live or probed, so a probe decodes exactly as the
/// live stage does: greedy, the given language and threads, the vocabulary
/// boost when there is one. Returns the segments' text joined with a space
/// (as on macOS) and every text token with its probability, for the word
/// scores.
fn decode(
    ctx: &WhisperContext,
    pcm_f32: &[f32],
    language: &str,
    threads: i32,
    bias: Option<&Bias>,
) -> Result<(String, Vec<ScoredToken>)> {
    let mut state = ctx
        .create_state()
        .map_err(|e| format!("whisper state: {e}"))?;
    let mut params = FullParams::new(SamplingStrategy::Greedy { best_of: 1 });
    params.set_print_progress(false);
    params.set_print_special(false);
    params.set_print_realtime(false);
    params.set_print_timestamps(false);
    params.set_language(Some(language));
    params.set_n_threads(threads);
    if let Some(bias) = bias {
        // SAFETY: the callback only reads `bias` and writes logits; `bias`
        // outlives `full`, which is the callback's only caller.
        unsafe {
            params.set_filter_logits_callback(Some(boost_bias_tokens));
            params
                .set_filter_logits_callback_user_data(bias as *const Bias as *mut std::ffi::c_void);
        }
    }

    state
        .full(params, pcm_f32)
        .map_err(|e| format!("whisper inference: {e}"))?;

    let n_segments = state
        .full_n_segments()
        .map_err(|e| format!("segments: {e}"))?;
    let eot = ctx.token_eot();
    let mut parts: Vec<String> = Vec::new();
    let mut tokens: Vec<ScoredToken> = Vec::new();
    for i in 0..n_segments {
        if let Ok(seg) = state.full_get_segment_text(i) {
            let seg = seg.trim();
            if !seg.is_empty() {
                parts.push(seg.to_string());
            }
        }
        let n = state.full_n_tokens(i).unwrap_or(0);
        for j in 0..n {
            let Ok(id) = state.full_get_token_id(i, j) else {
                continue;
            };
            if id >= eot {
                continue;
            }
            let (Ok(text), Ok(data)) = (
                state.full_get_token_text_lossy(i, j),
                state.full_get_token_data(i, j),
            ) else {
                continue;
            };
            tokens.push(ScoredToken {
                text,
                logprob: data.plog,
            });
        }
    }
    Ok((parts.join(" "), tokens))
}

struct StageArgs {
    model: Option<String>,
    language: String,
    threads: i32,
}

/// Positional: `whisper_stage <model>`. Flags: `--model <name-or-path>`,
/// `--language <code>`, `--threads <n>`. Pipeline params arrive as `--key
/// value` pairs (runner convention); unknown flags are skipped so new
/// params never break the stage.
fn parse_stage_args() -> StageArgs {
    let argv: Vec<String> = std::env::args().skip(1).collect();
    let mut model = None;
    let mut language = "en".to_string();
    let mut threads = std::thread::available_parallelism()
        .map(|n| n.get().min(4) as i32)
        .unwrap_or(1);
    let mut i = 0;
    while i < argv.len() {
        match argv[i].as_str() {
            "--model" => {
                if let Some(v) = argv.get(i + 1) {
                    model = Some(v.clone());
                    i += 1;
                }
            }
            "--language" => {
                if let Some(v) = argv.get(i + 1) {
                    language = v.clone();
                    i += 1;
                }
            }
            "--threads" => {
                if let Some(v) = argv.get(i + 1) {
                    if let Ok(n) = v.parse() {
                        threads = n;
                    }
                    i += 1;
                }
            }
            other if other.starts_with("--") => {
                // Unknown pipeline param: skip flag + value.
                i += 1;
            }
            positional if model.is_none() => model = Some(positional.to_string()),
            _ => {}
        }
        i += 1;
    }
    StageArgs {
        model,
        language,
        threads,
    }
}

/// Platform models directory: `<model root>/whisper`.
///
/// The root arrives via `BRANCHKIT_ARTIFACTS_DIR` (`ARTIFACTS_DIR_ENV`), set by the platform at spawn
/// from the SAME derivation its sandbox grant uses (`grant_named_artifact`) —
/// never re-derived here. A second spelling does not merely disagree — it
/// points the stage at a directory the profile denies; this file used to
/// carry its own `BRANCHKIT_DEV` branch that disagreed in exactly that way,
/// and after that fix it linked the whole platform for this one path.
/// `None` when run outside the platform (use `WHISPER_MODEL_PATH` or an
/// absolute `--model` then).
fn whisper_models_dir() -> Option<std::path::PathBuf> {
    std::env::var_os(branchkit_stage_sdk::ARTIFACTS_DIR_ENV)
        .map(|root| std::path::PathBuf::from(root).join("whisper"))
}

/// Resolve the model argument to a ggml file. Order: WHISPER_MODEL_PATH env
/// override → literal path → `<models>/whisper/<name>[.bin]` → any *.bin in
/// the models dir (manifests carry cross-platform model names that may not
/// exist as ggml files — e.g. WhisperKit names — so a placed model always
/// wins over a failed name lookup).
fn resolve_model(arg: Option<&str>) -> Result<std::path::PathBuf> {
    if let Ok(p) = std::env::var("WHISPER_MODEL_PATH") {
        return Ok(std::path::PathBuf::from(p));
    }
    let models_dir = whisper_models_dir();
    if let Some(name) = arg {
        let p = std::path::PathBuf::from(name);
        if p.is_absolute() || p.exists() {
            return Ok(p);
        }
        // A DECLARED model: the CLI provisions `provides.models` entries to
        // `<models>/<name>/` and the sandbox grants exactly that directory to
        // the stage the pipeline configured with `name`. Look there first, so
        // the pipeline param, the grant and the file agree — the way they do
        // for the Sherpa stage. (Before this, a ggml file could only be found
        // via the `whisper/` fallback below, which no pipeline names and no
        // grant reaches: dictation on Linux and Windows never loaded a model
        // under the sandbox.)
        if let Ok(root) = std::env::var(branchkit_stage_sdk::ARTIFACTS_DIR_ENV) {
            let dir = std::path::Path::new(&root).join(name);
            if dir.is_dir() {
                let mut bins: Vec<_> = std::fs::read_dir(&dir)
                    .into_iter()
                    .flatten()
                    .flatten()
                    .map(|e| e.path())
                    .filter(|p| p.extension().is_some_and(|e| e == "bin"))
                    .collect();
                bins.sort();
                if let Some(p) = bins.into_iter().next() {
                    return Ok(p);
                }
            }
        }
        if let Some(dir) = &models_dir {
            for candidate in [dir.join(name), dir.join(format!("{name}.bin"))] {
                if candidate.exists() {
                    return Ok(candidate);
                }
            }
        }
    }
    if let Some(dir) = &models_dir {
        let mut bins: Vec<_> = std::fs::read_dir(dir)
            .into_iter()
            .flatten()
            .flatten()
            .map(|e| e.path())
            .filter(|p| p.extension().is_some_and(|e| e == "bin"))
            .collect();
        bins.sort();
        if let Some(p) = bins.into_iter().next() {
            if let Some(name) = arg {
                stage_log::warn(&format!("model '{name}' not found; using {}", p.display()));
            }
            return Ok(p);
        }
    }
    Err(format!(
        "no whisper model found: set WHISPER_MODEL_PATH, pass --model <path>, or place a ggml-*.bin in {} (run `just whisper-model` to download one)",
        models_dir
            .map(|d| d.display().to_string())
            .unwrap_or_else(|| "<data dir>/models/whisper".into())
    )
    .into())
}
