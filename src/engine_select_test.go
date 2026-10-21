package main

import (
	"encoding/json"
	"os"
	"testing"
)

// The command_recognition pipeline record is seeded authoritatively by
// plugin.json (the plugin no longer rewrites it at startup). Guard the
// load-bearing fields there so a seed edit can't silently break recognition:
//   - the Sherpa command stage + model name must agree (a mismatch crashes the
//     stage with "eof before capability")
//   - cancel_delay_ms must be non-zero so the Sherpa end-of-hold flush (the
//     closing word of a two-word codeword) reaches the matcher before the reader
//     is cancelled — regression guard for the "say jar, pause, day" bug.
func TestPluginJSONSeedsCommandRecognition(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var manifest struct {
		CollectionData struct {
			Pipelines []struct {
				Name          string           `json:"name"`
				CancelDelayMs int              `json:"cancel_delay_ms"`
				Stages        []map[string]any `json:"stages"`
			} `json:"_platform.pipelines"`
		} `json:"collection_data"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse plugin.json: %v", err)
	}

	var cmd *struct {
		Name          string           `json:"name"`
		CancelDelayMs int              `json:"cancel_delay_ms"`
		Stages        []map[string]any `json:"stages"`
	}
	for i := range manifest.CollectionData.Pipelines {
		if manifest.CollectionData.Pipelines[i].Name == "command_recognition" {
			cmd = &manifest.CollectionData.Pipelines[i]
		}
	}
	if cmd == nil {
		t.Fatal("plugin.json has no command_recognition pipeline seed")
	}
	if cmd.CancelDelayMs <= 0 {
		t.Errorf("cancel_delay_ms = %d, want > 0 to drain the end-of-hold flush", cmd.CancelDelayMs)
	}

	var found bool
	for _, s := range cmd.Stages {
		if s["stage"] == "voice.sherpa_commands" {
			found = true
			params, _ := s["params"].(map[string]any)
			if params["model"] != "sherpa-offline-nemo" {
				t.Errorf("sherpa stage model = %v, want sherpa-offline-nemo", params["model"])
			}
		}
	}
	if !found {
		t.Error("command_recognition seed must name the voice.sherpa_commands stage")
	}
}
