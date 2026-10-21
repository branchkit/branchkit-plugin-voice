package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Recording defaults for voice's log collections.
//
// A log collection records only while its recording flag is on. The flag is
// the user's choice once they make one — the platform persists it beside the
// entries — and `privacy.default_recording` until then. So the default is
// what every install that never chose gets, including existing ones: nothing
// is materialised at install time, and changing it here changes them.

type collectionDecl struct {
	Preset    string `json:"preset"`
	Lifecycle *struct {
		Type          string `json:"type"`
		RetentionDays *int   `json:"retention_days"`
	} `json:"lifecycle"`
	Privacy *struct {
		DefaultRecording   *bool   `json:"default_recording"`
		ReasonForDefaultOn *string `json:"reason_for_default_on"`
	} `json:"privacy"`
}

func readCollectionDecl(t *testing.T, path string) collectionDecl {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d collectionDecl
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return d
}

// The recognition history — every matched command's phrase — is off until
// the user turns it on, and keeps 30 days when they do.
func TestRecognitionLogIsOffByDefaultWithThirtyDayRetention(t *testing.T) {
	d := readCollectionDecl(t, filepath.Join("..", "collections", "recognition_log.json"))
	if d.Preset != "log" {
		t.Fatalf("recognition_log preset = %q, want log", d.Preset)
	}
	// Stated, not inherited from the preset: the decision is visible in the
	// manifest, and a later preset change can't turn it on.
	if d.Privacy == nil || d.Privacy.DefaultRecording == nil {
		t.Fatal("recognition_log must declare privacy.default_recording explicitly")
	}
	if *d.Privacy.DefaultRecording {
		t.Error("recognition_log must not record by default")
	}
	if d.Privacy.ReasonForDefaultOn != nil {
		t.Error("recognition_log is off by default; reason_for_default_on must go")
	}
	if d.Lifecycle == nil || d.Lifecycle.Type != "retention" || d.Lifecycle.RetentionDays == nil {
		t.Fatalf("recognition_log needs a retention lifecycle, got %+v", d.Lifecycle)
	}
	if got := *d.Lifecycle.RetentionDays; got != 30 {
		t.Errorf("recognition_log retention = %d days, want 30", got)
	}
}

// calibrationLogs are the voice logs that record by default. Each is written
// only during a calibration sitting the user starts, and holds the prompted
// command and the engine's reading of it — never dictation or a command hold.
// The calibration screen is built from them, so off by default would leave it
// empty on first use. A new default-on log has to be added here, with its
// reason.
var calibrationLogs = map[string]string{
	"verification_attempts.json": "per-attempt calibration results: the score and per-command health",
	"verification_sessions.json": "per-sitting calibration summaries: the score history",
	"clip_fragility.json":        "per-clip readings of recorded calibration clips",
	"clip_bias_score.json":       "per-clip bias scores of recorded calibration clips",
}

func TestOnlyCalibrationLogsRecordByDefault(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "collections", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no collections found: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		d := readCollectionDecl(t, p)
		if d.Preset != "log" {
			continue
		}
		name := filepath.Base(p)
		on := d.Privacy != nil && d.Privacy.DefaultRecording != nil && *d.Privacy.DefaultRecording
		if !on {
			continue
		}
		seen[name] = true
		if _, ok := calibrationLogs[name]; !ok {
			t.Errorf("%s records by default but is not a calibration log; "+
				"default it off, or add it to calibrationLogs with its reason", name)
		}
		if d.Privacy.ReasonForDefaultOn == nil || strings.TrimSpace(*d.Privacy.ReasonForDefaultOn) == "" {
			t.Errorf("%s records by default without a reason_for_default_on", name)
		}
	}
	for name := range calibrationLogs {
		if !seen[name] {
			t.Errorf("calibrationLogs lists %s, which no longer records by default; drop it", name)
		}
	}
}
