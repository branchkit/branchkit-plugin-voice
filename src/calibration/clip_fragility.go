package calibration

import (
	"math"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Per-clip fragility history. Fragility is *measured per clip* (the probe
// re-decodes each clip across a noise ladder), so the per-clip reading is what we
// record; the per-word and per-target verdicts are rollups DERIVED from it at read
// time. clip_fragility is an append-only measurement stream keyed by clip file —
// each probe-run appends a new reading, so a re-probe (e.g. after a model swap)
// never overwrites and the corpus row stays frozen.

// ClipFragilityCollection is the append-only log of per-clip fragility readings
// (one entry per clip × probe-run). Declared in plugin.json.
//
// Never read this collection with nil ListLog opts: it grows as clips ×
// probe-runs and an unbounded read hits the stdio 1MB line cap (it already
// crashed this plugin once). Always pass Limit/Cursor.
const ClipFragilityCollection = "plugin.voice.clip_fragility"

// Clip outcomes.
const (
	ClipHeld       = "held"        // survived the whole noise ladder
	ClipFragile    = "fragile"     // decoded right clean, broke under noise
	ClipFailsClean = "fails_clean" // misheard at zero noise — the max-fragile state
)

// ClipFragilityRecord is one per-clip reading. A single clip folds one rep, so it
// carries no per-clip mushy/held-reps — those are multi-clip rollup fields (a
// single rep can't be mushy). The frozen prompt `Target` makes the row
// self-describing without joining the corpus.
type ClipFragilityRecord struct {
	ID      string `json:"id"`
	File    string `json:"file"`   // capture-root-relative; the per-clip history key
	Ts      string `json:"ts"`     // RFC3339, this probe-run
	Model   string `json:"model"`  // stable model version ("" when unknown)
	Target  string `json:"target"` // frozen prompt label
	Outcome string `json:"outcome"`
	// FallSNRdB is the SNR at which the clip broke — present only for "fragile".
	// A clip that breaks at the CLEAN step (a live match whose probe re-decode
	// diverges, fall = +∞) is recorded as "fails_clean", not "fragile", so this
	// never has to carry ∞.
	FallSNRdB *float64 `json:"fall_snr_db,omitempty"`
	// Heard is the clean mishear — present only for "fails_clean".
	Heard string `json:"heard,omitempty"`
	// Words are the per-constituent readings — laddered clips only.
	Words []ClipWordFragility `json:"words,omitempty"`
}

// ClipWordFragility is one word-position's reading within a clip.
type ClipWordFragility struct {
	Word     string `json:"word"`
	Position int    `json:"position"`
	// Confuser is the word this position slips to; "" means it held.
	Confuser string `json:"confuser,omitempty"`
}

// LatestClipPerFile keeps only the newest reading per clip file (the head of each
// clip's history), from a set of records that may span many probe-runs.
func LatestClipPerFile(records []ClipFragilityRecord) map[string]ClipFragilityRecord {
	latest := map[string]ClipFragilityRecord{}
	for _, r := range records {
		if cur, ok := latest[r.File]; !ok || r.Ts > cur.Ts {
			latest[r.File] = r
		}
	}
	return latest
}

// RollupClips derives a per-target WordFragility verdict from that target's latest
// per-clip readings — the denoised summary the per-target surfaces show. It mirrors
// AnalyzeWord's interpretation but over per-clip records (not raw ladders): each
// clip is one "rep", a fails_clean clip counts as fallen at clean (max fragile),
// and the per-constituent confuser is aggregated across clips. Pure.
func RollupClips(target string, clips []ClipFragilityRecord) WordFragility {
	tWords := targetWords(target)
	out := WordFragility{Target: target, Reps: len(clips), Constituents: make([]ConstituentFragility, len(tWords))}
	for i, w := range tWords {
		out.Constituents[i] = ConstituentFragility{Word: w, Position: i}
	}

	globalConfusers := make([]map[string]int, len(tWords))
	clipDominant := make([]map[int]string, len(tWords)) // position -> clip index -> its confuser
	for i := range tWords {
		globalConfusers[i] = map[string]int{}
		clipDominant[i] = map[int]string{}
	}

	var finiteFalls []float64
	fellCount, heldReps, failsCleanCount := 0, 0, 0
	for ci, c := range clips {
		switch c.Outcome {
		case ClipHeld:
			heldReps++
		case ClipFragile:
			fellCount++
			if c.FallSNRdB != nil {
				finiteFalls = append(finiteFalls, *c.FallSNRdB)
			}
		case ClipFailsClean:
			fellCount++ // broke at clean — fallen, no finite SNR
			failsCleanCount++
		}
		for _, w := range c.Words {
			if w.Confuser == "" || w.Position >= len(tWords) {
				continue
			}
			globalConfusers[w.Position][w.Confuser]++
			clipDominant[w.Position][ci] = w.Confuser
		}
	}

	maxConsistency := 0.0
	for i := range tWords {
		conf, count := modeOf(globalConfusers[i])
		out.Constituents[i].NearestConfuser = conf
		out.Constituents[i].ConfuserCount = count
		diverged := len(clipDominant[i])
		out.Constituents[i].RepsDiverged = diverged
		if diverged > 0 && conf != "" {
			agree := 0
			for _, d := range clipDominant[i] {
				if d == conf {
					agree++
				}
			}
			out.Constituents[i].Consistency = float64(agree) / float64(diverged)
			if diverged >= 2 && out.Constituents[i].Consistency > maxConsistency {
				maxConsistency = out.Constituents[i].Consistency
			}
		}
	}

	out.HeldReps = heldReps
	out.Held = heldReps == len(clips) && len(clips) > 0
	if len(finiteFalls) > 0 {
		sum := 0.0
		for _, f := range finiteFalls {
			sum += f
		}
		out.FallSNRdB = sum / float64(len(finiteFalls))
	} else {
		out.FallSNRdB = math.NaN()
	}
	// Mushy: enough clips fell to judge, yet no constituent owns a consistent
	// confuser — the "collapses to varying words" signature, same threshold as
	// AnalyzeWord.
	out.Mushy = fellCount >= 2 && maxConsistency < 0.6
	// Broken: every clip mishears even at clean — the engine never gets it.
	out.Broken = failsCleanCount == len(clips) && len(clips) > 0
	return out
}

// clipRecordFromLadder builds a record for a clip that ran the noise ladder:
// held → "held"; fell at a finite noise step → "fragile" + fall SNR; fell at the
// CLEAN step → "fails_clean" (matched live but the probe's un-perturbed re-decode
// diverged — the max-fragile state, same shape as a live mishear, with the clean
// decode as `heard`).
func clipRecordFromLadder(file, ts, model string, wf WordFragility) *ClipFragilityRecord {
	rec := &ClipFragilityRecord{
		ID:     NewID(),
		File:   file,
		Ts:     ts,
		Model:  model,
		Target: wf.Target,
	}
	switch {
	case wf.Held:
		rec.Outcome = ClipHeld
	case math.IsInf(wf.FallSNRdB, 1):
		// Fell at clean: fall SNR is +∞ (unserializable) and the divergence is
		// often length-mismatched (no per-constituent confuser). Record it as
		// fails-clean with what the probe heard, not a fall-less "fragile".
		rec.Outcome = ClipFailsClean
		rec.Heard = wf.CleanDecode
		return rec
	default:
		rec.Outcome = ClipFragile
		if !math.IsNaN(wf.FallSNRdB) {
			f := wf.FallSNRdB
			rec.FallSNRdB = &f
		}
	}
	for _, c := range wf.Constituents {
		rec.Words = append(rec.Words, ClipWordFragility{
			Word:     c.Word,
			Position: c.Position,
			Confuser: c.NearestConfuser,
		})
	}
	return rec
}

// persistClipFragility appends one reading per laddered clip + one per mishear to
// the append-only history, all stamped with the given model version. Every clip in
// the walk gets exactly one entry per probe-run — nothing is dropped silently.
// nil-safe when Append isn't wired.
func (s *State) persistClipFragility(model string, perClip map[string]WordFragility, mishears []CorpusEntry) {
	if s.Append == nil {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339)

	for file, wf := range perClip {
		if err := s.Append(ClipFragilityCollection, clipRecordFromLadder(file, ts, model, wf)); err != nil {
			branchkit.Logf("voice", "clip_fragility: append failed for %q: %v", file, err)
		}
	}
	for _, m := range mishears {
		if m.File == "" {
			continue
		}
		rec := &ClipFragilityRecord{
			ID:      NewID(),
			File:    m.File,
			Ts:      ts,
			Model:   model,
			Target:  m.Target,
			Outcome: ClipFailsClean,
			Heard:   m.Heard,
		}
		if err := s.Append(ClipFragilityCollection, rec); err != nil {
			branchkit.Logf("voice", "clip_fragility: append failed for mishear %q: %v", m.File, err)
		}
	}
}
