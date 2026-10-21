package calibration

import (
	"sort"
	"time"
)

// DrillRecommendation suggests which commands to verify next.
type DrillRecommendation struct {
	Commands []DrillCommand
}

// DrillCommand is a single command recommended for verification.
type DrillCommand struct {
	Command string
	Reason  string  // "never_tested", "low_accuracy", "confused_pair", "stale"
	Score   float32 // sort priority (higher = more urgent)
}

// RecommendDrill selects the most important commands to verify.
// maxCommands limits the result size.
func RecommendDrill(attempts []VerificationAttempt, maxCommands int) *DrillRecommendation {
	groups := ComputeHealth(attempts)
	graph := ComputeConfusion(attempts, time.Time{})

	var candidates []DrillCommand

	// 1. Never-tested commands (highest priority — unknown reliability)
	for _, g := range groups {
		for _, c := range g.Commands {
			if c.Attempts == 0 {
				candidates = append(candidates, DrillCommand{
					Command: c.Command,
					Reason:  "never_tested",
					Score:   1000,
				})
			}
		}
	}

	// 2. Low accuracy commands
	for _, g := range groups {
		for _, c := range g.Commands {
			if c.Attempts > 0 && c.Rate < 80 {
				candidates = append(candidates, DrillCommand{
					Command: c.Command,
					Reason:  "low_accuracy",
					Score:   500 - c.Rate*5, // lower accuracy = higher score
				})
			}
		}
	}

	// 3. Confused pairs — both sides
	if graph != nil {
		for _, edge := range graph.Edges {
			if edge.Rate >= 20 { // at least 20% confusion rate
				candidates = append(candidates, DrillCommand{
					Command: edge.Target,
					Reason:  "confused_pair",
					Score:   300 + edge.Rate,
				})
			}
		}
	}

	// 4. Stale commands (not tested in 14+ days)
	staleThreshold := time.Now().Add(-14 * 24 * time.Hour)
	for _, g := range groups {
		for _, c := range g.Commands {
			if c.Attempts > 0 && c.LastVerified.Before(staleThreshold) {
				daysSince := int(time.Since(c.LastVerified).Hours() / 24)
				candidates = append(candidates, DrillCommand{
					Command: c.Command,
					Reason:  "stale",
					Score:   100 + float32(daysSince),
				})
			}
		}
	}

	// 5. High-friction commands — words that fight the user (many mishears /
	//    silent holds before settling) even when they eventually pass. Accuracy
	//    alone misses these; surface them so a 100%-but-painful word like VEST
	//    resurfaces for practice. (Observed per-person difficulty — Rec C.)
	for _, fs := range ComputeFriction(attempts) {
		if fs.IsHigh() {
			candidates = append(candidates, DrillCommand{
				Command: fs.Word,
				Reason:  "high_friction",
				Score:   250 + fs.PerAttempt()*10,
			})
		}
	}

	// Deduplicate (same command may appear for multiple reasons — keep highest score)
	seen := make(map[string]int) // command → index in deduped
	var deduped []DrillCommand
	// Sort by score descending first so the first occurrence is the best reason
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	for _, c := range candidates {
		if _, ok := seen[c.Command]; !ok {
			seen[c.Command] = len(deduped)
			deduped = append(deduped, c)
		}
	}

	if maxCommands > 0 && len(deduped) > maxCommands {
		deduped = deduped[:maxCommands]
	}

	return &DrillRecommendation{Commands: deduped}
}
