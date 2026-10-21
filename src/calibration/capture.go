package calibration

import (
	"path/filepath"
	"strings"

	"github.com/branchkit/plugin-sdk-go"
)

// Trial-audio capture for the fragility ladder. When the user opts in (and the
// Sherpa offline engine is active), a calibration game points the command stage's
// `--dump-segments` at a per-game dir; the dumper writes each decoded segment's
// WAV + a manifest line. At game end we group the captured WAVs by their decoded
// text and run the perturbation ladder over them. Capture is OFF by default and
// the dir is set/cleared only around an active game, so the daily command pipeline
// is untouched unless the toggle is on.

// captureManifestLine is the subset of the dumper's manifest.jsonl this reads
// (stages/sherpa-commands/src/offline.rs SegmentDumper.dump writes the full line).
type captureManifestLine struct {
	File string `json:"file"`
	Text string `json:"text"`
	Ts   string `json:"ts"`
	// WordScores is the decode's per-word acoustic margins (engine posteriors),
	// aligned 1:1 with Text's words. Absent on pre-posteriors captures.
	WordScores []float64 `json:"word_scores,omitempty"`
}

// startCapture points the command stage at a fresh per-game capture dir, if the
// user enabled capture and the engine supports it. Must hold s.mu.
func (s *State) startCapture() {
	if s.CaptureEnabled == nil || s.SetCaptureDir == nil || !s.CaptureEnabled() {
		return
	}
	dir := filepath.Join(CaptureRoot(), NewID())
	if !s.SetCaptureDir(dir) {
		// The stage never got the dir, so nothing will be written. Say so
		// instead of announcing capture is on: this failed silently for weeks
		// (the record read was denied, the log line still said "enabled"), and
		// the only symptom was an empty corpus nobody looked at.
		branchkit.Logf("voice", "calibration: trial-audio capture REQUESTED BUT NOT ARMED — the command stage did not accept the capture dir, so no clips will be recorded")
		return
	}
	s.captureDir = dir
	branchkit.Logf("voice", "calibration: trial-audio capture enabled -> %s", dir)
}

// stopCapture clears the capture dir from the command stage (restoring the normal
// pipeline) and, when `analyze` is set, kicks off the fragility ladder over the
// captured audio in the background. Must hold s.mu.
func (s *State) stopCapture(analyze bool) {
	if s.captureDir == "" {
		return
	}
	dir := s.captureDir
	s.captureDir = ""
	if s.SetCaptureDir != nil {
		_ = s.SetCaptureDir("") // restore the command_recognition record
	}
	if !analyze {
		// Game abandoned/cancelled without finishing — its clips were never
		// labeled or indexed, so they'd leak. Reclaim that one game's dir via the
		// platform (the plugin can't delete the capture dir directly). Off the
		// game lock: this is an outbound RPC. Refresh the Recordings list after,
		// so the swept clips drop out of the live view.
		game := filepath.Base(dir)
		go func() {
			s.rpcSweepGame(game)
			if s.OnClipsChanged != nil {
				s.OnClipsChanged()
			}
			s.FlushRefresh()
		}()
		return
	}
	// Trial labeling windows (target + when it started) for the corpus index —
	// a clip is labeled by the trial that was active when it was recorded.
	windows := make([]captureWindow, 0, len(s.game.Results))
	for _, r := range s.game.Results {
		if len(r.Trial.Frames) == 0 {
			continue
		}
		windows = append(windows, captureWindow{Target: r.Trial.Target, Start: r.Trial.StartedAt})
	}
	engine := s.engineName()
	retention := s.retentionPolicy()
	// The ladder re-decode is a slow subprocess — run the whole finish off the game
	// lock so it never blocks gameplay or the RPC handler.
	go s.finishCaptureSession(dir, windows, engine, retention)
}

// retentionPolicy resolves the configured capture-retention policy.
//
// Every uncertain answer resolves to keep_all, because the alternatives DELETE
// the user's recordings and "I could not read the setting" is never a reason to
// destroy data. This used to default to keep_bounded — which prunes to the
// latest clip per word — so an unwired callback or a settings mirror that had
// not fetched yet silently pruned a curated corpus. On 2026-08-16 that cost
// nine confirmed clips: a game finished ~19s after a plugin restart, the mirror
// was not Ready, GetVoiceConfig returned the declared default (keep_bounded),
// and the finish path deleted every clip but the newest per target.
//
// Keeping too much is a disk-space problem the user can fix from the settings
// tab. Deleting too much is unrecoverable.
func (s *State) retentionPolicy() string {
	if s.RetentionPolicy == nil {
		return RetentionKeepAll
	}
	p, resolved := s.RetentionPolicy()
	if resolved && p != "" {
		return p
	}
	return RetentionKeepAll
}

// finishCaptureSession runs at game end (in its own goroutine): the fragility
// ladder, then the corpus index (labeled clips), then the retention policy. Order
// matters — fragility + corpus both need the WAVs, so retention deletes/prunes
// last.
//
// Reads and writes go straight to the capture store (capture_store.go) — the
// dir is ours, under $BRANCHKIT_PLUGIN_DATA. Labeling and retention policy stay
// here because they need this process's in-memory trial windows.
func (s *State) finishCaptureSession(dir string, windows []captureWindow, engine, retention string) {
	gameRel := filepath.Base(dir)
	lines := s.rpcReadManifest(gameRel)

	// One labeling pass: every clip labeled by its prompt (the corpus's invariant).
	// Fragility derives from the SAME rows — clips that decoded right run the noise
	// ladder; mishears are the max-fragile state and are recorded directly.
	rows := labelClips(gameRel, lines, windows, engine)
	s.analyzeFragilityFromRows(rows)

	switch retention {
	case RetentionDeleteAfter:
		// Privacy-first: keep the fragility readout (already persisted to the
		// fragility collection above), erase ALL of this game's audio — clips,
		// manifest, and the dir — not just the labeled rows. Index nothing.
		s.rpcSweepGame(gameRel)
	case RetentionKeepAll:
		s.rpcWriteCorpus(rows, "append")
	default: // keep_bounded
		s.rpcWriteCorpus(rows, "append")
		keep, drop := pruneBounded(s.rpcReadCorpus(), maxBoundedClips)
		if len(drop) > 0 {
			for _, e := range drop {
				s.rpcDeleteClip(e.File)
			}
			s.rpcWriteCorpus(keep, "rewrite")
		}
	}
}

// ReprobeCorpus re-runs the fragility ladder over already-kept clips — the whole
// corpus, or just one target when `target` is non-empty — and appends fresh
// readings to the per-clip history. It is the manual re-probe trigger behind the
// Recordings UI: history only accrues if old clips get re-measured (e.g. to ask,
// after a model swap, "did this finally start hearing right?"). The clip WAVs +
// corpus rows are the frozen capture facts; this reuses the SAME prompt-bucketed
// analysis as a walk finish, so a re-probe is indistinguishable from the original
// pass except for its model stamp + timestamp.
//
// Re-measuring is only meaningful when the clips still exist, so this operates on
// whatever the corpus holds: empty under delete-after retention (returns 0), the
// bounded recent set under keep_bounded, the full library under keep_all. The slow
// probe runs in its own goroutine; onDone fires after the history is written so the
// caller can refresh the view. Returns the clip count it will probe.
func (s *State) ReprobeCorpus(target string, onDone func()) int {
	rows := s.rpcReadCorpus()
	t := strings.ToLower(strings.TrimSpace(target))
	kept := make([]CorpusEntry, 0, len(rows))
	for _, r := range rows {
		// A clip the user marked "said it wrong" is misspoken audio — its fragility
		// reading doesn't measure the engine, so never (re-)probe it.
		if r.Disposition == DispositionMisspoke {
			continue
		}
		if t != "" && strings.ToLower(r.Target) != t {
			continue
		}
		kept = append(kept, r)
	}
	rows = kept
	if len(rows) == 0 {
		return 0
	}
	go func() {
		s.analyzeFragilityFromRows(rows)
		if onDone != nil {
			onDone()
		}
	}()
	return len(rows)
}

// DeleteClip removes one kept clip outright: the WAV (via the platform, which owns
// the capture dir the sandboxed plugin can't touch) plus its corpus index row, so
// the clip drops out of the recordings list and leaves no orphaned label. Mirrors
// the keep_bounded prune (delete the file, then rewrite the corpus without it). The
// clip's append-only fragility history is left as-is — it's a measurement log keyed
// by file, harmless once the clip is gone and never surfaced without a recording row.
func (s *State) DeleteClip(file string) {
	if file == "" {
		return
	}
	s.rpcDeleteClip(file)
	rows := s.rpcReadCorpus()
	kept := make([]CorpusEntry, 0, len(rows))
	for _, r := range rows {
		if r.File != file {
			kept = append(kept, r)
		}
	}
	if len(kept) != len(rows) {
		s.rpcWriteCorpus(kept, "rewrite")
	}
}

// DeleteTarget removes every kept clip for one target — the per-group bulk delete.
// Each WAV goes through the platform, then a single corpus rewrite drops all the
// target's rows. Returns the number of clips removed. Like DeleteClip, the clips'
// append-only fragility history is left as-is (harmless once the clips are gone).
func (s *State) DeleteTarget(target string) int {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return 0
	}
	rows := s.rpcReadCorpus()
	kept := make([]CorpusEntry, 0, len(rows))
	removed := 0
	for _, r := range rows {
		if strings.ToLower(r.Target) == t {
			s.rpcDeleteClip(r.File)
			removed++
			continue
		}
		kept = append(kept, r)
	}
	if removed > 0 {
		s.rpcWriteCorpus(kept, "rewrite")
	}
	return removed
}

// analyzeFragilityFromRows runs the fragility ladder over the labeled clips. Every
// clip is bucketed by its PROMPT (the corpus label), not its decode — so a clip
// the engine misheard lands under the word it was *meant* to be, the max-fragile
// state, rather than being silently dropped (the old decode-based grouping). Clips
// that decoded correctly are laddered; mishears are recorded as fails-clean. Runs
// in its own goroutine: touches no game state, only the set-once callbacks. The
// clip paths in `rows` are capture-root-relative — the platform resolves +
// confines them when it runs the probe.
func (s *State) analyzeFragilityFromRows(rows []CorpusEntry) {
	laddered, mishears := bucketByPrompt(rows)
	if len(laddered) == 0 && len(mishears) == 0 {
		return
	}
	analysis, err := s.RunFragilityAnalysis(laddered)
	if err != nil {
		// The probe is Sherpa-only; under any other engine (or a probe failure) we
		// record no clip history — a fails-clean stamp without a model version it
		// was measured against would be misleading.
		branchkit.Logf("voice", "calibration: fragility analysis failed: %v", err)
		return
	}
	model := analysis.ModelVersion
	if model == "" && len(mishears) > 0 {
		// A game with only mishears ran no probe, so the analysis carries no model
		// version. Fetch it directly (an empty-item probe just resolves + hashes the
		// model) so the deliberate-mishear case — its own game with no laddered clip
		// — still stamps the model its decode ran on.
		model = s.fetchModelVersion()
	}
	s.persistClipFragility(model, analysis.PerClip, mishears)
}

// bucketByPrompt splits labeled clips into the noise-ladder input (correctly
// decoded clips, grouped prompt→WAVs — the clean reps the ladder perturbs) and the
// mishears (clips whose clean decode differs from the prompt — already broken at
// zero noise, the max-fragile state). Pure.
func bucketByPrompt(rows []CorpusEntry) (laddered map[string][]string, mishears []CorpusEntry) {
	laddered = map[string][]string{}
	for _, r := range rows {
		if r.File == "" || r.Target == "" || r.Disposition == DispositionMisspoke {
			continue
		}
		if r.Matched {
			laddered[r.Target] = append(laddered[r.Target], r.File)
		} else {
			mishears = append(mishears, r)
		}
	}
	return laddered, mishears
}

// --- platform capture RPCs ---
//
// These wrap the capture store (capture_store.go), which is ordinary file I/O
// over this plugin's own data namespace. They used to be RPCs into the platform,
// because the capture dir was platform-named under app support and this process
// was denied it. The nil-safe shape is
// kept: tests construct State directly.

func (s *State) rpcReadManifest(game string) []captureManifestLine {
	return ReadManifest(game)
}

func (s *State) rpcReadCorpus() []CorpusEntry {
	return ReadCorpus()
}

func (s *State) rpcWriteCorpus(rows []CorpusEntry, mode string) {
	if err := WriteCorpus(rows, mode); err != nil {
		branchkit.Logf("voice", "calibration: write corpus(%s) failed: %v", mode, err)
	}
}

func (s *State) rpcDeleteClip(file string) {
	if err := DeleteClip(file); err != nil {
		branchkit.Logf("voice", "calibration: delete clip %s failed: %v", file, err)
	}
}

func (s *State) rpcSweepGame(game string) {
	if err := SweepGame(game); err != nil {
		branchkit.Logf("voice", "calibration: sweep(%s) failed: %v", game, err)
	}
}
