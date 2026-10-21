import Foundation
import Testing
@testable import TranscriptScoring

// The same cases as the whisper.cpp stage's scoring tests, so the two
// platforms are held to one scale.
struct TranscriptScoringTests {
    private func tok(_ text: String, _ p: Float) -> ScoredToken {
        ScoredToken(text: text, logprob: Float(log(Double(p))))
    }

    @Test func wordsGroupTheirPiecesAndTrailingPunctuation() {
        let tokens = [tok(" Open", 0.9), tok(" Branch", 0.8), tok("Kit", 0.1), tok(".", 0.99)]
        let ws = wordScores(tokens, text: "Open BranchKit.")
        #expect(ws?.count == 2)
        #expect((ws?[0] ?? -1) > 0)
        #expect((ws?[1] ?? 1) < 0)
    }

    @Test func aWordAtExactlyEvenOddsScoresZero() {
        let ws = wordScores([tok(" yes", 0.5)], text: "yes")
        #expect(abs(ws?[0] ?? 1) < 1e-5)
    }

    @Test func misalignedTokensGiveNoScores() {
        #expect(wordScores([tok(" hello", 0.9), tok(" world", 0.9)], text: "hello big world") == nil)
        #expect(wordScores([], text: "hello") == nil)
    }

    @Test func theFirstTokenStartsAWordWithoutASpace() {
        #expect(wordScores([tok("Hi", 0.9), tok(" there", 0.9)], text: "Hi there")?.count == 2)
    }

    @Test func confidenceIsTheMeanOfWordScoresElseOfTokens() {
        #expect(transcriptConfidence([], wordScores: [1, 3]) == 2)
        #expect(abs(transcriptConfidence([tok(" a", 0.5), tok(" b", 0.5)], wordScores: nil) ?? 1) < 1e-5)
        #expect(transcriptConfidence([], wordScores: nil) == nil)
    }
}
