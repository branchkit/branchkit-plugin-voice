package calibration

import (
	"math"
	"sort"
	"strings"
)

// The perturbation/repetition ladder turns the stage `probe` decodes into a
// per-person word-quality readout. It is PURE interpretation — no I/O, no
// subprocess — so it is table-testable against hand-built ladders. The Prober
// (probe.go) produces the LadderStep data; this file reads off the signal the
// Sherpa spikes validated: per-constituent nearest confuser, cross-rep
// consistency, and a fragility rank.

// CleanSNR marks the un-perturbed step. Higher SNR = quieter = less perturbation,
// so a word that falls at a HIGHER SNR is MORE fragile (it broke while barely
// touched). Clean sits above every finite step.
var CleanSNR = math.Inf(1)

// LadderStep is one perturbation level for one repetition of a target: the
// full-field decodes across the seeds run at that level (≥1, more smooths the
// single-draw jitter the validation flagged). Steps within a RepLadder are
// ordered cleanest-first (descending SNR).
type LadderStep struct {
	SNRdB   float64
	Decodes []string
}

// RepLadder is one repetition's ordered ladder (clean → noisiest) of the same
// captured utterance re-decoded against the full field at escalating noise.
type RepLadder struct {
	Steps []LadderStep
}

// ConstituentFragility is the readout for one word position within a target
// (a single-word target has one constituent; a codeword like "BAT QUENCH" has
// two). NearestConfuser is the word this position most often slips to.
type ConstituentFragility struct {
	Word            string `json:"word"`
	Position        int    `json:"position"`
	NearestConfuser string `json:"nearest_confuser,omitempty"`
	ConfuserCount   int    `json:"confuser_count"`
	RepsDiverged    int    `json:"reps_diverged"`
	// Consistency in [0,1]: of the reps where this position slipped, the fraction
	// whose dominant slip equals NearestConfuser. A stable confuser (BAT→BACK 3/3)
	// scores 1.0 and is a reliable warning; a varying one scores low.
	Consistency float64 `json:"consistency"`
}

// WordFragility is the per-target ladder readout, the unit Reports renders and
// the runner persists.
type WordFragility struct {
	Target       string                 `json:"target"`
	Constituents []ConstituentFragility `json:"constituents"`
	Reps         int                    `json:"reps"`
	// FallSNRdB is the mean SNR at which the target first diverged across the reps
	// that fell (higher = more fragile). Meaningless when Held.
	FallSNRdB float64 `json:"fall_snr_db"`
	HeldReps  int     `json:"held_reps"`
	// Held is true when the target survived the whole ladder in every rep — a
	// robust word (e.g. CAP RED), the least-fragile bucket.
	Held bool `json:"held"`
	// Mushy is true when the word collapses to varying words with no dominant,
	// consistent confuser — the codeword-design red flag (e.g. HARP PIT). Largely
	// speaker-independent, so it doubles as a vocabulary-authoring signal. Shown to
	// the user as "unstable".
	Mushy bool `json:"mushy"`
	// Broken is true when EVERY rep/clip mishears even at zero noise — the engine
	// can't recognize the word at all (the worst state). Distinct from a fragile
	// word that decodes clean but breaks under noise.
	Broken bool `json:"broken"`
	// CleanDecode is the un-perturbed decode (the clean ladder step). Used by the
	// per-clip path to surface what a clip decoded to when it *fails at clean* on
	// re-probe — a clip can be matched live yet have its probe clean re-decode
	// diverge (different beam/grammar than the live pipeline). Only meaningful
	// per-clip (one rep); for a multi-rep rollup it's an arbitrary rep's.
	CleanDecode string `json:"clean_decode,omitempty"`
}

// AnalyzeWord folds a target's repetition ladders into a single readout.
func AnalyzeWord(target string, reps []RepLadder) WordFragility {
	tWords := targetWords(target)
	out := WordFragility{Target: target, Reps: len(reps), Constituents: make([]ConstituentFragility, len(tWords))}
	for i, w := range tWords {
		out.Constituents[i] = ConstituentFragility{Word: w, Position: i}
	}

	// Per-constituent tallies: global confuser counts + per-rep dominant confuser.
	globalConfusers := make([]map[string]int, len(tWords))
	repDominant := make([]map[int]string, len(tWords)) // position -> rep -> rep's dominant confuser
	for i := range tWords {
		globalConfusers[i] = map[string]int{}
		repDominant[i] = map[int]string{}
	}

	var fallSNRs []float64 // one per rep that fell
	heldReps := 0
	fellAtCleanReps := 0 // reps that diverged at the clean (no-noise) step

	for r, rep := range reps {
		repConfusers := make([]map[string]int, len(tWords))
		for i := range tWords {
			repConfusers[i] = map[string]int{}
		}
		fellSNR := math.NaN()
		fell := false

		// Steps cleanest-first: the first step whose decodes majority-diverge is
		// this rep's fall point. Tally per-position confusers across ALL steps so
		// a confuser seen anywhere in the ladder counts toward the nearest-confuser
		// estimate (more samples = steadier signal).
		for _, step := range rep.Steps {
			if math.IsInf(step.SNRdB, 1) && len(step.Decodes) > 0 {
				out.CleanDecode = step.Decodes[0] // clean step carries one decode
			}
			diverged := 0
			for _, d := range step.Decodes {
				if !strings.EqualFold(strings.TrimSpace(d), target) {
					diverged++
				}
				tallyConstituentConfusers(d, tWords, repConfusers)
			}
			if !fell && len(step.Decodes) > 0 && diverged*2 > len(step.Decodes) {
				fell = true
				fellSNR = step.SNRdB
			}
		}

		if fell {
			fallSNRs = append(fallSNRs, fellSNR)
			if math.IsInf(fellSNR, 1) {
				fellAtCleanReps++ // diverged with no noise added — broken at clean
			}
		} else {
			heldReps++
		}
		// Roll this rep's confusers into the global tally + record the rep's
		// dominant confuser per position (for the consistency measure).
		for i := range tWords {
			for w, c := range repConfusers[i] {
				globalConfusers[i][w] += c
			}
			if dom, _ := modeOf(repConfusers[i]); dom != "" {
				repDominant[i][r] = dom
			}
		}
	}

	maxConsistency := 0.0
	for i := range tWords {
		conf, count := modeOf(globalConfusers[i])
		out.Constituents[i].NearestConfuser = conf
		out.Constituents[i].ConfuserCount = count
		repsDiverged := len(repDominant[i])
		out.Constituents[i].RepsDiverged = repsDiverged
		if repsDiverged > 0 && conf != "" {
			agree := 0
			for _, dom := range repDominant[i] {
				if dom == conf {
					agree++
				}
			}
			out.Constituents[i].Consistency = float64(agree) / float64(repsDiverged)
			// A confuser only counts as "dominant + consistent" with ≥2 reps behind
			// it — a single rep's slip is too thin to call stable.
			if repsDiverged >= 2 && out.Constituents[i].Consistency > maxConsistency {
				maxConsistency = out.Constituents[i].Consistency
			}
		}
	}

	out.HeldReps = heldReps
	out.Held = heldReps == len(reps) && len(reps) > 0
	if len(fallSNRs) > 0 {
		sum := 0.0
		for _, s := range fallSNRs {
			sum += s
		}
		out.FallSNRdB = sum / float64(len(fallSNRs))
	} else {
		out.FallSNRdB = math.NaN()
	}
	// Mushy: it falls in enough reps to judge, yet no constituent owns a dominant,
	// consistent confuser. That's the "collapses to varying words" signature.
	out.Mushy = len(fallSNRs) >= 2 && maxConsistency < 0.6
	// Broken: every rep mishears even at zero noise — the engine never gets it.
	out.Broken = fellAtCleanReps == len(reps) && len(reps) > 0
	return out
}

// tallyConstituentConfusers credits each target position with the non-target word
// the decode put there, but ONLY for same-length decodes (positional alignment is
// trustworthy then). Length-mismatched decodes (a dropped/merged word, e.g.
// "HARP PIT"→"PIT") are a shape break — they're left out of per-constituent
// confusers (they'd misalign) and instead surface as fragility/mushiness via the
// fall + low-consistency path.
func tallyConstituentConfusers(decode string, tWords []string, repConfusers []map[string]int) {
	dWords := strings.Fields(strings.ToUpper(strings.TrimSpace(decode)))
	if len(dWords) != len(tWords) {
		return
	}
	for i, tw := range tWords {
		if dWords[i] != tw {
			repConfusers[i][dWords[i]]++
		}
	}
}

// RankFragility orders results most-fragile first: clean-failing and high-fall-SNR
// words lead, held (robust) words sort last. Mushy words break ties upward.
func RankFragility(results []WordFragility) []WordFragility {
	out := make([]WordFragility, len(results))
	copy(out, results)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Held != b.Held {
			return !a.Held // non-held (fragile) before held
		}
		if a.Held && b.Held {
			return false // both robust — preserve input order
		}
		af, bf := fallKey(a.FallSNRdB), fallKey(b.FallSNRdB)
		if af != bf {
			return af > bf // higher fall SNR = more fragile = first
		}
		if a.Mushy != b.Mushy {
			return a.Mushy
		}
		return false
	})
	return out
}

// fallKey maps a fall SNR to a sortable number, treating NaN (never fell) as the
// least fragile finite value.
func fallKey(snr float64) float64 {
	if math.IsNaN(snr) {
		return math.Inf(-1)
	}
	return snr
}

// modeOf returns the highest-count key and its count (empty/0 for an empty map).
// Ties break by lexicographic key so the result is deterministic.
func modeOf(counts map[string]int) (string, int) {
	best := ""
	bestN := 0
	for w, c := range counts {
		if c > bestN || (c == bestN && w < best) {
			best, bestN = w, c
		}
	}
	return best, bestN
}

// targetWords splits and uppercases a (possibly multi-word) target into its
// constituent words.
func targetWords(target string) []string {
	return strings.Fields(strings.ToUpper(strings.TrimSpace(target)))
}
