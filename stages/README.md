# Voice's stages

The pipeline stages the voice plugin ships (`provides.stages` in
`../plugin.json`): separate processes that speak the stage wire protocol on
stdin/stdout. They live with the plugin because a stage a plugin ships belongs
to that plugin and builds with it, the way any third-party plugin's would. The protocol and helpers are the public stage SDK
(`branchkit-stage-sdk`, pinned in `Cargo.toml`).

The Rust stages are one Cargo workspace (`Cargo.toml` here, its own
`Cargo.lock` and `target/`). `whisperkit-stt` is a Swift package with its own
build root.

| Stage | Binary | Language | Capability (`type`, `name`) | Emits | Platforms |
|---|---|---|---|---|---|
| `vad-gate` | `vad_gate` | Rust (Silero VAD) | `vad_gate`, `silero` | `audio_start` / `audio_chunk` / `audio_stop` per detected speech segment, `flow_credit` | macOS, Linux (the Windows build skips it); declared by voice, no seeded pipeline uses it |
| `sherpa-commands` | `sherpa_commands` | Rust (sherpa-onnx, NeMo CTC) | `command_recognition`, `sherpa` | `transcript`, `flow_credit` | all |
| `whisper-stt` | `whisper_stage` | Rust (whisper.cpp) | `stt`, `whisper` | `transcript`, `flow_credit` | Linux, Windows (Metal backend if built on macOS) |
| `whisperkit-stt` | `whisperkit_stage` | Swift (WhisperKit, CoreML) | `stt`, `whisperkit-stt` | `transcript`; accepts `ext.branchkit.dictation_bias` config | macOS |
| `refine-llm` | `refine_llm` | Rust (llama.cpp, CPU) | `request`, `llama` | one answer per request: dictated text refined by a local language model | all |

The pipelines voice seeds name them: `command_recognition` (mic, then
`voice.sherpa_commands`) and `dictation` (mic, then the `_platform.stt` role,
which resolves to `whisperkit_stt` on macOS and `whisper_stt` elsewhere).
The build copies each binary beside the voice plugin, where the platform
resolves a plugin's stages.

## sherpa-commands links a patched sherpa-onnx

The recognizer needs a grammar API upstream sherpa-onnx lacks, carried as a
patch overlay (`sherpa-commands/patches/`, Apache-2.0 like sherpa-onnx; see
its README) against a pinned upstream
tag. `sherpa-commands/build-patched-sherpa.sh` (`.ps1` on Windows) builds the
patched static libraries once per target into `sherpa-commands/.sherpa-libs/`
(cached, a no-op until the version or a patch changes), and
`SHERPA_ONNX_LIB_DIR` points the build at them. Without it the sherpa-onnx
crate silently links the stock release libraries and the grammar API is gone.

## Building and testing

Here (the triple is your host's, e.g. `x86_64-unknown-linux-gnu`):

```bash
./sherpa-commands/build-patched-sherpa.sh
SHERPA_ONNX_LIB_DIR=$PWD/sherpa-commands/.sherpa-libs/aarch64-apple-darwin \
  cargo build --release
SHERPA_ONNX_LIB_DIR=... cargo test
(cd whisperkit-stt && swift build -c release && swift test)   # macOS
```

Copy the binaries from `target/release/` (and `whisperkit-stt/.build/release/`)
next to the plugin's `voice-plugin`; on macOS, sign each copy, since a copied
binary that is not re-signed is killed at launch. The stage SDK's conformance
suite (`branchkit-stage-sdk-test`) checks a built stage against the protocol.

Models are not in the tree. A stage receives voice's model root in
`BRANCHKIT_ARTIFACTS_DIR` from the platform; outside it, `whisper_stage` reads
`WHISPER_MODEL_PATH`. `branchkit-cli artifact download` provisions the models.

## Dictation latency

Off macOS, dictation waits on `whisper_stage` after the key is released.
`../scripts/whisper-latency.py` measures that wait: it speaks 5, 15 and 30 s
clips with the system's text-to-speech (or takes `--clips DIR`), fetches each
whisper.cpp model `plugin.json` declares, and times `whisper_stage probe` on
them, one warm-up and three timed decodes each, printing decode time and
real-time factor beside the CPU and thread count.

```bash
cargo build --release -p whisper-stt
python3 ../scripts/whisper-latency.py --json latency.json
```

The `Whisper latency` workflow (`../.github/workflows/whisper-latency.yml`)
runs the same script on x86 Linux and Windows runners. It starts only by
hand: `gh workflow run whisper-latency.yml`.
