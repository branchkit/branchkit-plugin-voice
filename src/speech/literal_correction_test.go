package speech

import "testing"

// Word boundaries are the whole point. `ApplyCorrections`' non-regex path is a
// case-insensitive SUBSTRING replace, so a rule for "so" without boundaries
// would rewrite every "also" in everything ever dictated.
func TestLiteralCorrectionIsWordBounded(t *testing.T) {
	rule, err := NewLiteralCorrection("so", "SO")
	if err != nil {
		t.Fatal(err)
	}
	got := ApplyCorrections("also so and lasso", []Correction{rule})
	if got != "also SO and lasso" {
		t.Fatalf("boundaries not applied: %q", got)
	}
}

// A word starting or ending in punctuation gets no boundary there — `\b`
// before "@datastar" would never match.
func TestLiteralCorrectionSkipsBoundaryAtNonWordEdge(t *testing.T) {
	rule, err := NewLiteralCorrection("agent datastar", "@datastar")
	if err != nil {
		t.Fatal(err)
	}
	if got := ApplyCorrections("ask agent datastar please", []Correction{rule}); got != "ask @datastar please" {
		t.Fatalf("got %q", got)
	}
}

// User input must never be readable as regex syntax.
func TestLiteralCorrectionEscapesInput(t *testing.T) {
	rule, err := NewLiteralCorrection("c++ (beta)", "C++")
	if err != nil {
		t.Fatal(err)
	}
	if got := ApplyCorrections("we use c++ (beta) here", []Correction{rule}); got != "we use C++ here" {
		t.Fatalf("metacharacters leaked: %q", got)
	}
}

// A CASE-ONLY correction must be accepted: `sherpa`->`Sherpa` is the single
// most common shape of dictation mistake worth fixing.
func TestLiteralCorrectionAcceptsCaseOnlyFix(t *testing.T) {
	rule, err := NewLiteralCorrection("sherpa", "Sherpa")
	if err != nil {
		t.Fatalf("case-only correction rejected: %v", err)
	}
	if got := ApplyCorrections("does sherpa work", []Correction{rule}); got != "does Sherpa work" {
		t.Fatalf("got %q", got)
	}
}

func TestLiteralCorrectionRejectsUselessInput(t *testing.T) {
	for _, tc := range [][2]string{{"", "x"}, {"x", ""}, {"  ", "x"}, {"same", "same"}} {
		if _, err := NewLiteralCorrection(tc[0], tc[1]); err == nil {
			t.Fatalf("expected rejection for %q -> %q", tc[0], tc[1])
		}
	}
}

// A rule created from a mistake is the user's, not shipped — so "Restore
// shipped rules" must never remove it.
func TestLiteralCorrectionIsUserOwned(t *testing.T) {
	rule, err := NewLiteralCorrection("quick base", "QuickBase")
	if err != nil {
		t.Fatal(err)
	}
	if rule.Source != SourceUser {
		t.Fatalf("expected %q, got %q", SourceUser, rule.Source)
	}
}
