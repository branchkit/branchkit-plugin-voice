package compare

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestRightIsVoiceRegressNormalization(t *testing.T) {
	if !Right("SNAP  Left", "snap left") {
		t.Error("case and spacing must not matter")
	}
	if Right("snap left four", "snap left") {
		t.Error("an extra word is wrong")
	}
}

func TestWeakest(t *testing.T) {
	if Weakest(nil) != nil {
		t.Error("no margins: nil")
	}
	if w := Weakest([]float64{3, -0.5, 1}); w == nil || *w != -0.5 {
		t.Errorf("weakest = %v", w)
	}
}

// A re-run decodes only what has no verdict for this model at this version;
// a changed model file (new version) needs every clip again.
func TestStoreAndMissing(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "sub", "results.jsonl")}
	clips := []Clip{{File: "a.wav", Target: "copy"}, {File: "b.wav", Target: "paste"}}
	if err := s.Append([]Result{{Model: "std", Version: "v1", Grammar: "g1", File: "a.wav", Heard: "copy", Right: true, DecodeMs: 10}}); err != nil {
		t.Fatal(err)
	}
	have := s.Load()
	if got := Missing(clips, "std", "v1", "g1", have); len(got) != 1 || got[0].File != "b.wav" {
		t.Errorf("missing at v1 = %+v", got)
	}
	if got := Missing(clips, "std", "v2", "g1", have); len(got) != 2 {
		t.Errorf("a new model version must need every clip, got %+v", got)
	}
	if got := Missing(clips, "light", "v1", "g1", have); len(got) != 2 {
		t.Errorf("another model must need every clip, got %+v", got)
	}
	// A grammar change starts afresh: an old verdict is never compared with
	// a new one.
	if got := Missing(clips, "std", "v1", "g2", have); len(got) != 2 {
		t.Errorf("a changed grammar must need every clip, got %+v", got)
	}
	// The last verdict written for a key wins.
	_ = s.Append([]Result{{Model: "std", Version: "v1", Grammar: "g1", File: "a.wav", Heard: "coffee", Right: false}})
	if r := s.Load()[Key("std", "v1", "g1", "a.wav")]; r.Right {
		t.Errorf("later verdict did not win: %+v", r)
	}
	if len((Store{Path: filepath.Join(t.TempDir(), "none.jsonl")}).Load()) != 0 {
		t.Error("a missing file is no verdicts")
	}
}

func TestSummarizeAndDisagreements(t *testing.T) {
	clips := []Clip{{File: "a", Target: "copy"}, {File: "b", Target: "paste"}, {File: "c", Target: "undo"}}
	have := map[string]Result{}
	put := func(m, f, heard, target string, ms float64, err string) {
		have[Key(m, "v", "g", f)] = Result{Model: m, Version: "v", Grammar: "g", File: f, Heard: heard, Right: Right(heard, target), DecodeMs: ms, Error: err}
	}
	put("std", "a", "copy", "copy", 10, "")
	put("std", "b", "paste", "paste", 30, "")
	put("std", "c", "", "undo", 0, "empty audio")
	put("light", "a", "copy", "copy", 5, "")
	put("light", "b", "face", "paste", 7, "")
	// light has no verdict on c yet

	s := Summarize(clips, "std", "v", "g", have)
	if s.Right != 2 || s.Total != 3 || s.Failed != 1 || s.MedianMs != 30 {
		t.Errorf("std summary = %+v", s)
	}
	l := Summarize(clips, "light", "v", "g", have)
	if l.Right != 1 || l.Total != 2 {
		t.Errorf("light summary (c not run yet) = %+v", l)
	}
	d := Disagreements(clips, map[string]string{"std": "v", "light": "v"}, "g", have)
	if len(d) != 1 || d[0].Clip.File != "b" || d[0].Heard["light"] != "face" || !d[0].Right["std"] {
		t.Errorf("disagreements = %+v", d)
	}
	if len(Disagreements(clips, map[string]string{"std": "v"}, "g", have)) != 0 {
		t.Error("one model has nothing to disagree with")
	}
}

// The acceptor skips what voice-regress skips: a word outside the union, or a
// sequence no command places. An open state takes any word of its alphabet.
func TestAcceptorSkipReason(t *testing.T) {
	vu := json.RawMessage(`{
		"words": ["snap", "left", "right", "say", "hello", "copy"],
		"grammar_dag": {
			"start": 0, "num_states": 4,
			"arcs": [
				{"from": 0, "to": 1, "word": "snap"},
				{"from": 1, "to": 0, "word": "left"},
				{"from": 1, "to": 0, "word": "right"},
				{"from": 0, "to": 0, "word": "copy"},
				{"from": 0, "to": 2, "word": "say"}
			],
			"open_states": [{"state": 2, "alphabet": null}]
		}
	}`)
	a, ok := NewAcceptor(vu)
	if !ok {
		t.Fatal("DAG not parsed")
	}
	for target, want := range map[string]string{
		"snap left":       "",
		"Copy":            "",
		"snap left copy":  "",
		"say hello hello": "",
		"snap copy":       "no grammar path",
		"snap":            "no grammar path",
		"snap up":         "words not in the grammar: up",
	} {
		if got := a.SkipReason(target, ok); got != want {
			t.Errorf("SkipReason(%q) = %q, want %q", target, got, want)
		}
	}
	// No DAG in the payload: only the union check.
	flat, ok := NewAcceptor(json.RawMessage(`{"words": ["snap", "copy"]}`))
	if ok || flat.SkipReason("copy snap", ok) != "" || flat.SkipReason("paste", ok) == "" {
		t.Error("flat fallback should check the union only")
	}
}

// The grammar's identity follows its content.
func TestGrammarID(t *testing.T) {
	a := GrammarID([]byte(`{"words":["copy"]}`))
	if a != GrammarID([]byte(`{"words":["copy"]}`)) || a == GrammarID([]byte(`{"words":["copy","paste"]}`)) {
		t.Error("GrammarID must be stable for the same grammar and differ for another")
	}
}

// In an exclusive mode the live stage decodes over narrow_to, so a word
// outside it is skipped even though the wide union holds it.
func TestAcceptorHonorsNarrowTo(t *testing.T) {
	a, ok := NewAcceptor(json.RawMessage(`{"words": ["left", "eight", "copy"], "narrow_to": ["left"]}`))
	if ok {
		t.Fatal("no DAG in this payload")
	}
	if a.SkipReason("left", ok) != "" || a.SkipReason("eight", ok) == "" {
		t.Error("the acceptor must check narrow_to, not the wide union")
	}
}
