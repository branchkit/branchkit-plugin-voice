package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	"github.com/branchkit/branchkit-plugin-voice/compare"
	branchkit "github.com/branchkit/plugin-sdk-go"
)

// "Said just now" on the Models tab: the user says a
// phrase on the spot, every downloaded command model decodes its clip, the tab
// asks which was right, and the answer joins the confirmed recordings, so the
// next comparison counts it.
//
// While armed, a command phrase is swallowed and never run, as Get started's
// command check swallows one (setup_health.go): the phrase is said to test the
// models, not to act. Like that check, it listens for one phrase and then
// stops, so someone driving the computer by voice has their commands back as
// soon as the phrase is heard. The clip is captured the way calibration
// captures trial audio (the stage's dump-segments into a fresh folder under
// calibration-capture/), and is kept only if the user confirms it: everything
// else a session recorded, the folder's manifest included, is swept.

const (
	// sayNowWindow is how long Say a phrase listens.
	sayNowWindow = 2 * time.Minute
	// sayNowSettle is how long the clips of one phrase must stop arriving
	// before they are decoded: one hold can write several segments.
	sayNowSettle = 1500 * time.Millisecond
	// sayNowPoll is how often the capture folder's manifest is read.
	sayNowPoll = 500 * time.Millisecond
)

// sayNowClip is one captured phrase waiting for its answer.
type sayNowClip struct {
	File       string            // corpus-relative: "<game>/seg_N.wav"
	Audio      string            // relative to the plugin data dir
	Heard      map[string]string // model -> what it heard
	Live       string            // what the live stage decoded at capture
	Ts         string
	WordScores []float64
}

// sayNow is the feature's state, guarded by mu.
type sayNow struct {
	mu sync.Mutex
	// filesMu serializes everything that changes which recordings are kept:
	// an answer (taken from pending, then written to the corpus) and the
	// sweep. Without it a sweep running between the two could delete a clip
	// being confirmed. Taken before mu, never inside it.
	filesMu    sync.Mutex
	armedUntil time.Time
	watching   string // the capture folder a watcher still reads, or ""
	pending    []sayNowClip
	err        string
}

// disarm ends the listening window.
func (s *sayNow) disarm() {
	s.mu.Lock()
	s.armedUntil = time.Time{}
	s.mu.Unlock()
}

// intercepts reports whether a command transcript belongs to Say a phrase
// (swallowed, not run).
func (s *sayNow) intercepts(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.armedUntil)
}

// snapshot is the state the Models tab draws.
func (s *sayNow) snapshot(now time.Time) (armed bool, pending []sayNowClip, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.armedUntil), append([]sayNowClip(nil), s.pending...), s.err
}

// handleSayNowStart arms Say a phrase: a fresh capture folder for the command
// stage, and a window during which command phrases are swallowed.
func (h *Host) handleSayNowStart(_ *struct{}) error {
	if calibration.CaptureRoot() == "" {
		return fmt.Errorf("recordings are not available in this install")
	}
	if h.calib.IsActive() {
		return fmt.Errorf("finish the calibration game first: it is recording too")
	}
	// The user's recording settings apply: no capture at all, or audio
	// deleted after analysis, means a phrase cannot be kept to compare on.
	if !h.config.Get().CalibrationCapture {
		return fmt.Errorf("recording is turned off in Calibration's settings")
	}
	if policy, _ := h.config.CaptureRetentionResolved(); policy == calibration.RetentionDeleteAfter {
		return fmt.Errorf("your recordings are deleted after analysis, so a phrase can't be kept")
	}
	now := time.Now()
	h.sayNow.mu.Lock()
	if now.Before(h.sayNow.armedUntil) || h.sayNow.watching != "" {
		h.sayNow.mu.Unlock()
		return nil // already listening, or the last phrase is still decoding
	}
	game := "saynow-" + calibration.NewID()
	h.sayNow.armedUntil = now.Add(sayNowWindow)
	h.sayNow.watching = game
	h.sayNow.err = ""
	h.sayNow.mu.Unlock()
	if !h.claimCapture("saynow:"+game, filepath.Join(calibration.CaptureRoot(), game)) {
		h.sayNow.mu.Lock()
		h.sayNow.armedUntil = time.Time{}
		h.sayNow.watching = ""
		h.sayNow.err = "the command stage could not start recording; if a calibration game is running, finish it first"
		h.sayNow.mu.Unlock()
		return nil
	}
	go h.watchSayNow(game)
	return nil
}

// handleSayNowStop ends the window early.
func (h *Host) handleSayNowStop(_ *struct{}) error {
	h.sayNow.mu.Lock()
	h.sayNow.armedUntil = time.Time{}
	h.sayNow.mu.Unlock()
	return nil
}

// watchSayNow reads the capture folder's manifest while armed, decodes the
// phrase's clips with every downloaded command model once they settle, then
// stops listening (one phrase per session), turns its recording off and
// sweeps what nobody needs.
func (h *Host) watchSayNow(game string) {
	seen := map[string]bool{}
	var batch []sayNowClip
	var lastNew time.Time
	for {
		time.Sleep(sayNowPoll)
		now := time.Now()
		for _, l := range calibration.ReadManifest(game) {
			if seen[l.File] {
				continue
			}
			seen[l.File] = true
			file := path.Join(game, l.File)
			batch = append(batch, sayNowClip{File: file, Audio: calibration.DataRelPath(file), Live: l.Text, Ts: l.Ts, WordScores: l.WordScores})
			lastNew = now
		}
		if len(batch) > 0 && now.Sub(lastNew) >= sayNowSettle {
			h.sayNow.disarm() // the phrase is heard: commands run again
			h.decodeSayNow(batch)
			batch = nil
		}
		if !h.sayNow.intercepts(now) && len(batch) == 0 {
			break
		}
	}
	h.claimCapture("saynow:"+game, "")
	h.sayNow.mu.Lock()
	h.sayNow.watching = ""
	h.sayNow.mu.Unlock()
	h.sweepSayNow()
	_ = h.plugin.SettingsRefresh()
}

// decodeSayNow decodes clips with every downloaded command model and adds them
// to the pending answers. Their verdicts are not stored: with no target yet
// there is nothing to judge them by, and once confirmed the next comparison
// decodes them like any confirmed recording.
func (h *Host) decodeSayNow(clips []sayNowClip) {
	compareClips := make([]compare.Clip, len(clips))
	for i, c := range clips {
		compareClips[i] = compare.Clip{File: c.File, Audio: c.Audio}
		clips[i].Heard = map[string]string{}
	}
	var failed []string
	for _, m := range commandModels {
		if !modelOnDisk(m.Name) {
			continue
		}
		version, err := h.redecodeVersion(m.Name)
		var results []compare.Result
		if err == nil {
			results, err = h.redecodeClips(m.Name, version, "", compareClips)
		}
		if err != nil {
			failed = append(failed, m.Label)
			branchkit.Logf("voice", "say now: %s: %v", m.Name, err)
			continue
		}
		for i, r := range results {
			if r.Error == "" {
				clips[i].Heard[m.Name] = compare.Normalize(r.Heard)
			}
		}
	}
	h.sayNow.mu.Lock()
	h.sayNow.pending = append(h.sayNow.pending, clips...)
	if len(failed) > 0 {
		h.sayNow.err = "could not decode with " + strings.Join(failed, ", ")
	}
	h.sayNow.mu.Unlock()
	_ = h.plugin.SettingsRefresh()
}

type sayNowAnswer struct {
	File string `json:"file"`
	Text string `json:"text"`
}

// handleSayNowConfirm records what a pending phrase really said: the clip
// joins the confirmed recordings with that text as its target.
func (h *Host) handleSayNowConfirm(req *sayNowAnswer) error {
	target := compare.Normalize(req.Text)
	if target == "" {
		return fmt.Errorf("say what the phrase was")
	}
	h.sayNow.filesMu.Lock()
	defer h.sayNow.filesMu.Unlock()
	clip, ok := h.takeSayNow(req.File)
	if !ok {
		return nil // already answered
	}
	entry := calibration.CorpusEntry{
		File:        clip.File,
		Target:      target,
		Heard:       clip.Live,
		Matched:     compare.Right(clip.Live, target),
		Engine:      "sherpa-offline-nemo",
		Ts:          clip.Ts,
		WordScores:  clip.WordScores,
		Disposition: calibration.DispositionConfirmed,
	}
	if err := calibration.WriteCorpus([]calibration.CorpusEntry{entry}, "append"); err != nil {
		h.sayNow.mu.Lock()
		h.sayNow.pending = append(h.sayNow.pending, clip)
		h.sayNow.mu.Unlock()
		return err
	}
	h.invalidateRecordingsCache()
	h.sweepSayNowLocked()
	return nil
}

// handleSayNowDismiss drops a pending phrase and deletes its clip.
func (h *Host) handleSayNowDismiss(req *sayNowAnswer) error {
	h.sayNow.filesMu.Lock()
	defer h.sayNow.filesMu.Unlock()
	if _, ok := h.takeSayNow(req.File); !ok {
		return nil
	}
	h.sweepSayNowLocked()
	return nil
}

// takeSayNow removes the pending phrase for file, if there is one.
func (h *Host) takeSayNow(file string) (sayNowClip, bool) {
	h.sayNow.mu.Lock()
	defer h.sayNow.mu.Unlock()
	for i, c := range h.sayNow.pending {
		if c.File == file {
			h.sayNow.pending = append(h.sayNow.pending[:i], h.sayNow.pending[i+1:]...)
			return c, true
		}
	}
	return sayNowClip{}, false
}

// sweepSayNow deletes what Say a phrase recorded that nobody kept: in every
// saynow-* folder except the one a watcher still reads, each clip that is
// neither in the corpus (confirmed) nor waiting for its answer, and the
// manifest once no clip of that folder is waiting (it holds the decoded text
// of every phrase, discarded ones too). A folder left empty is removed. It
// runs when a session ends, after each answer, and at start for what a
// restart left behind.
func (h *Host) sweepSayNow() {
	h.sayNow.filesMu.Lock()
	defer h.sayNow.filesMu.Unlock()
	h.sweepSayNowLocked()
}

// sweepSayNowLocked is sweepSayNow for a caller holding filesMu.
func (h *Host) sweepSayNowLocked() {
	root := calibration.CaptureRoot()
	if root == "" {
		return
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return
	}
	indexed := map[string]bool{}
	for _, e := range calibration.ReadCorpus() {
		indexed[e.File] = true
	}
	h.sayNow.mu.Lock()
	watching := h.sayNow.watching
	waiting := map[string]bool{}
	for _, c := range h.sayNow.pending {
		waiting[c.File] = true
	}
	h.sayNow.mu.Unlock()
	for _, d := range dirs {
		game := d.Name()
		if !d.IsDir() || !strings.HasPrefix(game, "saynow-") || game == watching {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, game))
		if err != nil {
			continue
		}
		folderWaiting := false
		for _, f := range files {
			if waiting[path.Join(game, f.Name())] {
				folderWaiting = true
			}
		}
		left := 0
		for _, f := range files {
			rel := path.Join(game, f.Name())
			keep := indexed[rel] || waiting[rel] || (f.Name() == "manifest.jsonl" && folderWaiting)
			if keep {
				left++
				continue
			}
			if err := os.Remove(filepath.Join(root, game, f.Name())); err != nil {
				left++
			}
		}
		if left == 0 {
			_ = os.Remove(filepath.Join(root, game))
		}
	}
}
