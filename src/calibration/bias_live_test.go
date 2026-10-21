package calibration

import (
	"testing"
	"time"
)

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad ts %q: %v", s, err)
	}
	return v
}

// pickLatestClip returns the newest clip written at or after the trial start,
// bounding out an earlier target's clip and keying the path under the game dir.
func TestPickLatestClip(t *testing.T) {
	start := ts(t, "2026-06-27T10:00:05Z")
	lines := []captureManifestLine{
		{File: "seg_1.wav", Text: "old", Ts: "2026-06-27T10:00:01Z"},  // before trial — prior target
		{File: "seg_2.wav", Text: "page", Ts: "2026-06-27T10:00:06Z"}, // this trial
		{File: "seg_3.wav", Text: "page", Ts: "2026-06-27T10:00:09Z"}, // this trial, newest
	}
	if got := pickLatestClip(lines, "g42", start); got != "g42/seg_3.wav" {
		t.Errorf("pickLatestClip = %q, want g42/seg_3.wav", got)
	}
}

// No clip at or after the trial start (a silent hold wrote nothing) yields "".
func TestPickLatestClip_None(t *testing.T) {
	start := ts(t, "2026-06-27T10:00:05Z")
	lines := []captureManifestLine{
		{File: "seg_1.wav", Text: "old", Ts: "2026-06-27T10:00:01Z"},
	}
	if got := pickLatestClip(lines, "g42", start); got != "" {
		t.Errorf("pickLatestClip = %q, want empty", got)
	}
}

// Rows missing a file or timestamp are skipped without tripping the picker.
func TestPickLatestClip_SkipsMalformed(t *testing.T) {
	start := ts(t, "2026-06-27T10:00:00Z")
	lines := []captureManifestLine{
		{File: "", Text: "x", Ts: "2026-06-27T10:00:06Z"},
		{File: "seg_2.wav", Text: "page", Ts: ""},
		{File: "seg_3.wav", Text: "page", Ts: "2026-06-27T10:00:07Z"},
	}
	if got := pickLatestClip(lines, "g", start); got != "g/seg_3.wav" {
		t.Errorf("pickLatestClip = %q, want g/seg_3.wav", got)
	}
}
