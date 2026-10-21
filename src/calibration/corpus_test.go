package calibration

import (
	"testing"
	"time"
)

func TestLabelClips_AssignsTargetByTrialWindow(t *testing.T) {
	base := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	rfc := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }

	windows := []captureWindow{
		{Target: "vest", Start: base},
		{Target: "zip", Start: base.Add(10 * time.Second)},
	}
	clips := []captureManifestLine{
		{File: "seg_00000.wav", Text: "FIRST", Ts: rfc(1 * time.Second), WordScores: []float64{-2.1}}, // mishear during vest
		{File: "seg_00001.wav", Text: "VEST", Ts: rfc(2 * time.Second), WordScores: []float64{5.4}},   // vest
		{File: "seg_00002.wav", Text: "ZIP", Ts: rfc(11 * time.Second)},                               // zip (pre-posteriors, no scores)
		{File: "seg_00003.wav", Text: "STRAY", Ts: rfc(-1 * time.Second)},                             // before any trial -> dropped
	}

	got := labelClips("game1", clips, windows, "sherpa-offline-nemo")

	if len(got) != 3 {
		t.Fatalf("want 3 labeled clips (stray dropped), got %d: %+v", len(got), got)
	}
	// First clip: a mishear, but labeled by ground-truth target (the valuable case).
	if got[0].Target != "vest" || got[0].Heard != "FIRST" || got[0].Matched {
		t.Errorf("clip 0 = %+v, want target=vest heard=FIRST matched=false", got[0])
	}
	// Its per-word margins ride onto the labeled row (ground-truth score record);
	// a clip with no manifest scores carries none rather than erroring.
	if len(got[0].WordScores) != 1 || got[0].WordScores[0] != -2.1 {
		t.Errorf("clip 0 WordScores = %v, want [-2.1]", got[0].WordScores)
	}
	if got[2].WordScores != nil {
		t.Errorf("clip 2 WordScores = %v, want nil (pre-posteriors)", got[2].WordScores)
	}
	if got[0].File != "game1/seg_00000.wav" {
		t.Errorf("clip 0 File = %q, want game1/seg_00000.wav", got[0].File)
	}
	if got[1].Target != "vest" || !got[1].Matched {
		t.Errorf("clip 1 = %+v, want target=vest matched=true", got[1])
	}
	if got[2].Target != "zip" || !got[2].Matched {
		t.Errorf("clip 2 = %+v, want target=zip matched=true", got[2])
	}
}

func TestPruneBounded_KeepsLatestPerWord(t *testing.T) {
	entries := []CorpusEntry{
		{File: "g1/a.wav", Target: "vest", Ts: "2026-06-19T10:00:00Z"},
		{File: "g2/b.wav", Target: "vest", Ts: "2026-06-19T11:00:00Z"}, // newer vest -> kept
		{File: "g1/c.wav", Target: "zip", Ts: "2026-06-19T10:30:00Z"},  // only zip -> kept
	}
	keep, drop := pruneBounded(entries, 100)

	if len(keep) != 2 || len(drop) != 1 {
		t.Fatalf("want keep=2 drop=1, got keep=%d drop=%d", len(keep), len(drop))
	}
	if drop[0].File != "g1/a.wav" {
		t.Errorf("expected the older vest clip dropped, got %q", drop[0].File)
	}
}

func TestPruneBounded_CapsTotal(t *testing.T) {
	entries := []CorpusEntry{
		{File: "a.wav", Target: "one", Ts: "2026-06-19T10:00:00Z"},
		{File: "b.wav", Target: "two", Ts: "2026-06-19T11:00:00Z"},
		{File: "c.wav", Target: "three", Ts: "2026-06-19T12:00:00Z"},
	}
	// All distinct targets, but cap at 2 -> keep the 2 most-recent.
	keep, drop := pruneBounded(entries, 2)
	if len(keep) != 2 || len(drop) != 1 {
		t.Fatalf("want keep=2 drop=1 under cap, got keep=%d drop=%d", len(keep), len(drop))
	}
	if drop[0].File != "a.wav" {
		t.Errorf("oldest should be dropped by the cap, got %q", drop[0].File)
	}
}
