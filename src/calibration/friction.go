package calibration

import "strings"

// Observed per-person "friction" — how hard a word is to land for THIS user,
// measured from real game attempts rather than the synthetic perturbation
// ladder. It complements two signals it is deliberately distinct from:
//
//   - accuracy (did the word eventually pass) — a high-friction word can be
//     100% pass yet take many tries each time (VEST);
//   - the fragility ladder (inherent, speaker-independent codeword-design
//     signal) — friction is per-user pronunciation/recognition reality.
//
// Friction is the observed answer to whether a word's confusability is
// per-person or inherent to the word.

const (
	// frictionMinAttempts is how many attempts a word needs before its
	// friction is trustworthy enough to act on (one bad run is noise).
	frictionMinAttempts = 2
	// frictionThreshold is the per-attempt friction at/above which a word is
	// flagged high-friction — on average this many extra recognitions (wrong
	// heards + silent holds) fought through per attempt before it settled.
	frictionThreshold = 1.5
)

// FrictionStat aggregates one word's observed friction across attempts.
type FrictionStat struct {
	Word        string
	Attempts    int
	Mishears    int // total recognitions that weren't the target
	SilentHolds int // total holds that decoded to nothing
}

// PerAttempt is the average number of extra recognitions (wrong + silent) the
// user fought through per attempt before the word settled. 0 = clean every time.
func (f FrictionStat) PerAttempt() float32 {
	if f.Attempts == 0 {
		return 0
	}
	return float32(f.Mishears+f.SilentHolds) / float32(f.Attempts)
}

// IsHigh reports whether this word should be flagged (and resurfaced for
// practice) as high-friction.
func (f FrictionStat) IsHigh() bool {
	return f.Attempts >= frictionMinAttempts && f.PerAttempt() >= frictionThreshold
}

// ComputeFriction aggregates observed friction per target word from
// verification attempts, keyed by lowercased target. Only attempts carrying the
// occurrence-counted trail + silent-hold counts contribute friction; older
// records (pre the recording-fidelity change) count as attempts with zero
// friction, so a word's score reflects its recent, faithfully-recorded runs.
func ComputeFriction(attempts []VerificationAttempt) map[string]FrictionStat {
	out := make(map[string]FrictionStat)
	for _, a := range attempts {
		if a.Target == "" {
			continue
		}
		key := strings.ToLower(a.Target)
		fs := out[key]
		fs.Word = a.Target
		fs.Attempts++
		for _, h := range a.HeardTallies {
			if !strings.EqualFold(h.Text, a.Target) {
				fs.Mishears += h.Count
			}
		}
		fs.SilentHolds += a.SilentHolds
		out[key] = fs
	}
	return out
}
