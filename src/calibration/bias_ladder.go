package calibration

import (
	"fmt"
	"sort"
	"strings"
)

// The bias-strength ladder is the noise-ladder idea applied to
// the never-standalone penalty instead of SNR: re-decode each captured clip across
// a ladder of penalty strengths and find the lowest strength at which it finally
// decodes to its prompt — the clip's flip threshold. Each recording then carries a
// per-person score ("no bias needed" / "flips at 4" / "never"), the data needed to
// tune the global penalty. Like ladder.go this file is PURE interpretation over the
// probe's decodes — no I/O — so it is table-testable.

// DefaultBiasLadder is the strength sweep calibration uses. 0 is the unbiased
// baseline; the rest climb past the typical live setting (≈8) up to where even a
// clean acoustic preference can be overcome (the spike flipped clean cases ≈30).
func DefaultBiasLadder() []float64 { return []float64{0, 2, 4, 6, 8, 12, 16, 24, 32} }

// BiasScore is one clip's strength-sweep readout. FlipAt is the lowest strength
// whose decode matched the prompt: nil = never matched in the ladder (the bias
// cannot save this clip in range); 0 = already correct unbiased (no bias needed);
// > 0 = the savable threshold. HoldsTo is the last rung of the contiguous
// correct run starting at FlipAt (nil when FlipAt is nil): a clip that stays
// correct through the top of the ladder holds to the top rung, while a clean
// clip that breaks at strength 16 holds to 12 — the per-clip ceiling the
// recommendation aggregates. Unbiased / Flipped carry the decode at strength 0
// and at the flip point, for a readable "four → page".
type BiasScore struct {
	Target   string
	File     string
	FlipAt   *float64
	HoldsTo  *float64
	Unbiased string
	Flipped  string
}

// AnalyzeBiasLadder re-decodes each captured clip across the strength ladder (via
// `call`) and computes its flip threshold. `clips` maps a prompt to its captured
// WAV paths (capture-root-relative; the platform resolves + confines them, and
// stamps the never-standalone word set at each requested strength). Returns the
// per-clip scores + the model stamp. Strength 0 is sent as an explicit
// `bias_strength` of 0 — which the platform treats as unbiased — so every rung
// rides the same biased path.
func (p *Prober) AnalyzeBiasLadder(call ProbeFunc, clips map[string][]string) ([]BiasScore, string, error) {
	ladder := p.BiasLadder
	if len(ladder) == 0 {
		ladder = DefaultBiasLadder()
	}

	type m struct {
		target, file string
		strength     float64
	}
	var items []probeItem
	meta := map[string]m{}
	id := 0
	for _, target := range sortedKeys(clips) {
		for _, wav := range clips[target] {
			for _, strength := range ladder {
				key := fmt.Sprintf("s%d", id)
				id++
				st := strength
				items = append(items, probeItem{ID: key, Audio: wav, BiasStrength: &st})
				meta[key] = m{target: target, file: wav, strength: strength}
			}
		}
	}
	if len(items) == 0 {
		return nil, "", nil
	}

	lines, model, err := call(items, p.MaxActive)
	if err != nil {
		return nil, "", err
	}

	// Reassemble: (target, file) -> strength -> decode.
	type clipKey struct{ target, file string }
	byClip := map[clipKey]map[float64]string{}
	for _, ln := range lines {
		mm, ok := meta[ln.ID]
		if !ok || ln.Error != "" {
			continue
		}
		k := clipKey{mm.target, mm.file}
		if byClip[k] == nil {
			byClip[k] = map[float64]string{}
		}
		byClip[k][mm.strength] = strings.ToUpper(strings.TrimSpace(ln.Text))
	}

	out := make([]BiasScore, 0, len(byClip))
	for _, target := range sortedKeys(clips) {
		for _, wav := range clips[target] {
			decodes, ok := byClip[clipKey{target, wav}]
			if !ok {
				continue
			}
			out = append(out, scoreClip(target, wav, ladder, decodes))
		}
	}
	return out, model, nil
}

// scoreClip folds one clip's per-strength decodes into a BiasScore: the lowest
// strength whose decode equals the prompt is the flip threshold, and the
// contiguous correct run from there gives the hold ceiling. Pure.
func scoreClip(target, file string, ladder []float64, decodes map[float64]string) BiasScore {
	want := strings.ToUpper(strings.TrimSpace(target))
	ordered := append([]float64(nil), ladder...)
	sort.Float64s(ordered)

	bs := BiasScore{Target: target, File: file}
	inRun := false
	for _, strength := range ordered {
		d, ok := decodes[strength]
		if !ok {
			continue
		}
		if strength == 0 {
			bs.Unbiased = d
		}
		if bs.FlipAt == nil && d == want {
			s := strength
			bs.FlipAt = &s
			bs.Flipped = d
			inRun = true
		}
		if inRun {
			if d == want {
				s := strength
				bs.HoldsTo = &s
			} else {
				inRun = false
			}
		}
	}
	return bs
}
