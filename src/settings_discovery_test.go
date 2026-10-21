package main

import "testing"

func TestPhraseEndsInCapture(t *testing.T) {
	cases := []struct {
		name  string
		slots []PhraseSlot
		want  bool
	}{
		{"literal + list", []PhraseSlot{{Type: "literal", Value: "jump"}, {Type: "list", Value: "browser_tabs"}}, true},
		{"capture only", []PhraseSlot{{Type: "list", Value: "browser_tabs"}}, false},
		{"all literals", []PhraseSlot{{Type: "literal", Value: "save"}, {Type: "literal", Value: "now"}}, false},
		{"capture then literal", []PhraseSlot{{Type: "literal", Value: "go"}, {Type: "list", Value: "x"}, {Type: "literal", Value: "now"}}, false},
		{"multi capture", []PhraseSlot{{Type: "literal", Value: "open"}, {Type: "list", Value: "x"}, {Type: "literal", Value: "to"}, {Type: "list", Value: "y"}}, false},
		{"literal + literal + list", []PhraseSlot{{Type: "literal", Value: "open"}, {Type: "literal", Value: "my"}, {Type: "list", Value: "files"}}, true},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			es := &EditorState{PhraseSlots: c.slots}
			if got := phraseEndsInCapture(es); got != c.want {
				t.Errorf("phraseEndsInCapture(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestBuildDiscoveryOptions_SelectsCurrent(t *testing.T) {
	opts := buildDiscoveryOptions("exclusive")
	var selected string
	for _, o := range opts {
		if o.Selected {
			selected = o.Value
		}
	}
	if selected != "exclusive" {
		t.Errorf("selected = %q, want \"exclusive\"", selected)
	}
	if len(opts) != 4 {
		t.Errorf("want 4 options (none/prefix/exclusive/select), got %d", len(opts))
	}
	if opts[3].Value != "select" {
		t.Errorf("last option should be select, got %q", opts[3].Value)
	}
	// "None" is the empty-value default.
	if opts[0].Value != "" {
		t.Errorf("first option should be the empty None value, got %q", opts[0].Value)
	}
}
