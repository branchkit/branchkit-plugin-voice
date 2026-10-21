package calibration

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// The capture store deletes the user's recordings, so these tests run each
// operation against a real directory under a temp plugin data dir.

// newCaptureRoot points the plugin data dir at a temp dir and returns the
// capture root inside it, created.
func newCaptureRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	root := CaptureRoot()
	if root == "" {
		t.Fatal("CaptureRoot empty with BRANCHKIT_PLUGIN_DATA set")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// writeClip creates root/<rel> (a WAV or any file) with a little content.
func writeClip(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("RIFF"+rel), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// listFiles returns every regular file under root, relative, sorted.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestCaptureRootUnsetMeansUnavailable(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", "")
	if CaptureRoot() != "" {
		t.Fatal("CaptureRoot must be empty without a data dir")
	}
	// Every mutator is a no-op rather than a guessed path.
	if err := WriteCorpus([]CorpusEntry{{File: "g/a.wav"}}, "rewrite"); err != nil {
		t.Fatal(err)
	}
	if n, err := ClearAll(); n != 0 || err != nil {
		t.Fatalf("ClearAll = %d, %v", n, err)
	}
	if SweepOrphans() != 0 || ReadCorpus() != nil || ScanRecordings() != nil {
		t.Fatal("reads must be empty without a data dir")
	}
}

// Paths that clean to the root itself, or climb out of it, must never be
// resolved: SweepGame(".") used to RemoveAll the whole capture root, corpus
// index included.
func TestClipPathRefusesRootAndEscapes(t *testing.T) {
	for _, rel := range []string{"", ".", "./", "a/..", "a/../", "..", "../x", "a/../../x", "/abs", "/"} {
		if p, ok := clipPath("/root", rel); ok {
			t.Errorf("clipPath(%q) = %q, accepted", rel, p)
		}
	}
	for rel, want := range map[string]string{
		"g":           "/root/g",
		"g/a.wav":     "/root/g/a.wav",
		"g/./a.wav":   "/root/g/a.wav",
		"g/x/../a.wa": "/root/g/a.wa",
		"..g":         "/root/..g", // a name that merely starts with dots is local
	} {
		if p, ok := clipPath("/root", rel); !ok || p != filepath.FromSlash(want) {
			t.Errorf("clipPath(%q) = %q,%v, want %q", rel, p, ok, want)
		}
	}
}

func TestSweepAndDeleteRefuseTheRoot(t *testing.T) {
	root := newCaptureRoot(t)
	writeClip(t, root, "g1/a.wav")
	if err := WriteCorpus([]CorpusEntry{{File: "g1/a.wav", Target: "alpha"}}, "rewrite"); err != nil {
		t.Fatal(err)
	}
	before := listFiles(t, root)

	for _, bad := range []string{"..", ".", "a/..", "", "../" + filepath.Base(filepath.Dir(root))} {
		if err := SweepGame(bad); err != nil {
			t.Errorf("SweepGame(%q): %v", bad, err)
		}
		if err := DeleteClip(bad); err != nil {
			t.Errorf("DeleteClip(%q): %v", bad, err)
		}
	}
	if !exists(root) {
		t.Fatal("capture root removed")
	}
	if got := listFiles(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("files changed: %v, want %v", got, before)
	}
	if !exists(filepath.Join(root, corpusIndexName)) {
		t.Fatal("corpus index removed")
	}
}

func TestSweepGameAndDeleteClip(t *testing.T) {
	root := newCaptureRoot(t)
	writeClip(t, root, "g1/a.wav")
	writeClip(t, root, "g1/b.wav")
	writeClip(t, root, "g2/c.wav")

	if err := DeleteClip("g1/a.wav"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteClip("g1/missing.wav"); err != nil {
		t.Fatalf("deleting an absent clip is not an error: %v", err)
	}
	if err := SweepGame("g2"); err != nil {
		t.Fatal(err)
	}
	if got, want := listFiles(t, root), []string{"g1/b.wav"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("left %v, want %v", got, want)
	}
}

func TestWriteCorpusAppendAndRewrite(t *testing.T) {
	newCaptureRoot(t)
	a := CorpusEntry{File: "g/a.wav", Target: "alpha", WordScores: []float64{0.5}}
	b := CorpusEntry{File: "g/b.wav", Target: "bravo", Matched: true}
	c := CorpusEntry{File: "g/c.wav", Target: "charlie"}

	if err := WriteCorpus([]CorpusEntry{a}, "append"); err != nil {
		t.Fatal(err)
	}
	if err := WriteCorpus([]CorpusEntry{b}, "append"); err != nil {
		t.Fatal(err)
	}
	if err := WriteCorpus(nil, "append"); err != nil { // no-op
		t.Fatal(err)
	}
	if got := ReadCorpus(); !reflect.DeepEqual(got, []CorpusEntry{a, b}) {
		t.Fatalf("after appends: %+v", got)
	}
	if err := WriteCorpus([]CorpusEntry{c}, "rewrite"); err != nil {
		t.Fatal(err)
	}
	if got := ReadCorpus(); !reflect.DeepEqual(got, []CorpusEntry{c}) {
		t.Fatalf("after rewrite: %+v", got)
	}
	// An empty rewrite empties the index (the prune dropped everything).
	if err := WriteCorpus(nil, "rewrite"); err != nil {
		t.Fatal(err)
	}
	if got := ReadCorpus(); len(got) != 0 {
		t.Fatalf("after empty rewrite: %+v", got)
	}
}

func TestReadCorpusSkipsCorruptLines(t *testing.T) {
	root := newCaptureRoot(t)
	body := `{"file":"g/a.wav","target":"alpha"}` + "\n" + "not json\n\n" + `{"file":"g/b.wav","target":"bravo"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, corpusIndexName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadCorpus()
	if len(got) != 2 || got[0].File != "g/a.wav" || got[1].File != "g/b.wav" {
		t.Fatalf("ReadCorpus = %+v", got)
	}
}

func TestSetDisposition(t *testing.T) {
	newCaptureRoot(t)
	rows := []CorpusEntry{{File: "g/a.wav", Target: "alpha"}, {File: "g/b.wav", Target: "bravo"}}
	if err := WriteCorpus(rows, "rewrite"); err != nil {
		t.Fatal(err)
	}
	if ok, err := SetDisposition("g/b.wav", DispositionConfirmed); !ok || err != nil {
		t.Fatalf("SetDisposition = %v, %v", ok, err)
	}
	got := ReadCorpus()
	if got[0].Disposition != "" || got[1].Disposition != DispositionConfirmed {
		t.Fatalf("dispositions = %q, %q", got[0].Disposition, got[1].Disposition)
	}
	if ok, _ := SetDisposition("g/b.wav", ""); !ok || ReadCorpus()[1].Disposition != "" {
		t.Fatal("clearing a disposition did not stick")
	}
	if ok, _ := SetDisposition("g/missing.wav", DispositionConfirmed); ok {
		t.Fatal("marked a clip the corpus does not index")
	}
	if len(ReadCorpus()) != 2 {
		t.Fatal("a miss rewrote the corpus")
	}
}

// Labeled clips and the index survive; unreferenced WAVs go, and a game dir
// left with no WAV goes with them. Non-WAV files in a game that keeps a
// labeled clip are left alone.
func TestSweepOrphans(t *testing.T) {
	root := newCaptureRoot(t)
	writeClip(t, root, "g1/kept.wav")
	writeClip(t, root, "g1/orphan.wav")
	writeClip(t, root, "g1/"+captureManifestName)
	writeClip(t, root, "g2/orphan.wav")
	writeClip(t, root, "g3/"+captureManifestName) // no WAV at all
	if err := WriteCorpus([]CorpusEntry{{File: "g1/kept.wav", Target: "k"}}, "rewrite"); err != nil {
		t.Fatal(err)
	}

	if n := SweepOrphans(); n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	want := []string{corpusIndexName, "g1/kept.wav", "g1/" + captureManifestName}
	sort.Strings(want)
	if got := listFiles(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("left %v, want %v", got, want)
	}
	for _, g := range []string{"g2", "g3"} {
		if exists(filepath.Join(root, g)) {
			t.Errorf("empty game dir %s not removed", g)
		}
	}
}

func TestClearAll(t *testing.T) {
	root := newCaptureRoot(t)
	writeClip(t, root, "g1/a.wav")
	writeClip(t, root, "g1/b.wav")
	writeClip(t, root, "g2/c.wav")
	if err := WriteCorpus([]CorpusEntry{{File: "g1/a.wav"}}, "rewrite"); err != nil {
		t.Fatal(err)
	}
	// An export archive lives in the data dir, beside the capture root, and
	// is not the capture store's to clear.
	outside := filepath.Join(filepath.Dir(root), "keep.tar.gz")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := ClearAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // three WAVs + the index
		t.Errorf("ClearAll counted %d, want 4", n)
	}
	if got := listFiles(t, root); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	if !exists(outside) {
		t.Fatal("ClearAll reached outside the capture root")
	}
	// A root that was never created is simply empty.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if n, err := ClearAll(); n != 0 || err != nil {
		t.Fatalf("missing root: ClearAll = %d, %v", n, err)
	}
}

func TestScanRecordingsJoinsLabelsAndManifests(t *testing.T) {
	root := newCaptureRoot(t)
	writeClip(t, root, "g1/a.wav")
	writeClip(t, root, "g1/b.wav")
	writeClip(t, root, "g1/"+captureManifestName)
	manifest := `{"file":"a.wav","text":"alfa","ts":"2026-01-01T00:00:01Z"}` + "\n" +
		`{"file":"b.wav","text":"brav","ts":"2026-01-01T00:00:02Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "g1", captureManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteCorpus([]CorpusEntry{{
		File: "g1/a.wav", Target: "alpha", Matched: true, Disposition: DispositionConfirmed, Ts: "2026-01-01T00:00:09Z",
	}}, "rewrite"); err != nil {
		t.Fatal(err)
	}

	got := ScanRecordings()
	want := []RecordingRow{
		{File: "g1/a.wav", Heard: "alfa", Target: "alpha", Matched: true, Disposition: DispositionConfirmed, Ts: "2026-01-01T00:00:09Z"},
		{File: "g1/b.wav", Heard: "brav", Ts: "2026-01-01T00:00:02Z"}, // unindexed: still listed
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanRecordings =\n%+v\nwant\n%+v", got, want)
	}
}

func TestExportableRows(t *testing.T) {
	rows := []CorpusEntry{
		{File: "a", Target: "alpha"},
		{File: "b", Target: "bravo", Disposition: DispositionConfirmed},
		{File: "c", Target: "charlie", Disposition: DispositionMisspoke},
		{File: "d"}, // unlabeled
	}
	files := func(rs []CorpusEntry) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.File)
		}
		return out
	}
	if got := files(ExportableRows(rows, false)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("all = %v", got)
	}
	if got := files(ExportableRows(rows, true)); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("confirmed only = %v", got)
	}
}

func TestBuildExportArchive(t *testing.T) {
	root := newCaptureRoot(t)
	writeClip(t, root, "g1/a.wav")
	if err := WriteCorpus([]CorpusEntry{
		{File: "g1/a.wav", Target: "Alpha"},
		{File: "g1/gone.wav", Target: "bravo"}, // WAV missing: skipped
		{File: "../escape.wav", Target: "x"},   // corrupt row: skipped
	}, "rewrite"); err != nil {
		t.Fatal(err)
	}

	name, n, err := BuildExportArchive(false)
	if err != nil || n != 1 {
		t.Fatalf("BuildExportArchive = %q, %d, %v", name, n, err)
	}
	path := filepath.Join(filepath.Dir(root), name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if !reflect.DeepEqual(names, []string{"clips/g1/a.wav", "manifest.jsonl"}) {
		t.Fatalf("archive holds %v", names)
	}

	// Closed before removing: Windows will not delete a file that is open.
	f.Close()
	RemoveExportArchive(name)
	if exists(path) {
		t.Fatal("RemoveExportArchive left the archive")
	}

	if _, _, err := BuildExportArchive(true); err == nil {
		t.Fatal("confirmed-only export with no confirmed clips must fail")
	}
}
