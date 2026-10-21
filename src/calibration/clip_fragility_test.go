package calibration

import (
	"math"
	"testing"
)

func TestPersistClipFragility(t *testing.T) {
	s := NewState()

	var got []*ClipFragilityRecord
	s.Append = func(collection string, payload any) error {
		if collection != ClipFragilityCollection {
			return nil
		}
		got = append(got, payload.(*ClipFragilityRecord))
		return nil
	}

	held := WordFragility{
		Target: "cap red", Held: true, FallSNRdB: math.NaN(),
		Constituents: []ConstituentFragility{{Word: "CAP", Position: 0}, {Word: "RED", Position: 1}},
	}
	fragile := WordFragility{
		Target: "bat quench", Held: false, FallSNRdB: 9,
		Constituents: []ConstituentFragility{
			{Word: "BAT", Position: 0, NearestConfuser: "BACK"},
			{Word: "QUENCH", Position: 1},
		},
	}
	perClip := map[string]WordFragility{"g/held.wav": held, "g/frag.wav": fragile}
	mishears := []CorpusEntry{{File: "g/miss.wav", Target: "bat quench", Heard: "BACK QUENCH"}}

	s.persistClipFragility("modelv1", perClip, mishears)

	if len(got) != 3 {
		t.Fatalf("want 3 appended records (held + fragile + mishear), got %d", len(got))
	}
	byFile := map[string]*ClipFragilityRecord{}
	for _, r := range got {
		byFile[r.File] = r
		if r.Ts == "" || r.Model != "modelv1" {
			t.Errorf("%s: ts/model not stamped: ts=%q model=%q", r.File, r.Ts, r.Model)
		}
	}

	h := byFile["g/held.wav"]
	if h.Outcome != ClipHeld || h.FallSNRdB != nil {
		t.Errorf("held clip: outcome=%q fall=%v, want held/nil", h.Outcome, h.FallSNRdB)
	}

	f := byFile["g/frag.wav"]
	if f.Outcome != ClipFragile {
		t.Errorf("fragile clip outcome = %q, want fragile", f.Outcome)
	}
	if f.FallSNRdB == nil || *f.FallSNRdB != 9 {
		t.Errorf("fragile clip fall = %v, want 9", f.FallSNRdB)
	}
	if len(f.Words) != 2 || f.Words[0].Word != "BAT" || f.Words[0].Confuser != "BACK" {
		t.Errorf("fragile clip words = %+v, want BAT->BACK at pos 0", f.Words)
	}
	if f.Words[1].Confuser != "" {
		t.Errorf("QUENCH held; confuser should be empty, got %q", f.Words[1].Confuser)
	}

	m := byFile["g/miss.wav"]
	if m.Outcome != ClipFailsClean || m.Heard != "BACK QUENCH" || m.FallSNRdB != nil || len(m.Words) != 0 {
		t.Errorf("mishear record = %+v, want fails_clean -> BACK QUENCH with no ladder", m)
	}
	if m.Target != "bat quench" {
		t.Errorf("mishear target = %q, want the frozen prompt 'bat quench'", m.Target)
	}
}

// A laddered clip can be matched live yet have its probe clean re-decode diverge —
// it "falls" at the clean step (fall SNR = +∞). That must record as fails_clean
// (with the clean decode as heard), NOT a fall-less "fragile" entry.
func fp(v float64) *float64 { return &v }

// RollupClips derives the per-target verdict from per-clip readings: all-held ->
// held; a stable cross-clip confuser -> fragile (not mushy); fails_clean clips
// count as fallen.
func TestRollupClips(t *testing.T) {
	t.Run("all held", func(t *testing.T) {
		clips := []ClipFragilityRecord{
			{File: "a", Outcome: ClipHeld, Words: []ClipWordFragility{{Word: "CAP", Position: 0}}},
			{File: "b", Outcome: ClipHeld, Words: []ClipWordFragility{{Word: "CAP", Position: 0}}},
		}
		got := RollupClips("cap", clips)
		if !got.Held || got.HeldReps != 2 || got.Reps != 2 {
			t.Errorf("all-held rollup = %+v, want Held with 2/2", got)
		}
		if !math.IsNaN(got.FallSNRdB) || got.Mushy {
			t.Errorf("held target has no fall and isn't mushy, got fall=%v mushy=%v", got.FallSNRdB, got.Mushy)
		}
	})

	t.Run("stable confuser is fragile not mushy", func(t *testing.T) {
		clips := []ClipFragilityRecord{
			{File: "a", Outcome: ClipFragile, FallSNRdB: fp(9), Words: []ClipWordFragility{{Word: "BAT", Position: 0, Confuser: "BACK"}, {Word: "QUENCH", Position: 1}}},
			{File: "b", Outcome: ClipFragile, FallSNRdB: fp(9), Words: []ClipWordFragility{{Word: "BAT", Position: 0, Confuser: "BACK"}, {Word: "QUENCH", Position: 1}}},
		}
		got := RollupClips("bat quench", clips)
		if got.Held || got.FallSNRdB != 9 {
			t.Errorf("rollup fall = %v held=%v, want 9 / not held", got.FallSNRdB, got.Held)
		}
		if got.Constituents[0].NearestConfuser != "BACK" || got.Constituents[0].RepsDiverged != 2 {
			t.Errorf("BAT constituent = %+v, want BACK across 2 clips", got.Constituents[0])
		}
		if got.Mushy {
			t.Error("a stable BAT->BACK confuser across clips is not mushy")
		}
	})

	t.Run("fails_clean counts as fallen and feeds mushy", func(t *testing.T) {
		clips := []ClipFragilityRecord{
			{File: "a", Outcome: ClipHeld, Words: []ClipWordFragility{{Word: "BAT", Position: 0}, {Word: "QUENCH", Position: 1}}},
			{File: "b", Outcome: ClipFragile, FallSNRdB: fp(9), Words: []ClipWordFragility{{Word: "BAT", Position: 0, Confuser: "BACK"}, {Word: "QUENCH", Position: 1}}},
			{File: "c", Outcome: ClipFailsClean, Heard: "FOR PIT"}, // no per-word; whole-string mishear
		}
		got := RollupClips("bat quench", clips)
		if got.HeldReps != 1 || got.Held {
			t.Errorf("HeldReps=%d Held=%v, want 1 / not held", got.HeldReps, got.Held)
		}
		if got.FallSNRdB != 9 {
			t.Errorf("fall = %v (mean of finite falls only), want 9", got.FallSNRdB)
		}
		if !got.Mushy {
			t.Error("2 clips fell with no >=2-clip consistent confuser -> mushy")
		}
		if got.Broken {
			t.Error("a mix of held/fragile/fails-clean is not fully broken")
		}
	})

	t.Run("all fails_clean is broken", func(t *testing.T) {
		clips := []ClipFragilityRecord{
			{File: "a", Outcome: ClipFailsClean, Heard: "BACK QUENCH"},
			{File: "b", Outcome: ClipFailsClean, Heard: "FOR QUENCH"},
		}
		got := RollupClips("bat quench", clips)
		if !got.Broken {
			t.Errorf("every clip misheard at clean -> broken, got %+v", got)
		}
		if got.Held || got.HeldReps != 0 {
			t.Errorf("broken target holds nothing, got Held=%v HeldReps=%d", got.Held, got.HeldReps)
		}
	})
}

func TestClipRecordFromLadder_FellAtClean(t *testing.T) {
	wf := WordFragility{
		Target:      "air bam",
		Held:        false,
		FallSNRdB:   math.Inf(1), // fell at the clean step
		CleanDecode: "AIR",
		Constituents: []ConstituentFragility{
			{Word: "AIR", Position: 0},
			{Word: "BAM", Position: 1},
		},
	}

	rec := clipRecordFromLadder("g/air_bam.wav", "ts", "modelv1", wf)

	if rec.Outcome != ClipFailsClean {
		t.Errorf("fell-at-clean outcome = %q, want fails_clean", rec.Outcome)
	}
	if rec.Heard != "AIR" {
		t.Errorf("fell-at-clean heard = %q, want the clean decode AIR", rec.Heard)
	}
	if rec.FallSNRdB != nil {
		t.Errorf("fell-at-clean must not carry a fall SNR (+∞ is unserializable), got %v", *rec.FallSNRdB)
	}
	if len(rec.Words) != 0 {
		t.Errorf("fell-at-clean has no per-word ladder rows, got %+v", rec.Words)
	}
}
