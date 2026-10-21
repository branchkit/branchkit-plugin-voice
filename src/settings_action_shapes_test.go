package main

// The editor's ONE internal action shape is the authored one-params dialect:
// {"type": "x.y", "params": {...}, "phase"?}.
// unwrapRoutedActionJSON converts the platform's routed form to it at the
// boundary; everything downstream reads params from the box and nowhere
// else. The parseSequenceSteps case pins the regression the dialect sweep
// introduced: top-level field reads returned empty values for every step of
// a nested-authored sequence.

import (
	"encoding/json"
	"testing"
)

func seedEditorSchemas(h *Host, t *testing.T) {
	t.Helper()
	prev := h.edState.actionSchemas
	t.Cleanup(func() { h.edState.actionSchemas = prev })
	h.edState.actionSchemas = map[string]ActionTypeSchema{
		"input.type": {
			Fields: []ActionFieldSchema{{Key: "text", FieldType: "string"}},
		},
		"input.key_by_name": {
			Fields: []ActionFieldSchema{{Key: "name", FieldType: "string"}},
		},
		"input.shortcut_by_name": {
			Fields: []ActionFieldSchema{
				{Key: "name", FieldType: "string"},
				{Key: "modifiers", FieldType: "string[]"},
			},
		},
	}
}

func TestUnwrapRoutedActionJSON(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			"routed becomes authored",
			`{"type":"plugin","action_type":"input.type","params":{"text":"hi"}}`,
			`{"params":{"text":"hi"},"type":"input.type"}`,
		},
		{
			"phase survives",
			`{"type":"plugin","action_type":"voice.speak","params":{},"phase":"start"}`,
			`{"phase":"start","type":"voice.speak"}`,
		},
		{
			"authored passes through",
			`{"type":"input.type","params":{"text":"hi"}}`,
			`{"type":"input.type","params":{"text":"hi"}}`,
		},
		{
			"sequence passes through",
			`{"type":"sequence","actions":[]}`,
			`{"type":"sequence","actions":[]}`,
		},
	}
	for _, tc := range cases {
		got := string(unwrapRoutedActionJSON(json.RawMessage(tc.in)))
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParseActionForEditReadsParamsBox(t *testing.T) {
	h := newTestHost()
	seedEditorSchemas(h, t)

	// Authored form (what commands.json carries since the sweep) — this
	// returned ("input.type", "") between the sweep and this fix.
	at, val := h.parseActionForEdit(json.RawMessage(
		`{"type":"input.type","params":{"text":"hello"}}`))
	if at != "input.type" || val != "hello" {
		t.Errorf("authored: got (%q, %q), want (input.type, hello)", at, val)
	}

	// Routed form (what the platform's enumerate carries) unwraps first.
	at, val = h.parseActionForEdit(json.RawMessage(
		`{"type":"plugin","action_type":"input.type","params":{"text":"hello"}}`))
	if at != "input.type" || val != "hello" {
		t.Errorf("routed: got (%q, %q), want (input.type, hello)", at, val)
	}

	// Shortcut modifiers compound from inside the box.
	at, val = h.parseActionForEdit(json.RawMessage(
		`{"type":"input.shortcut_by_name","params":{"name":"a","modifiers":["cmd","shift"]}}`))
	if at != "input.shortcut_by_name" || val != "cmd+shift+a" {
		t.Errorf("shortcut: got (%q, %q), want (input.shortcut_by_name, cmd+shift+a)", at, val)
	}
}

func TestParseSequenceStepsFromAuthoredForm(t *testing.T) {
	h := newTestHost()
	seedEditorSchemas(h, t)
	steps := h.parseSequenceSteps(json.RawMessage(`{
		"type": "sequence",
		"actions": [
			{"type": "input.key_by_name", "params": {"name": "escape"}},
			{"type": "input.type", "params": {"text": "done"}}
		]
	}`))
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].Type != "input.key_by_name" || steps[0].Val != "escape" {
		t.Errorf("step 0: got (%q, %q), want (input.key_by_name, escape)", steps[0].Type, steps[0].Val)
	}
	if steps[1].Type != "input.type" || steps[1].Val != "done" {
		t.Errorf("step 1: got (%q, %q), want (input.type, done)", steps[1].Type, steps[1].Val)
	}
}

func TestDecomposeActionJSONIsNestedOnly(t *testing.T) {
	h := newTestHost()
	seedEditorSchemas(h, t)

	at, fields := h.decomposeActionJSON(`{"type":"input.type","params":{"text":"hi"}}`)
	if at != "input.type" || len(fields) != 1 || fields[0].Val != "hi" {
		t.Errorf("nested: got (%q, %+v)", at, fields)
	}

	// Flat input decomposes to empty fields — the platform refuses flat at
	// load; the editor no longer keeps a private fallback for it.
	_, fields = h.decomposeActionJSON(`{"type":"input.type","text":"hi"}`)
	if len(fields) != 1 || fields[0].Val != "" {
		t.Errorf("flat should decompose empty, got %+v", fields)
	}
}
