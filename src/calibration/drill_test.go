package calibration

import (
	"testing"
	"time"
)

func TestRecommendDrill(t *testing.T) {
	ts := time.Now().UTC().Format(time.RFC3339)

	// "arch" has good accuracy
	// "bake" has low accuracy (0%)
	// "left" was tested 30 days ago (stale)
	oldTs := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)

	attempts := []VerificationAttempt{
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "arch", Heard: "arch", Matched: true, Source: "calibration", Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "bake", Heard: "bait", Matched: false, Source: "calibration", Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s0", Timestamp: oldTs, Target: "left", Heard: "left", Matched: true, Source: "calibration", Group: "snap", Engine: "sherpa-offline-nemo"},
	}

	rec := RecommendDrill(attempts, 10)
	if rec == nil {
		t.Fatal("expected recommendation, got nil")
	}

	if len(rec.Commands) < 2 {
		t.Fatalf("expected at least 2 recommendations, got %d", len(rec.Commands))
	}

	// "bake" should be recommended (low accuracy)
	found := false
	for _, c := range rec.Commands {
		if c.Command == "bake" && c.Reason == "low_accuracy" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'bake' with reason 'low_accuracy' in recommendations")
	}

	// "left" should be recommended (stale)
	found = false
	for _, c := range rec.Commands {
		if c.Command == "left" && c.Reason == "stale" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'left' with reason 'stale' in recommendations")
	}
}

func TestRecommendDrillMaxCommands(t *testing.T) {
	ts := time.Now().UTC().Format(time.RFC3339)

	// Create several low-accuracy commands
	words := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	attempts := make([]VerificationAttempt, 0, len(words))
	for _, w := range words {
		attempts = append(attempts, VerificationAttempt{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: w, Heard: "wrong", Matched: false, Source: "calibration", Group: "alphabet", Engine: "sherpa-offline-nemo"})
	}

	rec := RecommendDrill(attempts, 3)
	if len(rec.Commands) != 3 {
		t.Errorf("expected max 3 commands, got %d", len(rec.Commands))
	}
}
