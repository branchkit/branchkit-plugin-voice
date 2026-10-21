package calibration

import (
	"path"
	"sort"
	"strings"
	"time"
)

// The audio corpus: captured calibration clips labeled with the word you were
// asked to say. corpus.jsonl (one row per kept clip) is the (audio, ground-truth
// label) + context dataset a trainer or the future contribution program consumes.
// This file holds the pure data
// model + labeling/pruning logic; the capture dir lives in this plugin's own
// data namespace ($BRANCHKIT_PLUGIN_DATA/calibration-capture), and capture.go
// orchestrates it at game end.

// Retention policies — the CaptureRetention voice-config values.
const (
	RetentionDeleteAfter = "delete_after_analysis"
	RetentionKeepBounded = "keep_bounded"
	RetentionKeepAll     = "keep_all"

	// keep_bounded keeps the latest clip per word; this is a hard backstop on the
	// total kept regardless of how many distinct words have been calibrated.
	maxBoundedClips = 500

	captureDirName = "calibration-capture"
)

// CorpusEntry is one labeled clip — the dataset row. File is relative to the
// calibration-capture root (e.g. "<gameID>/seg_00007.wav").
type CorpusEntry struct {
	File    string `json:"file"`
	Target  string `json:"target"` // ground truth: the word shown
	Heard   string `json:"heard"`  // what the engine decoded for THIS clip
	Matched bool   `json:"matched"`
	Engine  string `json:"engine"`
	Ts      string `json:"ts"` // RFC3339, copied from the dump manifest
	// Disposition is the human review verdict (""/confirmed/misspoke), set via the
	// platform after listening to the clip. Carried here so the keep_bounded prune
	// (read → drop → rewrite) preserves marks the user already made on kept clips.
	Disposition string `json:"disposition,omitempty"`
	// WordScores is the capture decode's per-word acoustic margins (engine
	// posteriors), aligned 1:1 with Heard's words — the clip's ground-truth score
	// record. A labeled clip thus carries its margins, not just its decoded text,
	// so the genuine-margin distribution accumulates from calibration. Absent on
	// pre-posteriors clips or a decode whose readout didn't align.
	WordScores []float64 `json:"word_scores,omitempty"`
}

// Review verdicts a human stamps on a clip after listening (the axis the machine
// can't derive: did the recording faithfully say the prompt?). Orthogonal to the
// engine's matched flag + fragility verdict.
const (
	// DispositionConfirmed — "said it right": the recording faithfully says the
	// prompt. A vetted example; on a fragile clip, a high-value hard case. Counts
	// toward the verdict like an unreviewed clip — confirming only records review.
	DispositionConfirmed = "confirmed"
	// DispositionMisspoke — "said it wrong": the audio doesn't say the prompt, so
	// it's excluded from the fragility probe + per-target verdict (its reading
	// measures the wrong words, not the engine) and from training.
	DispositionMisspoke = "misspoke"
)

// captureWindow is a trial's labeling window: a clip written at-or-after Start
// (and before the next trial's Start) was spoken during this trial.
type captureWindow struct {
	Target string
	Start  time.Time
}

// labelClips assigns each captured clip to the trial that was active when it was
// written — the latest window whose Start <= the clip's ts — and emits a labeled
// row. Clips before the first window (clock skew / stray) are dropped. Every
// clip is labeled by its trial's target, so mishears are captured correctly
// (unlike the fragility path's decoded-text grouping). Pure.
func labelClips(gameRel string, clips []captureManifestLine, windows []captureWindow, engine string) []CorpusEntry {
	ws := append([]captureWindow(nil), windows...)
	sort.Slice(ws, func(i, j int) bool { return ws[i].Start.Before(ws[j].Start) })

	var out []CorpusEntry
	for _, c := range clips {
		if c.File == "" || c.Ts == "" {
			continue
		}
		ts, err := parseManifestTs(c.Ts)
		if err != nil {
			continue
		}
		w := windowFor(ws, ts)
		if w == nil {
			continue
		}
		out = append(out, CorpusEntry{
			// Forward-slash key (matches the platform's "<game>/<seg>" form) — not
			// filepath.Join, whose OS separator yields "game\seg.wav" on Windows.
			File:       path.Join(gameRel, c.File),
			Target:     w.Target,
			Heard:      c.Text,
			Matched:    strings.EqualFold(strings.TrimSpace(c.Text), strings.TrimSpace(w.Target)),
			Engine:     engine,
			Ts:         c.Ts,
			WordScores: c.WordScores,
		})
	}
	return out
}

// windowFor returns the latest window whose Start is not after ts (the trial in
// progress when the clip was written), or nil if ts precedes all windows.
func windowFor(sorted []captureWindow, ts time.Time) *captureWindow {
	var found *captureWindow
	for i := range sorted {
		if sorted[i].Start.After(ts) {
			break
		}
		found = &sorted[i]
	}
	return found
}

func parseManifestTs(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// pruneBounded splits entries into the ones to KEEP (the latest clip per target,
// capped at maxClips most-recent overall) and the rest to DROP. Pure.
func pruneBounded(entries []CorpusEntry, maxClips int) (keep, drop []CorpusEntry) {
	latest := map[string]CorpusEntry{}
	for _, e := range entries {
		k := strings.ToLower(e.Target)
		if cur, ok := latest[k]; !ok || e.Ts > cur.Ts {
			latest[k] = e
		}
	}
	kept := make([]CorpusEntry, 0, len(latest))
	for _, e := range latest {
		kept = append(kept, e)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Ts > kept[j].Ts })
	if len(kept) > maxClips {
		kept = kept[:maxClips]
	}
	keepFiles := make(map[string]bool, len(kept))
	for _, e := range kept {
		keepFiles[e.File] = true
	}
	for _, e := range entries {
		if keepFiles[e.File] {
			keep = append(keep, e)
		} else {
			drop = append(drop, e)
		}
	}
	return keep, drop
}
