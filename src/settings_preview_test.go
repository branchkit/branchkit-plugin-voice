package main

import (
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

func TestClassifyPreview(t *testing.T) {
	// Exact duplicate: a 2-word phrase fully consumed by an existing command.
	got := classifyPreview(resolveAPIResponse{
		Matched: true, ConsumedCount: 2, OwnerPlugin: strptr("voice"),
	}, 2)
	if got.Kind != "conflict" || !strings.Contains(got.Message, "already belongs") {
		t.Errorf("exact dup: %+v", got)
	}
	if !strings.Contains(got.Message, "voice") {
		t.Errorf("conflict should name owner: %q", got.Message)
	}

	// Shorter command shadows the start: 3 words, only 2 consumed. A
	// warning, not a refusal — the record is valid, it may just lose
	// (a collision blocks, a mishearing or shadow warns).
	got = classifyPreview(resolveAPIResponse{Matched: true, ConsumedCount: 2}, 3)
	if got.Kind != "shadow" || !strings.Contains(got.Message, "shorter") {
		t.Errorf("shadow: %+v", got)
	}

	// Partial: extends an existing capture command.
	got = classifyPreview(resolveAPIResponse{HasCompletions: true, Title: "jump <tabs>"}, 1)
	if got.Kind != "info" || !strings.Contains(got.Message, "extends") {
		t.Errorf("partial: %+v", got)
	}

	// Free: no match, no completions.
	got = classifyPreview(resolveAPIResponse{}, 1)
	if got.Kind != "free" || !got.Show {
		t.Errorf("free: %+v", got)
	}
}

func TestLiteralPreviewPhrase(t *testing.T) {
	cases := []struct {
		name  string
		slots []PhraseSlot
		want  string
	}{
		{"all literal", []PhraseSlot{{Type: "literal", Value: "copy"}, {Type: "literal", Value: "that"}}, "copy that"},
		{"stops at capture", []PhraseSlot{{Type: "literal", Value: "focus"}, {Type: "list", Value: "apps"}}, "focus"},
		{"alternatives first option", []PhraseSlot{{Type: "alternatives", Value: "refresh, reload"}}, "refresh"},
		{"capture first", []PhraseSlot{{Type: "list", Value: "apps"}}, ""},
	}
	for _, c := range cases {
		if got := literalPreviewPhrase(c.slots); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
