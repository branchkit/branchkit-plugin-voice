package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
)

func captureTestRecord(params map[string]any) map[string]any {
	stage := map[string]any{"stage": "voice.sherpa_commands"}
	if params != nil {
		stage["params"] = params
	}
	return map[string]any{"stages": []any{
		map[string]any{"stage": "_platform.mic", "params": map[string]any{"dump-segments": "not-ours"}},
		stage,
	}}
}

func sherpaParams(record map[string]any) map[string]any {
	params, _ := record["stages"].([]any)[1].(map[string]any)["params"].(map[string]any)
	return params
}

// A record that still names a capture folder is what a process that died
// mid-game leaves behind: clearing it must report a change, so the start
// path writes the record back.
func TestClearingALeftoverCaptureFolderChangesTheRecord(t *testing.T) {
	record := captureTestRecord(map[string]any{"dump-segments": "/data/calibration-capture/g1", "model": "m"})
	if !setCaptureDirParam(record, "") {
		t.Fatal("a record with a capture folder reported no change when cleared")
	}
	params := sherpaParams(record)
	if _, set := params["dump-segments"]; set {
		t.Error("the capture folder is still on the record")
	}
	if params["model"] != "m" {
		t.Error("clearing the capture folder touched another param")
	}
	mic := record["stages"].([]any)[0].(map[string]any)["params"].(map[string]any)
	if mic["dump-segments"] != "not-ours" {
		t.Error("clearing reached a stage other than the command stage")
	}
}

// The usual start: nothing was left on, so nothing is written.
func TestClearingARecordWithNoCaptureFolderIsNoChange(t *testing.T) {
	for _, params := range []map[string]any{nil, {}, {"model": "m"}} {
		if setCaptureDirParam(captureTestRecord(params), "") {
			t.Errorf("params %v: reported a change with no capture folder set", params)
		}
	}
}

func TestSettingACaptureFolder(t *testing.T) {
	record := captureTestRecord(nil)
	if !setCaptureDirParam(record, "/d") {
		t.Fatal("setting a folder reported no change")
	}
	if sherpaParams(record)["dump-segments"] != "/d" {
		t.Fatal("the folder is not on the command stage")
	}
	if setCaptureDirParam(record, "/d") {
		t.Error("setting the same folder again reported a change")
	}
}

func writeCaptureFile(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A fresh process deletes what a game it never finished left on disk and
// keeps what a finished game indexed, even when the record cannot be read.
func TestStartRemovesRecordingsNoFinishedGameKept(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	root := calibration.CaptureRoot()
	writeCaptureFile(t, root, "kept/seg_00001.wav")
	writeCaptureFile(t, root, "abandoned/seg_00001.wav")
	writeCaptureFile(t, root, "abandoned/manifest.jsonl")
	if err := calibration.WriteCorpus([]calibration.CorpusEntry{{File: "kept/seg_00001.wav", Target: "k"}}, "rewrite"); err != nil {
		t.Fatal(err)
	}

	newTestHost().reconcileOrphanedCapture()

	if _, err := os.Stat(filepath.Join(root, "abandoned")); !os.IsNotExist(err) {
		t.Error("the abandoned game's folder is still on disk")
	}
	if _, err := os.Stat(filepath.Join(root, "kept/seg_00001.wav")); err != nil {
		t.Errorf("an indexed clip was removed: %v", err)
	}
}

// Called while this process holds the capture (never at start, but the guard
// is what keeps a later call from deleting a running game's clips).
func TestReconcileLeavesAHeldCaptureAlone(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	root := calibration.CaptureRoot()
	writeCaptureFile(t, root, "running/seg_00001.wav")
	h := newTestHost()
	h.captureOwner = captureOwnerCalibration

	h.reconcileOrphanedCapture()

	if _, err := os.Stat(filepath.Join(root, "running/seg_00001.wav")); err != nil {
		t.Errorf("a running game's clip was removed: %v", err)
	}
}
