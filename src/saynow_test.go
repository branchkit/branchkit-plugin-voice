package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
)

// While armed, Say a phrase takes command transcripts (they are swallowed,
// never run); after the window it takes none.
func TestSayNowIntercepts(t *testing.T) {
	var s sayNow
	now := time.Now()
	if s.intercepts(now) {
		t.Error("unarmed: nothing intercepted")
	}
	s.armedUntil = now.Add(sayNowWindow)
	if !s.intercepts(now) || s.intercepts(now.Add(sayNowWindow+time.Second)) {
		t.Error("armed window wrong")
	}
}

// A row per phrase, a column per downloaded model, each distinct hearing
// offered once as an answer, and a signal per row for its typed answer.
func TestSayNowView(t *testing.T) {
	models := commandModels
	pending := []sayNowClip{
		{File: "saynow-1/seg_00000.wav", Audio: "calibration-capture/saynow-1/seg_00000.wav",
			Heard: map[string]string{defaultCommandModel: "snap right", "sherpa-nemo-small": "snap eight"}},
		{File: "saynow-1/seg_00001.wav", Audio: "calibration-capture/saynow-1/seg_00001.wav",
			Heard: map[string]string{defaultCommandModel: "copy", "sherpa-nemo-small": "copy"}},
	}
	v := sayNowView(true, "opt+g", "", models, pending)
	if !v.Armed || v.Key != "opt+g" || len(v.Columns) != 2 || len(v.Rows) != 2 {
		t.Fatalf("view: %+v", v)
	}
	if got := v.Rows[0].Choices; len(got) != 2 || got[0] != "snap right" || got[1] != "snap eight" {
		t.Errorf("choices: %v", got)
	}
	if got := v.Rows[1].Choices; len(got) != 1 || got[0] != "copy" {
		t.Errorf("a hearing the models share is offered once: %v", got)
	}
	if v.Rows[0].Signal == v.Rows[1].Signal {
		t.Error("each row needs its own answer box signal")
	}
	if v.Rows[0].AudioURL != "/plugins/voice/data?file=calibration-capture%2Fsaynow-1%2Fseg_00000.wav" {
		t.Errorf("audio url: %s", v.Rows[0].AudioURL)
	}
}

// Confirming writes the clip to the corpus as a confirmed recording with the
// answer as its target, so the next comparison counts it; dismissing deletes
// the clip. Each answer is taken once.
func TestSayNowConfirmAndDismiss(t *testing.T) {
	data := t.TempDir()
	t.Setenv("BRANCHKIT_PLUGIN_DATA", data)
	game := filepath.Join(calibration.CaptureRoot(), "saynow-1")
	if err := os.MkdirAll(game, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"seg_00000.wav", "seg_00001.wav"} {
		if err := os.WriteFile(filepath.Join(game, f), []byte("RIFF"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &Host{}
	h.sayNow.pending = []sayNowClip{
		{File: "saynow-1/seg_00000.wav", Live: "snap eight", Ts: "2026-10-20T00:00:00Z"},
		{File: "saynow-1/seg_00001.wav", Live: "copy"},
	}
	if err := h.handleSayNowConfirm(&sayNowAnswer{File: "saynow-1/seg_00000.wav", Text: " Snap  Right "}); err != nil {
		t.Fatal(err)
	}
	rows := calibration.ReadCorpus()
	if len(rows) != 1 || rows[0].Target != "snap right" || rows[0].Disposition != calibration.DispositionConfirmed || rows[0].Matched {
		t.Fatalf("corpus after confirm: %+v", rows)
	}
	if clips := confirmedClips(); len(clips) != 1 || clips[0].Target != "snap right" {
		t.Errorf("the confirmed phrase is not a comparison clip: %+v", clips)
	}
	if err := h.handleSayNowConfirm(&sayNowAnswer{File: "saynow-1/seg_00000.wav", Text: "snap left"}); err != nil || len(calibration.ReadCorpus()) != 1 {
		t.Error("a phrase answered twice was written twice")
	}
	if err := h.handleSayNowDismiss(&sayNowAnswer{File: "saynow-1/seg_00001.wav"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(game, "seg_00001.wav")); !os.IsNotExist(err) {
		t.Error("a dismissed phrase's clip is still on disk")
	}
	if len(h.sayNow.pending) != 0 {
		t.Errorf("pending left: %+v", h.sayNow.pending)
	}
	if err := h.handleSayNowConfirm(&sayNowAnswer{File: "x", Text: "  "}); err == nil {
		t.Error("an empty answer must be refused")
	}
}

// One capture setting, two users: a release clears it only for its owner (an
// old Say a phrase session cannot stop a new one, nor stop a calibration
// game), and a calibration game takes it over and ends a session.
func TestCaptureClaim(t *testing.T) {
	next, patch, end, refused := captureClaim("", "saynow:a", "/cap/a")
	if next != "saynow:a" || !patch || end || refused {
		t.Errorf("session claims a free capture: %q %v %v %v", next, patch, end, refused)
	}
	next, patch, _, _ = captureClaim("saynow:b", "saynow:a", "")
	if next != "saynow:b" || patch {
		t.Error("an old session's release cleared a new session's capture")
	}
	next, patch, end, _ = captureClaim("saynow:b", captureOwnerCalibration, "/cap/game")
	if next != captureOwnerCalibration || !patch || !end {
		t.Error("a calibration game must take the capture over and end the session")
	}
	next, patch, _, _ = captureClaim(captureOwnerCalibration, "saynow:b", "")
	if next != captureOwnerCalibration || patch {
		t.Error("a session ending stopped a calibration game's recording")
	}
	if _, patch, _, refused = captureClaim(captureOwnerCalibration, "saynow:c", "/cap/c"); !refused || patch {
		t.Error("a session must not take the capture from a calibration game")
	}
	next, patch, _, _ = captureClaim(captureOwnerCalibration, captureOwnerCalibration, "")
	if next != "" || !patch {
		t.Error("calibration's own release must clear the capture")
	}
}

// The sweep keeps confirmed clips and clips waiting for an answer, deletes
// every other clip and, once nothing waits, the manifest, removes emptied
// folders, and leaves the folder a watcher still reads alone.
func TestSweepSayNow(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	root := calibration.CaptureRoot()
	write := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		"saynow-a/seg_0.wav", "saynow-a/seg_1.wav", "saynow-a/manifest.jsonl", // 0 confirmed, 1 not
		"saynow-b/seg_0.wav", "saynow-b/manifest.jsonl", // nothing kept
		"saynow-c/seg_0.wav", "saynow-c/manifest.jsonl", // 0 waiting for its answer
		"saynow-live/seg_0.wav", // a watcher still reads it
		"game1/seg_0.wav",       // a calibration game's: not Say a phrase's to sweep
	} {
		write(f)
	}
	if err := calibration.WriteCorpus([]calibration.CorpusEntry{{File: "saynow-a/seg_0.wav", Target: "copy", Disposition: calibration.DispositionConfirmed}}, "append"); err != nil {
		t.Fatal(err)
	}
	h := &Host{}
	h.sayNow.watching = "saynow-live"
	h.sayNow.pending = []sayNowClip{{File: "saynow-c/seg_0.wav"}}
	h.sweepSayNow()
	exists := func(rel string) bool { _, err := os.Stat(filepath.Join(root, rel)); return err == nil }
	for rel, want := range map[string]bool{
		"saynow-a/seg_0.wav": true, "saynow-a/seg_1.wav": false, "saynow-a/manifest.jsonl": false,
		"saynow-b":           false,
		"saynow-c/seg_0.wav": true, "saynow-c/manifest.jsonl": true,
		"saynow-live/seg_0.wav": true,
		"game1/seg_0.wav":       true,
	} {
		if exists(rel) != want {
			t.Errorf("%s: exists=%v, want %v", rel, !want, want)
		}
	}
}
