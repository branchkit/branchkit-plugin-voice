package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	branchkit "github.com/branchkit/plugin-sdk-go"
)

// "Read a sentence" on the Models tab: the tab shows one of the
// fixed dictationSentences, the user dictates it, and the recording plus the
// sentence become a dictation recording the dictation comparison scores
// models on. The sentence is the ground truth, so nothing has to be labelled
// afterwards.
//
// While armed, the next dictation run is recorded (pipelines.run's
// record_to, into dictation-recordings/<session>/ in this plugin's data dir)
// and its text is swallowed, never typed: the sentence is read to test the
// models, not to write anywhere. One sentence per Read a sentence, as Say a
// phrase hears one phrase, so dictation works normally right after. A
// recording is kept only when the user keeps it; everything else is swept.

const (
	// dictationRecordingsDir holds the sessions' recordings and the corpus,
	// relative to the plugin's data dir.
	dictationRecordingsDir = "dictation-recordings"
	// dictationCorpusName is the kept recordings, one JSON line each.
	dictationCorpusName = "corpus.jsonl"
	// readSentenceWindow is how long Read a sentence waits for the dictation.
	readSentenceWindow = 2 * time.Minute
)

// dictationRecording is a kept recording: the clip and the sentence it reads.
type dictationRecording struct {
	File     string `json:"file"` // relative to the plugin's data dir
	Sentence string `json:"sentence"`
	Ts       string `json:"ts"`
}

// readClip is a recording waiting to be kept or discarded.
type readClip struct {
	File     string
	Sentence string
	Live     string // what live dictation heard, shown beside the sentence
}

// readSentence is the feature's state, guarded by mu. filesMu serializes
// keeping, discarding and sweeping, taken before mu.
type readSentence struct {
	filesMu    sync.Mutex
	mu         sync.Mutex
	armedUntil time.Time
	sentence   string
	session    string // the folder this arm records into
	recording  bool   // a dictation run started recording for this arm
	// recordedAt is when that run started. A run cancelled before any text
	// never clears recording, so a mark older than the window is stale:
	// a later, ordinary dictation must type as usual.
	recordedAt time.Time
	pending    []readClip
	err        string
}

// dictationDataDir is the recordings folder's absolute path, "" outside the
// platform.
func dictationDataDir() string {
	data := branchkit.PluginDataDir()
	if data == "" {
		return ""
	}
	return filepath.Join(data, dictationRecordingsDir)
}

// readDictationCorpus is every kept recording, oldest first.
func readDictationCorpus() []dictationRecording {
	dir := dictationDataDir()
	if dir == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, dictationCorpusName))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []dictationRecording
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r dictationRecording
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.File != "" && r.Sentence != "" {
			out = append(out, r)
		}
	}
	return out
}

// nextSentence is the sentence with the fewest kept recordings, the earliest
// in the set on a tie, so reading covers the whole set before repeating.
func nextSentence(kept []dictationRecording) string {
	count := map[string]int{}
	for _, r := range kept {
		count[r.Sentence]++
	}
	best := dictationSentences[0].Text
	for _, s := range dictationSentences {
		if count[s.Text] < count[best] {
			best = s.Text
		}
	}
	return best
}

// handleReadSentenceStart arms Read a sentence with the next sentence.
func (h *Host) handleReadSentenceStart(_ *struct{}) error {
	if dictationDataDir() == "" {
		return fmt.Errorf("recordings are not available in this install")
	}
	if !h.config.Get().CalibrationCapture {
		return fmt.Errorf("recording is turned off in Calibration's settings")
	}
	if policy, _ := h.config.CaptureRetentionResolved(); policy == calibration.RetentionDeleteAfter {
		return fmt.Errorf("your recordings are deleted after analysis, so a sentence can't be kept")
	}
	h.readSentence.mu.Lock()
	defer h.readSentence.mu.Unlock()
	if time.Now().Before(h.readSentence.armedUntil) {
		return nil
	}
	h.readSentence.armedUntil = time.Now().Add(readSentenceWindow)
	h.readSentence.sentence = nextSentence(readDictationCorpus())
	h.readSentence.session = path.Join(dictationRecordingsDir, "session-"+calibration.NewID())
	h.readSentence.recording = false
	h.readSentence.err = ""
	return nil
}

// handleReadSentenceStop disarms it.
func (h *Host) handleReadSentenceStop(_ *struct{}) error {
	h.readSentence.mu.Lock()
	h.readSentence.armedUntil = time.Time{}
	h.readSentence.mu.Unlock()
	return nil
}

// recordDirForRun is the folder a dictation run starting now records into,
// or "": armed, and no run has recorded for this arm yet. The arm ends here,
// so only this one run records.
func (r *readSentence) recordDirForRun(now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !now.Before(r.armedUntil) || r.recording {
		return ""
	}
	r.recording = true
	r.recordedAt = now
	r.armedUntil = time.Time{}
	return r.session
}

// clearRecording drops a recorded run's mark: the run never started, or
// ended without a transcript.
func (r *readSentence) clearRecording() {
	r.mu.Lock()
	r.recording = false
	r.mu.Unlock()
}

// takeRecordedTranscript reports whether a dictation transcript belongs to a
// recording run (swallowed, not typed), and returns the run's session and
// sentence.
func (r *readSentence) takeRecordedTranscript(now time.Time) (session, sentence string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recording {
		return "", "", false
	}
	r.recording = false
	if now.Sub(r.recordedAt) > readSentenceWindow {
		return "", "", false // a cancelled run's stale mark
	}
	return r.session, r.sentence, true
}

// collectReadRecording finds the recording a run wrote into session (the
// platform writes it when the audio stops, beside the transcript's decode)
// and offers it to keep or discard.
func (h *Host) collectReadRecording(session, sentence, live string) {
	data := branchkit.PluginDataDir()
	var file string
	for try := 0; try < 20 && file == ""; try++ {
		body, _ := os.ReadFile(filepath.Join(data, session, "manifest.jsonl"))
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var m struct {
				File string `json:"file"`
			}
			if json.Unmarshal([]byte(line), &m) == nil && m.File != "" {
				file = path.Join(session, m.File)
			}
		}
		if file == "" {
			time.Sleep(250 * time.Millisecond)
		}
	}
	h.readSentence.mu.Lock()
	if file == "" {
		h.readSentence.err = "the recording did not arrive; try again"
	} else {
		h.readSentence.pending = append(h.readSentence.pending, readClip{File: file, Sentence: sentence, Live: live})
	}
	h.readSentence.mu.Unlock()
	_ = h.plugin.SettingsRefresh()
}

type readAnswer struct {
	File string `json:"file"`
}

// handleReadSentenceKeep keeps a recording: it joins the dictation corpus.
func (h *Host) handleReadSentenceKeep(req *readAnswer) error {
	h.readSentence.filesMu.Lock()
	defer h.readSentence.filesMu.Unlock()
	clip, ok := h.takeReadClip(req.File)
	if !ok {
		return nil
	}
	line, _ := json.Marshal(dictationRecording{File: clip.File, Sentence: clip.Sentence, Ts: time.Now().UTC().Format(time.RFC3339)})
	f, err := os.OpenFile(filepath.Join(dictationDataDir(), dictationCorpusName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
		f.Close()
	}
	if err != nil {
		h.readSentence.mu.Lock()
		h.readSentence.pending = append(h.readSentence.pending, clip)
		h.readSentence.mu.Unlock()
		return err
	}
	h.sweepDictationRecordingsLocked()
	return nil
}

// handleReadSentenceDiscard drops a recording; the sweep deletes it.
func (h *Host) handleReadSentenceDiscard(req *readAnswer) error {
	h.readSentence.filesMu.Lock()
	defer h.readSentence.filesMu.Unlock()
	if _, ok := h.takeReadClip(req.File); !ok {
		return nil
	}
	h.sweepDictationRecordingsLocked()
	return nil
}

func (h *Host) takeReadClip(file string) (readClip, bool) {
	h.readSentence.mu.Lock()
	defer h.readSentence.mu.Unlock()
	for i, c := range h.readSentence.pending {
		if c.File == file {
			h.readSentence.pending = append(h.readSentence.pending[:i], h.readSentence.pending[i+1:]...)
			return c, true
		}
	}
	return readClip{}, false
}

// sweepDictationRecordings deletes every recording in a session folder that
// is neither kept (in the corpus) nor waiting for an answer, each manifest
// once nothing of its session waits, and emptied folders. It runs after each
// answer and at start, for what a restart left behind.
func (h *Host) sweepDictationRecordings() {
	h.readSentence.filesMu.Lock()
	defer h.readSentence.filesMu.Unlock()
	h.sweepDictationRecordingsLocked()
}

func (h *Host) sweepDictationRecordingsLocked() {
	root := dictationDataDir()
	if root == "" {
		return
	}
	sessions, err := os.ReadDir(root)
	if err != nil {
		return
	}
	kept := map[string]bool{}
	for _, r := range readDictationCorpus() {
		kept[r.File] = true
	}
	h.readSentence.mu.Lock()
	active := ""
	if h.readSentence.recording {
		active = path.Base(h.readSentence.session) // its run is still recording
	}
	waiting := map[string]bool{}
	for _, c := range h.readSentence.pending {
		waiting[c.File] = true
	}
	h.readSentence.mu.Unlock()
	for _, d := range sessions {
		name := d.Name()
		if !d.IsDir() || !strings.HasPrefix(name, "session-") || name == active {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			continue
		}
		sessionWaiting := false
		for _, f := range files {
			if waiting[path.Join(dictationRecordingsDir, name, f.Name())] {
				sessionWaiting = true
			}
		}
		left := 0
		for _, f := range files {
			rel := path.Join(dictationRecordingsDir, name, f.Name())
			if kept[rel] || waiting[rel] || (f.Name() == "manifest.jsonl" && sessionWaiting) {
				left++
				continue
			}
			if os.Remove(filepath.Join(root, name, f.Name())) != nil {
				left++
			}
		}
		if left == 0 {
			_ = os.Remove(filepath.Join(root, name))
		}
	}
}
