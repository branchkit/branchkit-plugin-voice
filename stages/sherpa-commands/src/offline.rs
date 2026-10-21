//! Offline (encode-once) command engine — the non-streaming command path.
//!
//! Talon-shaped: a silero VAD detects end-of-speech and bounds each utterance; the
//! offline CTC+HLG recognizer decodes the whole buffered segment once (full
//! bidirectional context, no streaming settle floor); the matcher slices the
//! decoded phrase into commands by grammar and bridges any mid-command split. The
//! VAD has ONE job (find a pause); the matcher does all command separation.
//!
//! This is the stage's ONLY engine: `main.rs` routes every run here (the
//! streaming engine and its `--engine` selector were retired; `main.rs` still
//! swallows the flag so stale pipeline params can't crash the stage).

use std::collections::{HashMap, HashSet, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Condvar, Mutex, mpsc};
use std::time::Instant;

use branchkit_stage_sdk::events::{AudioChunk, AudioStart, AudioStop, Transcript, event_type};
use branchkit_stage_sdk::grammar_dag::GrammarDagWire;
use branchkit_stage_sdk::stage::{
    self, AudioConsumer, AudioCtx, Chunk, CreditPolicy, Flow, InitialGrant, Result,
};
use branchkit_stage_sdk::wire::Event;
use serde_json::Value;

use crate::engine::{BuiltGraph, OfflineRecognizer, Vad};
use crate::{StageArgs, find_onnx, pcm_to_f32, resolve_model_dir, slog};

const SAMPLE_RATE: i32 = 16000;

/// Ring-buffered pre/post-roll extractor. silero trims a segment's start to where
/// speech-probability crosses threshold, eating soft/fricative onsets (`snap`→`nap`,
/// `harp`→`arp`); re-expanding each segment with a ~250ms pre-roll recovers them
/// (spike: 0ms → 25/36 fixtures, 100ms → 36/36). A small post-roll catches a clipped
/// tail. Kept pure so the index math is table-testable.
///
/// `fed` mirrors the VAD's internal sample clock exactly (we push the same samples to
/// both), so a segment's absolute `start` indexes straight into the retained audio.
pub struct Segmenter {
    ring: VecDeque<f32>,
    ring_start: i64, // absolute sample index of ring.front()
    fed: i64,        // total samples pushed (== VAD clock since last reset)
    capacity: usize,
    pre: usize,
    post: usize,
}

impl Segmenter {
    pub fn new(sample_rate: usize, pre_s: f32, post_s: f32, max_buffer_s: f32) -> Self {
        let capacity = (sample_rate as f32 * max_buffer_s) as usize;
        Self {
            ring: VecDeque::with_capacity(capacity),
            ring_start: 0,
            fed: 0,
            capacity: capacity.max(1),
            pre: (sample_rate as f32 * pre_s) as usize,
            post: (sample_rate as f32 * post_s) as usize,
        }
    }

    /// Retain `samples` (also fed to the VAD in lockstep) and advance the clock.
    pub fn push(&mut self, samples: &[f32]) {
        self.ring.extend(samples.iter().copied());
        self.fed += samples.len() as i64;
        while self.ring.len() > self.capacity {
            self.ring.pop_front();
            self.ring_start += 1;
        }
    }

    pub fn fed(&self) -> i64 {
        self.fed
    }

    /// Expand a VAD segment `[start, start+seg_len)` to `[start-pre, end+post)`,
    /// sliced from retained audio (clamped to what the ring still holds).
    pub fn expand(&self, start: i64, seg_len: usize) -> Vec<f32> {
        let end = start + seg_len as i64;
        let from = self.expanded_start(start);
        let to = (end + self.post as i64).min(self.fed);
        self.extract(from, to)
    }

    /// Absolute sample index where [`Self::expand`] of a segment starting at
    /// `start` begins — the base for converting a decode's in-buffer token
    /// timestamps back to absolute sample positions.
    pub fn expanded_start(&self, start: i64) -> i64 {
        // `ring_start` is the floor: 0 until the ring evicts, and below 0
        // after a [`Self::rebase`], when audio fed before it is still held.
        (start - self.pre as i64).max(self.ring_start)
    }

    fn extract(&self, from: i64, to: i64) -> Vec<f32> {
        if to <= from {
            return Vec::new();
        }
        let lo = (from - self.ring_start).max(0) as usize;
        let hi = ((to - self.ring_start).max(0) as usize).min(self.ring.len());
        if hi <= lo {
            return Vec::new();
        }
        self.ring.iter().skip(lo).take(hi - lo).copied().collect()
    }

    /// Clear at a session boundary; the next VAD reset restarts its clock at 0, so
    /// the segmenter's clock must restart too (they stay in lockstep).
    pub fn reset(&mut self) {
        self.ring.clear();
        self.ring_start = 0;
        self.fed = 0;
    }

    /// Restart the clock at 0 for a VAD reset that is not a session boundary
    /// (see [`vad_clock_due`]), KEEPING the retained audio: it moves to
    /// negative indices, so a segment the reset VAD opens at once still gets
    /// its pre-roll. Returns the shift, which the caller subtracts from every
    /// other position it holds on the old clock.
    pub fn rebase(&mut self) -> i64 {
        let shift = self.fed;
        self.ring_start -= shift;
        self.fed = 0;
        shift
    }
}

/// sherpa-onnx's VAD keeps its sample clock in an `int32_t` that only grows
/// (`csrc/circular-buffer.h`, v1.13.2: `head_`/`tail_`, "always increasing;
/// never wraps around") and is cleared only by a reset. At 16 kHz that
/// overflows after 2^31 samples, about 37.3 h, and the buffer then indexes
/// with a negative `% capacity`: memory corruption or a crash. A continuous
/// command pipeline is reset only at `audio_start` / `audio_stop`, and audio
/// flows to it around the clock, so a hands-free user who leaves listening on
/// reaches it in under two days.
///
/// The stage resets the clock itself: at the first pause after
/// [`VAD_CLOCK_SOFT_SAMPLES`] (one hour), where a reset loses nothing, and
/// unconditionally at [`VAD_CLOCK_HARD_SAMPLES`] (twelve hours), so audio the
/// VAD never stops calling speech cannot hold it off.
const VAD_CLOCK_SOFT_SAMPLES: i64 = SAMPLE_RATE as i64 * 3600;
const VAD_CLOCK_HARD_SAMPLES: i64 = SAMPLE_RATE as i64 * 3600 * 12;

/// Whether the VAD clock is to be reset after this chunk: `fed` is the clock,
/// `speaking` whether a segment is open, `queued` whether a closed one waits.
fn vad_clock_due(fed: i64, speaking: bool, queued: bool) -> bool {
    fed >= VAD_CLOCK_HARD_SAMPLES || (fed >= VAD_CLOCK_SOFT_SAMPLES && !speaking && !queued)
}

/// One decoded segment plus the timing needed to size command latency.
struct SegResult {
    text: String,
    /// Absolute sample index of the segment's speech-end (the VAD's clock), used
    /// to anchor the word-end→emit latency against the audio-arrival clock.
    audio_end: i64,
    /// Absolute sample index where the expanded (pre-rolled) decode buffer
    /// begins — the base the in-buffer `word_onsets_sec` are relative to.
    expanded_start: i64,
    /// Onset (seconds into the expanded buffer) of each word of `text`,
    /// derived from the decode's BPE token timestamps; `None` when the token
    /// readout doesn't align with the cleaned text.
    word_onsets_sec: Option<Vec<f32>>,
    /// Per-word acoustic margin (min over the word's token margins), aligned
    /// 1:1 with `text`'s words; `None` when the readout doesn't align or the
    /// linked sherpa lacks the posteriors patch (empty `margins`).
    word_scores: Option<Vec<f32>>,
    /// Offline encode-once wall-time (the model's per-command compute burst).
    encode_ms: u128,
}

/// Group a decode's BPE pieces into words, calling `on_piece(word_index,
/// token_index)` for every non-blank piece in decode order. A piece opens a new
/// word when sentencepiece marked it word-initial — sherpa's JSON readout
/// renders that marker as a leading SPACE or '▁' (probed live 2026-08-08:
/// "BOTTOM" → ["<blk>", " bo", "<blk>", "t", "<blk>", "t", "om", "<blk>"]) —
/// or when it is the first non-blank piece; NeMo's literal blank token and
/// empty pieces are transparent. This is the SINGLE definition of the
/// piece→word grouping, folded two ways by [`word_onsets_sec`] and
/// [`word_scores`]; keeping it in one place stops the two derivations drifting
/// apart when the marker handling is next corrected.
fn group_word_pieces<F: FnMut(usize, usize)>(tokens: &[String], mut on_piece: F) {
    let mut cur: Option<usize> = None;
    for (i, tok) in tokens.iter().enumerate() {
        if tok == "<blk>" || tok.trim().is_empty() {
            continue;
        }
        let is_start = tok.starts_with(' ') || tok.starts_with('\u{2581}');
        let word = match cur {
            None => 0,                    // first non-blank piece opens word 0
            Some(w) if is_start => w + 1, // a marked piece opens a new word
            Some(w) => w,                 // continuation piece of the same word
        };
        cur = Some(word);
        on_piece(word, i);
    }
}

/// Per-word onsets (seconds into the decoded buffer): the first piece's
/// timestamp for each word. `None` unless the timestamps are parallel to the
/// tokens and the grouping aligns 1:1 with the cleaned text's words — a wrong
/// grouping must degrade to no onsets, never to wrong onsets.
fn word_onsets_sec(detail: &crate::engine::DecodeDetail) -> Option<Vec<f32>> {
    if detail.timestamps.len() != detail.tokens.len() {
        return None;
    }
    let mut onsets: Vec<f32> = Vec::new();
    group_word_pieces(&detail.tokens, |word, i| {
        if word == onsets.len() {
            onsets.push(detail.timestamps[i]); // word-start: the onset
        }
        // continuation pieces don't move the onset
    });
    let words = detail.text.split_whitespace().count();
    (words > 0 && onsets.len() == words).then_some(onsets)
}

/// Per-word acoustic scores from a decode's per-token argmax margins (the
/// engine-posteriors patch). A word's
/// score is the MIN over its token margins — one coerced piece marks the whole
/// word. `None` unless the margins are parallel to the tokens (an unpatched lib
/// emits an empty array) and the grouping aligns 1:1 with the cleaned text's
/// words.
pub(crate) fn word_scores(detail: &crate::engine::DecodeDetail) -> Option<Vec<f32>> {
    if detail.margins.len() != detail.tokens.len() {
        return None;
    }
    let mut scores: Vec<f32> = Vec::new();
    group_word_pieces(&detail.tokens, |word, i| {
        let m = detail.margins[i];
        if word == scores.len() {
            scores.push(m); // word-start
        } else if let Some(last) = scores.last_mut() {
            *last = last.min(m); // continuation: min-fold
        }
    });
    let words = detail.text.split_whitespace().count();
    (words > 0 && scores.len() == words).then_some(scores)
}

/// Compact per-word margin readout for a `[PERF]` line — the field-soak
/// distribution source (log only, nothing enforced). `margins=none` when the
/// decode carried no usable scores.
fn margin_tag(scores: Option<&[f32]>) -> String {
    match scores {
        Some(s) if !s.is_empty() => {
            let mean = s.iter().sum::<f32>() / s.len() as f32;
            let per_word: Vec<String> = s.iter().map(|m| format!("{m:+.1}")).collect();
            format!("margins=[{}] mean={mean:+.1}", per_word.join(","))
        }
        _ => "margins=none".to_string(),
    }
}

/// Debug capture (`--dump-segments <dir>`). Writes the EXACT audio buffer handed
/// to the offline recognizer (the pre/post-roll-expanded segment) as a 16kHz mono
/// WAV plus a `manifest.jsonl` line carrying the decode + timing — a fixture set of
/// real misfires to measure candidate acoustic models against. Capture must never
/// disturb recognition, so every I/O failure is logged and swallowed, not fatal.
struct SegmentDumper {
    dir: PathBuf,
    seq: usize,
}

impl SegmentDumper {
    /// Create the dir and seed the counter past any existing `seg_*.wav` so repeated
    /// capture sessions append instead of clobbering an earlier fixture set.
    fn create(dir: &str) -> std::io::Result<Self> {
        let dir = PathBuf::from(dir);
        std::fs::create_dir_all(&dir)?;
        let seq = std::fs::read_dir(&dir)
            .map(|rd| {
                rd.flatten()
                    .filter(|e| {
                        e.file_name()
                            .to_str()
                            .map(|n| n.starts_with("seg_") && n.ends_with(".wav"))
                            .unwrap_or(false)
                    })
                    .count()
            })
            .unwrap_or(0);
        Ok(Self { dir, seq })
    }

    /// Write one segment: `seg_<NNNNN>.wav` (the decoded buffer) + a manifest line.
    /// `samples` is the expanded buffer the recognizer actually decoded, so a future
    /// eval that decodes the wav reproduces the captured `text`. `word_scores` is the
    /// live decode's per-word acoustic margins (the engine-posteriors signal), aligned
    /// 1:1 with `text`'s words — persisted so a captured calibration clip carries a
    /// ground-truth margin record, not just its decoded text (`None` = unpatched lib
    /// or a decode whose readout didn't align).
    fn dump(
        &mut self,
        samples: &[f32],
        text: &str,
        word_scores: Option<&[f32]>,
        encode_ms: u128,
        session: &str,
    ) {
        let name = format!("seg_{:05}.wav", self.seq);
        let wav_path = self.dir.join(&name);
        if let Err(e) = write_wav_16k_mono(&wav_path, samples) {
            slog!(
                "dump-segments: failed to write {}: {}",
                wav_path.display(),
                e
            );
            return;
        }
        let duration_ms = (samples.len() as f32 / SAMPLE_RATE as f32 * 1000.0) as u32;
        let line = serde_json::json!({
            "seq": self.seq,
            "file": name,
            "text": text,
            "word_scores": word_scores,
            "encode_ms": encode_ms,
            "samples": samples.len(),
            "duration_ms": duration_ms,
            "session": session,
            "ts": chrono::Utc::now().format("%Y-%m-%dT%H:%M:%S%.3fZ").to_string(),
        })
        .to_string();
        if let Err(e) = append_line(&self.dir.join("manifest.jsonl"), &line) {
            slog!("dump-segments: failed to append manifest: {e}");
        }
        slog!("dump-segments: wrote {name} (\"{text}\", {duration_ms}ms)");
        self.seq += 1;
    }
}

/// Write a canonical 16kHz mono 16-bit PCM WAV — the format the whole pipeline
/// speaks, so fixtures replay straight through `wav_source` into the offline stage.
fn write_wav_16k_mono(path: &Path, samples: &[f32]) -> std::result::Result<(), hound::Error> {
    let spec = hound::WavSpec {
        channels: 1,
        sample_rate: SAMPLE_RATE as u32,
        bits_per_sample: 16,
        sample_format: hound::SampleFormat::Int,
    };
    let mut w = hound::WavWriter::create(path, spec)?;
    for &s in samples {
        w.write_sample((s.clamp(-1.0, 1.0) * 32767.0).round() as i16)?;
    }
    w.finalize()
}

fn append_line(path: &Path, line: &str) -> std::io::Result<()> {
    use std::io::Write;
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)?;
    writeln!(f, "{line}")
}

/// Drain every closed VAD segment, expand with pre/post-roll, encode-once, and
/// return the non-empty decoded transcripts (uppercase, as the model emits) with
/// per-segment timing. When a dumper is present, every expanded segment is captured
/// to disk — including empty decodes (a dropped command is itself a failure to study).
fn drain_segments(
    vad: &Vad,
    seg: &Segmenter,
    recognizer: &OfflineRecognizer,
    dumper: &mut Option<SegmentDumper>,
    session: &str,
) -> Vec<SegResult> {
    let mut out = Vec::new();
    while let Some(s) = vad.front() {
        let audio_end = s.start + s.samples.len() as i64;
        let expanded_start = seg.expanded_start(s.start);
        let expanded = seg.expand(s.start, s.samples.len());
        vad.pop();
        if expanded.is_empty() {
            continue;
        }
        let t0 = Instant::now();
        let detail = recognizer.decode_detail(&expanded);
        let encode_ms = t0.elapsed().as_millis();
        let text = detail.text.trim().to_string();
        // Computed once here so the captured clip's manifest records the SAME
        // margins the SegResult carries downstream — one decode, one record.
        let word_scores = word_scores(&detail);
        if let Some(d) = dumper.as_mut() {
            d.dump(&expanded, &text, word_scores.as_deref(), encode_ms, session);
        }
        if !text.is_empty() {
            let word_onsets_sec = word_onsets_sec(&detail);
            out.push(SegResult {
                text,
                audio_end,
                expanded_start,
                word_onsets_sec,
                word_scores,
                encode_ms,
            });
        } else {
            // A VAD-qualified segment that decoded to NOTHING. Under a closed
            // grammar this is what off-grammar speech looks like (blank beats
            // every path — e.g. a codeword from a stale badge / retired deck),
            // and it is invisible to the user ("said it, nothing happened").
            // Log it so the failure has a grep signature; the greedy_probe test
            // (engine.rs) recovers the actual words from a --dump-segments
            // capture. (2026-07-02: this silence cost a full mis-diagnosis —
            // empty pair decodes were read as a missing-DAG-path regression.)
            slog!(
                "[PERF] empty decode (offline segment): {}ms audio matched no grammar path \
                 (off-grammar words, or noise past the VAD) encode={}ms",
                (expanded.len() as f32 / SAMPLE_RATE as f32 * 1000.0) as u32,
                encode_ms
            );
        }
    }
    out
}

/// One per-chunk clock entry: cumulative samples fed when a chunk arrived,
/// paired with the two timebases that map a decode's sample position back to a
/// real time — the local wall `Instant` (latency metric) and the machine-shared
/// `stamp_ms` (the AudioChunk `timestamp_ms` other pipelines share). Both live
/// on ONE entry so a reset or push can never leave the two out of sync.
#[derive(Clone, Copy)]
struct ClockTick {
    fed: i64,
    wall: Instant,
    stamp_ms: u64,
}

/// The wall-clock when the audio sample at `target` was received — the arrival
/// time of the first chunk whose cumulative fed-sample count reached it. Anchors
/// the honest word-audio-end → emit latency (the design's "finding #2" metric:
/// measure from real speech-end, not the recognizer's internal last-speech).
fn wall_at(clock: &VecDeque<ClockTick>, target: i64) -> Option<Instant> {
    clock.iter().find(|t| t.fed >= target).map(|t| t.wall)
}

/// The shared-clock stamp (an AudioChunk `timestamp_ms`) of the audio sample at
/// `target`: the stamp of the first chunk whose cumulative fed-sample count
/// reached it, backed off by the in-chunk distance. This is the bridge from a
/// decode's sample positions to the machine-wide chunk timebase other
/// pipelines share, so a stop phrase's onset can cut the dictation audio.
fn stamp_at(clock: &VecDeque<ClockTick>, target: i64) -> Option<u64> {
    clock.iter().find(|t| t.fed >= target).map(|t| {
        t.stamp_ms
            .saturating_sub(((t.fed - target).max(0) as u64 * 1000) / SAMPLE_RATE as u64)
    })
}

/// Convert a segment decode's per-word token onsets to shared-clock ms, one
/// entry per word of the emitted text. `None` when the decode carried no
/// aligned token onsets, or when any position predates the stamp window.
///
/// Emitting `None` rather than a fabricated fallback is deliberate: the only
/// consumer is the dictation stop-phrase audio cutoff, which reads
/// `onsets[stop_word_index]`. A fabricated per-word onset (the old
/// segment-start fill) makes that index resolve to the segment START, so the
/// cutoff drops the ENTIRE VAD segment — potentially seconds of the user's
/// real dictation in a one-breath "…last sentence speak over". Absent onsets
/// instead yield a nil cutoff (whisper transcribes everything) and the phrase
/// is removed by the text-domain backstop (speech.StripSpeakPhrases). Losing
/// nothing beats silently dropping a sentence.
fn onsets_to_shared_ms(
    onsets_sec: Option<&[f32]>,
    expanded_start: i64,
    stamps: &VecDeque<ClockTick>,
) -> Option<Vec<u64>> {
    let onsets_sec = onsets_sec?;
    let mut out = Vec::with_capacity(onsets_sec.len());
    for s in onsets_sec {
        let smp = expanded_start + (*s as f64 * SAMPLE_RATE as f64) as i64;
        out.push(stamp_at(stamps, smp)?);
    }
    (!out.is_empty()).then_some(out)
}

/// Parse the optional `word_weights` per-word bias map from a `vocabulary_update`
/// payload. Shape: a sparse JSON object `{ "WORD": cost, ... }` — only biased
/// words are listed; everything else is neutral. Keys are uppercased to match the
/// uppercased word union. Absent or non-object -> empty (unweighted). A sparse map
/// (not a parallel array) is deliberate: it keys bias to word *identity*, so it
/// can't drift out of alignment with the separately-built `words` union.
fn parse_word_weights(data: &Value) -> HashMap<String, f32> {
    data.get("word_weights")
        .and_then(|v| v.as_object())
        .map(|m| {
            m.iter()
                .filter_map(|(k, v)| v.as_f64().map(|f| (k.to_uppercase(), f as f32)))
                .collect()
        })
        .unwrap_or_default()
}

/// Parse the optional `grammar_dag` field (command-grammar compiler D2): the
/// structured word-level DAG the platform stamps onto a `vocabulary_update` when
/// the structured-grammar toggle is on. Absent / null / malformed -> `None`, and
/// the stage falls back to the flat word-list grammar. The DAG's arc words stay
/// in the platform's (lowercase) casing; the recognizer folds them and the union
/// to the model's case past the FFI, so the two line up.
fn parse_grammar_dag(data: &Value) -> Option<GrammarDagWire> {
    data.get("grammar_dag")
        .filter(|v| !v.is_null())
        .and_then(|v| serde_json::from_value(v.clone()).ok())
}

/// Materialize the FFI-parallel weight vector for `words` from a sparse bias map.
/// Empty map -> empty vec (the recognizer's unweighted fast path). Words absent
/// from the map get 0 (neutral). `words` and the map keys are both uppercase, so
/// lookups align; the recognizer lowercases both sides positionally past the FFI.
fn materialize_weights(words: &[String], bias: &HashMap<String, f32>) -> Vec<f32> {
    if bias.is_empty() {
        return Vec::new();
    }
    words.iter().map(|w| *bias.get(w).unwrap_or(&0.0)).collect()
}

/// Compact tag describing the per-word bias active at decode time, appended to the
/// recognized `[PERF]` line so every command is self-describing: the penalty
/// strength and how many words it weighted (`bias=off` when none). All entries
/// share one strength today, so the first value is representative.
fn bias_tag(weights: &HashMap<String, f32>) -> String {
    match weights.values().next() {
        Some(n) => format!("bias={n}/{}w", weights.len()),
        None => "bias=off".to_string(),
    }
}

/// One grammar the platform asked for: the word union (uppercase, sorted),
/// its per-word bias, and the structured DAG (`None` = the flat
/// word-list grammar).
struct GrammarSpec {
    words: Vec<String>,
    weights: HashMap<String, f32>,
    dag: Option<GrammarDagWire>,
}

/// A grammar built off the decode thread, waiting to be installed.
struct BuiltGrammar {
    generation: u64,
    spec: GrammarSpec,
    graph: Option<BuiltGraph>,
    build_ms: u128,
}

/// Build one grammar on the recognizer. Structured (D2) when the DAG is
/// present: the DAG constrains word *sequences* and the union is the lexicon
/// basis + the open-tail alphabet; the never-standalone per-word bias is not
/// applied there, structure supersedes it (a leading number simply has no
/// arc). Otherwise the flat word list with its bias. Words are uppercased
/// to match the model lexicon / BPE.
fn build_grammar(recognizer: &OfflineRecognizer, spec: &GrammarSpec) -> Option<BuiltGraph> {
    match &spec.dag {
        Some(d) => recognizer.build_graph_from_dag(d, &spec.words),
        None => {
            let weights = materialize_weights(&spec.words, &spec.weights);
            recognizer.build_graph_from_words(&spec.words, &weights)
        }
    }
}

/// The latest requested grammar, for the worker to pick up.
struct BuildSlot {
    next: Option<(u64, GrammarSpec)>,
    closed: bool,
}

/// Builds grammars on a worker thread so a build never blocks decoding
/// (swaps on the decode loop measured p90 218 ms, cache-miss
/// builds up to 7.5 s, and speech arriving meanwhile waited). The slot holds
/// only the newest request: one submitted while a build runs replaces any
/// older queued one, and a build finished after a newer request arrived is
/// dropped on the worker, so the decode thread never frees a superseded
/// graph. Builds run one at a time (the C++ builder is serialized anyway).
struct GrammarWorker {
    shared: Arc<(Mutex<BuildSlot>, Condvar)>,
    results: mpsc::Receiver<BuiltGrammar>,
}

impl GrammarWorker {
    fn spawn(recognizer: Arc<OfflineRecognizer>) -> std::io::Result<Self> {
        let shared = Arc::new((
            Mutex::new(BuildSlot {
                next: None,
                closed: false,
            }),
            Condvar::new(),
        ));
        let (tx, results) = mpsc::channel();
        let worker_shared = Arc::clone(&shared);
        std::thread::Builder::new()
            .name("grammar-build".into())
            .spawn(move || {
                // A decode can wait on this build (a segment spoken after the
                // request), so it runs at the decode thread's priority.
                crate::engine::raise_current_thread_to_interactive();
                let (slot, wake) = &*worker_shared;
                loop {
                    let (generation, spec) = {
                        let mut s = slot.lock().unwrap_or_else(|e| e.into_inner());
                        loop {
                            if s.closed {
                                return;
                            }
                            if let Some(job) = s.next.take() {
                                break job;
                            }
                            s = wake.wait(s).unwrap_or_else(|e| e.into_inner());
                        }
                    };
                    let t0 = Instant::now();
                    let graph = build_grammar(&recognizer, &spec);
                    let build_ms = t0.elapsed().as_millis();
                    if slot.lock().unwrap_or_else(|e| e.into_inner()).next.is_some() {
                        // Superseded while building: free it here.
                        continue;
                    }
                    let built = BuiltGrammar {
                        generation,
                        spec,
                        graph,
                        build_ms,
                    };
                    if tx.send(built).is_err() {
                        return;
                    }
                }
            })?;
        Ok(Self { shared, results })
    }

    fn submit(&self, generation: u64, spec: GrammarSpec) {
        let (slot, wake) = &*self.shared;
        slot.lock().unwrap_or_else(|e| e.into_inner()).next = Some((generation, spec));
        wake.notify_one();
    }

    /// A finished build, without waiting.
    fn try_take(&self) -> Option<BuiltGrammar> {
        self.results.try_recv().ok()
    }

    /// The next finished build, waiting for it. `None` = the worker is gone.
    fn wait(&self) -> Option<BuiltGrammar> {
        self.results.recv().ok()
    }
}

impl Drop for GrammarWorker {
    fn drop(&mut self) {
        // Not joined: a cache-miss build can run for seconds, and the stage is
        // exiting. The worker returns at its next look at the slot.
        let (slot, wake) = &*self.shared;
        slot.lock().unwrap_or_else(|e| e.into_inner()).closed = true;
        wake.notify_one();
    }
}

/// Run the offline command engine to completion (stdin closed).
pub async fn run(stage_args: StageArgs) -> Result {
    let continuous = stage_args.lifecycle_mode == crate::LifecycleMode::Continuous;
    let dynamic_grammar = stage_args.dynamic_grammar;

    let model_dir = resolve_model_dir(&stage_args.model);
    let onnx = find_onnx(&model_dir)
        .ok_or_else(|| format!("no .onnx model in {}", model_dir.display()))?;
    let tokens = model_dir.join("tokens.txt");
    let graph = stage_args
        .graph
        .clone()
        .map(|g| resolve_model_dir(&g))
        .unwrap_or_else(|| model_dir.join("HL.fst"));
    let vad_model = stage_args
        .vad_model
        .clone()
        .map(|v| resolve_model_dir(&v))
        .unwrap_or_else(|| model_dir.join("silero_vad.onnx"));

    // tokens + VAD are always required. HL.fst is required ONLY without
    // --dynamic-grammar: with dynamic grammar on, an absent seed is fine — the
    // engine SELF-SEEDS, constructing with no graph (the patched factory yields
    // the greedy decoder) and promoting to the closed-grammar FST decoder on the
    // platform's first vocabulary_update, which is injected at pipeline start
    // before any audio. A bundled HL.fst is still honored as a fallback.
    for (label, p) in [("tokens.txt", &tokens), ("silero_vad.onnx", &vad_model)] {
        if !p.exists() {
            return Err(format!("offline engine: {label} missing at {}", p.display()).into());
        }
    }
    let graph_present = graph.exists();
    if !graph_present && !dynamic_grammar {
        return Err(format!(
            "offline engine: HL.fst missing at {} (required without --dynamic-grammar)",
            graph.display()
        )
        .into());
    }

    let baked: HashSet<String> = if graph_present {
        std::fs::read_to_string(format!("{}.words", graph.display()))
            .map(|s| {
                s.lines()
                    .map(|w| w.trim().to_uppercase())
                    .filter(|w| !w.is_empty())
                    .collect()
            })
            .unwrap_or_default()
    } else {
        HashSet::new()
    };

    // Empty graph path -> the patched factory builds the recognizer with the
    // greedy decoder, which the first SetGraphFromWords promotes to the FST
    // decoder. With a file present this is the existing file-seeded path.
    let graph_arg = if graph_present {
        graph.to_string_lossy().into_owned()
    } else {
        String::new()
    };
    // Interactive priority for every thread that runs an encode: this one
    // (it calls Decode, and onnxruntime's caller joins the intra-op pool) and
    // the workers onnxruntime creates with the recognizer, which only the
    // creation hook can reach.
    let interactive = crate::engine::enable_interactive_inference_threads();
    let main_raised = crate::engine::raise_current_thread_to_interactive();
    let recognizer = OfflineRecognizer::create(
        &onnx.to_string_lossy(),
        &tokens.to_string_lossy(),
        &graph_arg,
        stage_args.max_active,
    )
    .ok_or("offline engine: failed to create recognizer")?;
    let recognizer = Arc::new(recognizer);
    let grammar_worker = GrammarWorker::spawn(Arc::clone(&recognizer))
        .map_err(|e| format!("offline engine: cannot start the grammar-build thread: {e}"))?;

    // A generous ring covers pre-roll + the longest segment + post-roll.
    let seg = Segmenter::new(
        SAMPLE_RATE as usize,
        stage_args.pre_roll,
        stage_args.post_roll,
        stage_args.max_speech + stage_args.pre_roll + stage_args.post_roll + 2.0,
    );
    let vad = Vad::create(
        &vad_model.to_string_lossy(),
        stage_args.vad_threshold,
        stage_args.min_silence,
        stage_args.min_speech,
        512,
        SAMPLE_RATE,
        stage_args.max_speech,
        stage_args.max_speech + 4.0,
    )
    .ok_or("offline engine: failed to create silero VAD")?;

    let graph_label = if graph_present {
        graph.display().to_string()
    } else {
        "(self-seed: no HL.fst, awaiting first vocabulary_update)".to_string()
    };
    slog!(
        "ready (offline encode-once): model={} graph={} ({} baked words) vad={} \
         pre_roll={}s post_roll={}s min_silence={}s min_speech={}s threshold={} \
         max_active={} mode={}",
        onnx.file_name().and_then(|s| s.to_str()).unwrap_or("?"),
        graph_label,
        baked.len(),
        vad_model
            .file_name()
            .and_then(|s| s.to_str())
            .unwrap_or("?"),
        stage_args.pre_roll,
        stage_args.post_roll,
        stage_args.min_silence,
        stage_args.min_speech,
        stage_args.vad_threshold,
        stage_args.max_active,
        if continuous { "continuous" } else { "session" },
    );
    slog!("dynamic_grammar={}", dynamic_grammar);
    if interactive {
        // The workers start asynchronously; their read-back lands within a few
        // ms of session creation.
        let mut stats = crate::engine::interactive_inference_thread_stats();
        for _ in 0..20 {
            if stats.0 > 0 {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(5));
            stats = crate::engine::interactive_inference_thread_stats();
        }
        slog!(
            "inference threads: decode thread {}, {} onnxruntime worker(s) created, {} at interactive priority",
            if main_raised { "raised" } else { "NOT raised" },
            stats.0,
            stats.1
        );
    } else {
        slog!("inference threads: default priority (no supported raise on this OS)");
    }
    // The in-process rebuild needs bpe.vocab for words absent from the model's
    // lexicon.txt; the offline dir has NO lexicon.txt, so every word routes
    // through bpe.vocab — without it a rebuilt/narrowed grammar drops everything.
    if dynamic_grammar && !model_dir.join("bpe.vocab").exists() {
        slog!(
            "WARN: --dynamic-grammar on but bpe.vocab missing in {} — the self-seeded \
             grammar can't tokenize words and will drop everything; re-provision the \
             model (dev: `just sherpa-model-nemo`)",
            model_dir.display()
        );
    }

    let cap = branchkit_stage_sdk::events::Capability::new("command_recognition", "sherpa")
        .audio_format(branchkit_stage_sdk::events::AudioFormat::PCM_16K_MONO)
        .persistent()
        .emits(["transcript", "flow_credit"]);

    // Shallow cadence, continuous-dependent window — an STT engine wants small
    // queues. The numbers are this stage's policy; the loop is the SDK's.
    let credit = CreditPolicy {
        initial: if continuous { 64 } else { 16 },
        every: 4,
        grant: if continuous { 8 } else { 4 },
        when: InitialGrant::OnStart,
    };

    // Debug capture: `--dump-segments <dir>` or the `BRANCHKIT_DUMP_SEGMENTS` env
    // var (the env fallback lets a standalone replay enable capture without editing
    // the plugin's pipeline params). Init failure disables capture, never aborts.
    let dump_dir = stage_args.dump_segments.clone().or_else(|| {
        std::env::var("BRANCHKIT_DUMP_SEGMENTS")
            .ok()
            .filter(|s| !s.is_empty())
    });
    let dumper = match dump_dir {
        Some(dir) => match SegmentDumper::create(&dir) {
            Ok(d) => {
                slog!("dump-segments: capturing decoded VAD segments to {}", dir);
                Some(d)
            }
            Err(e) => {
                slog!("dump-segments: cannot init {dir}: {e} — capture disabled");
                None
            }
        },
        None => None,
    };

    let baked_vocab = baked.clone();
    let mut engine = Engine {
        recognizer,
        grammar_worker,
        requested: 0,
        installed: 0,
        requested_at: None,
        live_weights: HashMap::new(),
        seg,
        vad,
        baked,
        dynamic_grammar,
        dumper,
        current_session: None,
        clock: VecDeque::new(),
        current_vocab: baked_vocab,
        current_weights: HashMap::new(),
        current_dag: None,
        grammar_seeded: graph_present,
        warned_unseeded: false,
    };

    stage::serve_audio_consumer(cap, credit, &mut engine).await
}

/// The decoding engine plus everything the read loop used to hold as locals.
struct Engine {
    recognizer: Arc<OfflineRecognizer>,
    /// Builds requested grammars off the decode thread.
    grammar_worker: GrammarWorker,
    /// Generation of the newest grammar requested / installed. They differ
    /// while a swap is pending (building, or built and waiting for a
    /// boundary).
    requested: u64,
    installed: u64,
    /// When the pending grammar was requested (for the swap log line).
    requested_at: Option<Instant>,
    /// The per-word bias of the INSTALLED grammar (for the `[PERF]` tag);
    /// `current_weights` is the requested one.
    live_weights: HashMap<String, f32>,
    seg: Segmenter,
    vad: Vad,
    baked: HashSet<String>,
    dynamic_grammar: bool,
    dumper: Option<SegmentDumper>,
    current_session: Option<String>,
    /// Per-chunk clock: cumulative samples fed → (wall Instant, shared stamp_ms),
    /// one entry per arriving chunk. Maps a segment's speech-end sample back to
    /// real time for the latency metric (wall) AND to the machine-shared timebase
    /// other pipelines use for the audio cutoff (stamp_ms). ~20s of ~100ms chunks
    /// covers the longest hangover + segment.
    clock: VecDeque<ClockTick>,
    /// Direction 2 / context narrowing (opt-in via --dynamic-grammar). The grammar
    /// starts as the baked offline vocabulary; when the platform pushes a DIFFERENT
    /// word set (a widen with new words OR a Phase-5c narrow to a subset), the new
    /// grammar is built on the worker and installed between decodes. The rule is
    /// grammar-EPOCH correctness: a segment that had closed when the request
    /// arrived decodes under the grammar the speaker was in (it is drained before
    /// the request is submitted), and every segment closing after it decodes under
    /// the new one (it waits for the build if the build is still running). An
    /// immediate mid-capture narrow would decode an in-flight free-context
    /// utterance against a modal grammar (empty decode, utterance lost), and the
    /// one-breath chain graft relies on the modal swap landing after the segment
    /// carrying the entry word. Off by default — the baked HL covers the static
    /// union. `current_*` hold the newest REQUESTED grammar (the change test).
    current_vocab: HashSet<String>,
    /// The per-word decoding bias currently baked into the live
    /// grammar. A weight-only change (same words, different bias) must still trigger
    /// a rebuild, so this is tracked and compared alongside `current_vocab`.
    current_weights: HashMap<String, f32>,
    /// Command-grammar compiler D2: the structured DAG currently live (None = the
    /// flat word-list grammar). Compared alongside vocab/weights so a structural
    /// change (same word set, different sequence structure) still triggers a swap.
    current_dag: Option<GrammarDagWire>,
    /// True once the recognizer has a real grammar. A file-seeded recognizer
    /// starts ready; a self-seeded one (no HL.fst) starts on the greedy decoder
    /// and only becomes ready after the first vocabulary_update promotes it to the
    /// FST decoder. Until then a decode is open-vocab garbage, so we drain audio
    /// without decoding rather than emit a mis-match. The startup seed is injected
    /// before any audio, so this only guards the rare app-launch race.
    grammar_seeded: bool,
    warned_unseeded: bool,
}

impl Engine {
    /// Hand a grammar to the worker. It is installed by [`Self::install_ready`]
    /// once built, or by [`Self::install_before_decode`] when a segment needs it.
    fn request_grammar(&mut self, spec: GrammarSpec) {
        self.requested += 1;
        self.requested_at = Some(Instant::now());
        self.grammar_worker.submit(self.requested, spec);
    }

    fn swap_pending(&self) -> bool {
        self.installed != self.requested
    }

    /// Install the requested grammar if its build has finished. Never waits.
    fn install_ready(&mut self) {
        while self.swap_pending() {
            let Some(built) = self.grammar_worker.try_take() else {
                return;
            };
            if built.generation == self.requested {
                self.install(built, None);
            }
        }
    }

    /// Before decoding a segment: every segment closing after a request
    /// decodes under the requested grammar, so wait for its build if it is
    /// still running. The build began at the request, so the wait is only
    /// what is left of it.
    fn install_before_decode(&mut self) {
        self.install_ready();
        if !self.swap_pending() {
            return;
        }
        let t0 = Instant::now();
        while self.swap_pending() {
            match self.grammar_worker.wait() {
                Some(built) if built.generation == self.requested => {
                    self.install(built, Some(t0.elapsed().as_millis()));
                }
                Some(_) => {}
                None => {
                    slog!("ERROR: grammar-build thread is gone; decoding with the installed grammar");
                    self.installed = self.requested;
                }
            }
        }
    }

    /// Swap a built grammar onto the recognizer: a pointer swap, so the next
    /// decode uses it. `waited_ms` is how long a decode waited for the build.
    fn install(&mut self, built: BuiltGrammar, waited_ms: Option<u128>) {
        let t0 = Instant::now();
        if let Some(graph) = built.graph {
            self.recognizer.install_graph(graph);
        }
        let install_ms = t0.elapsed().as_secs_f64() * 1000.0;
        let since_request = self
            .requested_at
            .take()
            .map(|t| t.elapsed().as_millis())
            .unwrap_or(0);
        let timing = format!(
            "{install_ms:.2}ms; built in {}ms off the decode thread, {}ms after the request{}",
            built.build_ms,
            since_request,
            waited_ms
                .map(|w| format!(", a segment waited {w}ms"))
                .unwrap_or_default()
        );
        match &built.spec.dag {
            Some(d) => slog!(
                "structured grammar swapped: {} states, {} arcs, {} union words ({timing})",
                d.num_states,
                d.arcs.len(),
                built.spec.words.len()
            ),
            None => slog!(
                "grammar swapped in-process: {} words, {} biased ({timing})",
                built.spec.words.len(),
                built.spec.weights.len()
            ),
        }
        self.live_weights = built.spec.weights;
        self.installed = built.generation;
        // The first swap on a self-seeded (no-HL.fst) recognizer promotes its
        // greedy decoder to the closed-grammar FST decoder — past this point the
        // engine has a real grammar and the decode is safe to emit.
        self.grammar_seeded = true;
    }

    /// Decode every closed segment and emit its transcript.
    async fn drain_and_emit(
        &mut self,
        ctx: &mut AudioCtx,
        fallback_session: &str,
        label: &str,
    ) -> Result {
        let dump_session = self
            .current_session
            .clone()
            .unwrap_or_else(|| fallback_session.to_string());
        for r in drain_segments(
            &self.vad,
            &self.seg,
            &self.recognizer,
            &mut self.dumper,
            &dump_session,
        ) {
            let session_id = self
                .current_session
                .clone()
                .unwrap_or_else(|| fallback_session.to_string());
            // word-audio-end → emit: the real per-command latency (VAD
            // hangover + encode + overhead), measured from speech-end.
            let latency_ms = wall_at(&self.clock, r.audio_end)
                .map(|t| t.elapsed().as_millis())
                .unwrap_or(0);
            slog!(
                "[PERF] recognized (offline {}): \"{}\" latency={}ms encode={}ms {} {}",
                label,
                r.text,
                latency_ms,
                r.encode_ms,
                bias_tag(&self.live_weights),
                margin_tag(r.word_scores.as_deref())
            );
            let onsets =
                onsets_to_shared_ms(r.word_onsets_sec.as_deref(), r.expanded_start, &self.clock);
            emit_transcript(ctx, session_id, r.text, false, onsets, r.word_scores).await?;
        }
        Ok(())
    }

    /// Reset the VAD's sample clock before it can overflow ([`vad_clock_due`]),
    /// moving the segmenter and the chunk clock onto the new one with it.
    fn rebase_vad_clock_if_due(&mut self) {
        let speaking = self.vad.detected();
        if !vad_clock_due(self.seg.fed(), speaking, !self.vad.is_empty()) {
            return;
        }
        self.vad.reset();
        let shift = self.seg.rebase();
        for tick in self.clock.iter_mut() {
            tick.fed -= shift;
        }
        slog!(
            "VAD clock reset after {:.1} h of continuous audio{}",
            shift as f64 / (SAMPLE_RATE as f64 * 3600.0),
            if speaking {
                " (forced, mid-speech: that segment is lost)"
            } else {
                ""
            }
        );
    }

    /// Drop buffered segments without decoding them (grammar not yet seeded).
    fn drop_unseeded(&mut self) {
        while self.vad.front().is_some() {
            self.vad.pop();
        }
    }
}

impl AudioConsumer for Engine {
    async fn on_audio_start(&mut self, ev: AudioStart, ctx: &mut AudioCtx) -> Result {
        if !ev.session_id.is_empty() {
            let sid = ev.session_id;
            branchkit_stage_sdk::stage_log::set_session(&sid);
            slog!("session start: {}", &sid[..8.min(sid.len())]);
            self.current_session = Some(sid);
        }
        self.vad.reset();
        self.seg.reset();
        self.clock.clear();
        // Install a finished grammar build at this boundary (idle now).
        self.install_ready();
        // Session-start re-grant (also resets the cadence counter).
        ctx.grant_now("", 16).await?;
        Ok(())
    }

    async fn on_audio_chunk(
        &mut self,
        chunk: AudioChunk,
        payload: &[u8],
        ctx: &mut AudioCtx,
    ) -> Result<Chunk> {
        // Stamp the ambient log session so the [PERF] recognized lines
        // emitted while draining this segment correlate (covers
        // continuous mode, where audio_start carries no session).
        if !chunk.session_id.is_empty() {
            branchkit_stage_sdk::stage_log::set_session(&chunk.session_id);
        }
        let samples = pcm_to_f32(payload);
        self.seg.push(&samples);
        self.vad.accept(&samples);
        self.clock.push_back(ClockTick {
            fed: self.seg.fed(),
            wall: Instant::now(),
            stamp_ms: chunk.timestamp_ms,
        });
        while self.clock.len() > 200 {
            self.clock.pop_front();
        }

        // A closed segment decodes under the newest requested grammar.
        if !self.vad.is_empty() {
            self.install_before_decode();
        }
        if self.grammar_seeded {
            self.drain_and_emit(ctx, &chunk.session_id, "segment")
                .await?;
        } else {
            // Self-seed startup race: audio arrived before the first
            // vocabulary_update established the grammar. Decoding now would
            // run the un-seeded greedy (open-vocab) decoder and mis-match, so
            // drain the VAD without decoding and wait for the seed.
            self.drop_unseeded();
            if !self.warned_unseeded {
                self.warned_unseeded = true;
                slog!(
                    "WARN: audio before first vocabulary_update — dropping segments \
                     until the grammar is seeded (self-seed startup)"
                );
            }
        }

        // Segment boundary: install a finished build now, so the next
        // segment rarely has to wait for one.
        self.install_ready();
        self.rebase_vad_clock_if_due();

        Ok(Chunk::Counted)
    }

    async fn on_audio_stop(&mut self, _ev: AudioStop, ctx: &mut AudioCtx) -> Result<Flow> {
        // Flush any buffered speech as a final segment, decode + emit, then
        // an empty is_final marker so the voice plugin runs session cleanup
        // without re-executing a command.
        self.vad.flush();
        if !self.vad.is_empty() {
            self.install_before_decode();
        }
        if self.grammar_seeded {
            let fallback = self.current_session.clone().unwrap_or_default();
            self.drain_and_emit(ctx, &fallback, "flush").await?;
        } else {
            // Un-seeded grammar (self-seed startup race) — discard rather
            // than decode the flush through the greedy decoder.
            self.drop_unseeded();
        }
        let session_id = self.current_session.take().unwrap_or_default();
        emit_transcript(ctx, session_id, String::new(), true, None, None).await?;
        branchkit_stage_sdk::stage_log::clear_session();
        self.vad.reset();
        self.seg.reset();
        self.clock.clear();
        Ok(Flow::Continue)
    }

    async fn on_other(&mut self, ev: Event, ctx: &mut AudioCtx) -> Result<Flow> {
        if ev.event_type != "vocabulary_update" {
            return Ok(Flow::Continue);
        }
        // Phase 5c: `narrow_to` (present only in an exclusive mode like
        // snap_mode) is the CTC narrowing target — honor it over the wide
        // `words` union. The neural CTC engine has no hallucination cliff,
        // so narrowing here makes off-mode acoustic neighbors
        // (snap "right"->"eight") unproducible. Absent -> fall back to
        // `words` (the 5b widen path). The platform sends `narrow_to` only
        // while an exclusive gate is active (services/vocabulary.rs
        // compute_narrow_to); non-exclusive context stays on the wide union.
        let target = ev
            .data
            .get("narrow_to")
            .and_then(|v| v.as_array())
            .or_else(|| ev.data.get("words").and_then(|v| v.as_array()));
        let Some(words) = target else {
            return Ok(Flow::Continue);
        };
        // Uppercase to match the baked vocab + the model lexicon/BPE.
        let incoming: Vec<String> = words
            .iter()
            .filter_map(|v| v.as_str().map(|s| s.to_uppercase()))
            .collect();
        if self.dynamic_grammar {
            // Trigger on ANY difference from the current grammar — a
            // widen (new words), a Phase-5c narrow (a subset), OR a
            // Bias reweight (same words, different per-word bias; a
            // weight change needs a rebuild to re-set the HL arc costs).
            // The platform pushes the full intended word set + bias each
            // time, so "differs" is the right swap trigger (unlike the
            // streaming path, which keys on new words only).
            let pushed: HashSet<String> = incoming.iter().cloned().collect();
            let weights = parse_word_weights(&ev.data);
            // D2: the structured grammar DAG (present only when the
            // platform's toggle is on). When present it drives the swap
            // (set_graph_from_dag); absent -> the flat word-list path.
            let dag = parse_grammar_dag(&ev.data);
            if pushed != self.current_vocab
                || weights != self.current_weights
                || dag != self.current_dag
            {
                let mut sorted: Vec<String> = pushed.iter().cloned().collect();
                sorted.sort();
                let added = pushed.difference(&self.current_vocab).count();
                let removed = self.current_vocab.difference(&pushed).count();
                slog!(
                    "preparing: grammar change ({} words, +{} -{}, {} biased, dag={}) -> building off the decode thread",
                    sorted.len(),
                    added,
                    removed,
                    weights.len(),
                    dag.as_ref()
                        .map(|d| format!("{}st/{}arc", d.num_states, d.arcs.len()))
                        .unwrap_or_else(|| "off".into()),
                );
                // Segments that closed before this request were spoken
                // under the old grammar: decode them now, before the
                // request, so everything closing later can take the new one.
                if !self.vad.is_empty() && self.grammar_seeded {
                    self.install_before_decode();
                    let fallback = self.current_session.clone().unwrap_or_default();
                    self.drain_and_emit(ctx, &fallback, "segment").await?;
                }
                self.current_vocab = pushed;
                self.current_weights = weights.clone();
                self.current_dag = dag.clone();
                self.request_grammar(GrammarSpec {
                    words: sorted,
                    weights,
                    dag,
                });
            }
        } else {
            // Baked-only: surface drift the static HL can't recognize.
            let missing: Vec<String> = incoming
                .iter()
                .filter(|w| !self.baked.is_empty() && !self.baked.contains(*w))
                .cloned()
                .collect();
            if !missing.is_empty() {
                let shown: Vec<&str> = missing.iter().take(12).map(|s| s.as_str()).collect();
                slog!(
                    "{} word(s) not in baked HL (unrecognized until --dynamic-grammar): {:?}{}",
                    missing.len(),
                    shown,
                    if missing.len() > shown.len() {
                        " …"
                    } else {
                        ""
                    },
                );
            }
        }
        Ok(Flow::Continue)
    }
}

async fn emit_transcript(
    ctx: &mut AudioCtx,
    session_id: String,
    text: String,
    is_final: bool,
    word_onsets_ms: Option<Vec<u64>>,
    word_scores: Option<Vec<f32>>,
) -> Result {
    // Scalar confidence for coarse consumers = mean per-word margin. Derived,
    // never independently set — word_scores stays the single source.
    let confidence = word_scores
        .as_ref()
        .filter(|s| !s.is_empty())
        .map(|s| s.iter().sum::<f32>() / s.len() as f32);
    ctx.emit(
        event_type::TRANSCRIPT,
        &Transcript {
            session_id,
            text,
            is_final,
            partial: false,
            confidence,
            word_onsets_ms,
            word_scores,
        },
    )
    .await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_word_weights_reads_sparse_map_uppercased() {
        let data = serde_json::json!({
            "words": ["FOUR", "PAGE"],
            "word_weights": { "four": 4.0, "Eight": 2.5 },
        });
        let m = parse_word_weights(&data);
        assert_eq!(m.get("FOUR"), Some(&4.0));
        assert_eq!(m.get("EIGHT"), Some(&2.5));
        assert_eq!(m.len(), 2);
    }

    #[test]
    fn parse_word_weights_absent_or_non_object_is_empty() {
        assert!(parse_word_weights(&serde_json::json!({ "words": ["FOUR"] })).is_empty());
        assert!(parse_word_weights(&serde_json::json!({ "word_weights": "nope" })).is_empty());
    }

    #[test]
    fn materialize_weights_aligns_to_words_else_zero() {
        let words = vec!["PAGE".to_string(), "FOUR".to_string(), "COPY".to_string()];
        let mut bias = HashMap::new();
        bias.insert("FOUR".to_string(), 4.0);
        // Parallel to `words`, neutral where unbiased — the exact array the FFI wants.
        assert_eq!(materialize_weights(&words, &bias), vec![0.0, 4.0, 0.0]);
    }

    #[test]
    fn materialize_weights_empty_map_is_empty_vec() {
        let words = vec!["PAGE".to_string()];
        // Empty -> empty vec, which the recognizer maps to the unweighted fast path.
        assert!(materialize_weights(&words, &HashMap::new()).is_empty());
    }

    #[test]
    fn dumper_writes_wav_and_manifest_then_seeds_seq() {
        let dir = std::env::temp_dir().join(format!("bk_dump_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);

        let mut d = SegmentDumper::create(dir.to_str().unwrap()).unwrap();
        // A short ramp; the round-trip checks sample count + format, not content.
        let samples: Vec<f32> = (0..160).map(|i| (i as f32 / 160.0) - 0.5).collect();
        d.dump(&samples, "SNAP EIGHT", Some(&[4.5, -1.25]), 42, "sess1234");

        // The wav exists, is 16k mono 16-bit, and holds every sample.
        let wav = dir.join("seg_00000.wav");
        assert!(wav.exists(), "wav not written");
        let reader = hound::WavReader::open(&wav).unwrap();
        let spec = reader.spec();
        assert_eq!(spec.sample_rate, SAMPLE_RATE as u32);
        assert_eq!(spec.channels, 1);
        assert_eq!(spec.bits_per_sample, 16);
        assert_eq!(reader.into_samples::<i16>().count(), samples.len());

        // The manifest carries the decode + the filename it labels.
        let manifest = std::fs::read_to_string(dir.join("manifest.jsonl")).unwrap();
        let line: serde_json::Value =
            serde_json::from_str(manifest.lines().next().unwrap()).unwrap();
        assert_eq!(line["text"], "SNAP EIGHT");
        assert_eq!(line["file"], "seg_00000.wav");
        assert_eq!(line["samples"], 160);
        // Per-word margins ride the manifest — the clip's ground-truth score record.
        assert_eq!(line["word_scores"], serde_json::json!([4.5, -1.25]));

        // A fresh dumper over the same dir resumes past the existing wav (no clobber).
        let d2 = SegmentDumper::create(dir.to_str().unwrap()).unwrap();
        assert_eq!(d2.seq, 1);

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn expand_includes_pre_and_post_roll() {
        // 16 samples/"second" for easy math: pre=4, post=2, ring big enough.
        let mut s = Segmenter::new(16, 0.25, 0.125, 10.0);
        let audio: Vec<f32> = (0..32).map(|i| i as f32).collect();
        s.push(&audio);
        // Segment [8, 12): expanded to [8-4, 12+2) = [4, 14).
        let out = s.expand(8, 4);
        assert_eq!(out, (4..14).map(|i| i as f32).collect::<Vec<_>>());
    }

    #[test]
    fn expand_clamps_pre_roll_at_stream_start() {
        let mut s = Segmenter::new(16, 0.25, 0.0, 10.0);
        let audio: Vec<f32> = (0..10).map(|i| i as f32).collect();
        s.push(&audio);
        // Segment [2, 5): pre-roll would reach -2 -> clamp to 0.
        let out = s.expand(2, 3);
        assert_eq!(out, (0..5).map(|i| i as f32).collect::<Vec<_>>());
    }

    #[test]
    fn expand_clamps_post_roll_at_fed_end() {
        let mut s = Segmenter::new(16, 0.0, 0.5, 10.0);
        let audio: Vec<f32> = (0..10).map(|i| i as f32).collect();
        s.push(&audio);
        // Segment [6, 10): post-roll would reach 18 -> clamp to fed (10).
        let out = s.expand(6, 4);
        assert_eq!(out, (6..10).map(|i| i as f32).collect::<Vec<_>>());
    }

    #[test]
    fn ring_evicts_old_audio_but_indices_stay_absolute() {
        // capacity 8; push 12 -> oldest 4 evicted, ring holds [4,12), ring_start=4.
        let mut s = Segmenter::new(8, 0.125, 0.0, 1.0); // cap = 8
        let audio: Vec<f32> = (0..12).map(|i| i as f32).collect();
        s.push(&audio);
        assert_eq!(s.fed(), 12);
        // Segment [9, 11): pre=1 -> [8, 11), all still retained.
        let out = s.expand(9, 2);
        assert_eq!(out, vec![8.0, 9.0, 10.0]);
        // A segment whose pre-roll predates the ring clamps to ring_start (4).
        let out2 = s.expand(5, 2); // [4, 7)
        assert_eq!(out2, vec![4.0, 5.0, 6.0]);
    }

    #[test]
    fn reset_restarts_the_clock() {
        let mut s = Segmenter::new(16, 0.25, 0.0, 10.0);
        s.push(&(0..20).map(|i| i as f32).collect::<Vec<_>>());
        s.reset();
        assert_eq!(s.fed(), 0);
        s.push(&[100.0, 101.0, 102.0, 103.0, 104.0]);
        // Fresh clock: segment [1, 3) -> pre 4 clamps to 0 -> [0, 3).
        assert_eq!(s.expand(1, 2), vec![100.0, 101.0, 102.0]);
    }

    #[test]
    fn rebase_restarts_the_clock_and_keeps_the_pre_roll() {
        let mut s = Segmenter::new(16, 0.25, 0.0, 10.0); // pre = 4
        s.push(&(0..20).map(|i| i as f32).collect::<Vec<_>>());
        assert_eq!(s.rebase(), 20);
        assert_eq!(s.fed(), 0);
        s.push(&[100.0, 101.0, 102.0]);
        // The reset VAD opens a segment at its sample 1: the pre-roll reaches
        // back across the rebase into the audio fed before it.
        assert_eq!(s.expanded_start(1), -3);
        assert_eq!(s.expand(1, 2), vec![17.0, 18.0, 19.0, 100.0, 101.0, 102.0]);
    }

    /// The overflow is arithmetic, so it is reproduced as arithmetic: the
    /// VAD's int32 clock at 16 kHz, and a feed of continuous audio well past
    /// the 37 h it takes to overflow, with the clock rule applied after every
    /// chunk as the stage applies it.
    #[test]
    fn the_vad_clock_never_reaches_int32_overflow() {
        let overflow_hours = i32::MAX as f64 / SAMPLE_RATE as f64 / 3600.0;
        assert!((37.0..37.5).contains(&overflow_hours), "{overflow_hours} h");
        // The bound holds with room for a chunk and the VAD's own lookahead.
        assert!(VAD_CLOCK_HARD_SAMPLES < i32::MAX as i64 / 2);

        let chunk = SAMPLE_RATE as i64 / 10; // 100 ms
        let chunks = 72 * 3600 * 10; // 72 h
        let check = |speaking: &dyn Fn(i64) -> bool| {
            let (mut fed, mut peak, mut resets) = (0i64, 0i64, 0u32);
            for i in 0..chunks {
                fed += chunk;
                peak = peak.max(fed);
                assert!(fed <= i32::MAX as i64, "overflowed at chunk {i}");
                if vad_clock_due(fed, speaking(i), false) {
                    fed = 0;
                    resets += 1;
                }
            }
            (peak, resets)
        };

        // Ordinary listening (a 2 s utterance every 10 s): reset at the first
        // pause past the hour, never later than that.
        let (peak, resets) = check(&|i| i % 100 < 20);
        assert!(peak < VAD_CLOCK_SOFT_SAMPLES + 3 * SAMPLE_RATE as i64);
        assert_eq!(resets, 72);

        // Audio the VAD calls speech without end (a fan, a TV): the hard
        // bound resets it anyway.
        let (peak, resets) = check(&|_| true);
        assert_eq!(peak, VAD_CLOCK_HARD_SAMPLES);
        assert_eq!(resets, 6);

        // Nothing resets inside the first hour, so a session is untouched.
        assert!(!vad_clock_due(VAD_CLOCK_SOFT_SAMPLES - 1, false, false));
        // A closed segment waiting to be decoded holds the soft reset off.
        assert!(!vad_clock_due(VAD_CLOCK_SOFT_SAMPLES, false, true));
    }

    fn detail(text: &str, tokens: &[&str], ts: &[f32]) -> crate::engine::DecodeDetail {
        crate::engine::DecodeDetail {
            text: text.into(),
            tokens: tokens.iter().map(|s| s.to_string()).collect(),
            timestamps: ts.to_vec(),
            margins: Vec::new(),
        }
    }

    /// Build a clock from (fed, stamp_ms) pairs; the wall Instant is irrelevant
    /// to the stamp/onset lookups these tests exercise.
    fn ticks(pairs: &[(i64, u64)]) -> VecDeque<ClockTick> {
        pairs
            .iter()
            .map(|&(fed, stamp_ms)| ClockTick {
                fed,
                wall: Instant::now(),
                stamp_ms,
            })
            .collect()
    }

    #[test]
    fn word_onsets_from_word_initial_pieces() {
        let d = detail("SPEAK OVER", &["▁SPEAK", "▁OVER"], &[0.50, 0.92]);
        assert_eq!(word_onsets_sec(&d), Some(vec![0.50, 0.92]));
    }

    #[test]
    fn word_onsets_skip_blanks_and_group_continuation_pieces() {
        // Multi-piece word: continuation pieces don't open a word; literal
        // blank tokens are transparent (must not force a word start after).
        let d = detail(
            "SPEAK OVER",
            &["▁SP", "EAK", "<blk>", "▁O", "VER"],
            &[0.50, 0.61, 0.70, 0.92, 1.01],
        );
        assert_eq!(word_onsets_sec(&d), Some(vec![0.50, 0.92]));
    }

    #[test]
    fn word_onsets_accept_space_marked_pieces() {
        // The live FST readout renders the sentencepiece word marker as a
        // leading SPACE (probed 2026-08-08): "BOTTOM" decodes as
        // ["<blk>", " bo", "<blk>", "t", "<blk>", "t", "om", "<blk>"].
        let d = detail(
            "BOTTOM",
            &["<blk>", " bo", "<blk>", "t", "<blk>", "t", "om", "<blk>"],
            &[0.0, 0.24, 0.28, 0.32, 0.36, 0.40, 0.48, 0.52],
        );
        assert_eq!(word_onsets_sec(&d), Some(vec![0.24]));
        // Two space-marked words.
        let d2 = detail(
            "SPEAK OVER",
            &[" sp", "eak", "<blk>", " over"],
            &[0.10, 0.22, 0.30, 0.52],
        );
        assert_eq!(word_onsets_sec(&d2), Some(vec![0.10, 0.52]));
    }

    fn detail_scored(text: &str, tokens: &[&str], margins: &[f32]) -> crate::engine::DecodeDetail {
        crate::engine::DecodeDetail {
            text: text.into(),
            tokens: tokens.iter().map(|s| s.to_string()).collect(),
            timestamps: vec![0.0; tokens.len()],
            margins: margins.to_vec(),
        }
    }

    #[test]
    fn word_scores_min_over_word_pieces_skipping_blanks() {
        // Two words; the second word's weakest piece (-1.2) marks the word.
        let d = detail_scored(
            "SPEAK OVER",
            &["▁SP", "EAK", "<blk>", "▁O", "VER"],
            &[5.0, 3.5, 0.0, 4.0, -1.2],
        );
        assert_eq!(word_scores(&d), Some(vec![3.5, -1.2]));
        // Space-marked pieces (the live FST readout form) group the same way.
        let d2 = detail_scored(
            "BOTTOM",
            &["<blk>", " bo", "<blk>", "t", "om", "<blk>"],
            &[0.0, 6.0, 0.0, 2.0, 4.5, 0.0],
        );
        assert_eq!(word_scores(&d2), Some(vec![2.0]));
    }

    #[test]
    fn word_scores_none_on_misalignment_or_missing_margins() {
        // No word-boundary markers: grouping can't align a two-word text.
        let d = detail_scored("SPEAK OVER", &["SPEAK", "OVER"], &[1.0, 2.0]);
        assert_eq!(word_scores(&d), None);
        // Unpatched lib: tokens present, margins empty — not parallel.
        let d2 = detail_scored("SPEAK OVER", &["▁SPEAK", "▁OVER"], &[]);
        assert_eq!(word_scores(&d2), None);
        // Empty readout (greedy path).
        assert_eq!(word_scores(&detail_scored("SPEAK OVER", &[], &[])), None);
    }

    #[test]
    fn margin_tag_prints_per_word_and_mean() {
        assert_eq!(
            margin_tag(Some(&[5.25, -1.75])),
            "margins=[+5.2,-1.8] mean=+1.8"
        );
        assert_eq!(margin_tag(None), "margins=none");
        assert_eq!(margin_tag(Some(&[])), "margins=none");
    }

    #[test]
    fn word_onsets_none_when_grouping_disagrees_with_text() {
        // No word-boundary markers at all: only the first piece opens a word,
        // so a two-word text can't align — fall back rather than mis-cut.
        let d = detail("SPEAK OVER", &["SPEAK", "OVER"], &[0.50, 0.92]);
        assert_eq!(word_onsets_sec(&d), None);
        // Empty readout (greedy path): same.
        assert_eq!(word_onsets_sec(&detail("SPEAK OVER", &[], &[])), None);
    }

    #[test]
    fn stamp_at_backs_off_within_the_chunk() {
        let clock = ticks(&[(320, 1000), (640, 1020)]);
        // Exactly at a chunk end.
        assert_eq!(stamp_at(&clock, 320), Some(1000));
        // Mid-chunk: 160 samples (10ms at 16k) before the chunk-end stamp.
        assert_eq!(stamp_at(&clock, 160), Some(990));
        // Past the newest stamp: unknown.
        assert_eq!(stamp_at(&clock, 800), None);
    }

    #[test]
    fn onsets_to_shared_ms_maps_aligned_onsets() {
        let clock = ticks(&[(16000, 2000), (32000, 3000)]);
        // Token path: onset 0.5s into a buffer starting at sample 8000
        // -> absolute 16000 -> stamp 2000.
        assert_eq!(
            onsets_to_shared_ms(Some(&[0.5]), 8000, &clock),
            Some(vec![2000])
        );
    }

    #[test]
    fn onsets_to_shared_ms_none_without_aligned_onsets() {
        let clock = ticks(&[(16000, 2000), (32000, 3000)]);
        // No aligned token onsets -> None (NOT a fabricated segment-start fill):
        // the consumer's audio cutoff must not drop the whole segment; the text
        // backstop removes the phrase instead.
        assert_eq!(onsets_to_shared_ms(None, 8000, &clock), None);
        // Any unmappable position poisons the whole vector.
        assert_eq!(onsets_to_shared_ms(Some(&[5.0]), 0, &clock), None);
    }
}
