use std::env;
use std::path::PathBuf;

mod engine;
mod offline;
mod probe;

// Stage diagnostics route through the shared stage logger
// (`branchkit_stage_sdk::stage_log`): one canonical `BKLOG1` line the
// platform parses into a correlated `stage.diagnostic` bus event (timestamp +
// stage name live on the bus envelope). `slog!` defaults to `info`; the
// session id is stamped ambiently from `set_session`.
macro_rules! slog {
    ($($arg:tt)*) => {{
        branchkit_stage_sdk::stage_log::info(&format!($($arg)*));
    }};
}
pub(crate) use slog;

#[tokio::main(flavor = "current_thread")]
async fn main() {
    branchkit_stage_sdk::stage::run(run()).await
}

async fn run() -> branchkit_stage_sdk::stage::Result {
    let args: Vec<String> = env::args().skip(1).collect();

    // `probe` is a one-shot offline batch decode evaluator (the calibration
    // re-decode harness): it reads a JSON request, decodes captured WAVs against
    // swapped grammars + optional noise, and writes NDJSON results. It bypasses
    // the stdio pipeline entirely — dispatch before arg parsing.
    if args.first().map(|s| s.as_str()) == Some("probe") {
        return probe::run(&args[1..]);
    }

    let stage_args = parse_args(&args)?;
    offline::run(stage_args).await
}

pub(crate) fn pcm_to_f32(payload: &[u8]) -> Vec<f32> {
    payload
        .chunks_exact(2)
        .map(|b| i16::from_le_bytes([b[0], b[1]]) as f32 / 32768.0)
        .collect()
}

pub(crate) fn find_onnx(dir: &std::path::Path) -> Option<PathBuf> {
    let mut onnx: Vec<PathBuf> = std::fs::read_dir(dir)
        .ok()?
        .flatten()
        .map(|e| e.path())
        .filter(|p| p.extension().and_then(|s| s.to_str()) == Some("onnx"))
        // The offline model dir also ships the VAD model (silero_vad.onnx) beside
        // the acoustic model; never mistake it for the acoustic ONNX (feeding it to
        // the recognizer throws an uncatchable C++ exception → abort).
        .filter(|p| {
            let name = p
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or("")
                .to_lowercase();
            !name.contains("silero") && !name.contains("vad")
        })
        .collect();
    // Prefer the fp32 model when both fp32 and int8 are present. fp32 is the
    // shipped default (fixes a real-mic miss int8 drops — BAT->BACK — at
    // identical latency, ~14% more CPU). int8 stays parked as the low-power
    // option but is never auto-selected.
    onnx.sort_by_key(|p| p.to_string_lossy().contains("int8"));
    onnx.into_iter().next()
}

pub(crate) struct StageArgs {
    pub(crate) model: String,
    pub(crate) graph: Option<String>,
    pub(crate) lifecycle_mode: LifecycleMode,
    // Direction 2 in-process dynamic grammar: rebuild + swap the HL grammar when
    // the platform pushes new words. With no bundled HL.fst this is also what
    // self-seeds the grammar at startup. OFF by default — opt in with
    // `--dynamic-grammar true`.
    pub(crate) dynamic_grammar: bool,
    pub(crate) vad_model: Option<String>,
    pub(crate) pre_roll: f32,
    pub(crate) post_roll: f32,
    pub(crate) min_silence: f32,
    pub(crate) min_speech: f32,
    pub(crate) vad_threshold: f32,
    pub(crate) max_active: i32,
    pub(crate) max_speech: f32,
    /// Debug capture: dir to write each decoded VAD segment's wav + manifest line.
    /// Falls back to the `BRANCHKIT_DUMP_SEGMENTS` env var when the flag is absent.
    pub(crate) dump_segments: Option<String>,
}

#[derive(Clone, Copy, PartialEq)]
pub(crate) enum LifecycleMode {
    Session,
    Continuous,
}

fn parse_args(args: &[String]) -> Result<StageArgs, String> {
    let mut model = None;
    let mut graph = None;
    let mut lifecycle_mode = None;
    let mut dynamic_grammar = false;
    let mut vad_model = None;
    // Pre/post-roll re-expand each VAD segment (silero trims fricative onsets).
    // 0.25s/0.10s matched the passing segmentation gate (spike, 2026-06-15).
    // pre_roll is mandatory, not a tuning nicety: silero trims soft/fricative
    // onsets (`snap`->`TOP`, `harp pit`->`PIT`); measured 2026-06-15, 0ms
    // pre-roll decoded 25/36 real-mic fixtures, 100ms decoded 36/36.
    let mut pre_roll = 0.25f32;
    let mut post_roll = 0.10f32;
    // min_silence: the eager end-of-speech pause. The chosen operating point is
    // SHORT (~0.15s) + the matcher bridge as the fragment rule; we default to the
    // 0.30s "0 over-split" safe floor until the Phase-4 continuous bridge clear is
    // verified, then flip down. Tunable for A/B. Below ~0.30s it splits
    // multi-word commands on natural intra-command pauses (1/15 at 200ms, 4/15
    // at 150ms); the matcher bridge re-merges those, so shorter is a latency
    // trade, not a correctness one.
    let mut min_silence = 0.30f32;
    let mut min_speech = 0.10f32;
    let mut vad_threshold = 0.5f32;
    // FST search beam.
    let mut max_active = 3000i32;
    // Cap a single VAD segment; bounds the ring buffer too.
    let mut max_speech = 15.0f32;
    // Debug capture: write each decoded VAD segment's wav + a manifest line to
    // this dir, building a fixture set of real misfires. Off by default.
    let mut dump_segments = None;
    let mut positional = Vec::new();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--model" => {
                i += 1;
                model = Some(args.get(i).ok_or("--model requires a value")?.clone());
            }
            "--graph" => {
                i += 1;
                graph = Some(args.get(i).ok_or("--graph requires a value")?.clone());
            }
            "--lifecycle_mode" => {
                i += 1;
                lifecycle_mode = Some(
                    args.get(i)
                        .ok_or("--lifecycle_mode requires a value")?
                        .clone(),
                );
            }
            "--dynamic-grammar" => {
                i += 1;
                dynamic_grammar = match args.get(i).map(|s| s.as_str()) {
                    Some("true") | Some("1") => true,
                    Some("false") | Some("0") => false,
                    _ => return Err("--dynamic-grammar must be true|false".into()),
                };
            }
            "--vad-model" => {
                i += 1;
                vad_model = Some(args.get(i).ok_or("--vad-model requires a value")?.clone());
            }
            "--pre-roll" => {
                i += 1;
                pre_roll = args
                    .get(i)
                    .ok_or("--pre-roll requires a value")?
                    .parse()
                    .map_err(|_| "--pre-roll must be a float")?;
            }
            "--post-roll" => {
                i += 1;
                post_roll = args
                    .get(i)
                    .ok_or("--post-roll requires a value")?
                    .parse()
                    .map_err(|_| "--post-roll must be a float")?;
            }
            "--min-silence" => {
                i += 1;
                min_silence = args
                    .get(i)
                    .ok_or("--min-silence requires a value")?
                    .parse()
                    .map_err(|_| "--min-silence must be a float")?;
            }
            "--min-speech" => {
                i += 1;
                min_speech = args
                    .get(i)
                    .ok_or("--min-speech requires a value")?
                    .parse()
                    .map_err(|_| "--min-speech must be a float")?;
            }
            "--vad-threshold" => {
                i += 1;
                vad_threshold = args
                    .get(i)
                    .ok_or("--vad-threshold requires a value")?
                    .parse()
                    .map_err(|_| "--vad-threshold must be a float")?;
            }
            "--max-active" => {
                i += 1;
                max_active = args
                    .get(i)
                    .ok_or("--max-active requires a value")?
                    .parse()
                    .map_err(|_| "--max-active must be an integer")?;
            }
            "--max-speech" => {
                i += 1;
                max_speech = args
                    .get(i)
                    .ok_or("--max-speech requires a value")?
                    .parse()
                    .map_err(|_| "--max-speech must be a float")?;
            }
            "--dump-segments" => {
                i += 1;
                dump_segments = Some(
                    args.get(i)
                        .ok_or("--dump-segments requires a directory")?
                        .clone(),
                );
            }
            // Unknown `--flags` are ignored (lenient, so a stale pipeline param like
            // the retired `--engine` can't crash the stage); bare positionals are a
            // model-name fallback.
            other if !other.starts_with("--") => positional.push(other.to_string()),
            _ => {}
        }
        i += 1;
    }
    let model = model
        .or_else(|| positional.first().cloned())
        .unwrap_or_else(|| "sherpa".to_string());
    // Continuous mode segments on its own, and voice's command pipeline has no
    // vad_gate in front of this stage on purpose: an external VAD gate produced
    // 2-4 s deaf windows between sessions, dropping commands silently.
    // The dictation pipeline has no vad_gate either: WhisperKit
    // transcribes the whole hold and does its own chunking past 30 s.
    let mode = match lifecycle_mode.as_deref() {
        Some("continuous") => LifecycleMode::Continuous,
        _ => LifecycleMode::Session,
    };
    Ok(StageArgs {
        model,
        graph,
        lifecycle_mode: mode,
        dynamic_grammar,
        vad_model,
        pre_roll,
        post_roll,
        min_silence,
        min_speech,
        vad_threshold,
        max_active,
        max_speech,
        dump_segments,
    })
}

/// Resolve a model dir name against the platform model root (`<root>/<name>`).
/// Absolute/existing paths pass through.
///
/// The root arrives via `BRANCHKIT_ARTIFACTS_DIR` (`ARTIFACTS_DIR_ENV`), set by the platform at spawn
/// from the SAME derivation its sandbox grant uses (`grant_named_artifact`) —
/// never re-derived here. A second spelling does not merely disagree — it
/// points the stage at a directory the profile denies; this function used to
/// carry its own `BRANCHKIT_DEV` branch that disagreed in exactly that way,
/// and after that fix it linked the whole platform for this one path.
pub(crate) fn resolve_model_dir(name: &str) -> PathBuf {
    let p = PathBuf::from(name);
    if p.is_absolute() || p.exists() {
        return p;
    }
    if let Some(root) = std::env::var_os(branchkit_stage_sdk::ARTIFACTS_DIR_ENV) {
        let resolved = PathBuf::from(root).join(&p);
        if resolved.exists() {
            return resolved;
        }
    }
    p
}
