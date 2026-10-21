package calibration

import (
	"testing"
	"time"
)

func TestComputeHealth(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)

	attempts := []VerificationAttempt{
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "arch", Source: "calibration", Group: "alphabet", Heard: "arch", Matched: true, DurationMs: 800, Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "bake", Source: "calibration", Group: "alphabet", Heard: "bait", Matched: false, DurationMs: 1500, Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "check", Source: "calibration", Group: "alphabet", Heard: "check", Matched: true, DurationMs: 900, Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s2", Timestamp: ts, Target: "left", Source: "calibration", Group: "snap", Heard: "left", Matched: true, DurationMs: 600, Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s2", Timestamp: ts, Target: "right", Source: "calibration", Group: "snap", Heard: "right", Matched: true, DurationMs: 700, Engine: "sherpa-offline-nemo"},
	}

	groups := ComputeHealth(attempts)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	var alphaGroup *GroupHealth
	var snapGroup *GroupHealth
	for i := range groups {
		switch groups[i].Group {
		case "alphabet":
			alphaGroup = &groups[i]
		case "snap":
			snapGroup = &groups[i]
		}
	}

	if alphaGroup == nil {
		t.Fatal("alphabet group not found")
	}
	if len(alphaGroup.Commands) != 3 {
		t.Errorf("alphabet commands = %d, want 3", len(alphaGroup.Commands))
	}

	for _, c := range alphaGroup.Commands {
		if c.Command == "bake" {
			if c.Rate != 0 {
				t.Errorf("bake rate = %v, want 0", c.Rate)
			}
		}
		if c.Command == "arch" {
			if c.Rate != 100 {
				t.Errorf("arch rate = %v, want 100", c.Rate)
			}
		}
	}

	if snapGroup == nil {
		t.Fatal("snap group not found")
	}
	for _, c := range snapGroup.Commands {
		if c.Rate != 100 {
			t.Errorf("snap %s rate = %v, want 100", c.Command, c.Rate)
		}
	}
}
