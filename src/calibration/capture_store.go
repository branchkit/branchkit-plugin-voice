package calibration

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// The capture store: ordinary file I/O over this plugin's own capture data.
//
// Every function here replaced an RPC. Nine of them, across
// `calibration.recordings.*` and `calibration.capture.*`, existed in the
// platform solely because this process could not open a directory the platform
// named `calibration-capture` under app support. The directory is
// `$BRANCHKIT_PLUGIN_DATA/calibration-capture` now — a namespace this plugin and
// the stages it ships share, and no other plugin can see — so reading it is
// `os.ReadDir`.
//
// The sherpa stage writes the WAVs and manifests here; this process reads and
// prunes them. That handoff is the reason the namespace is shared between a
// plugin and its stages rather than being per-process.

const (
	corpusIndexName     = "corpus.jsonl"
	captureManifestName = "manifest.jsonl"
)

// CaptureRoot is the directory holding captured WAVs + the corpus index.
//
// Empty when BRANCHKIT_PLUGIN_DATA is unset — i.e. not launched by the
// platform. Callers treat that as "capture unavailable" rather than guessing a
// path: a guessed path is one the sandbox denies, which surfaces as a
// permission error that reads exactly like a missing file.
func CaptureRoot() string {
	data := branchkit.PluginDataDir()
	if data == "" {
		return ""
	}
	return filepath.Join(data, captureDirName)
}

// DataRelPath turns a corpus-relative clip path into one relative to this
// plugin's DATA DIR — the spelling both the iframe media route and
// `recognition.redecode` take, since both are rooted at the data dir
// rather than at the capture subdirectory. Keeps the layout in one place
// instead of at each call site.
func DataRelPath(file string) string {
	return captureDirName + "/" + file
}

// clipPath resolves a corpus-relative clip path under the capture root,
// refusing anything that escapes it. The paths come from our own manifests, so
// this is a consistency check rather than a trust boundary — but a corrupt
// manifest should fail loudly rather than delete something outside the root.
//
// A path that cleans to the root itself ("." or "a/..") is refused too: every
// caller names something INSIDE the root, and SweepGame(".") used to
// RemoveAll the whole capture root, corpus index included.
func clipPath(root, rel string) (string, bool) {
	if root == "" || rel == "" {
		return "", false
	}
	clean := filepath.Clean(rel)
	if clean == "." || !filepath.IsLocal(clean) {
		return "", false
	}
	return filepath.Join(root, clean), true
}

// ReadManifest returns one game's capture manifest lines, in file order.
func ReadManifest(game string) []captureManifestLine {
	dir, ok := clipPath(CaptureRoot(), game)
	if !ok {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(dir, captureManifestName))
	if err != nil {
		return nil
	}
	var out []captureManifestLine
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row captureManifestLine
		if err := json.Unmarshal([]byte(line), &row); err == nil {
			out = append(out, row)
		}
	}
	return out
}

// ReadCorpus returns every labeled clip in the index, in file order.
func ReadCorpus() []CorpusEntry {
	root := CaptureRoot()
	if root == "" {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(root, corpusIndexName))
	if err != nil {
		return nil
	}
	var out []CorpusEntry
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row CorpusEntry
		if err := json.Unmarshal([]byte(line), &row); err == nil {
			out = append(out, row)
		}
	}
	return out
}

// WriteCorpus appends rows to the index, or replaces it entirely when
// mode is "rewrite" (the bounded-retention prune reads, drops, and rewrites).
func WriteCorpus(rows []CorpusEntry, mode string) error {
	root := CaptureRoot()
	if root == "" {
		return nil
	}
	if mode == "append" && len(rows) == 0 {
		return nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	var body strings.Builder
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		body.Write(b)
		body.WriteByte('\n')
	}
	path := filepath.Join(root, corpusIndexName)
	if mode == "rewrite" {
		return os.WriteFile(path, []byte(body.String()), 0o600)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(body.String())
	return err
}

// DeleteClip removes one captured WAV. Retention deletes route through here.
func DeleteClip(file string) error {
	abs, ok := clipPath(CaptureRoot(), file)
	if !ok {
		return nil
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetDisposition stamps (or, with "", clears) a clip's human review verdict on
// its corpus row. Only indexed clips can be marked — an unlabeled on-disk clip
// has no ground truth to curate against. Returns false when the clip is absent.
func SetDisposition(file, disposition string) (bool, error) {
	rows := ReadCorpus()
	found := false
	for i := range rows {
		if rows[i].File == file {
			rows[i].Disposition = disposition
			found = true
		}
	}
	if !found {
		return false, nil
	}
	return true, WriteCorpus(rows, "rewrite")
}

// SweepGame removes one game's directory outright — the cancelled- or
// abandoned-game path, where nothing was ever indexed.
func SweepGame(game string) error {
	dir, ok := clipPath(CaptureRoot(), game)
	if !ok || game == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SweepOrphans reclaims WAVs no corpus row references, plus game dirs left
// without any WAV. Labeled clips and the index survive.
func SweepOrphans() int {
	root := CaptureRoot()
	if root == "" {
		return 0
	}
	kept := map[string]bool{}
	for _, r := range ReadCorpus() {
		kept[r.File] = true
	}
	games, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, g := range games {
		if !g.IsDir() {
			continue
		}
		dir := filepath.Join(root, g.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		wavsLeft := 0
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
				continue
			}
			rel := g.Name() + "/" + e.Name()
			if kept[rel] {
				wavsLeft++
				continue
			}
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				removed++
			}
		}
		if wavsLeft == 0 {
			_ = os.RemoveAll(dir)
		}
	}
	return removed
}

// ClearAll wipes every captured WAV and the corpus index (the privacy action).
// Fragility records live elsewhere and are untouched.
func ClearAll() (int, error) {
	root := CaptureRoot()
	if root == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if e.IsDir() {
			if sub, err := os.ReadDir(path); err == nil {
				removed += len(sub)
			}
			_ = os.RemoveAll(path)
			continue
		}
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed, nil
}

// RecordingRow is one row of the Recordings list: every WAV actually on disk,
// joined with its corpus label when it has one.
type RecordingRow struct {
	File        string
	Target      string
	Heard       string
	Matched     bool
	Ts          string
	Disposition string
}

// ScanRecordings walks the capture dir and returns one row per WAV on disk,
// newest first.
//
// It scans the filesystem rather than the corpus index because clips from
// cancelled or abandoned games are never indexed (indexing only runs on a
// finished game), so an index-only read would make them invisible — and they
// are exactly the ones a user wants to find and clear.
func ScanRecordings() []RecordingRow {
	root := CaptureRoot()
	if root == "" {
		return nil
	}
	labels := map[string]CorpusEntry{}
	for _, r := range ReadCorpus() {
		labels[r.File] = r
	}
	games, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var rows []RecordingRow
	for _, g := range games {
		if !g.IsDir() {
			continue
		}
		manifest := map[string]captureManifestLine{}
		for _, m := range ReadManifest(g.Name()) {
			manifest[m.File] = m
		}
		entries, err := os.ReadDir(filepath.Join(root, g.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
				continue
			}
			rel := g.Name() + "/" + e.Name()
			row := RecordingRow{File: rel}
			if m, ok := manifest[e.Name()]; ok {
				row.Heard = m.Text
				row.Ts = m.Ts
			}
			if c, ok := labels[rel]; ok {
				row.Target = c.Target
				row.Matched = c.Matched
				row.Disposition = c.Disposition
				if c.Ts != "" {
					row.Ts = c.Ts
				}
			}
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ts != rows[j].Ts {
			return rows[i].Ts > rows[j].Ts
		}
		return rows[i].File > rows[j].File
	})
	return rows
}

// ExportableRows are the corpus rows worth sharing: labeled, and not a
// self-reported misspeak (audio that does not match its label would poison
// training). confirmedOnly narrows further to clips the user positively
// reviewed.
//
// A clip's label is the word prompted at capture time, frozen on the row —
// never re-derived from current vocabulary. A codeword rename is a binding
// change, not a relabel; retiring a word is an export-time filter here, never
// a per-clip mark.
func ExportableRows(rows []CorpusEntry, confirmedOnly bool) []CorpusEntry {
	var out []CorpusEntry
	for _, r := range rows {
		if r.Target == "" || r.Disposition == DispositionMisspoke {
			continue
		}
		if confirmedOnly && r.Disposition != DispositionConfirmed {
			continue
		}
		out = append(out, r)
	}
	return out
}

// BuildExportArchive writes a tar.gz of the exportable clips plus a NeMo-format
// `manifest.jsonl` into this plugin's data dir, and returns its data-dir-relative
// name and the clip count.
//
// Audio paths in the manifest are relative to the archive (`clips/<game>/<f>.wav`)
// so it is portable; `text` is the frozen prompt (the training label), with the
// curation fields carried along. A clip whose WAV has gone missing is skipped
// rather than aborting the whole export.
//
// The archive is built here, not in the platform, because the corpus schema and
// the training-manifest layout are this plugin's. Only the last hop — landing
// the file in the user's Downloads — is the platform's, because egress out of
// the sandbox is precisely what a plugin may not do for itself.
func BuildExportArchive(confirmedOnly bool) (string, int, error) {
	root := CaptureRoot()
	if root == "" {
		return "", 0, fmt.Errorf("capture storage unavailable")
	}
	kept := ExportableRows(ReadCorpus(), confirmedOnly)
	if len(kept) == 0 {
		return "", 0, fmt.Errorf("no exportable clips — record + keep some labeled clips first")
	}

	// Timestamped so each export is a distinct artifact rather than silently
	// overwriting the previous one.
	name := "branchkit-voice-clips-" + time.Now().Format("20060102-150405") + ".tar.gz"
	out, err := os.Create(filepath.Join(branchkit.PluginDataDir(), name))
	if err != nil {
		return "", 0, err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	var manifest strings.Builder
	count := 0
	for _, r := range kept {
		abs, ok := clipPath(root, r.File)
		if !ok {
			continue
		}
		body, err := os.ReadFile(abs)
		if err != nil {
			continue // missing/unreadable WAV — skip, don't abort
		}
		arcName := "clips/" + r.File
		if err := tw.WriteHeader(&tar.Header{
			Name: arcName, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			return "", 0, err
		}
		if _, err := tw.Write(body); err != nil {
			return "", 0, err
		}
		line, err := json.Marshal(map[string]any{
			"audio_filepath": arcName,
			"text":           strings.ToLower(r.Target),
			"target":         r.Target,
			"heard":          r.Heard,
			"matched":        r.Matched,
			"disposition":    r.Disposition,
			"engine":         r.Engine,
			"ts":             r.Ts,
			"word_scores":    r.WordScores,
		})
		if err != nil {
			continue
		}
		manifest.Write(line)
		manifest.WriteByte('\n')
		count++
	}
	if count == 0 {
		return "", 0, fmt.Errorf("no clip audio found to export")
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "manifest.jsonl", Mode: 0o644, Size: int64(manifest.Len()),
	}); err != nil {
		return "", 0, err
	}
	if _, err := tw.Write([]byte(manifest.String())); err != nil {
		return "", 0, err
	}
	return name, count, nil
}

// RemoveExportArchive deletes a staged archive from the data dir.
func RemoveExportArchive(name string) {
	if root := branchkit.PluginDataDir(); root != "" && name != "" {
		_ = os.Remove(filepath.Join(root, name))
	}
}
