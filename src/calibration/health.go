package calibration

import (
	"sort"
	"time"
)

// CommandHealth represents per-command accuracy and freshness.
//
// It is deliberately narrow, and got that way twice. First a time window and
// the lifetime pair computed to compare against it went, because the window
// was never set. Then average duration, first-verified and the pass streak
// went, because the only things that ever read them were the tests asserting
// they were computed correctly — production reads Rate, Attempts and
// LastVerified, and nothing else ever has.
type CommandHealth struct {
	Command      string
	Rate         float32
	Attempts     int
	LastVerified time.Time
}

// GroupHealth summarizes health for a command group. Its one consumer is
// RecommendDrill, which walks Commands; the per-group aggregates that used to
// sit here were rolled up for nobody.
type GroupHealth struct {
	Group    string
	Commands []CommandHealth
}

// CatalogHealth aggregates verification attempts into per-command health,
// keyed by the command's stable identity: CommandID when the attempt carries
// one, else the Target (pattern) — so legacy/codeword records still aggregate.
// The catalog joins these onto the enumerate set, looking up by command id
// first and falling back to pattern. Group is left empty (the catalog groups
// by owner from enumerate, not by the selection that produced the attempt).
func CatalogHealth(attempts []VerificationAttempt) map[string]CommandHealth {
	type stat struct {
		attempts, matches int
		last              time.Time
	}
	stats := make(map[string]*stat)

	for _, a := range attempts {
		key := a.CommandID
		if key == "" {
			key = a.Target
		}
		s, ok := stats[key]
		if !ok {
			s = &stat{}
			stats[key] = s
		}
		ts, _ := time.Parse(time.RFC3339, a.Timestamp)
		s.attempts++
		if a.Matched {
			s.matches++
		}
		if ts.After(s.last) {
			s.last = ts
		}
	}

	out := make(map[string]CommandHealth, len(stats))
	for key, s := range stats {
		h := CommandHealth{
			Command:      key,
			Attempts:     s.attempts,
			LastVerified: s.last,
		}
		if s.attempts > 0 {
			h.Rate = float32(s.matches) / float32(s.attempts) * 100
		}
		out[key] = h
	}
	return out
}

// ComputeHealth computes command health from a slice of verification attempts.
func ComputeHealth(attempts []VerificationAttempt) []GroupHealth {
	if len(attempts) == 0 {
		return nil
	}

	type cmdKey struct{ group, command string }
	type cmdStats struct {
		group    string
		command  string
		attempts int
		matches  int
		last     time.Time
	}

	stats := make(map[cmdKey]*cmdStats)

	for _, a := range attempts {
		key := cmdKey{group: a.Group, command: a.Target}
		s, ok := stats[key]
		if !ok {
			s = &cmdStats{group: a.Group, command: a.Target}
			stats[key] = s
		}

		ts, _ := time.Parse(time.RFC3339, a.Timestamp)

		s.attempts++
		if a.Matched {
			s.matches++
		}
		if ts.After(s.last) {
			s.last = ts
		}
	}

	// Group commands by group label
	groupMap := make(map[string][]CommandHealth)
	for _, s := range stats {
		h := CommandHealth{
			Command:      s.command,
			Attempts:     s.attempts,
			LastVerified: s.last,
		}
		if s.attempts > 0 {
			h.Rate = float32(s.matches) / float32(s.attempts) * 100
		}
		groupMap[s.group] = append(groupMap[s.group], h)
	}

	var groups []GroupHealth
	for groupName, commands := range groupMap {
		sort.Slice(commands, func(i, j int) bool {
			return commands[i].Command < commands[j].Command
		})

		groups = append(groups, GroupHealth{Group: groupName, Commands: commands})
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Group < groups[j].Group
	})

	return groups
}
