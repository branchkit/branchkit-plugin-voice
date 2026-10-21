package speech

import (
	"reflect"
	"strings"
	"testing"
)

// Commas, semicolons and newlines separate entries; spaces do not, because
// "Claude Code" is one thing the user says.
func TestParseVocabularyInputSplitsOnSeparatorsNotSpaces(t *testing.T) {
	got, err := ParseVocabularyInput(" BranchKit, Claude  Code;\nGhostty ,, ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"BranchKit", "Claude Code", "Ghostty"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseVocabularyInput = %v, want %v", got, want)
	}
}

func TestParseVocabularyInputRefusesWhatCannotBeAWord(t *testing.T) {
	for _, in := range []string{"", " , ", "1234", strings.Repeat("a", maxVocabularyWordLen+1)} {
		if _, err := ParseVocabularyInput(in); err == nil {
			t.Errorf("ParseVocabularyInput(%q) accepted it", in)
		}
	}
}

// The stage receives one set: the user's list and the correction targets,
// normalised the same way and in a stable order.
func TestBiasWordsUnionsVocabularyWithCorrectionTargets(t *testing.T) {
	corr := []Correction{
		{Replacement: "kubectl", Enabled: true},
		{Replacement: "@datastar", Enabled: true},
		{Replacement: "Playwright", Enabled: false},
	}
	got := BiasWords(corr, []string{"Ghostty", "kubectl", "Claude Code", "42"})
	want := []string{"Claude Code", "Ghostty", "datastar", "kubectl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BiasWords = %v, want %v", got, want)
	}
}

// A fix-a-word rule reads back as the word that was misheard; a pattern
// written by hand does not, even when it looks close.
func TestLiteralHeardRoundTripsFixAWordRules(t *testing.T) {
	for _, heard := range []string{"Ghosty", "web sack", "@datastar", "C++", "what?"} {
		rule, err := NewLiteralCorrection(heard, "x"+heard)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := LiteralHeard(rule)
		if !ok || got != heard {
			t.Errorf("LiteralHeard(%q rule) = %q, %v", heard, got, ok)
		}
	}
	for _, pattern := range []string{`(?i)cube\s*ctl`, `(?i)\bWebSack`, `ghosty`, `(?i)play\s*wri(?:ght|te)`} {
		if h, ok := LiteralHeard(Correction{MatchPattern: pattern, IsRegex: true}); ok {
			t.Errorf("LiteralHeard(%q) = %q, want not literal", pattern, h)
		}
	}
}

// One row per word: the list's words first, fixes under the word they write
// (in any capitalisation), pattern-only words after; shipped and empty rules
// stay out.
func TestWordRowsGroupsFixesUnderTheirWord(t *testing.T) {
	ghosty, _ := NewLiteralCorrection("Ghosty", "Ghostty")
	ghostie, _ := NewLiteralCorrection("Ghostie", "ghostty")
	websack, _ := NewLiteralCorrection("WebSack", "WebStack")
	corr := []Correction{
		ghosty, ghostie, websack,
		{ID: "k", MatchPattern: `(?i)cube\s*ctl`, Replacement: "kubectl", IsRegex: true, Enabled: true, Source: SourceUser},
		{ID: "h", MatchPattern: `(?i)^\s*thank you\s*$`, Replacement: "", IsRegex: true, Enabled: true, Source: SourceSeeded},
	}
	got := WordRows(corr, []string{"Ghostty", "BranchKit"})
	want := []WordRow{
		{Word: "Ghostty", Fixes: []WordFix{{ID: ghosty.ID, Heard: "Ghosty"}, {ID: ghostie.ID, Heard: "Ghostie"}}},
		{Word: "BranchKit"},
		{Word: "kubectl", HasPattern: true},
		{Word: "WebStack", Fixes: []WordFix{{ID: websack.ID, Heard: "WebSack"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WordRows =\n%+v\nwant\n%+v", got, want)
	}
}
