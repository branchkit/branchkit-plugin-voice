//! `probe` subcommand — an offline batch decode evaluator.
//!
//! A one-shot, stdin/file-driven harness that re-decodes captured WAV audio
//! through the offline CTC+HLG recognizer against arbitrary grammars (in-process
//! swap) and optional injected noise, returning the 1-best text plus the per-token
//! BPE pieces + onset timestamps. It is a GENERIC recognizer diagnostic: it knows
//! nothing about calibration, confusers, or fragility — all interpretation lives in
//! the caller (the voice plugin's calibration ladder). The live stdio pipeline path
//! (`offline::run` / streaming) is untouched; this is reached only via the `probe`
//! arg, so the engine stays generic and the platform never sees it.
//!
//! Wire: read one JSON `ProbeRequest` (from a file path arg or stdin), emit one
//! NDJSON `ProbeResult` line per item on stdout (aligned to the request by `id`),
//! diagnostics on stderr only.

use std::collections::{BTreeSet, HashMap};
use std::io::{Read, Write};

use serde::{Deserialize, Serialize};

use crate::engine::OfflineRecognizer;
use crate::{find_onnx, resolve_model_dir};

#[derive(Deserialize)]
struct ProbeRequest {
    /// Model dir name or absolute path (resolved like the pipeline stage).
    model: String,
    /// FST search beam; mirrors the live stage's `--max-active` (default 3000).
    #[serde(default)]
    max_active: Option<i32>,
    items: Vec<ProbeItem>,
}

#[derive(Deserialize)]
struct ProbeItem {
    /// Caller-assigned key echoed back on the result, so the caller aligns the
    /// per-item readout (target × rep × perturbation step) without ordering deps.
    id: String,
    /// Path to a 16kHz mono WAV to decode.
    audio: String,
    /// Grammar to decode against. Absent = decode against the baked full field
    /// (the model's HL.fst.words union); present = swap to exactly these words
    /// (e.g. a constituent-floor grammar). Uppercased before the swap.
    #[serde(default)]
    words: Option<Vec<String>>,
    /// Optional injected-noise perturbation; absent = decode the clean audio.
    #[serde(default)]
    noise: Option<NoiseSpec>,
    /// Optional per-word decoding bias applied to this item's grammar:
    /// a sparse `{ "WORD": cost }` map (positive penalizes, negative boosts).
    /// Keys are uppercased before lookup. Absent = unweighted decode. Used to
    /// sweep penalty strengths against captured collisions for tuning.
    #[serde(default)]
    bias: Option<HashMap<String, f32>>,
    /// Optional structured pattern-DAG (the `grammar_dag` from a live
    /// `pipelines.grammar` export). Present -> the item decodes through the
    /// D2 structured grammar (`set_graph_from_dag`) with `words` as the union
    /// (lexicon basis + open-tail alphabet), exactly like the live stage;
    /// `bias` is ignored on this path (structure supersedes it, as in
    /// `offline::build_grammar`). Absent -> the flat word-list grammar.
    #[serde(default)]
    grammar_dag: Option<branchkit_stage_sdk::grammar_dag::GrammarDagWire>,
}

#[derive(Deserialize)]
struct NoiseSpec {
    /// Target signal-to-noise ratio in dB (lower = noisier). The ladder step.
    snr_db: f32,
    /// PRNG seed — the same seed reproduces the same noise (deterministic ladder).
    #[serde(default)]
    seed: u64,
}

#[derive(Serialize)]
struct ProbeResult {
    id: String,
    text: String,
    tokens: Vec<String>,
    timestamps: Vec<f32>,
    /// Per-token argmax margin (engine posteriors patch): the token's peak-frame
    /// acoustic log-prob minus the best competing symbol on that frame. Parallel
    /// to `tokens`, `<blk>` entries included. Negative = the grammar coerced the
    /// path.
    margins: Vec<f32>,
    /// Per-word margin (min over each word's token margins, aligned 1:1 with
    /// `text`'s words) — the same derivation the live stage puts on
    /// `Transcript.word_scores`, so harness readouts and field logs speak one
    /// statistic. Absent when the readout doesn't align with the text.
    #[serde(skip_serializing_if = "Option::is_none")]
    word_margins: Option<Vec<f32>>,
    /// Wall time of this item's decode alone (grammar swaps are excluded — a
    /// swap is amortized across every item sharing the grammar). Lets a caller
    /// compare decode cost across grammars or across engine builds (the
    /// lazy-composition migration's A/B ran two binaries over this field)
    /// without in-process instrumentation.
    decode_ms: f64,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
}

impl ProbeResult {
    fn failed(id: String, error: String) -> Self {
        ProbeResult { id, text: String::new(), tokens: Vec::new(), timestamps: Vec::new(), margins: Vec::new(), word_margins: None, decode_ms: 0.0, error: Some(error) }
    }
}

/// Run the probe to completion. `args` are everything after the `probe` token.
/// Synchronous (offline decode has no async work) — the async `main` returns this
/// directly. The only stdout output is NDJSON results.
pub fn run(args: &[String]) -> Result<(), Box<dyn std::error::Error>> {
    // `probe <requests.json>` reads the file; `probe` alone reads stdin.
    let raw = match args.iter().find(|a| !a.starts_with("--")) {
        Some(path) => std::fs::read_to_string(path)
            .map_err(|e| format!("probe: cannot read requests file {path}: {e}"))?,
        None => {
            let mut buf = String::new();
            std::io::stdin().read_to_string(&mut buf)?;
            buf
        }
    };
    let req: ProbeRequest = serde_json::from_str(&raw).map_err(|e| format!("probe: bad request JSON: {e}"))?;

    let model_dir = resolve_model_dir(&req.model);
    let onnx = find_onnx(&model_dir).ok_or_else(|| format!("probe: no .onnx model in {}", model_dir.display()))?;
    let tokens = model_dir.join("tokens.txt");
    let graph = model_dir.join("HL.fst");
    if !tokens.exists() {
        return Err(format!("probe: tokens.txt missing at {}", tokens.display()).into());
    }
    // Self-seed: no bundled HL.fst (the live stage builds its grammar in-process).
    // The recognizer starts on the greedy decoder and each item seeds its grammar
    // via set_graph_from_words — the platform stamps the live union on every item.
    // A present HL.fst is still honored as a fallback for a dev dir that has one.
    let graph_present = graph.exists();
    let graph_arg = if graph_present { graph.to_string_lossy().into_owned() } else { String::new() };

    // The baked full-field union (HL.fst.words sidecar) — only present alongside a
    // bundled HL.fst. Items with no explicit grammar fall back to it; absent (the
    // self-seed case) such items are failed below rather than decoding open-vocab.
    let baked_union: Vec<String> = if graph_present {
        std::fs::read_to_string(format!("{}.words", graph.display()))
            .map(|s| s.lines().map(|w| w.trim().to_uppercase()).filter(|w| !w.is_empty()).collect())
            .unwrap_or_default()
    } else {
        Vec::new()
    };

    let recognizer = OfflineRecognizer::create(
        &onnx.to_string_lossy(),
        &tokens.to_string_lossy(),
        &graph_arg,
        req.max_active.unwrap_or(3000),
    )
    .ok_or("probe: failed to create offline recognizer")?;

    eprintln!(
        "probe: model={} baked_union={} items={}",
        onnx.file_name().and_then(|s| s.to_str()).unwrap_or("?"),
        baked_union.len(),
        req.items.len()
    );

    let stdout = std::io::stdout();
    let mut out = std::io::BufWriter::new(stdout.lock());
    // The currently-applied grammar (sorted word set) + bias, so we only swap when
    // an item's desired grammar or bias actually differs from the last one — a no-op
    // item run costs only a decode. `None` = the original baked HL is still loaded
    // (untouched). The bias is tracked separately because a weight-only change (same
    // words, different penalty — the N-sweep case) still needs a rebuild.
    let mut applied: Option<BTreeSet<String>> = None;
    let mut applied_bias: HashMap<String, f32> = HashMap::new();
    // The structured DAG currently applied (None = flat word-list grammar), so
    // consecutive items sharing a DAG pay one swap — mirrors `offline::run`'s
    // `current_dag` dedup.
    let mut applied_dag: Option<branchkit_stage_sdk::grammar_dag::GrammarDagWire> = None;

    for item in &req.items {
        let samples = match read_wav_f32(&item.audio) {
            Ok(s) if !s.is_empty() => s,
            Ok(_) => {
                write_result(&mut out, &ProbeResult::failed(item.id.clone(), "empty audio".into()))?;
                continue;
            }
            Err(e) => {
                write_result(&mut out, &ProbeResult::failed(item.id.clone(), e))?;
                continue;
            }
        };

        // Desired grammar: explicit words (uppercased), else the baked union (only a
        // bundled HL.fst supplies one). When neither is available (self-seed dir),
        // `desired` is None and the item is failed below — there's no grammar to
        // decode against, and the un-seeded greedy decoder would emit open-vocab text.
        let desired: Option<BTreeSet<String>> = match &item.words {
            Some(w) => Some(w.iter().map(|s| s.to_uppercase()).collect()),
            None if !baked_union.is_empty() => Some(baked_union.iter().cloned().collect()),
            None => None,
        };
        // Uppercase the bias keys so they align with the uppercased grammar words.
        let bias: HashMap<String, f32> = item
            .bias
            .as_ref()
            .map(|m| m.iter().map(|(k, v)| (k.to_uppercase(), *v)).collect())
            .unwrap_or_default();
        if let Some(want) = &desired {
            if applied.as_ref() != Some(want)
                || applied_bias != bias
                || applied_dag != item.grammar_dag
            {
                let words: Vec<String> = want.iter().cloned().collect();
                if let Some(dag) = &item.grammar_dag {
                    // Structured grammar: words are the union (lexicon basis +
                    // open-tail alphabet), the DAG constrains sequences. No
                    // bias on this path (structure supersedes it).
                    recognizer.set_graph_from_dag(dag, &words);
                } else {
                    // Parallel weights for `words` from the sparse bias (0 where unbiased);
                    // empty when no bias so the recognizer takes its unweighted fast path.
                    let weights: Vec<f32> = if bias.is_empty() {
                        Vec::new()
                    } else {
                        words.iter().map(|w| *bias.get(w).unwrap_or(&0.0)).collect()
                    };
                    recognizer.set_graph_from_words(&words, &weights);
                }
                applied = Some(want.clone());
                applied_bias = bias;
                applied_dag = item.grammar_dag.clone();
            }
        }

        // Self-seed with nothing to fall back to: an item that supplied no words (and
        // no prior item seeded a grammar) would decode against the un-seeded greedy
        // decoder — open-vocab garbage. The platform stamps `words` on every item, so
        // this only guards hand-crafted requests against a seedless model dir.
        if !graph_present && applied.is_none() {
            write_result(
                &mut out,
                &ProbeResult::failed(
                    item.id.clone(),
                    "no grammar: supply `words` (no bundled HL.fst fallback)".into(),
                ),
            )?;
            continue;
        }

        let audio = match &item.noise {
            Some(n) => mix_noise(&samples, n.snr_db, n.seed),
            None => samples,
        };
        let t0 = std::time::Instant::now();
        let detail = recognizer.decode_detail(&audio);
        let decode_ms = t0.elapsed().as_secs_f64() * 1e3;
        let word_margins = crate::offline::word_scores(&detail);
        write_result(
            &mut out,
            &ProbeResult { id: item.id.clone(), text: detail.text, tokens: detail.tokens, timestamps: detail.timestamps, margins: detail.margins, word_margins, decode_ms, error: None },
        )?;
    }
    out.flush()?;
    Ok(())
}

fn write_result<W: Write>(out: &mut W, r: &ProbeResult) -> Result<(), Box<dyn std::error::Error>> {
    let line = serde_json::to_string(r)?;
    writeln!(out, "{line}")?;
    Ok(())
}

/// Read a WAV into f32 [-1, 1] mono. Handles the pipeline's canonical 16-bit PCM
/// (what `--dump-segments` writes) and 32-bit float; both are read via hound so the
/// header chunks are parsed properly. Non-mono is read as-is (fixtures are mono).
fn read_wav_f32(path: &str) -> Result<Vec<f32>, String> {
    let reader = hound::WavReader::open(path).map_err(|e| format!("open wav {path}: {e}"))?;
    let spec = reader.spec();
    let samples: Vec<f32> = match (spec.sample_format, spec.bits_per_sample) {
        (hound::SampleFormat::Int, 16) => reader
            .into_samples::<i16>()
            .filter_map(Result::ok)
            .map(|s| s as f32 / 32768.0)
            .collect(),
        (hound::SampleFormat::Float, 32) => {
            reader.into_samples::<f32>().filter_map(Result::ok).collect()
        }
        (fmt, bits) => return Err(format!("unsupported wav format {fmt:?}/{bits}bit in {path}")),
    };
    if spec.channels != 1 {
        eprintln!("probe: WARN {path} has {} channels (expected mono)", spec.channels);
    }
    Ok(samples)
}

/// Mix seeded white Gaussian noise into `samples` at a target `snr_db`
/// (signal-to-noise ratio). Deterministic given `seed`, so a ladder step is exactly
/// reproducible. A generic audio perturbation — no calibration semantics. Unclamped
/// (the recognizer normalizes features; no WAV round-trip).
pub fn mix_noise(samples: &[f32], snr_db: f32, seed: u64) -> Vec<f32> {
    if samples.is_empty() {
        return Vec::new();
    }
    let sig_power: f64 =
        samples.iter().map(|&s| (s as f64) * (s as f64)).sum::<f64>() / samples.len() as f64;
    if sig_power <= 0.0 {
        return samples.to_vec();
    }
    let noise_power = sig_power / 10f64.powf(snr_db as f64 / 10.0);
    let sigma = noise_power.sqrt();
    let mut rng = Rng::new(seed);
    samples.iter().map(|&s| (s as f64 + rng.next_gaussian() * sigma) as f32).collect()
}

/// xorshift64 PRNG + Box-Muller Gaussian. Self-contained and seeded so noise is
/// reproducible (the platform forbids ambient randomness in deterministic paths).
struct Rng(u64);

impl Rng {
    fn new(seed: u64) -> Self {
        // xorshift dies at state 0; map the (legal) 0 seed to a fixed nonzero start.
        Rng(if seed == 0 { 0x9E3779B97F4A7C15 } else { seed })
    }

    fn next_u64(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.0 = x;
        x
    }

    /// Uniform in [0, 1).
    fn next_f64(&mut self) -> f64 {
        (self.next_u64() >> 11) as f64 / (1u64 << 53) as f64
    }

    /// Standard-normal sample (Box-Muller).
    fn next_gaussian(&mut self) -> f64 {
        let u1 = self.next_f64().max(1e-12);
        let u2 = self.next_f64();
        (-2.0 * u1.ln()).sqrt() * (2.0 * std::f64::consts::PI * u2).cos()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mix_noise_is_deterministic_for_a_seed() {
        let sig: Vec<f32> = (0..1000).map(|i| (i as f32 * 0.01).sin() * 0.3).collect();
        let a = mix_noise(&sig, 12.0, 42);
        let b = mix_noise(&sig, 12.0, 42);
        assert_eq!(a, b, "same seed must reproduce identical noise");
        let c = mix_noise(&sig, 12.0, 43);
        assert_ne!(a, c, "different seed must change the noise");
    }

    #[test]
    fn mix_noise_hits_the_target_snr() {
        // A pure tone; measure realized SNR against the requested one.
        let sig: Vec<f32> = (0..16000).map(|i| (i as f32 * 0.05).sin() * 0.5).collect();
        let snr_db = 10.0f64;
        let noisy = mix_noise(&sig, snr_db as f32, 7);
        let sig_p: f64 = sig.iter().map(|&s| (s as f64).powi(2)).sum::<f64>() / sig.len() as f64;
        let noise_p: f64 = sig
            .iter()
            .zip(&noisy)
            .map(|(&s, &n)| ((n - s) as f64).powi(2))
            .sum::<f64>()
            / sig.len() as f64;
        let realized = 10.0 * (sig_p / noise_p).log10();
        // Stochastic, but a 16k-sample average lands within ~1 dB of target.
        assert!((realized - snr_db).abs() < 1.0, "realized SNR {realized:.2} dB off target {snr_db}");
    }

    #[test]
    fn mix_noise_passes_through_silence() {
        let silence = vec![0.0f32; 256];
        assert_eq!(mix_noise(&silence, 6.0, 1), silence, "no signal -> no injected noise");
        assert!(mix_noise(&[], 6.0, 1).is_empty());
    }
}
