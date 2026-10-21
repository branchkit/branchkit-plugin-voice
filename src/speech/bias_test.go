package speech

import (
	"reflect"
	"testing"
)

func TestBiasVocabularyDerivesSpeakableReplacements(t *testing.T) {
	got := BiasVocabulary([]Correction{
		// The ordinary case: a proper noun Whisper mishears.
		{MatchPattern: `(?i)quick\s*base`, Replacement: "QuickBase", Enabled: true},
		{MatchPattern: `(?i)cube\s*ctl`, Replacement: "kubectl", Enabled: true},
		// Sigils are typed decoration around a word the user actually says —
		// bias toward the word; the correction still adds the sigil back.
		{MatchPattern: `(?i)agent\s+datastar`, Replacement: "@datastar", Enabled: true},
		{MatchPattern: `(?i)slash\s+commit`, Replacement: "/commit", Enabled: true},
		// The hallucination filter DELETES rather than naming a word, so it
		// contributes no vocabulary at all.
		{MatchPattern: `(?i)^\s*thank you\s*$`, Replacement: "", Enabled: true},
		// A disabled rule is not in force, so it must not bias the decoder.
		{MatchPattern: `(?i)play\s*write`, Replacement: "Playwright", Enabled: false},
		// Duplicates collapse: two patterns often repair toward one spelling.
		{MatchPattern: `(?i)quickbase`, Replacement: "QuickBase", Enabled: true},
		// Nothing speakable in it.
		{MatchPattern: `x`, Replacement: "---", Enabled: true},
	})

	want := []string{"QuickBase", "commit", "datastar", "kubectl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BiasVocabulary() = %v, want %v", got, want)
	}
}

// A stable payload matters: the stage only re-tokenizes when the list changes,
// and map iteration order would make an unchanged set look changed every push.
func TestBiasVocabularyIsOrderStable(t *testing.T) {
	corr := []Correction{
		{Replacement: "zeta", Enabled: true},
		{Replacement: "alpha", Enabled: true},
		{Replacement: "mike", Enabled: true},
	}
	first := BiasVocabulary(corr)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(BiasVocabulary(corr), first) {
			t.Fatalf("BiasVocabulary() is not order-stable: %v vs %v", BiasVocabulary(corr), first)
		}
	}
	if !reflect.DeepEqual(first, []string{"alpha", "mike", "zeta"}) {
		t.Fatalf("expected sorted output, got %v", first)
	}
}
