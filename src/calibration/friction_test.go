package calibration

import (
	"testing"
	"time"
)

func att(target string, tallies []HeardTally, silent int) VerificationAttempt {
	return VerificationAttempt{Target: target, HeardTallies: tallies, SilentHolds: silent}
}

func TestComputeFriction(t *testing.T) {
	attempts := []VerificationAttempt{
		// VEST: passes both times but fights hard — 6+2 then 3+1 = 12 over 2 = 6/try.
		att("vest", []HeardTally{{"next", 6}, {"vest", 1}}, 2),
		att("vest", []HeardTally{{"first", 3}, {"vest", 1}}, 1),
		// CAP: clean both times.
		att("cap", []HeardTally{{"cap", 1}}, 0),
		att("cap", []HeardTally{{"cap", 1}}, 0),
	}
	f := ComputeFriction(attempts)

	vest := f["vest"]
	if vest.Attempts != 2 || vest.Mishears != 9 || vest.SilentHolds != 3 {
		t.Fatalf("vest = %+v, want {Attempts:2 Mishears:9 SilentHolds:3}", vest)
	}
	if got := vest.PerAttempt(); got != 6 {
		t.Errorf("vest PerAttempt = %v, want 6", got)
	}
	if !vest.IsHigh() {
		t.Error("vest should be high-friction")
	}

	cap := f["cap"]
	if cap.Mishears != 0 || cap.SilentHolds != 0 {
		t.Errorf("cap should have zero friction, got %+v", cap)
	}
	if cap.IsHigh() {
		t.Error("clean word should not be high-friction")
	}
}

func TestFrictionTargetMatchIsCaseInsensitive(t *testing.T) {
	// The settled value equal to the target (any case) is not a mishear.
	f := ComputeFriction([]VerificationAttempt{
		att("vest", []HeardTally{{"VEST", 1}}, 0),
	})
	if m := f["vest"].Mishears; m != 0 {
		t.Errorf("case-insensitive target match should yield 0 mishears, got %d", m)
	}
}

func TestFrictionNeedsMinAttempts(t *testing.T) {
	// One painful run isn't enough to flag — could be a fluke.
	f := ComputeFriction([]VerificationAttempt{
		att("vest", []HeardTally{{"next", 9}, {"vest", 1}}, 0),
	})
	if f["vest"].IsHigh() {
		t.Error("a single attempt should not be flagged high-friction")
	}
}

func TestRecommendDrillSurfacesHighFriction(t *testing.T) {
	// A word that PASSES every time but fights hard must still be recommended.
	now := time.Now().Format(time.RFC3339)
	attempts := []VerificationAttempt{
		{Target: "vest", Timestamp: now, Matched: true, HeardTallies: []HeardTally{{"next", 5}, {"vest", 1}}, SilentHolds: 1},
		{Target: "vest", Timestamp: now, Matched: true, HeardTallies: []HeardTally{{"next", 4}, {"vest", 1}}, SilentHolds: 2},
	}
	rec := RecommendDrill(attempts, 0)
	found := false
	for _, d := range rec.Commands {
		if d.Command == "vest" && d.Reason == "high_friction" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected vest surfaced as high_friction; got %+v", rec.Commands)
	}
}
