package calibration

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// The Prober shapes a batch of re-decode requests over captured trial WAVs (the
// full field across a noise ladder, several seeds per step) and folds the NDJSON
// results into RepLadders for AnalyzeWord. The actual `sherpa_commands probe`
// subprocess runs in the PLATFORM (recognition.redecode), and not because of
// file access — we read our own clips directly now. Two things keep it there:
// the re-decode must see the LIVE grammar (the committed union and the
// never-standalone weights, both projections of the platform's own command/tag
// derivation, which this process cannot compute), and it execs, which plugins
// may not. All ladder shaping + analysis stays here.

// OfflineModelName is the default command model dir. The probe sends the model
// the live stage loads (State.CommandModel) as the `model` param, so its decodes
// match the live engine's; this is the fallback when that is not wired.
const OfflineModelName = "sherpa-offline-nemo"

// SherpaOfflineEngine is the command_engine id under which the ladder applies —
// the probe re-decode mechanism is Sherpa-offline-only.
const SherpaOfflineEngine = "sherpa-offline-nemo"

// ProbeStage is the registered recognizer stage whose `probe` subcommand the
// platform runs for the re-decode.
const ProbeStage = "voice.sherpa_commands"

// Prober configures a batch re-decode run — ladder shape only. The binary +
// model live in the platform now.
type Prober struct {
	SNRLadder    []float64 // noise steps, dB, noisiest implied last (descending)
	SeedsPerStep int       // decodes per noise step (>1 smooths single-draw jitter)
	MaxActive    int       // FST beam; 0 -> stage default
	BiasLadder   []float64 // L2 never-standalone penalty strengths (ascending, 0 first)
}

// ProbeFunc runs a batch of re-decode items and returns the NDJSON result lines
// plus the model version the platform stamped for this run (a content hash of the
// model — "" when unavailable). In production it forwards to the platform's
// recognition.redecode RPC; a test can supply one that spawns the binary
// directly.
type ProbeFunc func(items []probeItem, maxActive int) (lines []probeResultLine, modelVersion string, err error)

// DefaultSNRLadder is the perturbation ladder used by calibration. Descends from
// barely-perturbed to clearly-noisy.
func DefaultSNRLadder() []float64 { return []float64{20, 15, 12, 9, 6} }

// NewProber builds the default ladder config.
func NewProber() *Prober {
	return &Prober{
		SNRLadder:    DefaultSNRLadder(),
		SeedsPerStep: 3,
		MaxActive:    3000,
		BiasLadder:   DefaultBiasLadder(),
	}
}

// --- wire types (must mirror stages/sherpa-commands/src/probe.rs and the
// platform's recognition.redecode ProbeItem/ProbeLine) ---

type probeItem struct {
	ID    string      `json:"id"`
	Audio string      `json:"audio"` // capture-root-relative; the platform resolves + confines it
	Words []string    `json:"words,omitempty"`
	Noise *probeNoise `json:"noise,omitempty"`
	// ApplyBias makes the platform stamp the live never-standalone penalty
	// onto this item's grammar before decoding (the word set comes from the
	// platform, not here). Off = unbiased decode. Used to A/B a clip biased vs not.
	ApplyBias bool `json:"apply_bias,omitempty"`
	// BiasStrength requests the L2 strength sweep: the platform stamps the
	// never-standalone word set at exactly this cost (overrides ApplyBias). The word
	// set is still the platform's; only the cost is requested. nil = use ApplyBias.
	BiasStrength *float64 `json:"bias_strength,omitempty"`
}

type probeNoise struct {
	SNRdB float64 `json:"snr_db"`
	Seed  uint64  `json:"seed"`
}

type probeResultLine struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}

// itemMeta records what an item id stands for, so results reassemble into ladders
// without depending on output ordering.
type itemMeta struct {
	target string
	rep    int
	snr    float64 // CleanSNR for the un-perturbed decode
}

// LadderAnalysis is the two-level readout the ladder produces: the per-clip
// readings (the measured truth — one single-rep fold per recorded WAV, keyed by
// its capture-root-relative path) and the per-target rollup AnalyzeWord folds
// from all of a target's reps (ranked most-fragile first).
//
// A per-clip reading is a single-rep AnalyzeWord, so its Mushy is structurally
// always false (Mushy needs ≥2 fallen reps) and its cross-rep consistency
// collapses — mushiness is a target-level verdict only. Per-clip still carries a
// usable nearest confuser (the ladder's seeds give multiple perturbed draws of
// the one clip) and that clip's own fall SNR / held flag.
type LadderAnalysis struct {
	//deadfield:ignore staged bias analysis — the multi-rep rollup is
	// computed and covered by probe_test/probe_integration_test, and no
	// production path consumes it yet (persistClipFragility stores PerClip;
	// settings_render calls RankFragility on its own dataset). Kept rather
	// than deleted because it belongs to an open workstream and the call is
	// its owner's, not a sweep's. Delete it or wire it — do not leave it here
	// on the strength of this note alone.
	PerTarget    []WordFragility          // the denoised rollup, ranked most-fragile first
	PerClip      map[string]WordFragility // clip file path -> single-clip readout
	ModelVersion string                   // content version of the model these decodes ran on ("" if unknown)
}

// BiasEffect is the per-clip never-standalone counterfactual: the clip's decode
// with the live never-standalone penalty OFF vs ON. Changed is true when the bias flips the
// decode (the penalty mattered for this clip — "without bias this would've been
// Off; with it, On").
type BiasEffect struct {
	//deadfield:ignore identity for AnalyzeBias's batch return — that analyser
	// has no production caller yet (bias_live builds a single effect for a clip
	// it already knows), so these two are unread today. They are what makes a
	// []BiasEffect interpretable at all, which is why they stay: deleting them
	// would leave the batch shape unable to say which clip an effect describes.
	// Same open bias work as LadderAnalysis.PerTarget.
	Target string
	//deadfield:ignore same as Target above — AnalyzeBias's batch return needs
	// to say which clip each effect describes, and that analyser has no
	// production caller yet.
	File    string
	Off     string
	On      string
	Changed bool
}

// AnalyzeBias re-decodes each captured clip twice — penalty off and on — over the
// live union grammar (no noise) and reports where the never-standalone bias
// changes the decode. This is the clean A/B behind "did the bias help THIS
// command". The bias word set is the platform's (apply_bias just opts in), so when
// the live penalty is off both passes match and nothing is flagged. `clips` maps a
// prompt to its captured WAV paths. Returns the per-clip effects + the model stamp.
func (p *Prober) AnalyzeBias(call ProbeFunc, clips map[string][]string) ([]BiasEffect, string, error) {
	type m struct {
		target, file string
		on           bool
	}
	var items []probeItem
	meta := map[string]m{}
	id := 0
	for _, target := range sortedKeys(clips) {
		for _, wav := range clips[target] {
			for _, on := range []bool{false, true} {
				key := fmt.Sprintf("b%d", id)
				id++
				items = append(items, probeItem{ID: key, Audio: wav, ApplyBias: on})
				meta[key] = m{target: target, file: wav, on: on}
			}
		}
	}
	if len(items) == 0 {
		return nil, "", nil
	}
	lines, model, err := call(items, 0)
	if err != nil {
		return nil, "", err
	}
	type pair struct {
		off, on         string
		haveOff, haveOn bool
		target, file    string
	}
	byClip := map[string]*pair{}
	for _, ln := range lines {
		mm, ok := meta[ln.ID]
		if !ok {
			continue
		}
		k := mm.target + "\x00" + mm.file
		pp := byClip[k]
		if pp == nil {
			pp = &pair{target: mm.target, file: mm.file}
			byClip[k] = pp
		}
		if mm.on {
			pp.on, pp.haveOn = strings.ToUpper(strings.TrimSpace(ln.Text)), true
		} else {
			pp.off, pp.haveOff = strings.ToUpper(strings.TrimSpace(ln.Text)), true
		}
	}
	out := make([]BiasEffect, 0, len(byClip))
	for _, pp := range byClip {
		if !pp.haveOff || !pp.haveOn {
			continue
		}
		out = append(out, BiasEffect{
			Target:  pp.target,
			File:    pp.file,
			Off:     pp.off,
			On:      pp.on,
			Changed: pp.off != pp.on,
		})
	}
	return out, model, nil
}

// DecodeClipUnbiased re-decodes a single captured clip with the never-standalone
// penalty OFF over the live union grammar (no noise) and returns the uppercased
// decode plus the model stamp. It is the "off" side of the live Level-1
// counterfactual: the caller diffs it against the live biased result the pipeline
// already produced (one extra decode per attempt — cheap enough to run live). The
// decode is "" when the probe yields no line for the clip (a silent/failed hold).
func (p *Prober) DecodeClipUnbiased(call ProbeFunc, wav string) (decode, model string, err error) {
	lines, model, err := call([]probeItem{{ID: "u0", Audio: wav, ApplyBias: false}}, 0)
	if err != nil {
		return "", model, err
	}
	for _, ln := range lines {
		if ln.ID == "u0" {
			return strings.ToUpper(strings.TrimSpace(ln.Text)), model, nil
		}
	}
	return "", model, nil
}

// AnalyzeTargets re-decodes each target's rep WAVs across the noise ladder (via
// `call`) and returns both the per-target rollup (most-fragile first) and the
// per-clip readings keyed by file. `targets` maps a target word/codeword to its
// captured WAV paths (capture-root-relative; the platform resolves + confines
// them).
func (p *Prober) AnalyzeTargets(call ProbeFunc, targets map[string][]string) (LadderAnalysis, error) {
	var items []probeItem
	meta := map[string]itemMeta{}
	id := 0
	add := func(target string, rep int, wav string, snr float64, seed uint64) {
		key := fmt.Sprintf("i%d", id)
		id++
		item := probeItem{ID: key, Audio: wav}
		if !math.IsInf(snr, 1) {
			item.Noise = &probeNoise{SNRdB: snr, Seed: seed}
		}
		items = append(items, item)
		meta[key] = itemMeta{target: target, rep: rep, snr: snr}
	}

	// Deterministic order: targets sorted, reps in given order, clean then each
	// noise step × seed. Seeds are deterministic so a re-run reproduces the noise.
	for _, target := range sortedKeys(targets) {
		for rep, wav := range targets[target] {
			add(target, rep, wav, CleanSNR, 0)
			for si, snr := range p.SNRLadder {
				for seed := 0; seed < p.SeedsPerStep; seed++ {
					s := uint64((rep+1)*100000 + si*1000 + seed + 1)
					add(target, rep, wav, snr, s)
				}
			}
		}
	}
	if len(items) == 0 {
		return LadderAnalysis{}, nil
	}

	lines, modelVersion, err := call(items, p.MaxActive)
	if err != nil {
		return LadderAnalysis{}, err
	}

	// Reassemble: target -> rep -> snr -> []decodes.
	type repKey struct {
		target string
		rep    int
	}
	byRep := map[repKey]map[float64][]string{}
	for _, ln := range lines {
		m, ok := meta[ln.ID]
		if !ok || ln.Error != "" {
			continue
		}
		rk := repKey{m.target, m.rep}
		if byRep[rk] == nil {
			byRep[rk] = map[float64][]string{}
		}
		byRep[rk][m.snr] = append(byRep[rk][m.snr], ln.Text)
	}

	res := LadderAnalysis{PerClip: map[string]WordFragility{}, ModelVersion: modelVersion}
	var out []WordFragility
	for _, target := range sortedKeys(targets) {
		var reps []RepLadder
		for rep, wav := range targets[target] {
			steps := buildSteps(byRep[repKey{target, rep}], p.SNRLadder)
			if len(steps) == 0 {
				continue
			}
			ladder := RepLadder{Steps: steps}
			reps = append(reps, ladder)
			// Per-clip reading: fold THIS one rep into a single-clip readout keyed
			// by the actual WAV. Key off the loop's `wav`, not a `reps` index —
			// `reps` is compacted (steps-less reps are skipped) so its index no
			// longer aligns to the clip list. Mushy is always false at one rep.
			res.PerClip[wav] = AnalyzeWord(target, []RepLadder{ladder})
		}
		if len(reps) > 0 {
			out = append(out, AnalyzeWord(target, reps))
		}
	}
	res.PerTarget = RankFragility(out)
	return res, nil
}

// buildSteps orders a rep's decode buckets cleanest-first (clean, then each ladder
// SNR descending), dropping steps that produced no decodes.
func buildSteps(buckets map[float64][]string, ladder []float64) []LadderStep {
	if buckets == nil {
		return nil
	}
	var steps []LadderStep
	if d := buckets[CleanSNR]; len(d) > 0 {
		steps = append(steps, LadderStep{SNRdB: CleanSNR, Decodes: d})
	}
	ordered := append([]float64(nil), ladder...)
	sort.Sort(sort.Reverse(sort.Float64Slice(ordered)))
	for _, snr := range ordered {
		if d := buckets[snr]; len(d) > 0 {
			steps = append(steps, LadderStep{SNRdB: snr, Decodes: d})
		}
	}
	return steps
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
