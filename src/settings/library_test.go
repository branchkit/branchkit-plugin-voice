package settings

import (
	"encoding/json"
	"testing"
)

func TestCaptureDisplayName(t *testing.T) {
	tests := []struct {
		inner string
		want  string
	}{
		{"keys", "[key]"},
		{"alphabet", "[alphabet]"},
		{"modifiers", "[modifier]"},
		{"keys|alphabet", "[key]"},
		{"direction", "[direction]"},
		{"number", "[number]"},
	}
	for _, tt := range tests {
		got := CaptureDisplayName(tt.inner)
		if got != tt.want {
			t.Errorf("CaptureDisplayName(%q) = %q, want %q", tt.inner, got, tt.want)
		}
	}
}

func TestFormatPhrasePart_Literals(t *testing.T) {
	display, capture := FormatPhrasePart("press")
	if display != "press" || capture != "" {
		t.Errorf("literal: got (%q, %q), want (\"press\", \"\")", display, capture)
	}
}

func TestFormatPhrasePart_SimpleCapture(t *testing.T) {
	display, capture := FormatPhrasePart("<direction>")
	if display != "[direction]" {
		t.Errorf("display = %q, want \"[direction]\"", display)
	}
	if capture != "direction" {
		t.Errorf("capture = %q, want \"direction\"", capture)
	}
}

func TestFormatPhrasePart_UnionCapture(t *testing.T) {
	display, capture := FormatPhrasePart("<keys|alphabet>")
	if display != "[key]" {
		t.Errorf("display = %q, want \"[key]\"", display)
	}
	if capture != "keys|alphabet" {
		t.Errorf("capture = %q, want \"keys|alphabet\"", capture)
	}
}

func TestFormatPhrasePart_QuantifierStar(t *testing.T) {
	display, capture := FormatPhrasePart("<modifiers>*")
	if display != "[modifier]*" {
		t.Errorf("display = %q, want \"[modifier]*\"", display)
	}
	if capture != "modifiers" {
		t.Errorf("capture = %q, want \"modifiers\"", capture)
	}
}

func TestFormatPhrasePart_QuantifierPlus(t *testing.T) {
	display, capture := FormatPhrasePart("<alphabet>+")
	if display != "[alphabet]+" {
		t.Errorf("display = %q, want \"[alphabet]+\"", display)
	}
	if capture != "alphabet" {
		t.Errorf("capture = %q, want \"alphabet\"", capture)
	}
}

func TestParsePhraseForDeps_SimplePhrase(t *testing.T) {
	raw := json.RawMessage(`["undo"]`)
	display, captures := ParsePhraseForDeps(raw)
	if display != "undo" {
		t.Errorf("display = %q, want \"undo\"", display)
	}
	if len(captures) != 0 {
		t.Errorf("captures = %v, want empty", captures)
	}
}

func TestParsePhraseForDeps_Alternatives(t *testing.T) {
	raw := json.RawMessage(`[["new line", "return", "enter"]]`)
	display, captures := ParsePhraseForDeps(raw)
	if display != "new line" {
		t.Errorf("display = %q, want \"new line\"", display)
	}
	if len(captures) != 0 {
		t.Errorf("captures = %v, want empty", captures)
	}
}

func TestParsePhraseForDeps_SingleCapture(t *testing.T) {
	raw := json.RawMessage(`["scroll", "<direction>"]`)
	display, captures := ParsePhraseForDeps(raw)
	if display != "scroll [direction]" {
		t.Errorf("display = %q, want \"scroll [direction]\"", display)
	}
	if len(captures) != 1 || captures[0] != "direction" {
		t.Errorf("captures = %v, want [\"direction\"]", captures)
	}
}

func TestParsePhraseForDeps_MultiCapture(t *testing.T) {
	raw := json.RawMessage(`["press", "<modifiers>*", "<keys|alphabet>"]`)
	display, captures := ParsePhraseForDeps(raw)
	if display != "press [modifier]* [key]" {
		t.Errorf("display = %q, want \"press [modifier]* [key]\"", display)
	}
	if len(captures) != 2 {
		t.Fatalf("captures len = %d, want 2", len(captures))
	}
	if captures[0] != "modifiers" {
		t.Errorf("captures[0] = %q, want \"modifiers\"", captures[0])
	}
	if captures[1] != "keys|alphabet" {
		t.Errorf("captures[1] = %q, want \"keys|alphabet\"", captures[1])
	}
}

func TestParsePhraseForDeps_PlusQuantifier(t *testing.T) {
	raw := json.RawMessage(`["spell", "<alphabet>+"]`)
	display, captures := ParsePhraseForDeps(raw)
	if display != "spell [alphabet]+" {
		t.Errorf("display = %q, want \"spell [alphabet]+\"", display)
	}
	if len(captures) != 1 || captures[0] != "alphabet" {
		t.Errorf("captures = %v, want [\"alphabet\"]", captures)
	}
}

func TestFormatImpact_SingleList(t *testing.T) {
	got := FormatImpact("scroll [direction]", "direction", "up", "up")
	want := "scroll up \u2192 up"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatImpact_UnionCapture(t *testing.T) {
	// For keys|alphabet, the display has [key]. Substituting "arch" for the key slot.
	got := FormatImpact("press [modifier]* [key]", "keys|alphabet", "arch", "a")
	want := "press [modifier]* arch \u2192 a"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatImpact_ModifierCapture(t *testing.T) {
	got := FormatImpact("press [modifier]* [key]", "modifiers", "command", "cmd")
	want := "press command* [key] \u2192 cmd"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEntryDependencies_Basic(t *testing.T) {
	parsed := []parsedCommand{
		{PhraseDisplay: "scroll [direction]", Captures: []string{"direction"}},
	}
	lists := map[string]map[string]string{
		"direction": {"up": "up", "down": "down"},
	}

	deps := EntryDependencies(parsed, lists)

	dirDeps, ok := deps["direction"]
	if !ok {
		t.Fatal("expected direction in deps")
	}
	upDeps := dirDeps["up"]
	if len(upDeps) != 1 {
		t.Fatalf("expected 1 dep for 'up', got %d", len(upDeps))
	}
	if upDeps[0].CommandPhrase != "scroll [direction]" {
		t.Errorf("CommandPhrase = %q", upDeps[0].CommandPhrase)
	}
	if upDeps[0].IsCovered {
		t.Error("expected IsCovered=false for single-list capture")
	}
}

func TestEntryDependencies_UnionCoverage(t *testing.T) {
	parsed := []parsedCommand{
		{PhraseDisplay: "press [modifier]* [key]", Captures: []string{"keys|alphabet"}},
	}
	lists := map[string]map[string]string{
		"keys":     {"tab": "tab", "escape": "escape"},
		"alphabet": {"tab": "t", "arch": "a"},
	}

	deps := EntryDependencies(parsed, lists)

	// "tab" exists in both keys and alphabet → covered in both
	keyTabDeps := deps["keys"]["tab"]
	if len(keyTabDeps) != 1 {
		t.Fatalf("expected 1 dep for keys/tab, got %d", len(keyTabDeps))
	}
	if !keyTabDeps[0].IsCovered {
		t.Error("keys/tab should be covered (alphabet also has 'tab')")
	}

	alphaTabDeps := deps["alphabet"]["tab"]
	if len(alphaTabDeps) != 1 {
		t.Fatalf("expected 1 dep for alphabet/tab, got %d", len(alphaTabDeps))
	}
	if !alphaTabDeps[0].IsCovered {
		t.Error("alphabet/tab should be covered (keys also has 'tab')")
	}

	// "arch" only in alphabet → not covered
	alphaArchDeps := deps["alphabet"]["arch"]
	if len(alphaArchDeps) != 1 {
		t.Fatalf("expected 1 dep for alphabet/arch, got %d", len(alphaArchDeps))
	}
	if alphaArchDeps[0].IsCovered {
		t.Error("alphabet/arch should NOT be covered (only in alphabet)")
	}

	// "escape" only in keys → not covered
	keyEscDeps := deps["keys"]["escape"]
	if len(keyEscDeps) != 1 {
		t.Fatalf("expected 1 dep for keys/escape, got %d", len(keyEscDeps))
	}
	if keyEscDeps[0].IsCovered {
		t.Error("keys/escape should NOT be covered (only in keys)")
	}
}

func TestEntryDependencies_NoCaptures(t *testing.T) {
	parsed := []parsedCommand{
		{PhraseDisplay: "undo", Captures: nil},
		{PhraseDisplay: "copy", Captures: []string{}},
	}
	lists := map[string]map[string]string{
		"alphabet": {"arch": "a"},
	}

	deps := EntryDependencies(parsed, lists)
	if len(deps) != 0 {
		t.Errorf("expected no deps for commands without captures, got %v", deps)
	}
}

func TestListUsedByCommands(t *testing.T) {
	parsed := []parsedCommand{
		{PhraseDisplay: "press [modifier]* [key]", Captures: []string{"modifiers", "keys|alphabet"}},
		{PhraseDisplay: "scroll [direction]", Captures: []string{"direction"}},
		{PhraseDisplay: "spell [alphabet]+", Captures: []string{"alphabet"}},
	}

	usedBy := ListUsedByCommands(parsed)

	if len(usedBy["alphabet"]) != 2 {
		t.Errorf("alphabet used by %d commands, want 2 (press + spell)", len(usedBy["alphabet"]))
	}
	if len(usedBy["keys"]) != 1 {
		t.Errorf("keys used by %d commands, want 1 (press)", len(usedBy["keys"]))
	}
	if len(usedBy["direction"]) != 1 {
		t.Errorf("direction used by %d commands, want 1 (scroll)", len(usedBy["direction"]))
	}
	if len(usedBy["modifiers"]) != 1 {
		t.Errorf("modifiers used by %d commands, want 1 (press)", len(usedBy["modifiers"]))
	}
}

func TestListUsedByCommands_Dedup(t *testing.T) {
	// Same command phrase referenced twice should appear once
	parsed := []parsedCommand{
		{PhraseDisplay: "press [key]", Captures: []string{"keys|alphabet"}},
		{PhraseDisplay: "press [key]", Captures: []string{"keys|alphabet"}},
	}

	usedBy := ListUsedByCommands(parsed)
	if len(usedBy["keys"]) != 1 {
		t.Errorf("expected dedup: keys used by %d, want 1", len(usedBy["keys"]))
	}
}
