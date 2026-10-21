package main

import (
	"encoding/json"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/settings"
)

func TestRawPhraseCanonical_Simple(t *testing.T) {
	raw := json.RawMessage(`["undo"]`)
	got := settings.RawPhraseCanonical(raw)
	if got != "undo" {
		t.Errorf("got %q, want \"undo\"", got)
	}
}

func TestRawPhraseCanonical_Alternatives(t *testing.T) {
	raw := json.RawMessage(`[["new line", "return", "enter"]]`)
	got := settings.RawPhraseCanonical(raw)
	if got != "new line" {
		t.Errorf("got %q, want \"new line\"", got)
	}
}

func TestRawPhraseCanonical_Captures(t *testing.T) {
	raw := json.RawMessage(`["press", "<modifiers>*", "<keys|alphabet>"]`)
	got := settings.RawPhraseCanonical(raw)
	want := "press <modifiers>* <keys|alphabet>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRawPhraseCanonical_MixedAltsAndCaptures(t *testing.T) {
	raw := json.RawMessage(`[["show", "open"], ["alphabet", "spelling"]]`)
	got := settings.RawPhraseCanonical(raw)
	if got != "show alphabet" {
		t.Errorf("got %q, want \"show alphabet\"", got)
	}
}

func TestRawPhraseCanonical_SingleString(t *testing.T) {
	raw := json.RawMessage(`"undo"`)
	got := settings.RawPhraseCanonical(raw)
	if got != "undo" {
		t.Errorf("got %q, want \"undo\"", got)
	}
}

// RawPhraseFull preserves alternatives (so editing doesn't drop options),
// wrapping multi-option groups in parens so multi-word options survive.

func TestRawPhraseFull_AlternativesWrappedInParens(t *testing.T) {
	raw := json.RawMessage(`[["refresh", "reload"]]`)
	got := settings.RawPhraseFull(raw)
	if got != "(refresh, reload)" {
		t.Errorf("got %q, want \"(refresh, reload)\"", got)
	}
}

func TestRawPhraseFull_MultiWordAlternativesPreserved(t *testing.T) {
	raw := json.RawMessage(`[["new line", "return", "enter"]]`)
	got := settings.RawPhraseFull(raw)
	if got != "(new line, return, enter)" {
		t.Errorf("multi-word alts must round-trip via parens; got %q, want \"(new line, return, enter)\"", got)
	}
}

func TestRawPhraseFull_MixedAltsAndCaptures(t *testing.T) {
	raw := json.RawMessage(`[["show", "open"], "<apps>"]`)
	got := settings.RawPhraseFull(raw)
	if got != "(show, open) <apps>" {
		t.Errorf("got %q, want \"(show, open) <apps>\"", got)
	}
}

// --- editor slot round-trip (parse <-> serialize) for alternatives ---

func TestParseSlots_StandaloneAlternatives(t *testing.T) {
	slots := parsePhraseToSlots("refresh,reload")
	if len(slots) != 1 || slots[0].Type != "alternatives" || slots[0].Value != "refresh, reload" {
		t.Fatalf("got %+v, want one alternatives slot 'refresh, reload'", slots)
	}
	if got := slotsToPhrase(slots); got != "(refresh, reload)" {
		t.Errorf("round-trip got %q, want \"(refresh, reload)\"", got)
	}
}

func TestParseSlots_MultiWordParenAlternativesRoundTrip(t *testing.T) {
	slots := parsePhraseToSlots("(new line, return) please")
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2: %+v", len(slots), slots)
	}
	if slots[0].Type != "alternatives" || slots[0].Value != "new line, return" {
		t.Errorf("slot 0 = %+v, want alternatives 'new line, return'", slots[0])
	}
	if slots[1].Type != "literal" || slots[1].Value != "please" {
		t.Errorf("slot 1 = %+v, want literal 'please'", slots[1])
	}
	if got := slotsToPhrase(slots); got != "(new line, return) please" {
		t.Errorf("round-trip got %q, want \"(new line, return) please\"", got)
	}
}

func TestParseSlots_AlternativesBetweenLiterals(t *testing.T) {
	slots := parsePhraseToSlots("scroll refresh,reload down")
	if len(slots) != 3 {
		t.Fatalf("got %d slots, want 3: %+v", len(slots), slots)
	}
	if slots[0].Type != "literal" || slots[0].Value != "scroll" {
		t.Errorf("slot 0 = %+v, want literal 'scroll'", slots[0])
	}
	if slots[1].Type != "alternatives" || slots[1].Value != "refresh, reload" {
		t.Errorf("slot 1 = %+v, want alternatives 'refresh, reload'", slots[1])
	}
	if slots[2].Type != "literal" || slots[2].Value != "down" {
		t.Errorf("slot 2 = %+v, want literal 'down'", slots[2])
	}
}

func TestParseSlots_CapturesStillWork(t *testing.T) {
	// Regression: adding alternatives must not disturb literal/list parsing.
	slots := parsePhraseToSlots("press <modifiers>* <keys|alphabet>")
	if len(slots) != 3 || slots[0].Type != "literal" || slots[1].Type != "list" || slots[2].Type != "list" {
		t.Fatalf("got %+v, want literal + 2 list slots", slots)
	}
	if slots[1].Repeat != "*" {
		t.Errorf("repeat lost: %+v", slots[1])
	}
}

func TestAltsList_TrimsSpacesAndEmpties(t *testing.T) {
	got := altsList("new line, return ,")
	if len(got) != 2 || got[0] != "new line" || got[1] != "return" {
		t.Errorf("got %+v, want [\"new line\", \"return\"]", got)
	}
}

func TestLoadCommandDetails_KeyMatchesRawCanonical(t *testing.T) {
	// Simulate what loadCommandDetails does: key by rawPhraseCanonical
	raw := json.RawMessage(`["press", "<modifiers>*", "<keys|alphabet>"]`)
	canonical := settings.RawPhraseCanonical(raw)

	// This should match what the platform sends as Phrase/Canonical
	expected := "press <modifiers>* <keys|alphabet>"
	if canonical != expected {
		t.Errorf("rawPhraseCanonical = %q, want %q", canonical, expected)
	}

	// parsePhraseForDeps produces a DIFFERENT format (display) — should NOT be used as key
	display, _ := settings.ParsePhraseForDeps(raw)
	if display == expected {
		t.Error("parsePhraseForDeps should produce formatted display, not raw canonical")
	}
	if display != "press [modifier]* [key]" {
		t.Errorf("parsePhraseForDeps display = %q, want \"press [modifier]* [key]\"", display)
	}
}
