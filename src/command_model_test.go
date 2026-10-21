package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/settings"
)

func TestResolveCommandModel(t *testing.T) {
	if got := resolveCommandModel("sherpa-nemo-small"); got != "sherpa-nemo-small" {
		t.Errorf("a command model resolved to %q", got)
	}
	for _, junk := range []string{"", "openai_whisper-large-v3-v20240930", "sherpa-nemo-medium"} {
		if got := resolveCommandModel(junk); got != defaultCommandModel {
			t.Errorf("resolveCommandModel(%q) = %q, want the default", junk, got)
		}
	}
}

// The stage must never be handed a --model that is not on disk while another
// command model is: choosing Light, still downloading, keeps Standard running.
func TestCommandModelOnDisk(t *testing.T) {
	on := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, x := range names {
				if x == n {
					return true
				}
			}
			return false
		}
	}
	small := "sherpa-nemo-small"
	cases := []struct {
		name, chosen string
		onDisk       func(string) bool
		want         string
	}{
		{"chosen on disk", small, on(defaultCommandModel, small), small},
		{"chosen still downloading", small, on(defaultCommandModel), defaultCommandModel},
		{"only the light one on disk", defaultCommandModel, on(small), small},
		{"nothing on disk", small, on(), small},
	}
	for _, c := range cases {
		if got := commandModelOnDisk(c.chosen, c.onDisk); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Every command model voice offers is an artifact the manifest declares, and
// the setting's declared default is the default command model, in both the
// manifest and the pre-Ready fallback.
func TestCommandModelsMatchTheManifest(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Provides struct {
			Artifacts   map[string]json.RawMessage `json:"artifacts"`
			Collections map[string]json.RawMessage `json:"collections"`
		} `json:"provides"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, c := range commandModels {
		if _, ok := m.Provides.Artifacts[c.Name]; !ok {
			t.Errorf("command model %q is not a declared artifact", c.Name)
		}
	}
	var cfg struct {
		Fields []struct {
			Key     string          `json:"key"`
			Default json.RawMessage `json:"default"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(m.Provides.Collections["plugin.voice.config"], &cfg); err != nil {
		t.Fatal(err)
	}
	var def string
	for _, f := range cfg.Fields {
		if f.Key == "command_model" {
			_ = json.Unmarshal(f.Default, &def)
		}
	}
	if def != defaultCommandModel {
		t.Errorf("manifest command_model default = %q, want %q", def, defaultCommandModel)
	}
	if got := settings.DefaultVoiceConfig().CommandModel; got != defaultCommandModel {
		t.Errorf("DefaultVoiceConfig().CommandModel = %q, want %q", got, defaultCommandModel)
	}
}

// A failed download of either command model stays on screen while that model
// is missing, even with the chosen dictation model on disk.
func TestSetupDownloadErrorClassifiesEveryCommandModel(t *testing.T) {
	dl := &ModelDownloadData{Model: "voice/sherpa-nemo-small", Error: "checksum mismatch"}
	onDisk := map[string]string{"dictation-model": "1 GB", defaultCommandModel: "183 MB"}
	if got := setupDownloadError(dl, "voice/", "dictation-model", onDisk); got != "checksum mismatch" {
		t.Errorf("the light command model's failure was hidden: %q", got)
	}
}

// The boot warm-up records the default before the settings arrive; when they
// name another model the stage moves. A Use click and the settings refresh it
// fires both claim the same model: only the first moves the stage.
func TestClaimCommandModelState(t *testing.T) {
	small := "sherpa-nemo-small"
	if applied, move := claimCommandModelState(defaultCommandModel, small, false); !move || applied != small {
		t.Errorf("settings naming Light after a default warm-up: applied %q move %v", applied, move)
	}
	if applied, move := claimCommandModelState(small, small, true); move || applied != small {
		t.Errorf("the second claim of the same model moved the stage again (applied %q)", applied)
	}
	if applied, move := claimCommandModelState("", small, false); move || applied != "" {
		t.Errorf("a refresh before the warm-up moved the stage (applied %q)", applied)
	}
	if applied, move := claimCommandModelState("", small, true); !move || applied != small {
		t.Errorf("an explicit choice before the warm-up did not move the stage (applied %q)", applied)
	}
}
