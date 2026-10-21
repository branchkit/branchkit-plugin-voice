package main

import (
	"encoding/json"
	"github.com/branchkit/plugin-sdk-go"
	"testing"
)

// The Command Editor form's fixes: a new command opens on a type the registry
// declares, the select never renders blank, and the test box describes a
// match by its registry label rather than its routed envelope.

func TestDefaultActionType_PrefersShortcutThenFirstDeclared(t *testing.T) {
	schemas := map[string]ActionTypeSchema{
		"input.shortcut_by_name": {Label: "Keyboard Shortcut"},
		"input.type":             {Label: "Type Text"},
	}
	if got := defaultActionType(schemas); got != "input.shortcut_by_name" {
		t.Fatalf("got %q, want the shortcut shape", got)
	}
	delete(schemas, "input.shortcut_by_name")
	if got := defaultActionType(schemas); got != "input.type" {
		t.Fatalf("got %q, want the first declared type", got)
	}
	if got := defaultActionType(nil); got != "shell" {
		t.Fatalf("got %q, want shell when nothing is declared", got)
	}
}

func TestBuildActionTypeOptions_CurrentAlwaysHasASelectedOption(t *testing.T) {
	schemas := map[string]ActionTypeSchema{"input.type": {Label: "Type Text"}}
	for _, current := range []string{"input.type", "sequence", "legacy.unknown"} {
		selected := 0
		for _, o := range buildActionTypeOptions(schemas, current) {
			if o.Selected {
				selected++
				if o.Value != current {
					t.Fatalf("current %q: selected option is %q", current, o.Value)
				}
			}
		}
		if selected != 1 {
			t.Fatalf("current %q: %d selected options, want exactly one", current, selected)
		}
	}
}

func TestDescribeMatchEffect(t *testing.T) {
	h := newTestHost()
	if h.edState == nil {
		h.edState = &editorState{}
	}
	h.edState.mu.Lock()
	h.edState.actionSchemas = map[string]ActionTypeSchema{"wm.snap": {Label: "Snap Window"}}
	h.edState.mu.Unlock()

	// The resolve response carries the generated, routed Action now, so
	// these build that rather than a raw-JSON stand-in. Same three cases:
	// a routed plugin action, a mode-entry _noop, and a native action.
	snap, dir := "wm.snap", json.RawMessage(`{"direction":"left"}`)
	routed := branchkit.Action{Type: branchkit.ActionKindPlugin, ActionType: &snap, Params: dir}
	if got := h.describeMatchEffect(&resolveAPIResponse{Action: &routed}); got != "snap window left" {
		t.Fatalf("routed plugin action: got %q", got)
	}

	noop := branchkit.Action{Type: branchkit.ActionKind("_noop")}
	got := h.describeMatchEffect(&resolveAPIResponse{Action: &noop, SetsTags: []string{"plugin.placement.snap_mode"}})
	if got != "activates snap mode" {
		t.Fatalf("mode entry: got %q", got)
	}

	// A third case used to live here, feeding describeMatchEffect a FLAT
	// native action (`{"type":"input.key_by_name","name":"escape"}`). Typing
	// the resolve response showed it was fictional: the platform's Action
	// enum has exactly two serializable variants, Plugin and Sequence
	// (Template and CompiledTemplate are #[serde(skip)]), so a flat native
	// action cannot arrive in `resolve.action` at all. The behaviour it was
	// checking is DescribeEffect's, reached from command RECORDS rather than
	// from a resolve, and settings.TestDescribeEffect_KeyByName covers that
	// exact input. Removed 2026-09-20 rather than reshaped into something
	// equally unreachable.
}

// Increment 2: the "It does" picker offers the everyday shapes the registry
// declares, then every type under its prefix group, and its face names the
// current choice in the same words.
func TestBuildActionPicker(t *testing.T) {
	h := newTestHost()
	if h.edState == nil {
		h.edState = &editorState{}
	}
	schemas := map[string]ActionTypeSchema{
		"input.shortcut_by_name": {Label: "Keyboard Shortcut"},
		"input.type":             {Label: "Type Text"},
		"placement.snap":         {Label: "Snap Window"},
	}
	p := buildActionPicker(&EditorState{ActionType: "placement.snap"}, schemas)

	var everyday []string
	for _, e := range p.Everyday {
		everyday = append(everyday, e.Value)
	}
	// Only declared shapes — and no "sequence": steps arrive through
	// "+ add a step", never as a shape.
	want := []string{"input.shortcut_by_name", "input.type"}
	if len(everyday) != len(want) {
		t.Fatalf("everyday = %v, want %v (only declared shapes)", everyday, want)
	}
	for i := range want {
		if everyday[i] != want[i] {
			t.Fatalf("everyday = %v, want %v", everyday, want)
		}
	}
	if len(p.Groups) != 2 || p.Groups[0].Plugin != "Input" || p.Groups[1].Plugin != "Placement" {
		t.Fatalf("groups = %+v, want Input then Placement", p.Groups)
	}
	if p.Total != 3 {
		t.Fatalf("total = %d, want 3", p.Total)
	}
	if groups := buildSeqTypeGroups(p); len(groups) != 3 || groups[0].Label != "Everyday" || len(groups[0].Options) != 2 {
		t.Fatalf("step groups = %+v, want Everyday(2), Input, Placement", groups)
	}
	if p.Current != "Snap Window" || p.CurrentSub != "Placement" {
		t.Fatalf("face = %q / %q", p.Current, p.CurrentSub)
	}

	p = buildActionPicker(&EditorState{ActionType: "input.type"}, schemas)
	if p.Current != "Type text" || p.CurrentSub != "" {
		t.Fatalf("everyday face = %q / %q, want the everyday name alone", p.Current, p.CurrentSub)
	}
	p = buildActionPicker(&EditorState{ActionType: "sequence", SeqSteps: []EditorSeqStep{{}, {}}}, schemas)
	if p.Current != "Several steps, in order" || p.CurrentSub != "2 steps" {
		t.Fatalf("sequence face = %q / %q", p.Current, p.CurrentSub)
	}
}

func TestClipNote(t *testing.T) {
	if got := clipNote("Held while the find bar is open. Gates the jump commands, which also…", 110); got != "Held while the find bar is open." {
		t.Fatalf("first sentence: got %q", got)
	}
	long := "a paragraph without any sentence break that keeps going well past the limit of the picker line and beyond"
	got := clipNote(long, 40)
	if len(got) > 44 || got[len(got)-3:] != "…" {
		t.Fatalf("clipped: got %q", got)
	}
	if got := clipNote("short", 40); got != "short" {
		t.Fatalf("short: got %q", got)
	}
}

func TestHumanizeTag(t *testing.T) {
	for tag, want := range map[string]string{
		"plugin.voice.mode.spelling": "Spelling",
		"plugin.placement.snap_mode": "Snap mode",
		"speaking":                   "Speaking",
	} {
		if got := humanizeTag(tag); got != want {
			t.Fatalf("%s: got %q, want %q", tag, got, want)
		}
	}
}

// A blank binds by name, the way the platform reads templates: the explicit
// `name:` when given, else the first list's last dotted segment.
func TestParseCaptures_BindsByName(t *testing.T) {
	caps := parseCaptures("press <modifiers>* <keys|alphabet|number> <name:audio_outputs> <plugin.acme.widgets>")
	want := []string{"modifiers", "keys", "name", "widgets"}
	if len(caps) != len(want) {
		t.Fatalf("got %d captures, want %d", len(caps), len(want))
	}
	for i, c := range caps {
		if c.Name != want[i] {
			t.Fatalf("capture %d: name %q, want %q", i, c.Name, want[i])
		}
	}
	if caps[1].Label != "keys/alphabet/number" || caps[2].Label != "audio_outputs" {
		t.Fatalf("labels: %q %q", caps[1].Label, caps[2].Label)
	}
}
