import Foundation
import Testing
@testable import StageLog

struct StageLogTests {
    @Test func aLineIsTheStructuredStageLogFormat() {
        #expect(stageLogLine(level: "info", session: "sess-1", message: "model loaded")
            == "BKLOG1\tinfo\tsess-1\tmodel loaded\n")
        #expect(stageLogLine(level: "warn", session: "", message: "a\nb\rc")
            == "BKLOG1\twarn\t\ta b c\n")
    }

    @Test func thePerfLineCarriesTimingsAndNoText() {
        var cost = TranscriptionCost()
        cost.audioSeconds = 3.456
        cost.logmels = 0.012
        cost.encoding = 0.6104
        cost.prefill = 0.003
        cost.decodingLoop = 0.2
        cost.fullPipeline = 0.83
        cost.decodingLoops = 14
        let line = perfLine(cost, wallMs: 845, samples: 55296, tokens: 12,
                            model: "openai_whisper-large-v3-v20240930",
                            compute: "mel=gpu encoder=ane decoder=ane prefill=cpu")
        #expect(line == "[PERF] transcribed: audio_s=3.46 samples=55296 latency=845ms "
            + "pipeline=830ms mel=12ms encoder=610ms prefill=3ms decode=200ms "
            + "decode_loops=14 fallbacks=0 tokens=12 "
            + "model=openai_whisper-large-v3-v20240930 "
            + "compute=mel=gpu encoder=ane decoder=ane prefill=cpu")
    }

    @Test func aClipThatFitsOneWindowUsesTheShortHoldFloor() {
        #expect(windowClipTime(samples: 4800, windowSamples: 480_000) == 0.1)
        #expect(windowClipTime(samples: 480_000, windowSamples: 480_000) == 0.1)
    }

    @Test func aClipLongerThanOneWindowKeepsWhisperKitsDefault() {
        // Over one window WhisperKit VAD-chunks the audio, and the floor also
        // decides whether a short trailing chunk (silence) is decoded alone.
        #expect(windowClipTime(samples: 480_001, windowSamples: 480_000) == 1.0)
        #expect(windowClipTime(samples: 16000 * 90, windowSamples: 480_000) == 1.0)
    }

    @Test func aTranscriptWithTextHasNoEmptyDiagnostic() {
        #expect(emptyTranscriptDiagnostic(text: "Yes.", samples: 8000, sampleRate: 16000,
                                          floorSeconds: 0.1) == nil)
    }

    @Test func audioAtOrUnderTheFloorIsReportedAsNeverTranscribed() {
        let d = emptyTranscriptDiagnostic(text: "", samples: 1600, sampleRate: 16000, floorSeconds: 0.1)
        #expect(d?.level == "warn")
        #expect(d?.message == "empty transcript: 0.10 s of audio is at or under the "
            + "0.10 s decode floor, not transcribed")
    }

    @Test func audioOverTheFloorIsReportedAsHeardNothing() {
        let d = emptyTranscriptDiagnostic(text: "", samples: 1601, sampleRate: 16000, floorSeconds: 0.1)
        #expect(d?.level == "info")
        #expect(d?.message == "empty transcript: the model heard no words in 0.10 s of audio")
    }
}
