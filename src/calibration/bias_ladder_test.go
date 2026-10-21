package calibration

import (
	"strings"
	"testing"
)

// fakeLadderDecoder returns, per clip (keyed by a substring of its audio path), the
// decode at a given strength: below the clip's flip strength it returns the wrong
// word, at/above it the prompt. A flip strength of 0 means always-correct; a very
// high one means never-flips within the ladder.
func biasCall(flip map[string]float64, wrong map[string]string, prompt map[string]string) ProbeFunc {
	return func(items []probeItem, _ int) ([]probeResultLine, string, error) {
		var out []probeResultLine
		for _, it := range items {
			var clip string
			for k := range flip {
				if strings.Contains(it.Audio, k) {
					clip = k
					break
				}
			}
			s := 0.0
			if it.BiasStrength != nil {
				s = *it.BiasStrength
			}
			text := wrong[clip]
			if s >= flip[clip] {
				text = prompt[clip]
			}
			out = append(out, probeResultLine{ID: it.ID, Text: text})
		}
		return out, "modelB", nil
	}
}

func TestAnalyzeBiasLadder(t *testing.T) {
	p := NewProber()
	p.BiasLadder = []float64{0, 2, 4, 6, 8}
	clips := map[string][]string{
		"page": {"g/page.wav"}, // flips at 6
		"copy": {"g/copy.wav"}, // already correct unbiased (flip 0)
		"pair": {"g/pair.wav"}, // never flips in range
	}
	call := biasCall(
		map[string]float64{"page.wav": 6, "copy.wav": 0, "pair.wav": 999},
		map[string]string{"page.wav": "four", "pair.wav": "bear"},
		map[string]string{"page.wav": "page", "copy.wav": "copy", "pair.wav": "pair"},
	)
	scores, model, err := p.AnalyzeBiasLadder(call, clips)
	if err != nil {
		t.Fatal(err)
	}
	if model != "modelB" {
		t.Errorf("model = %q, want modelB", model)
	}
	by := map[string]BiasScore{}
	for _, s := range scores {
		by[s.Target] = s
	}

	// page: flips at 6, unbiased FOUR, flipped PAGE; correct through the top.
	if e := by["page"]; e.FlipAt == nil || *e.FlipAt != 6 {
		t.Errorf("page FlipAt = %v, want 6", e.FlipAt)
	} else if e.Unbiased != "FOUR" || e.Flipped != "PAGE" {
		t.Errorf("page decodes = %q -> %q, want FOUR -> PAGE", e.Unbiased, e.Flipped)
	} else if e.HoldsTo == nil || *e.HoldsTo != 8 {
		t.Errorf("page HoldsTo = %v, want 8 (top of ladder)", e.HoldsTo)
	}
	// copy: no bias needed (flip 0), holds through the top.
	if e := by["copy"]; e.FlipAt == nil || *e.FlipAt != 0 {
		t.Errorf("copy FlipAt = %v, want 0 (no bias needed)", e.FlipAt)
	} else if e.HoldsTo == nil || *e.HoldsTo != 8 {
		t.Errorf("copy HoldsTo = %v, want 8", e.HoldsTo)
	}
	// pair: never flips in the ladder — no flip, no hold ceiling.
	if e := by["pair"]; e.FlipAt != nil || e.HoldsTo != nil {
		t.Errorf("pair FlipAt/HoldsTo = %v/%v, want nil/nil (never)", e.FlipAt, e.HoldsTo)
	}
}

// A clip that is correct unbiased but regresses at high strength holds only to
// the last rung before the break — the per-clip ceiling the recommendation uses.
func TestScoreClip_HoldCeilingOnRegression(t *testing.T) {
	ladder := []float64{0, 2, 4, 6, 8}
	decodes := map[float64]string{0: "COPY", 2: "COPY", 4: "COPY", 6: "CROP", 8: "CROP"}
	bs := scoreClip("copy", "g/copy.wav", ladder, decodes)
	if bs.FlipAt == nil || *bs.FlipAt != 0 {
		t.Fatalf("FlipAt = %v, want 0", bs.FlipAt)
	}
	if bs.HoldsTo == nil || *bs.HoldsTo != 4 {
		t.Errorf("HoldsTo = %v, want 4 (breaks at 6)", bs.HoldsTo)
	}
	// A later re-match above the break does not extend the ceiling: the run is
	// contiguous from the flip point.
	decodes[8] = "COPY"
	bs = scoreClip("copy", "g/copy.wav", ladder, decodes)
	if bs.HoldsTo == nil || *bs.HoldsTo != 4 {
		t.Errorf("HoldsTo = %v, want 4 (non-contiguous re-match ignored)", bs.HoldsTo)
	}
}

// A dropped strength rung (probe error) is skipped, not treated as a flip.
func TestAnalyzeBiasLadder_SkipsErrors(t *testing.T) {
	p := NewProber()
	p.BiasLadder = []float64{0, 4}
	clips := map[string][]string{"page": {"g/page.wav"}}
	call := func(items []probeItem, _ int) ([]probeResultLine, string, error) {
		var out []probeResultLine
		for _, it := range items {
			// Error out the strength-4 rung; baseline stays FOUR.
			if it.BiasStrength != nil && *it.BiasStrength == 4 {
				out = append(out, probeResultLine{ID: it.ID, Error: "boom"})
				continue
			}
			out = append(out, probeResultLine{ID: it.ID, Text: "four"})
		}
		return out, "m", nil
	}
	scores, _, err := p.AnalyzeBiasLadder(call, clips)
	if err != nil {
		t.Fatal(err)
	}
	// FlipAt nil is what "the errored rung was dropped" now looks like from the
	// outside: the erroring rung is the one whose strength could have flipped
	// this clip, so counting it would have produced a FlipAt. This used to
	// assert on len(Steps) as well — the per-rung trail was built for every
	// clip and persisted nowhere, so it went.
	if len(scores) != 1 || scores[0].FlipAt != nil {
		t.Errorf("scores = %+v, want one clip, never-flips", scores)
	}
}
