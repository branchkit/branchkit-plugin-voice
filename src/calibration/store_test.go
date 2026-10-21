package calibration

import (
	"testing"
	"time"
)

// targetsForWords builds an acoustic-only walk queue from literal words.
func targetsForWords(words ...string) []Target {
	out := make([]Target, len(words))
	for i, w := range words {
		out[i] = CodewordTarget(w)
	}
	return out
}

func TestNewID(t *testing.T) {
	id1 := NewID()
	id2 := NewID()

	if len(id1) != 32 {
		t.Errorf("ID length = %d, want 32", len(id1))
	}
	if id1 == id2 {
		t.Error("two generated IDs should not be equal")
	}
}

func TestFinishGameWritesVerificationRecords(t *testing.T) {
	s := NewState()

	var attempts []*VerificationAttempt
	var sessions []*VerificationSession
	s.Append = func(collection string, payload any) error {
		switch collection {
		case "plugin.voice.verification_attempts":
			if a, ok := payload.(*VerificationAttempt); ok {
				attempts = append(attempts, a)
			}
		case "plugin.voice.verification_sessions":
			if sess, ok := payload.(*VerificationSession); ok {
				sessions = append(sessions, sess)
			}
		}
		return nil
	}

	words := []string{"arch", "bake", "check"}
	s.mu.Lock()
	s.game.Start(targetsForWords(words...))
	s.game.GrammarWords = words
	s.game.Source = "alphabet"
	s.game.SourcePrefix = nil

	// "arch" — matched, advance
	s.game.ProcessFrame("arch", true)
	s.game.Advance()

	// "bake" — misheard as "bait", advance (will be failed)
	s.game.ProcessFrame("bait", true)
	s.game.Advance()

	// "check" — matched, advance
	s.game.ProcessFrame("check", true)
	s.game.Advance()

	s.finishGame()
	s.mu.Unlock()

	// 3 words, 3 attempts (one per trial)
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(attempts))
	}

	// arch matched
	if !attempts[0].Matched {
		t.Error("arch: expected matched = true")
	}
	if attempts[0].Heard != "arch" {
		t.Errorf("arch: heard = %q, want %q", attempts[0].Heard, "arch")
	}

	// bake failed
	if attempts[1].Matched {
		t.Error("bake: expected matched = false")
	}
	if attempts[1].Heard != "bait" {
		t.Errorf("bake: heard = %q, want %q", attempts[1].Heard, "bait")
	}

	// check matched
	if !attempts[2].Matched {
		t.Error("check: expected matched = true")
	}

	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	sess := sessions[0]
	if sess.WordCount != 3 {
		t.Errorf("word_count = %d, want 3", sess.WordCount)
	}
	if sess.MatchCount != 2 {
		t.Errorf("match_count = %d, want 2", sess.MatchCount)
	}
	if sess.Group != "alphabet" {
		t.Errorf("group = %q, want %q", sess.Group, "alphabet")
	}
	if len(sess.GrammarSnapshot) != 3 {
		t.Errorf("grammar_snapshot len = %d, want 3", len(sess.GrammarSnapshot))
	}
}

func TestTrialDerived(t *testing.T) {
	trial := &Trial{
		Target:    "snap",
		StartedAt: time.Now().Add(-2 * time.Second),
		Frames: []Frame{
			{Text: "snip", IsFinal: false, OffsetMs: 300},
			{Text: "snap", IsFinal: true, OffsetMs: 800},
		},
	}

	if trial.FinalHeard() != "snap" {
		t.Errorf("FinalHeard() = %q, want %q", trial.FinalHeard(), "snap")
	}
	if trial.FirstHeard() != "snip" {
		t.Errorf("FirstHeard() = %q, want %q", trial.FirstHeard(), "snip")
	}
	if trial.Revisions() != 1 {
		t.Errorf("Revisions() = %d, want 1", trial.Revisions())
	}
	if !trial.Matched() {
		t.Error("expected Matched() = true")
	}
}

func TestAdvance(t *testing.T) {
	g := NewGame()
	g.Start(targetsForWords("arch", "bake"))

	g.ProcessFrame("arch", true)
	gameOver := g.Advance()
	if gameOver {
		t.Error("game should not be over after first advance")
	}
	if g.CurrentIndex != 1 {
		t.Errorf("CurrentIndex = %d, want 1", g.CurrentIndex)
	}
	if len(g.Results) != 1 {
		t.Fatalf("Results len = %d, want 1", len(g.Results))
	}
	if !g.Results[0].Passed {
		t.Error("arch should be passed")
	}

	g.ProcessFrame("bake", true)
	gameOver = g.Advance()
	if !gameOver {
		t.Error("game should be over after last advance")
	}
}

func TestRetry(t *testing.T) {
	g := NewGame()
	g.Start(targetsForWords("arch"))

	g.ProcessFrame("wrong", true)
	g.Retry()

	if g.CurrentTrial == nil {
		t.Fatal("CurrentTrial should not be nil after retry")
	}
	if len(g.CurrentTrial.Frames) != 0 {
		t.Errorf("Frames should be empty after retry, got %d", len(g.CurrentTrial.Frames))
	}
	if g.CurrentIndex != 0 {
		t.Errorf("CurrentIndex = %d, want 0", g.CurrentIndex)
	}
}

func TestBack(t *testing.T) {
	g := NewGame()
	g.Start(targetsForWords("arch", "bake"))

	g.ProcessFrame("arch", true)
	g.Advance()

	g.Back()
	if g.CurrentIndex != 0 {
		t.Errorf("CurrentIndex = %d, want 0", g.CurrentIndex)
	}
	if len(g.Results) != 0 {
		t.Errorf("Results len = %d, want 0", len(g.Results))
	}
}

func TestSkip(t *testing.T) {
	g := NewGame()
	g.Start(targetsForWords("arch", "bake"))

	gameOver := g.Skip()
	if gameOver {
		t.Error("game should not be over after skipping first word")
	}
	if len(g.Results) != 1 {
		t.Fatalf("Results len = %d, want 1", len(g.Results))
	}
	if g.Results[0].Trial.Outcome != "skipped" {
		t.Errorf("outcome = %q, want %q", g.Results[0].Trial.Outcome, "skipped")
	}
	if g.Results[0].Passed {
		t.Error("skipped word should not be passed")
	}
}

func TestNoAutoAdvance(t *testing.T) {
	g := NewGame()
	g.Start(targetsForWords("arch", "bake"))

	// Process a matching frame — should NOT auto-advance
	g.ProcessFrame("arch", true)
	if g.CurrentIndex != 0 {
		t.Errorf("CurrentIndex = %d, want 0 (no auto-advance)", g.CurrentIndex)
	}
	if len(g.Results) != 0 {
		t.Errorf("Results len = %d, want 0 (no auto-advance)", len(g.Results))
	}
}
