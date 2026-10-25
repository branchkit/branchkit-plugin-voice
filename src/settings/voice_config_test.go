package settings

import (
	"encoding/json"
	"testing"
)

func TestDefaultVoiceConfig(t *testing.T) {
	conf := DefaultVoiceConfig()
	if conf.Model != "openai_whisper-large-v3-v20240930" {
		t.Errorf("expected default model, got %s", conf.Model)
	}
	if conf.Language != "en" {
		t.Errorf("expected en, got %s", conf.Language)
	}
	if conf.CommandLanguage != "en-US" {
		t.Errorf("expected en-US, got %s", conf.CommandLanguage)
	}
	if !conf.FixAllCaps {
		t.Error("expected fix_all_caps to default to true")
	}
	if !conf.CalibrationCapture {
		t.Error("expected calibration_capture to default to true")
	}
	if conf.CaptureRetention != "keep_bounded" {
		t.Errorf("expected keep_bounded, got %s", conf.CaptureRetention)
	}
	if conf.CalibrationAutoAdvance {
		t.Error("expected calibration_auto_advance to default to false")
	}
}

// DefaultVoiceConfig is the diff base for sparse relays AND the pre-mirror
// fallback; plugin.json's declared defaults are the authoritative copy. This
// pin ensures a manifest edit that forgets the Go side (or vice versa) fails
// a test instead of silently double-sourcing.
func TestDefaultsStayInSyncWithManifestShape(t *testing.T) {
	b, err := json.Marshal(DefaultVoiceConfig())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"model", "command_model", "language", "command_language", "fix_all_caps", "spoken_punctuation", "refine_dictation",
		"dictation_enabled", "voice_commands_enabled",
		"calibration_capture", "capture_retention", "calibration_auto_advance",
		"dictation_sounds", "input_device", "input_device_name",
		"command_confidence_gate", "command_confidence_floor",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("VoiceConfig lost manifest field %q", key)
		}
	}
	if len(m) != 17 {
		t.Errorf("VoiceConfig has %d fields; plugin.json declares 17 — update BOTH", len(m))
	}
}
