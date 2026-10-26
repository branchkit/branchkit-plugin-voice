import CoreML
import Foundation
import WhisperKit
import TranscriptScoring
import StageLog

/// Adds a constant to the logit of every token in the bias vocabulary, at every
/// decode step.
///
/// This is how dictation biasing is done here, and the reason is measured.
/// `DecodingOptions.promptTokens` — the obvious mechanism — puts text in the
/// decoder's CONTEXT, and on whisper-large-v3 that is brittle in a way no rule
/// predicts: a 12-token prompt empties every transcript, an 18-token one
/// improves them, adding a full stop to the winning prompt breaks it again.
/// A settings table whose contents change cannot sit on top of that.
///
/// A logits filter never touches context. It cannot tip the model toward an
/// early end-of-text, because there is nothing for the model to read. Measured
/// over 18 clips (`scripts/wkprobe`): 0 empties at every boost from 1 to 8,
/// against 2/18 for the best prompt, and every single change it made was a
/// correction — `Branch Kit`→`BranchKit`, `sherpa`→`Sherpa`, `snip it`→`snippet`.
final class VocabularyBoostFilter: LogitsFiltering {
    /// 2.0 sits in the middle of a flat plateau: boosts of 1, 2 and 4 produced
    /// byte-identical output, and only at 8 did collateral changes appear
    /// (13 clips touched instead of 4). Picked for the width of the margin
    /// around it, not for being the best score.
    static let defaultBoost: Float = 2.0

    private let lock = NSLock()
    private var indexes: [[NSNumber]] = []
    private let boost: Float

    init(boost: Float) { self.boost = boost }

    /// Replace the boosted set. Called from the read loop while the decode loop
    /// may be running, hence the lock.
    func setTokens(_ ids: Set<Int>) {
        let built = ids.sorted().map { [0, 0, $0 as NSNumber] }
        lock.lock()
        indexes = built
        lock.unlock()
    }

    func filterLogits(_ logits: MLMultiArray, withTokens _: [Int]) -> MLMultiArray {
        lock.lock()
        let idx = indexes
        lock.unlock()
        for i in idx {
            logits[i] = NSNumber(value: logits[i].floatValue + boost)
        }
        return logits
    }
}

let stdoutHandle = FileHandle.standardOutput
let stderrHandle = FileHandle.standardError

/// A condition this stage cannot start under. The caller logs it as
/// `fatal: failed to load model` and exits non-zero — refuse-don't-degrade,
/// which for a speech stage means the pipeline fails visibly instead of
/// transcribing nothing.
enum StageError: LocalizedError {
    case fatal(String)

    var errorDescription: String? {
        switch self {
        case .fatal(let message): return message
        }
    }
}

func writeStdout(_ string: String) {
    guard let data = string.data(using: .utf8) else { return }
    stdoutHandle.write(data)
}

func writeStderr(_ string: String) {
    guard let data = string.data(using: .utf8) else { return }
    stderrHandle.write(data)
}

/// One diagnostic in the structured stage-log format (`StageLog`), so the
/// platform correlates it to `session`'s `tr_`. Empty `session` for a line
/// that belongs to no dictation (start-up, configuration).
func slog(_ message: String, level: String = "info", session: String = "") {
    writeStderr(stageLogLine(level: level, session: session, message: message))
}

/// A compute-units setting as the `[PERF]` line names it.
func computeLabel(_ units: MLComputeUnits) -> String {
    switch units {
    case .cpuOnly: return "cpu"
    case .cpuAndGPU: return "gpu"
    case .cpuAndNeuralEngine: return "ane"
    case .all: return "all"
    @unknown default: return "other"
    }
}

func writeEvent(type eventType: String, data: [String: Any], payloadLength: Int = 0) {
    var event: [String: Any] = ["type": eventType, "data": data]
    if payloadLength > 0 {
        event["payload_length"] = payloadLength
    }
    guard let jsonData = try? JSONSerialization.data(withJSONObject: event),
          let jsonString = String(data: jsonData, encoding: .utf8) else { return }
    writeStdout(jsonString + "\n")
}

func parseArgs() -> (model: String, language: String) {
    let env = ProcessInfo.processInfo.environment
    var modelName = env["WHISPER_MODEL"] ?? "openai_whisper-large-v3-v20240930"
    var language = env["WHISPER_LANGUAGE"] ?? "en"

    var i = 1
    let args = CommandLine.arguments
    while i < args.count {
        if args[i] == "--model" && i + 1 < args.count {
            modelName = args[i + 1]
            i += 2
        } else if args[i] == "--language" && i + 1 < args.count {
            language = args[i + 1]
            i += 2
        } else {
            i += 1
        }
    }

    return (modelName, language)
}

class WhisperKitStage {
    private var whisperKit: WhisperKit?
    private var currentSessionId: String = ""
    private var pcmSamples: [Int16] = []
    private var framesSinceCredit: Int = 0
    // Chunk stamps for the audio-cutoff path:
    // (absolute end-sample since session start, the chunk's shared-clock
    // timestamp_ms). An audio_stop carrying cutoff_ms truncates the buffer at
    // that position before transcribing, so the stop phrase never reaches the
    // model.
    private var chunkMarks: [(endSample: Int, timestampMs: UInt64)] = []
    // Whether this session reached maxSamples (said once, when it does).
    private var capped: Bool = false
    private let modelName: String
    private let language: String

    // Vocabulary biasing through the logits filter below. The words arrive as
    // configuration from the voice plugin (`ext.branchkit.dictation_bias`);
    // the token IDs are derived once per change, at transcribe time, because
    // the tokenizer needs the model and the config can arrive before it has
    // loaded. (The decoder prompt is NOT used: see `promptTokens` in
    // `transcribe()`.)
    private let biasEventType = "ext.branchkit.dictation_bias"
    private var biasWords: [String] = []
    private var biasTokensStale: Bool = false
    /// Installed once at model load and mutated in place afterwards, so a
    /// vocabulary change never needs the model reloaded.
    private let biasFilter = VocabularyBoostFilter(boost: VocabularyBoostFilter.defaultBoost)

    /// The shortest audio WhisperKit decodes, in seconds, for this clip
    /// (`windowClipTime(samples:windowSamples:)` in StageLog). WhisperKit's
    /// default is 1.0: `TranscribeTask` decodes a window only if it starts
    /// that long before the end of the clip, a guard against hallucinating on
    /// the cut-off tail of a STREAMING window. A dictation is a finished hold,
    /// so there is no such tail, and the default silently returned "" for
    /// every hold under a second — "Yes.", "Sounds good." — in 0-5 ms, with
    /// nothing in any log. Measured 2026-10-11, offline: eight
    /// 0.28-0.54 s `say` words empty on every run at 1.0, all eight correct
    /// at 0.1; live, the two sub-second holds in an hour of logs were both
    /// empty. 0.1 s is below the shortest spoken word, so every real hold
    /// (the post-roll alone is ~200 ms) reaches the model. Silence and noise
    /// of 0.2-0.9 s now decode to "you" / "Thank you." — what 1.2-1.8 s of
    /// silence already gave at 1.0, and what voice's hallucination correction
    /// blanks. Over one window the default stays: see the StageLog function.
    static func windowClipTime(samples: Int, kit: WhisperKit) -> Float {
        StageLog.windowClipTime(
            samples: samples,
            windowSamples: kit.featureExtractor.windowSamples ?? Constants.defaultWindowSamples)
    }

    private let initialCredit: Int = 8
    private let creditGrant: Int = 4
    // Safety bound: 11 minutes. The voice plugin stops a dictation at 10
    // (its dictationLimit); the extra minute covers the audio between its
    // timer and the first chunk. Past it the END is dropped, never the
    // start: keeping the last minutes typed a long dictation without its
    // opening. WhisperKit VAD chunking handles segmentation.
    private let maxSamples: Int = 16000 * 60 * 11

    init(modelName: String, language: String) {
        self.modelName = modelName
        self.language = language
    }

    func loadModel() async throws {
        // The platform hands every stage its model root in BRANCHKIT_ARTIFACTS_DIR
        // and grants the sandbox exactly the model the pipeline named, under
        // that same root. This stage used to rebuild the path itself — app
        // support, the dev/prod app-name branch, and a hardcoded `whisperkit`
        // directory — which meant it knew a layout the platform owns, and a
        // stage that looks somewhere its sandbox does not grant fails as a
        // denial that reads exactly like a missing model.
        // BRANCHKIT_ARTIFACTS_DIR is the name since the models -> artifacts
        // rename; BRANCHKIT_MODELS_DIR is its deprecated one-release alias.
        // Read the new name first, so dropping the alias does not silently
        // take macOS dictation with it.
        let env = ProcessInfo.processInfo.environment
        guard let modelsRoot = [env["BRANCHKIT_ARTIFACTS_DIR"], env["BRANCHKIT_MODELS_DIR"]]
            .compactMap({ $0 }).first(where: { !$0.isEmpty }) else {
            throw StageError.fatal(
                "BRANCHKIT_ARTIFACTS_DIR is not set — this stage is spawned by the "
                    + "BranchKit pipeline, which sets it, and cannot locate models without it")
        }
        let modelFolder = URL(fileURLWithPath: modelsRoot)
            .appendingPathComponent(modelName)
            .path

        slog("loading model: \(modelName) from \(modelFolder)")

        let config = WhisperKitConfig(
            model: modelName,
            modelFolder: modelFolder,
            tokenizerFolder: URL(fileURLWithPath: modelFolder),
            logitsFilters: [biasFilter],
            verbose: false,
            logLevel: .error,
            download: false
        )
        whisperKit = try await WhisperKit(config)
        slog("model loaded")
        self.modelFolder = modelFolder
    }

    /// Where the loaded model lives, for the compute-plan diagnostic.
    private var modelFolder: String?

    /// Log the compute plan off the start path. Reading a plan loads each
    /// model again: about a second warm, and 20 s of a 45 s start with a
    /// cold compiled-model cache (measured 2026-10-06), during which the
    /// stage had not yet said it was ready and a dictation waited on it.
    /// The line is a tripwire for a regressed grant, not something a
    /// transcription needs, so it comes after the capability.
    func logComputeDispatchInBackground() {
        guard let modelFolder else { return }
        Task.detached(priority: .background) { [self] in
            await self.logComputeDispatch(modelFolder: modelFolder)
        }
    }

    // One boot line per model: which compute device each operation is planned
    // onto. This is the sandbox tripwire — the confined stage's ANE grants
    // lean on private service names, and if
    // an OS update invalidates them the failure must be attributable from the
    // log, not inferred from slow dictation. All-CPU dispatch here means the
    // grants regressed.
    private func logComputeDispatch(modelFolder: String) async {
        guard #available(macOS 14.4, *) else { return }
        for (file, units) in [
            ("AudioEncoder.mlmodelc", MLComputeUnits.cpuAndNeuralEngine),
            ("TextDecoder.mlmodelc", MLComputeUnits.cpuAndNeuralEngine),
        ] {
            let url = URL(fileURLWithPath: modelFolder).appendingPathComponent(file)
            guard FileManager.default.fileExists(atPath: url.path) else { continue }
            let cfg = MLModelConfiguration()
            cfg.computeUnits = units
            do {
                let plan = try await MLComputePlan.load(contentsOf: url, configuration: cfg)
                var counts: [String: Int] = [:]
                if case .program(let program) = plan.modelStructure {
                    for (_, fn) in program.functions {
                        for op in fn.block.operations {
                            if let usage = plan.deviceUsage(for: op) {
                                let label: String
                                switch usage.preferred {
                                case .cpu: label = "cpu"
                                case .gpu: label = "gpu"
                                case .neuralEngine: label = "ane"
                                @unknown default: label = "other"
                                }
                                counts[label, default: 0] += 1
                            }
                        }
                    }
                }
                let summary = counts.sorted { $0.key < $1.key }
                    .map { "\($0.key)=\($0.value)" }.joined(separator: " ")
                slog("compute plan \(file): \(summary)")
            } catch {
                slog("compute plan \(file): unavailable (\(error.localizedDescription))", level: "warn")
            }
        }
    }

    func emitCapability() {
        writeEvent(type: "capability", data: [
            "stage_type": "stt",
            "stage_name": "whisperkit-stt",
            "audio_formats": [["channels": 1, "rate": 16000, "width": 2]],
            "lifecycle_modes": ["persistent"],
            "feature_flags": [String: Any](),
            // Vocabulary the owning plugin wants the decoder biased toward.
            // The platform retains the last one sent and replays it when this
            // pipeline starts, so a respawn does not silently lose the bias.
            "accepts_config": [biasEventType],
        ])
    }

    func readLoop() {
        let fd = FileHandle.standardInput.fileDescriptor
        var leftover = Data()

        while true {
            var lineData = Data()
            while true {
                if let nlIndex = leftover.firstIndex(of: 0x0A) {
                    lineData = leftover.prefix(upTo: nlIndex)
                    leftover = leftover.suffix(from: leftover.index(after: nlIndex))
                    break
                }
                var buf = [UInt8](repeating: 0, count: 4096)
                let n = read(fd, &buf, buf.count)
                if n <= 0 { return }
                leftover.append(contentsOf: buf[0..<n])
            }

            guard let json = try? JSONSerialization.jsonObject(with: lineData) as? [String: Any],
                  let eventType = json["type"] as? String,
                  let data = json["data"] as? [String: Any] else {
                continue
            }

            let payloadLength = json["payload_length"] as? Int ?? 0

            var payload = Data()
            if payloadLength > 0 {
                if leftover.count >= payloadLength {
                    payload = leftover.prefix(payloadLength)
                    leftover = leftover.suffix(from: leftover.index(leftover.startIndex, offsetBy: payloadLength))
                } else {
                    payload = leftover
                    leftover = Data()
                    var remaining = payloadLength - payload.count
                    while remaining > 0 {
                        var buf = [UInt8](repeating: 0, count: remaining)
                        let n = read(fd, &buf, remaining)
                        if n <= 0 { break }
                        payload.append(contentsOf: buf[0..<n])
                        remaining -= n
                    }
                }
            }

            switch eventType {
            case "audio_start":
                if let sid = data["session_id"] as? String {
                    currentSessionId = sid
                    pcmSamples.removeAll()
                    chunkMarks.removeAll()
                    capped = false
                    framesSinceCredit = 0
                    slog("session start", session: sid)

                    writeEvent(type: "flow_credit", data: [
                        "frames": initialCredit,
                        "session_id": sid,
                    ])
                }

            case "audio_chunk":
                let room = maxSamples - pcmSamples.count
                if room > 0 {
                    payload.withUnsafeBytes { rawBuffer in
                        let buffer = rawBuffer.bindMemory(to: Int16.self)
                        pcmSamples.append(contentsOf: buffer.prefix(room))
                    }
                    if let ts = (data["timestamp_ms"] as? NSNumber)?.uint64Value {
                        chunkMarks.append((endSample: pcmSamples.count, timestampMs: ts))
                    }
                } else if !capped {
                    capped = true
                    slog("buffer full at \(maxSamples / 16000) s: later audio is not transcribed",
                         level: "warn", session: currentSessionId)
                }
                framesSinceCredit += 1
                if framesSinceCredit >= creditGrant {
                    framesSinceCredit = 0
                    writeEvent(type: "flow_credit", data: [
                        "frames": creditGrant,
                        "session_id": currentSessionId,
                    ])
                }

            case biasEventType:
                // Replaces rather than merges: the sender owns the whole list,
                // so a word it removed must actually disappear. Same posture as
                // the vocabulary union, and it keeps "what is the decoder
                // biased toward" answerable from one place.
                let words = (data["words"] as? [String]) ?? []
                if words != biasWords {
                    biasWords = words
                    biasTokensStale = true
                    slog("bias vocabulary: \(words.count) words")
                }

            case "audio_stop":
                if let cutoff = (data["cutoff_ms"] as? NSNumber)?.uint64Value {
                    truncateBuffer(atCutoffMs: cutoff)
                }
                slog("session end, transcribing \(pcmSamples.count) samples", session: currentSessionId)
                transcribe()

            default:
                break
            }
        }
    }

    // Drop buffered samples at/after the shared-clock cutoff: find the first
    // chunk whose end-stamp reached it, back off the in-chunk distance
    // (16 samples/ms at 16kHz). A cutoff past all captured audio keeps
    // everything (the stop raced ahead of the chunks — nothing to cut).
    private func truncateBuffer(atCutoffMs cutoff: UInt64) {
        guard let mark = chunkMarks.first(where: { $0.timestampMs >= cutoff }) else { return }
        let backSamples = Int(mark.timestampMs - cutoff) * 16
        let keep = max(0, mark.endSample - backSamples)
        if keep < pcmSamples.count {
            let dropped = pcmSamples.count - keep
            slog("cutoff \(cutoff)ms: dropping \(dropped) buffered samples (\(dropped / 16)ms)",
                 session: currentSessionId)
            pcmSamples.removeLast(dropped)
        }
    }

    /// Push the current bias vocabulary into the logits filter.
    ///
    /// Lazy because the tokenizer belongs to the loaded model and configuration
    /// can arrive before loading finishes — the live boot log has the voice
    /// plugin's push landing a second before this stage even starts, so
    /// resolving on receipt would drop the first vocabulary every boot.
    ///
    /// Every SUBWORD of a vocabulary word is boosted, which is coarse: those
    /// pieces occur in unrelated words too. Measured, that coarseness is
    /// harmless at this boost — the only changes it produced were corrections —
    /// but it is why the boost is small and why raising it is not free.
    private func refreshBiasTokensIfNeeded(_ kit: WhisperKit) {
        guard biasTokensStale else { return }
        biasTokensStale = false
        var ids = Set<Int>()
        if !biasWords.isEmpty, let tokenizer = kit.tokenizer {
            for word in biasWords {
                // Leading space: Whisper's BPE encodes a word differently at a
                // word boundary than mid-token, and dictation always hits it
                // at a boundary.
                for t in tokenizer.encode(text: " " + word)
                where t < tokenizer.specialTokens.specialTokenBegin && isWordPiece(tokenizer, t) {
                    ids.insert(t)
                }
            }
        }
        biasFilter.setTokens(ids)
        slog("bias: \(biasWords.count) words → \(ids.count) boosted tokens")
    }

    /// Every decoded text token of the results with the log-probability of
    /// choosing it, in order; special tokens (timestamps, end of text) left
    /// out.
    private func scoredTokens(_ results: [TranscriptionResult], _ kit: WhisperKit) -> [ScoredToken] {
        guard let tokenizer = kit.tokenizer else { return [] }
        let special = tokenizer.specialTokens.specialTokenBegin
        var out: [ScoredToken] = []
        for result in results {
            for segment in result.segments {
                for (i, token) in segment.tokens.enumerated() where token < special {
                    guard i < segment.tokenLogProbs.count,
                          let logprob = segment.tokenLogProbs[i][token] ?? segment.tokenLogProbs[i].values.first
                    else { continue }
                    out.append(ScoredToken(text: tokenizer.decode(tokens: [token]), logprob: logprob))
                }
            }
        }
        return out
    }

    /// Whether a token carries a letter or digit. A vocabulary entry like
    /// `git-bug` tokenizes as ` git`, `-`, `bug`, and boosting the bare `-`
    /// raises hyphens everywhere: on the owner's recorded dictations
    /// (2026-10-04, `scripts/wkseek`) that one entry turned "copy paste" into
    /// "copy-paste" and "Plugins" into "Plug-ins". Punctuation is Whisper's
    /// call, never the vocabulary's.
    private func isWordPiece(_ tokenizer: WhisperTokenizer, _ token: Int) -> Bool {
        tokenizer.decode(tokens: [token])
            .unicodeScalars.contains { CharacterSet.alphanumerics.contains($0) }
    }

    private func transcribe() {
        guard let kit = whisperKit else {
            writeEvent(type: "error", data: [
                "code": "model_not_loaded",
                "message": "WhisperKit model not loaded",
                "fatal": true,
            ])
            return
        }

        let samples = pcmSamples.map { Float($0) / 32768.0 }
        let sessionId = currentSessionId
        refreshBiasTokensIfNeeded(kit)
        let clipFloor = WhisperKitStage.windowClipTime(samples: samples.count, kit: kit)

        let done = DispatchSemaphore(value: 0)
        Task {
            do {
                let options = dictationDecodingOptions(language: language, clipFloor: clipFloor)
                let started = Date()
                let results = try await kit.transcribe(
                    audioArray: samples,
                    decodeOptions: options
                )
                let wallMs = Int((Date().timeIntervalSince(started) * 1000).rounded())
                // Transduction only. This stage used to carry text POLICY too —
                // an English hallucination denylist that blanked "you", "so",
                // "bye", "thanks for watching", and an all-caps corrector —
                // which silently deleted a user's dictated "so" with no surface
                // to see or change it. Both live in the voice plugin now, where
                // the user-editable corrections system already was.
                // There is no N-best to rescore: WhisperKit decodes greedily
                // (topK is a sampling knob; we run temperature 0), the
                // [Result] list is one per input audio array, and
                // tokenLogProbs keeps one entry per position (the chosen
                // token). Those per-token log-probabilities become the
                // contract's per-word scores (TranscriptScoring).
                let text = transcriptText(results)
                let tokens = scoredTokens(results, kit)
                let scores = wordScores(tokens, text: text)
                let confidence = transcriptConfidence(tokens, wordScores: scores)

                slog("transcript: \(text.prefix(80))", session: sessionId)
                if let empty = emptyTranscriptDiagnostic(
                    text: text, samples: samples.count, sampleRate: WhisperKit.sampleRate,
                    floorSeconds: clipFloor)
                {
                    slog(empty.message, level: empty.level, session: sessionId)
                }
                slog(perfLine(transcriptionCost(results), wallMs: wallMs, samples: samples.count,
                              tokens: tokens.count, model: modelName,
                              compute: computeSummary(kit.modelCompute)),
                     session: sessionId)
                var event: [String: Any] = [
                    "session_id": sessionId,
                    "text": text,
                    "final": true,
                    "partial": false,
                ]
                if let confidence { event["confidence"] = confidence }
                if let scores { event["word_scores"] = scores }
                writeEvent(type: "transcript", data: event)
            } catch {
                slog("transcription error: \(error)", level: "error", session: sessionId)
                writeEvent(type: "error", data: [
                    "code": "inference_failed",
                    "message": "WhisperKit inference failed: \(error.localizedDescription)",
                    "fatal": false,
                ])
            }
            done.signal()
        }
        done.wait()
    }
}

/// The decoding options every dictation decode uses, live or probed, so a
/// probe decodes exactly as the live stage would.
func dictationDecodingOptions(language: String, clipFloor: Float) -> DecodingOptions {
    let pinned = !language.isEmpty
    // Experiment knob (lab only): Whisper's reference decoder escapes a
    // degenerate decode by retrying hotter. Pinning the language turns
    // that off here, so a prompt-induced empty result has no recovery.
    // `scripts/whisperkit-prompt-lab.py` flips this to test whether
    // fallback rescues prompting.
    let fallbackCount =
        Int(ProcessInfo.processInfo.environment["WHISPER_TEMP_FALLBACK"] ?? "") ?? 0
    return DecodingOptions(
        task: .transcribe,
        language: pinned ? language : nil,
        temperature: 0,
        temperatureIncrementOnFallback: (pinned && fallbackCount == 0) ? 0 : 0.2,
        temperatureFallbackCount: pinned ? fallbackCount : 5,
        usePrefillPrompt: pinned,
        withoutTimestamps: true,
        windowClipTime: clipFloor,
        // promptTokens stays nil FOREVER on this stage. It is
        // not disabled pending work — it was measured and rejected:
        // brittle in a way no rule predicts, and unusable behind a
        // vocabulary the user edits. Biasing happens in
        // `VocabularyBoostFilter` instead.
        promptTokens: nil,
        chunkingStrategy:
            ProcessInfo.processInfo.environment["WHISPER_CHUNKING"] == "none"
            ? nil : .vad
    )
}

/// One transcription's text: every result's, trimmed, joined by spaces.
func transcriptText(_ results: [TranscriptionResult]) -> String {
    results
        .compactMap { $0.text.trimmingCharacters(in: .whitespacesAndNewlines) }
        .filter { !$0.isEmpty }
        .joined(separator: " ")
        .trimmingCharacters(in: .whitespacesAndNewlines)
}

/// WhisperKit's own timings for one transcription, summed over its results.
func transcriptionCost(_ results: [TranscriptionResult]) -> TranscriptionCost {
    var cost = TranscriptionCost()
    for t in results.map(\.timings) {
        cost.audioSeconds += t.inputAudioSeconds
        cost.logmels += t.logmels
        cost.encoding += t.encoding
        cost.prefill += t.prefill
        cost.decodingLoop += t.decodingLoop
        cost.fullPipeline += t.fullPipeline
        cost.decodingLoops += Int(t.totalDecodingLoops)
        cost.fallbacks += Int(t.totalDecodingFallbacks)
    }
    return cost
}

/// Where each model runs, as configured.
func computeSummary(_ c: ModelComputeOptions) -> String {
    "mel=\(computeLabel(c.melCompute)) encoder=\(computeLabel(c.audioEncoderCompute)) "
        + "decoder=\(computeLabel(c.textDecoderCompute)) prefill=\(computeLabel(c.prefillCompute))"
}

/// Point the frameworks' temp dir at the one the sandbox grants this stage.
///
/// The platform names this stage's own dir as `$TMPDIR` (under the per-user
/// temp dir, `T/branchkit/<id>/`) and refuses the rest of `T/`. Foundation
/// ignores `$TMPDIR` on macOS: `NSTemporaryDirectory()` reads
/// `confstr(_CS_DARWIN_USER_TEMP_DIR)`, which is the shared `T/`. Metal's
/// graph compiler (MPSGraph, which CoreML uses as soon as the GPU is reachable
/// — voice declares `stage.gpu` for refine_llm, and every voice stage gets it)
/// writes its executables there, gets refused, and aborts the process on an
/// assertion mid-transcription (measured 2026-10-06: every dictation ended
/// with no transcript). `_set_user_dir_suffix` moves `confstr`'s answer to
/// `T/<suffix>/`, so taking the suffix from `$TMPDIR` lands every framework
/// in the granted dir. Unconfined (no `$TMPDIR` under `T/`), nothing changes.
func adoptSandboxTempDir() {
    guard let tmp = ProcessInfo.processInfo.environment["TMPDIR"] else { return }
    var buf = [CChar](repeating: 0, count: Int(PATH_MAX))
    guard confstr(_CS_DARWIN_USER_TEMP_DIR, &buf, buf.count) > 0 else { return }
    let resolve = { (p: String) in URL(fileURLWithPath: p).resolvingSymlinksInPath().path }
    let base = resolve(String(cString: buf))
    let own = resolve(tmp)
    guard own.hasPrefix(base + "/") else { return }
    let suffix = String(own.dropFirst(base.count + 1))
    typealias SetSuffix = @convention(c) (UnsafePointer<CChar>?) -> Bool
    guard let sym = dlsym(dlopen(nil, RTLD_NOW), "_set_user_dir_suffix") else { return }
    let setSuffix = unsafeBitCast(sym, to: SetSuffix.self)
    if !setSuffix(suffix) {
        slog("could not move the temp dir to \(suffix)", level: "warn")
    }
}

adoptSandboxTempDir()

// `probe`: the offline re-decode the platform's recognition.redecode runs
// (Probe.swift). It exits when done; nothing below runs.
if CommandLine.arguments.dropFirst().first == "probe" {
    runProbe()
}

let (modelName, language) = parseArgs()
let stage = WhisperKitStage(modelName: modelName, language: language)

slog("starting (model: \(modelName), language: \(language))")

let modelReady = DispatchSemaphore(value: 0)
Task {
    do {
        try await stage.loadModel()
    } catch {
        slog("fatal: failed to load model: \(error)", level: "error")
        exit(1)
    }
    modelReady.signal()
}
modelReady.wait()

stage.emitCapability()
stage.logComputeDispatchInBackground()

stage.readLoop()
