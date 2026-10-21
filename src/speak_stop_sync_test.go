package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/speech"
)

// The speak-stop/cancel phrases live in two places: the canonical tables
// (speech.StopPhrases / speech.CancelPhrases — which drive the sherpa-side
// detector and the text-domain backstop speech.StripSpeakPhrases) decide what
// a transcript means during speak dictation, and the gated command patterns in
// commands.json put the phrase words into the platform's union vocabulary so
// the recognizer can emit them at all. A desync between the two silently
// breaks voice-stop — it did for 25 days once.
// This test pins the two sources together.
func TestSpeakPhraseTablesMatchCommandPatterns(t *testing.T) {
	raw, err := os.ReadFile("../commands.json")
	if err != nil {
		t.Fatalf("read commands.json: %v", err)
	}
	var entries []struct {
		Pattern []json.RawMessage `json:"pattern"`
		Action  struct {
			Type  string `json:"type"`
			Phase string `json:"phase"`
		} `json:"action"`
		RequiresTags []string `json:"requires_tags"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse commands.json: %v", err)
	}

	fromCommands := map[string]bool{}
	for _, e := range entries {
		if e.Action.Type != "voice.speak" || e.Action.Phase != "stop" {
			continue
		}
		gated := false
		for _, tag := range e.RequiresTags {
			if tag == "plugin.voice.speaking" {
				gated = true
			}
		}
		if !gated {
			continue
		}
		for _, phrase := range expandPattern(t, e.Pattern) {
			fromCommands[phrase] = true
		}
	}

	fromTables := map[string]bool{}
	for _, p := range append(append([][]string{}, speech.StopPhrases...), speech.CancelPhrases...) {
		fromTables[strings.Join(p, " ")] = true
	}

	for phrase := range fromTables {
		if !fromCommands[phrase] {
			t.Errorf("phrase %q is in the detector tables but has no gated command pattern in commands.json — its words won't ride the union grammar and the recognizer can never emit it", phrase)
		}
	}
	for phrase := range fromCommands {
		if !fromTables[phrase] {
			t.Errorf("commands.json declares speak-stop phrase %q but the detector tables won't accept it — saying it during dictation does nothing", phrase)
		}
	}
}

// expandPattern cross-products a commands.json pattern (each slot is a
// word or a list of alternatives) into its concrete phrases.
func expandPattern(t *testing.T, slots []json.RawMessage) []string {
	t.Helper()
	phrases := []string{""}
	for _, slot := range slots {
		var word string
		var alts []string
		if err := json.Unmarshal(slot, &word); err == nil {
			alts = []string{word}
		} else if err := json.Unmarshal(slot, &alts); err != nil {
			t.Fatalf("pattern slot %s is neither a word nor an alternatives list", slot)
		}
		var next []string
		for _, prefix := range phrases {
			for _, alt := range alts {
				if prefix == "" {
					next = append(next, alt)
				} else {
					next = append(next, prefix+" "+alt)
				}
			}
		}
		phrases = next
	}
	return phrases
}
