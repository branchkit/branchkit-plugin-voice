import Foundation
// Per-word scores for a transcript, on the stage contract's terms: sign is the
// contract, magnitude the engine's own scale, positive means the audio
// supported the word. This stage's scale is the word's mean natural-log token
// probability MINUS ln(0.5): positive when the decoder gave the word's tokens
// better than even odds. The whisper.cpp stage (Linux, Windows) computes the
// same quantity, so consumers read one scale on every OS.

/// ln(0.5), the even-odds line.
private let evenOddsLn: Float = -0.6931472

/// One decoded token: its text as the tokenizer renders it (a leading space
/// marks a word start) and the log-probability of choosing it.
public struct ScoredToken: Equatable {
    public let text: String
    public let logprob: Float
    public init(text: String, logprob: Float) {
        self.text = text
        self.logprob = logprob
    }
}

/// The per-word scores for `text`, aligned 1:1 with its whitespace-split
/// words, or nil when the tokens cannot be aligned to those words: no scores
/// serve a consumer better than scores on the wrong words.
///
/// A token whose text begins with whitespace starts a word; any other token (a
/// word's later piece, or punctuation) belongs to the word before it, which is
/// how whitespace splitting groups "Kit." with "Branch".
public func wordScores(_ tokens: [ScoredToken], text: String) -> [Float]? {
    var words: [(sum: Float, count: Int)] = []
    for t in tokens {
        if t.text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { continue }
        if t.text.first?.isWhitespace == true || words.isEmpty {
            words.append((t.logprob, 1))
        } else {
            words[words.count - 1].sum += t.logprob
            words[words.count - 1].count += 1
        }
    }
    let textWords = text.split(whereSeparator: { $0.isWhitespace }).count
    guard !words.isEmpty, words.count == textWords else { return nil }
    return words.map { $0.sum / Float($0.count) - evenOddsLn }
}

/// The transcript-level score: the mean of the word scores when there are
/// any, else the mean token log-probability on the same scale.
public func transcriptConfidence(_ tokens: [ScoredToken], wordScores: [Float]?) -> Float? {
    if let ws = wordScores, !ws.isEmpty {
        return ws.reduce(0, +) / Float(ws.count)
    }
    let counted = tokens
        .filter { !$0.text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }
        .map(\.logprob)
    guard !counted.isEmpty else { return nil }
    return counted.reduce(0, +) / Float(counted.count) - evenOddsLn
}
