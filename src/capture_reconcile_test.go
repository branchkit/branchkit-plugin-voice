package main

import (
	"errors"
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

func TestCaptureDirParamReadsTheCommandStage(t *testing.T) {
	if got := captureDirParam(captureTestRecord(map[string]any{"dump-segments": "/d/g1"})); got != "/d/g1" {
		t.Errorf("got %q", got)
	}
	// The mic stage's param of the same name is not the capture folder.
	if got := captureDirParam(captureTestRecord(nil)); got != "" {
		t.Errorf("no folder set, got %q", got)
	}
}

// Only a calibration game's folder directly under the capture root is
// deleted at start. A Say a phrase folder holds clips the person may have
// confirmed, and anything else is not ours to remove.
func TestLeftoverGameNamesOnlyAGameFolderUnderTheRoot(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	root := calibration.CaptureRoot()
	cases := map[string]string{
		filepath.Join(root, "g1"):              "g1",
		filepath.Join(root, "g1") + "/":        "g1",
		filepath.Join(root, "saynow-abc"):      "",
		filepath.Join(root, "g1", "deeper"):    "",
		root:                                   "",
		filepath.Dir(root):                     "",
		filepath.Join(root, "..", "elsewhere"): "",
		"/somewhere/else/g1":                   "",
		"":                                     "",
	}
	for dir, want := range cases {
		if got := leftoverGame(dir); got != want {
			t.Errorf("leftoverGame(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestLeftoverGameWithNoDataDirNamesNothing(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", "")
	if got := leftoverGame("/x/calibration-capture/g1"); got != "" {
		t.Errorf("got %q with no capture root", got)
	}
}

// With the record unreadable nothing is known to be abandoned, so nothing
// on disk is touched: recordings the index does not list stay (a crash can
// cut the index short, and they are still in the Recordings list).
func TestStartDeletesNothingWhenTheRecordCannotBeRead(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	root := calibration.CaptureRoot()
	p := filepath.Join(root, "g1", "seg_00001.wav")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}

	newTestHost().reconcileOrphanedCapture()

	if _, err := os.Stat(p); err != nil {
		t.Errorf("a recording was removed with the record unread: %v", err)
	}
}

// leftoverRecord sets up a capture root holding one game folder with a clip
// and a manifest, and the record a process that died mid-game leaves.
func leftoverRecord(t *testing.T, game string) (record map[string]any, clip string) {
	t.Helper()
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	dir := filepath.Join(calibration.CaptureRoot(), game)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	clip = filepath.Join(dir, "seg_00001.wav")
	for _, p := range []string{clip, filepath.Join(dir, "manifest.jsonl")} {
		if err := os.WriteFile(p, []byte("RIFF"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return captureTestRecord(map[string]any{"dump-segments": dir}), clip
}

// The whole case: the record names a game folder, so the record is written
// back without it and the game's recordings are deleted.
func TestALeftoverCaptureIsTurnedOffAndItsGameDeleted(t *testing.T) {
	record, clip := leftoverRecord(t, "g1")
	puts := 0
	clearLeftoverCapture(record, func(written map[string]any) error {
		puts++
		if captureDirParam(written) != "" {
			t.Error("the record written back still names a capture folder")
		}
		return nil
	})
	if puts != 1 {
		t.Fatalf("the record was written %d times, want 1", puts)
	}
	if _, err := os.Stat(filepath.Dir(clip)); !os.IsNotExist(err) {
		t.Error("the abandoned game's folder is still on disk")
	}
}

// Nothing left on: no write, and nothing on disk is touched.
func TestNoLeftoverCaptureWritesAndDeletesNothing(t *testing.T) {
	_, clip := leftoverRecord(t, "g1")
	clearLeftoverCapture(captureTestRecord(map[string]any{"model": "m"}), func(map[string]any) error {
		t.Error("a record with no capture folder was written back")
		return nil
	})
	if _, err := os.Stat(clip); err != nil {
		t.Errorf("a recording was removed with no capture left on: %v", err)
	}
}

// If the record cannot be cleared the stage may still be writing there, and
// the next start tries again: nothing is deleted.
func TestAFailedWriteDeletesNothing(t *testing.T) {
	record, clip := leftoverRecord(t, "g1")
	clearLeftoverCapture(record, func(map[string]any) error { return errors.New("denied") })
	if _, err := os.Stat(clip); err != nil {
		t.Errorf("a recording was removed though the record was not cleared: %v", err)
	}
}

// A game that finished while its release failed has indexed clips in the
// folder the record still names. They are kept recordings.
func TestALeftoverFolderWithIndexedClipsIsKept(t *testing.T) {
	record, clip := leftoverRecord(t, "g1")
	if err := calibration.WriteCorpus([]calibration.CorpusEntry{{File: "g1/seg_00001.wav", Target: "k"}}, "rewrite"); err != nil {
		t.Fatal(err)
	}
	cleared := false
	clearLeftoverCapture(record, func(map[string]any) error { cleared = true; return nil })
	if !cleared {
		t.Error("the capture was not turned off")
	}
	if _, err := os.Stat(clip); err != nil {
		t.Errorf("an indexed clip was removed: %v", err)
	}
}

// A Say a phrase folder is turned off but left to that feature's own sweep.
func TestALeftoverSayAPhraseFolderIsNotDeletedHere(t *testing.T) {
	record, clip := leftoverRecord(t, "saynow-abc")
	clearLeftoverCapture(record, func(map[string]any) error { return nil })
	if _, err := os.Stat(clip); err != nil {
		t.Errorf("a Say a phrase clip was removed by the game cleanup: %v", err)
	}
}
