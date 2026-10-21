package calibration

import (
	"strings"
	"testing"
)

func biasRec(file string, flipAt, holdsTo *float64, ts string) ClipBiasRecord {
	return ClipBiasRecord{ID: NewID(), File: file, Ts: ts, Target: "page", FlipAt: flipAt, HoldsTo: holdsTo}
}

func TestRecommendBias(t *testing.T) {
	ladder := []float64{0, 2, 4, 6, 8, 12, 16, 24, 32}

	t.Run("margin rung above the floor, under the ceiling", func(t *testing.T) {
		rec := RecommendBias([]ClipBiasRecord{
			biasRec("a.wav", fp(6), fp(32), "t1"), // savable, flips at 6
			biasRec("b.wav", fp(0), fp(16), "t1"), // clean, regresses past 16
			biasRec("c.wav", nil, nil, "t1"),      // never
		}, ladder)
		if !rec.OK || rec.Conflict {
			t.Fatalf("rec = %+v, want OK", rec)
		}
		if rec.Strength != 8 {
			t.Errorf("Strength = %g, want 8 (first rung above 6 under ceiling 16)", rec.Strength)
		}
		if rec.Needed != 6 || rec.Ceiling == nil || *rec.Ceiling != 16 {
			t.Errorf("Needed/Ceiling = %g/%v, want 6/16", rec.Needed, rec.Ceiling)
		}
		if rec.Savable != 1 || rec.Clean != 1 || rec.Never != 1 {
			t.Errorf("counts = %d/%d/%d, want 1/1/1", rec.Savable, rec.Clean, rec.Never)
		}
	})

	t.Run("floor itself when the margin rung would break a clean clip", func(t *testing.T) {
		rec := RecommendBias([]ClipBiasRecord{
			biasRec("a.wav", fp(6), fp(32), "t1"),
			biasRec("b.wav", fp(0), fp(6), "t1"), // ceiling exactly at the floor
		}, ladder)
		if !rec.OK || rec.Strength != 6 {
			t.Errorf("rec = %+v, want Strength 6 (no rung fits between floor and ceiling)", rec)
		}
	})

	t.Run("conflict when a mishear needs more than a clean clip tolerates", func(t *testing.T) {
		rec := RecommendBias([]ClipBiasRecord{
			biasRec("a.wav", fp(24), fp(32), "t1"),
			biasRec("b.wav", fp(0), fp(8), "t1"),
		}, ladder)
		if rec.OK || !rec.Conflict {
			t.Fatalf("rec = %+v, want Conflict", rec)
		}
		if !strings.Contains(rec.Label(), "no single safe strength") {
			t.Errorf("Label = %q", rec.Label())
		}
	})

	t.Run("no savable mishears means no recommendation", func(t *testing.T) {
		rec := RecommendBias([]ClipBiasRecord{
			biasRec("a.wav", fp(0), fp(32), "t1"),
			biasRec("b.wav", nil, nil, "t1"),
		}, ladder)
		if rec.OK || rec.Conflict {
			t.Errorf("rec = %+v, want not-OK, no conflict", rec)
		}
		if !strings.Contains(rec.Label(), "nothing for the bias to fix") {
			t.Errorf("Label = %q", rec.Label())
		}
	})

	t.Run("pre-HoldsTo records recommend unbounded", func(t *testing.T) {
		rec := RecommendBias([]ClipBiasRecord{
			biasRec("a.wav", fp(6), nil, "t1"), // legacy record: no ceiling measured
		}, ladder)
		if !rec.OK || rec.Strength != 8 || rec.Ceiling != nil {
			t.Errorf("rec = %+v, want Strength 8 with nil Ceiling", rec)
		}
	})

	t.Run("only the newest reading per file counts", func(t *testing.T) {
		rec := RecommendBias([]ClipBiasRecord{
			biasRec("a.wav", fp(24), fp(32), "t1"), // stale: would push the floor to 24
			biasRec("a.wav", fp(4), fp(32), "t2"),  // newest wins
		}, ladder)
		if !rec.OK || rec.Needed != 4 {
			t.Errorf("rec = %+v, want Needed 4 (stale reading ignored)", rec)
		}
	})
}
