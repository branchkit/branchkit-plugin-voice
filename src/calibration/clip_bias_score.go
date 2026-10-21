package calibration

import (
	"fmt"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Per-clip bias-score history. Like clip_fragility, the score is
// measured per clip — the probe sweeps each clip across a penalty-strength ladder
// (bias_ladder.go) and records the flip threshold — so this is an append-only
// measurement stream keyed by clip file: each scoring run appends a new reading,
// stamped with the model it ran on, so a re-score after a model swap never
// overwrites.

// BiasScoreCollection is the append-only log of per-clip bias scores (one entry per
// clip × scoring-run). Declared in plugin.json.
const BiasScoreCollection = "plugin.voice.clip_bias_score"

// ClipBiasRecord is one per-clip bias-score reading. FlipAt is the lowest penalty
// strength whose decode matched the prompt: nil = never (the bias can't save this
// clip in range), 0 = correct unbiased (no bias needed), > 0 = the savable
// threshold. HoldsTo is the top of the contiguous correct run starting at FlipAt
// (nil on never-scores and on pre-HoldsTo records) — the per-clip ceiling the
// strength recommendation aggregates. Unbiased / Flipped carry the strength-0
// decode and the flip-point decode for a readable "four → page". The frozen
// `Target` makes the row self-describing without joining the corpus.
type ClipBiasRecord struct {
	ID       string   `json:"id"`
	File     string   `json:"file"`   // capture-root-relative; the per-clip history key
	Ts       string   `json:"ts"`     // RFC3339, this scoring-run
	Model    string   `json:"model"`  // stable model version ("" when unknown)
	Target   string   `json:"target"` // frozen prompt label
	FlipAt   *float64 `json:"flip_at,omitempty"`
	HoldsTo  *float64 `json:"holds_to,omitempty"`
	Unbiased string   `json:"unbiased,omitempty"`
	Flipped  string   `json:"flipped,omitempty"`
}

// Label renders the one-line readout shown on the recordings row: "no bias needed",
// "flips at 6 (four→page)", or "never (four)".
func (r ClipBiasRecord) Label() string {
	switch {
	case r.FlipAt == nil:
		if r.Unbiased != "" {
			return fmt.Sprintf("never (%s)", strings.ToLower(r.Unbiased))
		}
		return "never"
	case *r.FlipAt == 0:
		return "no bias needed"
	default:
		return fmt.Sprintf("flips at %g (%s→%s)", *r.FlipAt, strings.ToLower(r.Unbiased), strings.ToLower(r.Flipped))
	}
}

// LatestBiasPerFile keeps only the newest reading per clip file (the head of each
// clip's bias-score history). Mirrors LatestClipPerFile.
func LatestBiasPerFile(records []ClipBiasRecord) map[string]ClipBiasRecord {
	latest := map[string]ClipBiasRecord{}
	for _, r := range records {
		if cur, ok := latest[r.File]; !ok || r.Ts > cur.Ts {
			latest[r.File] = r
		}
	}
	return latest
}

// ScoreBiasCorpus runs the bias strength sweep over already-kept clips — the
// whole corpus, or just one target when `target` is non-empty — and appends fresh
// per-clip scores. It is the manual "Score bias" trigger behind the Recordings UI.
// Clips marked "said it wrong" are excluded (their audio doesn't measure the
// engine). The slow sweep runs in its own goroutine; onDone fires after the scores
// are written so the caller can refresh the view. Returns the clip count it scored.
func (s *State) ScoreBiasCorpus(target string, onDone func()) int {
	rows := s.rpcReadCorpus()
	t := strings.ToLower(strings.TrimSpace(target))
	kept := make([]CorpusEntry, 0, len(rows))
	for _, r := range rows {
		if r.Disposition == DispositionMisspoke {
			continue
		}
		if t != "" && strings.ToLower(r.Target) != t {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return 0
	}
	go func() {
		s.analyzeBiasFromRows(kept)
		if onDone != nil {
			onDone()
		}
	}()
	return len(kept)
}

// analyzeBiasFromRows buckets the kept clips by their PROMPT and runs the strength
// sweep over all of them — both clips that decode right unbiased (score "no bias
// needed") and ones that mishear (find the flip threshold). Runs in its own
// goroutine: touches no game state, only the set-once callbacks.
func (s *State) analyzeBiasFromRows(rows []CorpusEntry) {
	clips := map[string][]string{}
	for _, r := range rows {
		if r.File == "" || r.Target == "" || r.Disposition == DispositionMisspoke {
			continue
		}
		clips[r.Target] = append(clips[r.Target], r.File)
	}
	if len(clips) == 0 {
		return
	}
	scores, model, err := s.RunBiasLadderAnalysis(clips)
	if err != nil {
		branchkit.Logf("voice", "calibration: bias-score analysis failed: %v", err)
		return
	}
	if model == "" {
		model = s.fetchModelVersion()
	}
	s.persistClipBiasScore(model, scores)
}

// RunBiasLadderAnalysis re-decodes the given clips across the penalty-strength
// ladder via the Sherpa probe and returns the per-clip scores + the model stamp.
// Returns an error (and scores nothing) under any non-Sherpa engine — the probe +
// the never-standalone bias are Sherpa-offline-only, same as fragility.
func (s *State) RunBiasLadderAnalysis(clips map[string][]string) ([]BiasScore, string, error) {
	if engine := s.engineName(); engine != SherpaOfflineEngine {
		return nil, "", fmt.Errorf("bias-score analysis needs the Sherpa offline engine (active: %q)", engine)
	}
	if len(clips) == 0 {
		return nil, "", nil
	}
	return NewProber().AnalyzeBiasLadder(s.probeViaPlatform, clips)
}

// persistClipBiasScore appends one reading per scored clip to the append-only
// history, stamped with the model version. nil-safe when Append isn't wired.
func (s *State) persistClipBiasScore(model string, scores []BiasScore) {
	if s.Append == nil {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	for _, sc := range scores {
		if sc.File == "" {
			continue
		}
		rec := &ClipBiasRecord{
			ID:       NewID(),
			File:     sc.File,
			Ts:       ts,
			Model:    model,
			Target:   sc.Target,
			FlipAt:   sc.FlipAt,
			HoldsTo:  sc.HoldsTo,
			Unbiased: sc.Unbiased,
			Flipped:  sc.Flipped,
		}
		if err := s.Append(BiasScoreCollection, rec); err != nil {
			branchkit.Logf("voice", "clip_bias_score: append failed for %q: %v", sc.File, err)
		}
	}
}
