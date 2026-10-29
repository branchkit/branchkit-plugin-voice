//! `whisper_stage probe`: the offline re-decode the platform's
//! recognition.redecode op runs, so a plugin can compare dictation models on
//! recordings it kept (voice's Models tab). The sherpa command stage's probe
//! wire: one JSON request on stdin,
//!
//! ```json
//! {"model": "<absolute model dir or ggml file>", "language": "en",
//!  "items": [{"id": "...", "audio": "<absolute 16 kHz mono 16-bit WAV>"}]}
//! ```
//!
//! and one JSON line per item on stdout, in order: `{"id", "text",
//! "decode_ms"}`, or `{"id", "text": "", "error"}`.
//!
//! Two optional request fields serve the latency bench
//! (`scripts/whisper-latency.py`) and change nothing when left out:
//! `threads`, the decode's thread count (default: the machine's, up to 8),
//! and `use_gpu: false`, which keeps a GPU-built stage (Metal on macOS) on
//! the CPU, so one machine can measure both.
//!
//! An item asking for noise (the sherpa probe's ladder) is refused with an
//! error: this probe does not mix noise in.
//!
//! Every item decodes through the live path's `decode`. Not yet applied: the
//! vocabulary boost the live stage takes from the plugin's bias events, so a
//! probe matches a live decode made without one. A command item's `words`,
//! `grammar_dag` and `bias` are ignored. `word_margins` is left out: Whisper's
//! per-word scores are log-probabilities, not the margins that field reports.
//! The model loads once, for every item.

use serde::Deserialize;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use whisper_rs::{WhisperContext, WhisperContextParameters};

#[derive(Deserialize)]
struct Request {
    model: String,
    #[serde(default)]
    language: Option<String>,
    /// The decode's thread count, at least 1.
    #[serde(default)]
    threads: Option<i32>,
    /// false keeps a GPU-built stage on the CPU. Left out, the build decides.
    #[serde(default)]
    use_gpu: Option<bool>,
    items: Vec<Item>,
}

#[derive(Deserialize)]
struct Item {
    id: String,
    audio: String,
    /// A noise ladder step ({snr_db, seed}) the sherpa probe mixes in. This
    /// probe does not, so an item asking for one is refused, never decoded
    /// clean and reported as if it had been.
    #[serde(default)]
    noise: Option<serde_json::Value>,
}

/// Refused requests, as an item's error.
const NO_NOISE: &str = "this probe does not mix in noise";

/// The ggml file for a model path: the path itself, or the first `*.bin` in it
/// when it is a directory (the CLI provisions a model as `<artifacts>/<name>/`).
pub fn ggml_file(model: &Path) -> Option<PathBuf> {
    if model.is_file() {
        return Some(model.to_path_buf());
    }
    let mut bins: Vec<PathBuf> = std::fs::read_dir(model)
        .ok()?
        .flatten()
        .map(|e| e.path())
        .filter(|p| p.extension().is_some_and(|e| e == "bin"))
        .collect();
    bins.sort();
    bins.into_iter().next()
}

/// Reads a 16 kHz mono 16-bit PCM WAV as samples in [-1, 1].
pub fn read_pcm16_mono(path: &Path) -> Result<Vec<f32>, String> {
    let mut reader =
        hound::WavReader::open(path).map_err(|e| format!("open {}: {e}", path.display()))?;
    let spec = reader.spec();
    if spec.channels != 1 || spec.sample_rate != 16000 || spec.bits_per_sample != 16 {
        return Err(format!(
            "want 16 kHz mono 16-bit PCM, got {} Hz, {} channel(s), {} bits",
            spec.sample_rate, spec.channels, spec.bits_per_sample
        ));
    }
    reader
        .samples::<i16>()
        .map(|s| {
            s.map(|v| v as f32 / 32768.0)
                .map_err(|e| format!("read {}: {e}", path.display()))
        })
        .collect()
}

/// Runs the probe to completion. Exit status: 0 when the request was read and
/// the model loaded (per-item failures are lines of their own), 1 otherwise.
pub fn run() -> i32 {
    let mut raw = String::new();
    if std::io::stdin().read_to_string(&mut raw).is_err() {
        eprintln!("probe: cannot read stdin");
        return 1;
    }
    let req: Request = match serde_json::from_str(&raw) {
        Ok(r) => r,
        Err(e) => {
            eprintln!("probe: bad request: {e}");
            return 1;
        }
    };
    let Some(model) = ggml_file(Path::new(&req.model)) else {
        eprintln!("probe: no ggml model at {}", req.model);
        return 1;
    };
    if req.threads.is_some_and(|n| n < 1) {
        eprintln!("probe: threads must be at least 1");
        return 1;
    }
    let mut params = WhisperContextParameters::default();
    if let Some(gpu) = req.use_gpu {
        params.use_gpu(gpu);
    }
    let ctx = match WhisperContext::new_with_params(&model.to_string_lossy(), params) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("probe: failed to load model {}: {e}", model.display());
            return 1;
        }
    };
    let language = req.language.unwrap_or_else(|| "en".into());
    let threads = req.threads.unwrap_or_else(|| {
        std::thread::available_parallelism().map_or(4, |n| n.get().min(8) as i32)
    });
    let mut out = std::io::stdout().lock();
    for item in &req.items {
        if item.noise.as_ref().is_some_and(|n| !n.is_null()) {
            let _ = writeln!(
                out,
                "{}",
                serde_json::json!({"id": item.id, "text": "", "error": NO_NOISE})
            );
            continue;
        }
        let line = match read_pcm16_mono(Path::new(&item.audio)).and_then(|pcm| {
            let started = std::time::Instant::now();
            crate::decode(&ctx, &pcm, &language, threads, None)
                .map(|(text, _)| (text, started.elapsed().as_secs_f64() * 1e3))
                .map_err(|e| e.to_string())
        }) {
            Ok((text, ms)) => {
                serde_json::json!({"id": item.id, "text": text, "decode_ms": (ms * 10.0).round() / 10.0})
            }
            Err(e) => serde_json::json!({"id": item.id, "text": "", "error": e}),
        };
        let _ = writeln!(out, "{line}");
    }
    0
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The bench's fields are optional: a request without them (what
    /// recognition.redecode sends) reads as before, and one with them keeps
    /// them.
    #[test]
    fn request_bench_fields_are_optional() {
        let plain: Request =
            serde_json::from_str(r#"{"model": "/m", "items": [{"id": "a", "audio": "/a.wav"}]}"#)
                .unwrap();
        assert_eq!((plain.threads, plain.use_gpu), (None, None));
        let bench: Request =
            serde_json::from_str(r#"{"model": "/m", "threads": 4, "use_gpu": false, "items": []}"#)
                .unwrap();
        assert_eq!((bench.threads, bench.use_gpu), (Some(4), Some(false)));
    }

    /// A model directory resolves to its ggml file; a file to itself.
    #[test]
    fn ggml_file_resolves_a_dir_or_a_file() {
        let dir = std::env::temp_dir().join(format!("bk-ggml-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        assert_eq!(ggml_file(&dir), None, "no .bin, no model");
        std::fs::write(dir.join("ggml-base.en.bin"), b"x").unwrap();
        std::fs::write(dir.join("README.md"), b"x").unwrap();
        assert_eq!(ggml_file(&dir), Some(dir.join("ggml-base.en.bin")));
        assert_eq!(
            ggml_file(&dir.join("ggml-base.en.bin")),
            Some(dir.join("ggml-base.en.bin"))
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Only 16 kHz mono 16-bit PCM is decoded; anything else is refused with
    /// what it is.
    #[test]
    fn read_pcm16_mono_checks_the_format() {
        let dir = std::env::temp_dir().join(format!("bk-wav-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let write = |name: &str, rate: u32, channels: u16| {
            let path = dir.join(name);
            let spec = hound::WavSpec {
                channels,
                sample_rate: rate,
                bits_per_sample: 16,
                sample_format: hound::SampleFormat::Int,
            };
            let mut w = hound::WavWriter::create(&path, spec).unwrap();
            for s in [0i16, 16384, -32768] {
                w.write_sample(s).unwrap();
            }
            w.finalize().unwrap();
            path
        };
        let ok = read_pcm16_mono(&write("ok.wav", 16000, 1)).unwrap();
        assert_eq!(ok, vec![0.0, 0.5, -1.0]);
        assert!(
            read_pcm16_mono(&write("rate.wav", 44100, 1))
                .unwrap_err()
                .contains("44100 Hz")
        );
        assert!(read_pcm16_mono(&dir.join("missing.wav")).is_err());
        let _ = std::fs::remove_dir_all(&dir);
    }
}
