package speech

import (
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// A rule written before the field existed must be classified, not left blank —
// and an unknown pattern must land on the SAFE side, because mislabelling a
// user's rule as seeded would expose it to a restore.
func TestClassifySourceDerivesProvenance(t *testing.T) {
	shipped := DefaultCorrections()[0]
	shipped.Source = ""
	if got := classifySource(shipped); got != SourceSeeded {
		t.Fatalf("a shipped pattern should classify as seeded, got %q", got)
	}
	mine := Correction{MatchPattern: `(?i)\bwidgetron\b`, Replacement: "Widgetron"}
	if got := classifySource(mine); got != SourceUser {
		t.Fatalf("an unknown pattern must default to the user, got %q", got)
	}
	explicit := Correction{MatchPattern: `anything`, Source: SourceLearned}
	if got := classifySource(explicit); got != SourceLearned {
		t.Fatalf("an explicit source must win, got %q", got)
	}
}

// The whole point of provenance: restoring shipped rules must not discard the
// user's. This is the behaviour that makes the button pressable.
func TestResetKeepsUserRules(t *testing.T) {
	mine := Correction{ID: "u1", MatchPattern: `(?i)\bwidgetron\b`,
		Replacement: "Widgetron", Enabled: true, Source: SourceUser}
	learned := Correction{ID: "l1", MatchPattern: `(?i)\bfoo bar\b`,
		Replacement: "FooBar", Enabled: true, Source: SourceLearned}

	c := New(branchkit.NewDetachedPlugin(), func() bool { return true }, func() bool { return false })
	c.list = []Correction{mine, learned}

	current := c.Get()
	kept := make([]Correction, 0, len(current))
	for _, c := range current {
		if classifySource(c) != SourceSeeded {
			kept = append(kept, c)
		}
	}
	restored := append(DefaultCorrections(), kept...)

	var sawMine, sawLearned bool
	for _, c := range restored {
		if c.ID == "u1" {
			sawMine = true
		}
		if c.ID == "l1" {
			sawLearned = true
		}
	}
	if !sawMine || !sawLearned {
		t.Fatalf("restore dropped non-seeded rules: user=%v learned=%v", sawMine, sawLearned)
	}
	if len(restored) != len(DefaultCorrections())+2 {
		t.Fatalf("expected shipped + 2 kept, got %d", len(restored))
	}
}
