package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func renderDictation(t *testing.T, data DictationViewData) string {
	t.Helper()
	var b bytes.Buffer
	if err := Dictation(data).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The page leads with the user's real key and plain sections, the machinery
// (rule table) sits inside Advanced, and the microphone is one line pointing
// at Get started, where it and the model are chosen.
func TestDictationLeadsWithWordsAndFoldsTheMachinery(t *testing.T) {
	html := renderDictation(t, DictationViewData{
		DictateKeys: []string{"⌥", "T"},
		Words: []WordRowView{
			{Word: "Ghostty", Fixes: []WordFixView{{ID: "r1", Heard: "Ghosty"}}},
			{Word: "kubectl", HasPattern: true},
		},
		MicLabel:    "MacBook Air Microphone (system default)",
		Corrections: []CorrectionRowView{{ID: "r1", MatchPattern: `(?i)\bGhosty\b`, Replacement: "Ghostty", IsRegex: true, Enabled: true}},
	})
	if strings.Contains(html, "@keys") || strings.Contains(html, "data.") {
		t.Fatalf("template source leaked into the page")
	}
	for _, want := range []string{
		"<kbd>⌥</kbd><kbd>T</kbd>",
		"double-tap",
		"Words it should know", "Ghostty", "Ghosty", "also a pattern rule",
		"Recent",
		"MacBook Air Microphone (system default)", `href="/plugins/voice/settings/start"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	adv := strings.Index(html, `class="dict-advanced"`)
	if adv < 0 {
		t.Fatal("no Advanced section")
	}
	for _, folded := range []string{`(?i)\bGhosty\b`, "Fix All Caps"} {
		if i := strings.Index(html, folded); i < adv {
			t.Errorf("%q appears before Advanced (at %d, Advanced at %d)", folded, i, adv)
		}
	}
	// A pattern-only word cannot be forgotten from here without the rule, so
	// the page says where that rule lives rather than offering a fix list.
	if strings.Contains(html, "Stop fixing “kubectl”") {
		t.Error("pattern rule offered as a removable fix")
	}
}
