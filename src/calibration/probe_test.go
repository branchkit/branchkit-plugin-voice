package calibration

import (
	"strings"
	"testing"
)

// AnalyzeTargets emits a reading per CLIP (keyed by file) as well as the
// per-target rollup. Two clips of one target: one holds the ladder, one falls —
// the per-clip map must distinguish them, and the rollup must fold both.
func TestAnalyzeTargets_PerClipReadings(t *testing.T) {
	targets := map[string][]string{"BAT QUENCH": {"g/a.wav", "g/b.wav"}}

	// a.wav falls to BACK QUENCH once the noise reaches 9 dB; b.wav holds.
	call := func(items []probeItem, _ int) ([]probeResultLine, string, error) {
		out := make([]probeResultLine, 0, len(items))
		for _, it := range items {
			text := "BAT QUENCH"
			if strings.HasSuffix(it.Audio, "/a.wav") && it.Noise != nil && it.Noise.SNRdB <= 9 {
				text = "BACK QUENCH"
			}
			out = append(out, probeResultLine{ID: it.ID, Text: text})
		}
		return out, "test-model-v1", nil
	}

	got, err := NewProber().AnalyzeTargets(call, targets)
	if err != nil {
		t.Fatalf("AnalyzeTargets: %v", err)
	}

	if got.ModelVersion != "test-model-v1" {
		t.Errorf("ModelVersion = %q, want it threaded from the probe (test-model-v1)", got.ModelVersion)
	}

	if len(got.PerClip) != 2 {
		t.Fatalf("want a reading per clip (2), got %d: %v", len(got.PerClip), keysOf(got.PerClip))
	}

	a, ok := got.PerClip["g/a.wav"]
	if !ok {
		t.Fatal("missing per-clip reading for g/a.wav")
	}
	if a.Reps != 1 {
		t.Errorf("a.wav reading should fold exactly one rep, got Reps=%d", a.Reps)
	}
	if a.Held {
		t.Error("a.wav fell to BACK QUENCH under noise; Held should be false")
	}
	if a.FallSNRdB != 9 {
		t.Errorf("a.wav fall SNR = %v, want 9", a.FallSNRdB)
	}
	if a.Constituents[0].Word != "BAT" || a.Constituents[0].NearestConfuser != "BACK" {
		t.Errorf("a.wav constituent[0] = %q→%q, want BAT→BACK", a.Constituents[0].Word, a.Constituents[0].NearestConfuser)
	}
	if a.Mushy {
		t.Error("a single-clip reading can never be mushy (Mushy needs ≥2 fallen reps)")
	}

	b, ok := got.PerClip["g/b.wav"]
	if !ok {
		t.Fatal("missing per-clip reading for g/b.wav")
	}
	if !b.Held {
		t.Errorf("b.wav held the whole ladder; Held should be true, got %+v", b)
	}

	// Rollup folds both reps: one fell, so the target isn't held.
	if len(got.PerTarget) != 1 {
		t.Fatalf("want 1 per-target rollup, got %d", len(got.PerTarget))
	}
	rollup := got.PerTarget[0]
	if rollup.Target != "BAT QUENCH" {
		t.Errorf("rollup target = %q, want BAT QUENCH", rollup.Target)
	}
	if rollup.Held {
		t.Error("one of two reps fell; the rollup should not be Held")
	}
}

// A clip whose decodes all error out produces no ladder steps and is dropped from
// the compacted per-target reps slice. The per-clip map must still key the
// SURVIVING clips by their own files — a naive reps-index lookup would shift a
// later clip's reading onto the wrong file. Here the middle clip (1.wav) drops;
// 0.wav and 2.wav must keep their own correct readings.
func TestAnalyzeTargets_PerClipFileMappingSurvivesDroppedRep(t *testing.T) {
	targets := map[string][]string{"COPY": {"g/0.wav", "g/1.wav", "g/2.wav"}}

	call := func(items []probeItem, _ int) ([]probeResultLine, string, error) {
		out := make([]probeResultLine, 0, len(items))
		for _, it := range items {
			ln := probeResultLine{ID: it.ID}
			switch {
			case strings.HasSuffix(it.Audio, "/1.wav"):
				ln.Error = "boom" // this clip never decodes -> no steps -> dropped
			case strings.HasSuffix(it.Audio, "/2.wav"):
				if it.Noise != nil && it.Noise.SNRdB <= 12 {
					ln.Text = "TAB"
				} else {
					ln.Text = "COPY"
				}
			default: // 0.wav
				ln.Text = "COPY"
			}
			out = append(out, ln)
		}
		return out, "", nil
	}

	got, err := NewProber().AnalyzeTargets(call, targets)
	if err != nil {
		t.Fatalf("AnalyzeTargets: %v", err)
	}

	if len(got.PerClip) != 2 {
		t.Fatalf("want readings for the 2 surviving clips, got %d: %v", len(got.PerClip), keysOf(got.PerClip))
	}
	if _, ok := got.PerClip["g/1.wav"]; ok {
		t.Error("1.wav produced no decodes; it must not appear in the per-clip map")
	}
	if c0, ok := got.PerClip["g/0.wav"]; !ok || !c0.Held {
		t.Errorf("0.wav should hold; got ok=%v %+v", ok, c0)
	}
	c2, ok := got.PerClip["g/2.wav"]
	if !ok {
		t.Fatal("missing per-clip reading for g/2.wav")
	}
	if c2.FallSNRdB != 12 || c2.Held {
		t.Errorf("2.wav should fall at 12; got Held=%v fall=%v", c2.Held, c2.FallSNRdB)
	}
	if c2.Constituents[0].NearestConfuser != "TAB" {
		t.Errorf("2.wav confuser = %q, want TAB", c2.Constituents[0].NearestConfuser)
	}
}

func keysOf(m map[string]WordFragility) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// AnalyzeBias decodes each clip bias-off and bias-on; a flip (off != on) is the
// penalty changing the outcome for that clip.
func TestAnalyzeBias(t *testing.T) {
	p := NewProber()
	clips := map[string][]string{
		"page": {"g/page1.wav"}, // the bias flips four -> page
		"copy": {"g/copy1.wav"}, // unaffected
	}
	call := func(items []probeItem, _ int) ([]probeResultLine, string, error) {
		var out []probeResultLine
		for _, it := range items {
			text := ""
			switch {
			case strings.Contains(it.Audio, "page"):
				if it.ApplyBias {
					text = "page"
				} else {
					text = "four"
				}
			case strings.Contains(it.Audio, "copy"):
				text = "copy"
			}
			out = append(out, probeResultLine{ID: it.ID, Text: text})
		}
		return out, "modelX", nil
	}
	effects, model, err := p.AnalyzeBias(call, clips)
	if err != nil {
		t.Fatal(err)
	}
	if model != "modelX" {
		t.Errorf("model = %q, want modelX", model)
	}
	by := map[string]BiasEffect{}
	for _, e := range effects {
		by[e.Target] = e
	}
	if e := by["page"]; !e.Changed || e.Off != "FOUR" || e.On != "PAGE" {
		t.Errorf("page effect = %+v, want changed FOUR->PAGE", e)
	}
	if e := by["copy"]; e.Changed || e.On != "COPY" {
		t.Errorf("copy effect = %+v, want no change", e)
	}
}

// DecodeClipUnbiased decodes one clip with the penalty OFF (apply_bias never set)
// and returns the uppercased decode — the "off" side the live L1 card diffs
// against the pipeline's biased result.
func TestDecodeClipUnbiased(t *testing.T) {
	p := NewProber()
	var gotBias bool
	call := func(items []probeItem, _ int) ([]probeResultLine, string, error) {
		if len(items) != 1 {
			t.Fatalf("want 1 item, got %d", len(items))
		}
		gotBias = items[0].ApplyBias
		return []probeResultLine{{ID: items[0].ID, Text: "four"}}, "modelX", nil
	}
	decode, model, err := p.DecodeClipUnbiased(call, "g/page1.wav")
	if err != nil {
		t.Fatal(err)
	}
	if gotBias {
		t.Error("DecodeClipUnbiased must not opt into the bias")
	}
	if decode != "FOUR" {
		t.Errorf("decode = %q, want FOUR", decode)
	}
	if model != "modelX" {
		t.Errorf("model = %q, want modelX", model)
	}
}

// A probe that returns no line for the clip (silent/failed hold) yields an empty
// decode, not an error — the caller then shows no counterfactual.
func TestDecodeClipUnbiased_NoLine(t *testing.T) {
	p := NewProber()
	call := func(_ []probeItem, _ int) ([]probeResultLine, string, error) {
		return nil, "modelX", nil
	}
	decode, _, err := p.DecodeClipUnbiased(call, "g/silent.wav")
	if err != nil {
		t.Fatal(err)
	}
	if decode != "" {
		t.Errorf("decode = %q, want empty", decode)
	}
}
