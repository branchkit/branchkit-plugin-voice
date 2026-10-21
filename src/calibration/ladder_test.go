package calibration

import (
	"math"
	"testing"
)

// step builds a single-seed ladder step.
func step(snr float64, decode string) LadderStep {
	return LadderStep{SNRdB: snr, Decodes: []string{decode}}
}

// stepN builds a multi-seed ladder step.
func stepN(snr float64, decodes ...string) LadderStep {
	return LadderStep{SNRdB: snr, Decodes: decodes}
}

// bat_quench: a stable per-constituent confuser (BAT→BACK across reps), QUENCH
// occasionally → BRANCH. Not mushy: BAT owns a consistent confuser.
func TestAnalyzeWord_StableConstituentConfuser(t *testing.T) {
	reps := []RepLadder{
		{Steps: []LadderStep{step(CleanSNR, "BAT QUENCH"), step(20, "BACK QUENCH"), step(15, "BACK BRANCH"), step(12, "BAT QUENCH")}},
		{Steps: []LadderStep{step(CleanSNR, "BAT QUENCH"), step(20, "BACK BRANCH"), step(15, "BACK BRANCH")}},
		{Steps: []LadderStep{step(CleanSNR, "BAT QUENCH"), step(20, "BAT QUENCH"), step(9, "BACK QUENCH")}},
	}
	got := AnalyzeWord("BAT QUENCH", reps)

	if len(got.Constituents) != 2 {
		t.Fatalf("want 2 constituents, got %d", len(got.Constituents))
	}
	bat := got.Constituents[0]
	if bat.Word != "BAT" || bat.NearestConfuser != "BACK" {
		t.Errorf("constituent[0] = %q→%q, want BAT→BACK", bat.Word, bat.NearestConfuser)
	}
	if bat.Consistency != 1.0 {
		t.Errorf("BAT confuser consistency = %.2f, want 1.0 (BACK every rep)", bat.Consistency)
	}
	if got.Constituents[1].NearestConfuser != "BRANCH" {
		t.Errorf("constituent[1] confuser = %q, want BRANCH", got.Constituents[1].NearestConfuser)
	}
	if got.Mushy {
		t.Error("BAT QUENCH should NOT be flagged mushy (it has a stable confuser)")
	}
	if got.Held {
		t.Error("BAT QUENCH fell under noise; Held should be false")
	}
}

// harp_pit: collapses to varying short words with shape breaks — no constituent
// owns a consistent confuser. The mushy / codeword-design red flag.
func TestAnalyzeWord_MushyCodeword(t *testing.T) {
	reps := []RepLadder{
		{Steps: []LadderStep{step(CleanSNR, "HARP PIT"), step(20, "PIT"), step(15, "HARP PIT"), step(9, "BAT"), step(6, "FOR"), step(3, "")}},
		{Steps: []LadderStep{step(CleanSNR, "HARP PIT"), step(20, "FOR PIT"), step(15, "FOR PIT"), step(12, "PIT")}},
		{Steps: []LadderStep{step(CleanSNR, "HARP PIT"), step(20, ""), step(15, "PIT"), step(12, "HARP"), step(9, "BAT")}},
	}
	got := AnalyzeWord("HARP PIT", reps)

	if !got.Mushy {
		t.Errorf("HARP PIT should be mushy; constituents=%+v", got.Constituents)
	}
	if got.Held {
		t.Error("HARP PIT fell; Held should be false")
	}
}

// cap_red: held the entire ladder in every rep — robust, never falls.
func TestAnalyzeWord_RobustHolds(t *testing.T) {
	rep := RepLadder{Steps: []LadderStep{
		step(CleanSNR, "CAP RED"), step(20, "CAP RED"), step(15, "CAP RED"),
		step(12, "CAP RED"), step(9, "CAP RED"), step(6, "CAP RED"), step(3, "CAP RED"),
	}}
	got := AnalyzeWord("CAP RED", []RepLadder{rep, rep, rep})

	if !got.Held {
		t.Errorf("CAP RED held every step; Held should be true, got %+v", got)
	}
	if got.HeldReps != 3 {
		t.Errorf("HeldReps = %d, want 3", got.HeldReps)
	}
	if !math.IsNaN(got.FallSNRdB) {
		t.Errorf("a held word has no fall SNR; want NaN, got %v", got.FallSNRdB)
	}
	if got.Mushy {
		t.Error("a robust word is not mushy")
	}
	if got.CleanDecode != "CAP RED" {
		t.Errorf("CleanDecode = %q, want the clean-step decode CAP RED", got.CleanDecode)
	}
}

// Multi-seed steps: a fall requires a MAJORITY of seeds to diverge, smoothing
// single-draw jitter.
func TestAnalyzeWord_MajorityAcrossSeeds(t *testing.T) {
	reps := []RepLadder{{Steps: []LadderStep{
		stepN(CleanSNR, "COPY", "COPY", "COPY"),
		stepN(20, "COPY", "COPY", "TAB"), // 1/3 diverge — not a fall
		stepN(12, "TAB", "TAB", "COPY"),  // 2/3 diverge — fall here
	}}}
	got := AnalyzeWord("COPY", reps)

	if got.Held {
		t.Error("COPY fell at snr12 (majority); Held should be false")
	}
	if got.FallSNRdB != 12 {
		t.Errorf("fall SNR = %v, want 12 (the majority-diverge step, not the 1/3 step)", got.FallSNRdB)
	}
	if got.Constituents[0].NearestConfuser != "TAB" {
		t.Errorf("COPY confuser = %q, want TAB", got.Constituents[0].NearestConfuser)
	}
}

// Ranking: held (robust) words sort last; the word that falls at the highest SNR
// (most fragile) leads.
func TestRankFragility_RobustSortsLast(t *testing.T) {
	held := WordFragility{Target: "CAP RED", Held: true}
	fragileEarly := WordFragility{Target: "HARP PIT", FallSNRdB: 20, Mushy: true}
	fragileLate := WordFragility{Target: "COPY", FallSNRdB: 6}

	ranked := RankFragility([]WordFragility{held, fragileLate, fragileEarly})

	if ranked[0].Target != "HARP PIT" {
		t.Errorf("most fragile (highest fall SNR) should lead; got %q", ranked[0].Target)
	}
	if ranked[len(ranked)-1].Target != "CAP RED" {
		t.Errorf("robust word should sort last; got %q", ranked[len(ranked)-1].Target)
	}
}
