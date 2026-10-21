# Voice plugin

The first-party BranchKit plugin for speech: spoken commands, dictation, the
phonetic alphabet, calibration, and the command HUD. It is an ordinary
BranchKit plugin (Go, `id: voice`) running under the same sandbox and grants
as any third-party one; the platform does the matching and routing, and this
plugin supplies the recognition pipelines, the core command set and the
settings UI around speech.

MIT (`LICENSE`), except the sherpa-onnx patches under
`stages/sherpa-commands/patches/`, which are Apache-2.0 like sherpa-onnx itself.
Developed in the BranchKit app workspace at `plugins/voice/`.

## What it provides

- **Two pipelines**, seeded into `_platform.pipelines` from `plugin.json`
  (`collection_data`):
  - `command_recognition` — mic, then the `sherpa_commands` stage (NeMo CTC,
    dynamic grammar). Dispatch is delegated to the platform matcher.
  - `dictation` — mic, then the `_platform.stt` role, which resolves to this
    plugin's `whisperkit_stt` stage on macOS and `whisper_stt` on Linux and
    Windows. Dispatch is `plugin`: the transcript is delivered to this plugin,
    which places the text and keeps the dictation history.
- **Stages** it ships (`provides.stages`): `vad_gate`, `sherpa_commands`,
  `whisperkit_stt` (macOS), `whisper_stt` (Linux, Windows) and `refine_llm`.
  Their source is under `stages/`; see `stages/README.md`.
- **Models** (`provides.artifacts`), downloaded by `branchkit-cli`, not kept in
  git, each pinned by revision or sha256: the command models (NVIDIA NeMo CTC,
  CC BY 4.0, BranchKit's ONNX conversions on Hugging Face; the NOTICE is under
  `assets/`), Silero VAD (MIT), WhisperKit models (macOS, MIT), whisper.cpp
  models (Linux, Windows, MIT) and the Qwen models dictation clean-up and
  styles use (Apache-2.0).
- **Default bindings** (`_platform.bindings`): hold Option+G for commands, hold
  Option+T for dictation, Esc cancels a dictation in progress, Option+C toggles
  continuous listening, Option+B toggles the command help HUD.
- **Core commands** in `commands.json` (editing, navigation, keyboard, system,
  applications, dictation, repeat, disambiguation), plus the `alphabet`,
  `number` and `direction` entities under `entities/`.
- **Settings tabs**: Get started (the microphone, the speech models and a
  quick check that it all works), Dictation, Command Editor, Alphabet,
  Calibration, Recordings, Models (every model with its license; command models compared on your
  confirmed recordings and on a phrase said on the spot, dictation models on
  sentences read aloud from a fixed set), HUD. Its pipelines and stages show on the
  Overview tab the platform gives every plugin.
- **HUD channels**: `main` (command list), `discovery` (live transcript),
  `notification` (toasts).

The full declaration (privileges, consumed collections, action types, effects)
is `plugin.json`; it is the source of truth for everything above.

## Layout

| Path | Contents |
|---|---|
| `plugin.json` | Manifest |
| `commands.json`, `entities/`, `collections/` | Command set, entity lists, collection schemas |
| `assets/sherpa-offline-nemo/` | The command model's NOTICE (the model and tokenizer are fetched) |
| `src/` | Go module: `main.go` wiring, `actions.go` dispatch, dictation (`dictation_*.go`), engine and pipeline control (`engine_select.go`, `input_device.go`), settings rendering |
| `src/speech/` | Phonetics, vocabulary, dictation bias and corrections |
| `src/calibration/` | Calibration, practice and recognition-quality measurement |
| `src/hud/`, `src/views/` | HUD rendering and templ settings views |
| `src/actions_gen.go`, `src/collections_gen.go` | Generated from `plugin.json` by `branchkit-gen` |
| `stages/` | The pipeline stages it ships (Rust workspace, plus the Swift `whisperkit-stt`) |

## Build and test

```bash
cd src
templ generate
go build -o ../voice-plugin .
go test ./...
```

That is the build recipe `dev.build` in `plugin.json`. The stages build
separately (`stages/README.md`), and their binaries go next to
`voice-plugin`, where the platform looks for a plugin's stages.

Recognition and matching are split. The `sherpa_commands` stage decodes
against the union of every plugin's command words, so what it can hear stays
stable as context changes; the platform's matcher then decides which of the
recognized commands count right now, from the tags each command requires.
Only an exclusive mode narrows the recognizer's grammar itself.
