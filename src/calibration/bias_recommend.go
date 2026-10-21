package calibration

import (
	"fmt"
	"sort"
)

// The strength recommendation folds the corpus's latest per-clip bias scores
// (bias_ladder.go) into one number the user can apply: the lowest strength that
// flips every savable mishear, with a one-rung margin, kept under the ceiling
// where correct clips start regressing. Pure interpretation over persisted
// ClipBiasRecords — no I/O — so it is table-testable. The apply path goes
// through the platform's recognition.bias.apply, which stamps `calibration`
// provenance and refuses over a manually-set value unless forced.

// BiasRecommendation is the corpus-level readout. Strength is meaningful only
// when OK. Ceiling is nil when no scored clip carries a hold ceiling (records
// predating HoldsTo, or an empty corpus).
type BiasRecommendation struct {
	Strength float64
	OK       bool
	Needed   float64  // highest flip threshold across savable clips
	Ceiling  *float64 // lowest hold ceiling across measured clips
	Savable  int      // clips a bias fixes (flip_at > 0)
	Clean    int      // clips correct unbiased (flip_at == 0)
	Never    int      // clips no strength in range fixes
	Conflict bool     // needed exceeds ceiling — no single safe strength
}

// RecommendBias aggregates the newest reading per clip file into a
// recommendation. Savable clips set the floor (max flip threshold); every
// measured hold ceiling bounds it from above (a clip that regresses past its
// HoldsTo is a clip the bias breaks). The recommended strength is the first
// ladder rung strictly above the floor that stays under the ceiling — margin
// for day-to-day acoustic variation — falling back to the floor itself when no
// rung fits.
func RecommendBias(records []ClipBiasRecord, ladder []float64) BiasRecommendation {
	if len(ladder) == 0 {
		ladder = DefaultBiasLadder()
	}
	rec := BiasRecommendation{}
	for _, r := range LatestBiasPerFile(records) {
		switch {
		case r.FlipAt == nil:
			rec.Never++
			continue // wrong at every strength; neither floor nor ceiling
		case *r.FlipAt == 0:
			rec.Clean++
		default:
			rec.Savable++
			if *r.FlipAt > rec.Needed {
				rec.Needed = *r.FlipAt
			}
		}
		if r.HoldsTo != nil && (rec.Ceiling == nil || *r.HoldsTo < *rec.Ceiling) {
			c := *r.HoldsTo
			rec.Ceiling = &c
		}
	}
	if rec.Savable == 0 {
		return rec // nothing to fix; OK stays false
	}
	if rec.Ceiling != nil && rec.Needed > *rec.Ceiling {
		rec.Conflict = true
		return rec
	}
	rec.Strength = rec.Needed
	ordered := append([]float64(nil), ladder...)
	sort.Float64s(ordered)
	for _, rung := range ordered {
		if rung > rec.Needed && (rec.Ceiling == nil || rung <= *rec.Ceiling) {
			rec.Strength = rung
			break
		}
	}
	rec.OK = true
	return rec
}

// Label renders the one-line recommendation shown in the Recordings header.
func (r BiasRecommendation) Label() string {
	switch {
	case r.Conflict:
		return fmt.Sprintf("no single safe strength — mishears need %g but correct commands regress past %g", r.Needed, *r.Ceiling)
	case !r.OK:
		return "no savable mishears scored — nothing for the bias to fix"
	case r.Ceiling != nil:
		return fmt.Sprintf("recommend %g — mishears flip by %g, correct commands hold to %g", r.Strength, r.Needed, *r.Ceiling)
	default:
		return fmt.Sprintf("recommend %g — mishears flip by %g", r.Strength, r.Needed)
	}
}
