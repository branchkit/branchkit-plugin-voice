import Foundation

// The stage's diagnostics in the platform's structured stage-log format,
// `BKLOG1<TAB><level><TAB><session_id><TAB><message>`, the one the Rust,
// Go, TS and Python stage SDKs write. The platform turns each line into a
// `stage.diagnostic` event correlated to the session's `tr_`; a plain stderr
// line reaches the log uncorrelated and at info, which is how this stage's
// lines arrived before (0 of 700 carried a `tr_`). Kept free of
// WhisperKit so `swift test` checks it without a model.

public let stageLogPrefix = "BKLOG1\t"

/// One structured line, newline-terminated. Embedded newlines are flattened
/// to spaces so one diagnostic stays one line.
public func stageLogLine(level: String, session: String, message: String) -> String {
    let flat = message.replacingOccurrences(of: "\r", with: " ")
        .replacingOccurrences(of: "\n", with: " ")
    return "\(stageLogPrefix)\(level)\t\(session)\t\(flat)\n"
}

/// What one transcription cost, from WhisperKit's own timings (seconds, as
/// WhisperKit reports them). Summed over the results when the audio came
/// back as more than one.
public struct TranscriptionCost: Equatable {
    public var audioSeconds: Double = 0
    public var logmels: Double = 0
    public var encoding: Double = 0
    public var prefill: Double = 0
    public var decodingLoop: Double = 0
    public var fullPipeline: Double = 0
    public var decodingLoops: Int = 0
    public var fallbacks: Int = 0

    public init() {}
}

/// The `[PERF]` line for one transcription, the dictation counterpart of the
/// command stage's `[PERF] recognized` line. Durations, counts and fixed
/// labels only: never the transcript.
public func perfLine(
    _ cost: TranscriptionCost,
    wallMs: Int,
    samples: Int,
    tokens: Int,
    model: String,
    compute: String
) -> String {
    func ms(_ s: Double) -> Int { Int((s * 1000).rounded()) }
    return "[PERF] transcribed: audio_s=\(String(format: "%.2f", cost.audioSeconds)) "
        + "samples=\(samples) latency=\(wallMs)ms pipeline=\(ms(cost.fullPipeline))ms "
        + "mel=\(ms(cost.logmels))ms encoder=\(ms(cost.encoding))ms "
        + "prefill=\(ms(cost.prefill))ms decode=\(ms(cost.decodingLoop))ms "
        + "decode_loops=\(cost.decodingLoops) fallbacks=\(cost.fallbacks) tokens=\(tokens) "
        + "model=\(model) compute=\(compute)"
}

/// The shortest audio a dictation hold needs to be decoded, in seconds.
public let shortHoldWindowClipTime: Float = 0.1
/// WhisperKit's own `windowClipTime` default.
public let whisperKitDefaultWindowClipTime: Float = 1.0

/// The `windowClipTime` for one transcription of `samples` samples.
///
/// WhisperKit uses it in two places. Within one window it is how close to the
/// end of the clip a window may start, and its 1.0 s default returned "" for
/// every hold under a second. For audio longer than one window
/// (`windowSamples`, 30 s), `VADAudioChunker.chunkAll` also uses it to skip a
/// trailing chunk shorter than it, which is usually the silence after the
/// last word. There, 0.1 s would decode that silence on its own and append
/// "Thank you." to a long dictation, which voice's anchored hallucination
/// row cannot strip. So: 0.1 s for a clip that fits in one window, the
/// default for anything longer.
public func windowClipTime(samples: Int, windowSamples: Int) -> Float {
    samples <= windowSamples ? shortHoldWindowClipTime : whisperKitDefaultWindowClipTime
}

/// The diagnostic for a transcription that came back empty, nil for one that
/// did not. An empty result types nothing, so the log says which kind it was:
/// audio at or under `floorSeconds` (WhisperKit's `windowClipTime`, whose
/// window loop runs only while `seek < samples - floor`) never reached the
/// model at all; anything longer did, and the model wrote nothing.
public func emptyTranscriptDiagnostic(
    text: String,
    samples: Int,
    sampleRate: Int,
    floorSeconds: Float
) -> (level: String, message: String)? {
    guard text.isEmpty else { return nil }
    let seconds = String(format: "%.2f", Double(samples) / Double(sampleRate))
    let floor = String(format: "%.2f", floorSeconds)
    if samples <= Int(floorSeconds * Float(sampleRate)) {
        return ("warn", "empty transcript: \(seconds) s of audio is at or under the "
            + "\(floor) s decode floor, not transcribed")
    }
    return ("info", "empty transcript: the model heard no words in \(seconds) s of audio")
}
