package main

import (
	"testing"
)

// commandRecognitionRunParams names the pipeline and carries only the chosen
// model: the seeded record (plugin.json) stays authoritative for the rest.
func TestCommandRecognitionRunParams_CarriesOnlyTheModel(t *testing.T) {
	params := commandRecognitionRunParams("sherpa-nemo-small", true)
	if params.Name != "command_recognition" {
		t.Errorf("name = %v, want command_recognition", params.Name)
	}
	if len(params.Overrides) != 1 || string(params.Overrides["model"]) != `"sherpa-nemo-small"` {
		t.Errorf("overrides = %v, want only model=sherpa-nemo-small", params.Overrides)
	}
}

func TestCommandRecognitionRunParams_EphemeralFlag(t *testing.T) {
	if e := commandRecognitionRunParams(defaultCommandModel, true).Ephemeral; e == nil || !*e {
		t.Error("expected ephemeral=true when ephemeral")
	}
	if commandRecognitionRunParams(defaultCommandModel, false).Ephemeral != nil {
		t.Error("expected no ephemeral flag when non-ephemeral")
	}
}
