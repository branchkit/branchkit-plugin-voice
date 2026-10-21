import Foundation
import WhisperKit

// `whisperkit_stage probe`: the offline re-decode the platform's
// recognition.redecode op runs, so a plugin can compare dictation models on
// recordings it kept (voice's Models tab). It is the sherpa command stage's
// probe wire: one JSON request on stdin,
//
//     {"model": "<absolute model dir>", "language": "en",
//      "items": [{"id": "...", "audio": "<absolute 16 kHz mono 16-bit WAV>"}]}
//
// and one JSON line per item on stdout, in order:
//
//     {"id": "...", "text": "...", "decode_ms": 812.4}
//     {"id": "...", "text": "", "error": "..."}
//
// Every item decodes with the live stage's options
// (dictationDecodingOptions). An item asking for noise (the sherpa probe's
// ladder) is refused with an error: this probe does not mix noise in. Not yet
// applied: the vocabulary boost the live
// stage takes from the plugin's bias events, so a probe matches a live decode
// made without one. A command item's `words`, `grammar_dag` and `bias` are
// ignored. `word_margins` is left out: Whisper's per-word scores are
// log-probabilities, not the margins that field reports. The model loads
// once, for every item.

struct ProbeRequest: Decodable {
    let model: String
    let language: String?
    let items: [ProbeItem]
}

struct ProbeItem: Decodable {
    let id: String
    let audio: String
    /// A noise ladder step the sherpa probe mixes in. This probe does not, so
    /// an item asking for one is refused, never decoded clean and reported as
    /// if it had been.
    let noise: ProbeNoise?
}

struct ProbeNoise: Decodable {
    let snr_db: Double
}

/// Reads a 16 kHz mono 16-bit PCM WAV into samples in [-1, 1].
func readPCM16Mono(_ path: String) throws -> [Float] {
    let data = try Data(contentsOf: URL(fileURLWithPath: path))
    guard data.count >= 12, data.prefix(4) == Data("RIFF".utf8), data[8..<12] == Data("WAVE".utf8) else {
        throw StageError.fatal("not a WAV file")
    }
    var offset = 12
    var format: (channels: UInt16, rate: UInt32, bits: UInt16)?
    while offset + 8 <= data.count {
        let id = String(decoding: data[offset..<offset + 4], as: UTF8.self)
        let size = Int(data[offset + 4..<offset + 8].withUnsafeBytes { $0.loadUnaligned(as: UInt32.self) })
        let body = offset + 8
        guard body + size <= data.count else { break }
        if id == "fmt " && size >= 16 {
            let f = data[body..<body + 16]
            format = (
                f.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 2, as: UInt16.self) },
                f.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 4, as: UInt32.self) },
                f.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 14, as: UInt16.self) }
            )
        } else if id == "data" {
            guard let fmt = format, fmt.channels == 1, fmt.rate == 16000, fmt.bits == 16 else {
                throw StageError.fatal("want 16 kHz mono 16-bit PCM")
            }
            return data[body..<body + size].withUnsafeBytes { raw in
                raw.bindMemory(to: Int16.self).map { Float(Int16(littleEndian: $0)) / 32768.0 }
            }
        }
        offset = body + size + (size & 1)
    }
    throw StageError.fatal("no audio data")
}

/// One result line, as JSON.
func probeLine(_ fields: [String: Any]) {
    guard let json = try? JSONSerialization.data(withJSONObject: fields),
          let line = String(data: json, encoding: .utf8) else { return }
    writeStdout(line + "\n")
}

/// Runs the probe to completion and exits: 0 when the request was read and the
/// model loaded (per-item failures are lines of their own), 1 otherwise.
func runProbe() -> Never {
    let input = FileHandle.standardInput.readDataToEndOfFile()
    guard let req = try? JSONDecoder().decode(ProbeRequest.self, from: input) else {
        slog("probe: bad request", level: "error")
        exit(1)
    }
    let language = req.language ?? "en"
    let finished = DispatchSemaphore(value: 0)
    Task {
        let kit: WhisperKit
        do {
            kit = try await WhisperKit(WhisperKitConfig(
                model: URL(fileURLWithPath: req.model).lastPathComponent,
                modelFolder: req.model,
                tokenizerFolder: URL(fileURLWithPath: req.model),
                verbose: false,
                logLevel: .error,
                download: false
            ))
        } catch {
            slog("probe: failed to load model \(req.model): \(error)", level: "error")
            exit(1)
        }
        slog("probe: model=\(URL(fileURLWithPath: req.model).lastPathComponent) items=\(req.items.count)")
        for item in req.items {
            if item.noise != nil {
                probeLine(["id": item.id, "text": "", "error": "this probe does not mix in noise"])
                continue
            }
            do {
                let samples = try readPCM16Mono(item.audio)
                let options = dictationDecodingOptions(
                    language: language,
                    clipFloor: WhisperKitStage.windowClipTime(samples: samples.count, kit: kit))
                let started = Date()
                let results = try await kit.transcribe(audioArray: samples, decodeOptions: options)
                let ms = Date().timeIntervalSince(started) * 1000
                // A decimal, so the line carries 812.4 rather than 812.39999999999998.
                let decodeMs = NSDecimalNumber(string: String(format: "%.1f", ms))
                probeLine(["id": item.id, "text": transcriptText(results), "decode_ms": decodeMs])
            } catch {
                probeLine(["id": item.id, "text": "", "error": "\(error)"])
            }
        }
        finished.signal()
    }
    finished.wait()
    exit(0)
}
