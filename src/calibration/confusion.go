package calibration

import (
	"sort"
	"time"
)

// ConfusionEdge represents a confusion pair: target was heard as something else.
type ConfusionEdge struct {
	Target string
	Heard  string
	Count  int
	Rate   float32 // count / total attempts for this target
}

// ConfusionGraph aggregates all confusion pairs from verification attempts.
type ConfusionGraph struct {
	Edges    []ConfusionEdge            // sorted by count descending
	ByTarget map[string][]ConfusionEdge // lookup: what does target get confused with?
}

// ComputeConfusion computes the confusion graph from a slice of verification attempts.
// Only considers attempts where the engine produced a hypothesis (heard != "")
// and the result didn't match the target.
func ComputeConfusion(attempts []VerificationAttempt, since time.Time) *ConfusionGraph {
	if len(attempts) == 0 {
		return &ConfusionGraph{ByTarget: make(map[string][]ConfusionEdge)}
	}

	type edgeKey struct{ target, heard string }
	type edgeData struct {
		count    int
		lastSeen time.Time
	}

	// Count total attempts per target (for rate calculation)
	targetAttempts := make(map[string]int)
	edges := make(map[edgeKey]*edgeData)

	for _, a := range attempts {
		if a.Heard == "" {
			continue
		}

		ts, _ := time.Parse(time.RFC3339, a.Timestamp)
		if !since.IsZero() && ts.Before(since) {
			continue
		}

		targetAttempts[a.Target]++

		if !a.Matched {
			key := edgeKey{target: a.Target, heard: a.Heard}
			e, ok := edges[key]
			if !ok {
				e = &edgeData{}
				edges[key] = e
			}
			e.count++
			if ts.After(e.lastSeen) {
				e.lastSeen = ts
			}
		}
	}

	graph := &ConfusionGraph{
		ByTarget: make(map[string][]ConfusionEdge),
	}

	for key, data := range edges {
		rate := float32(0)
		if total := targetAttempts[key.target]; total > 0 {
			rate = float32(data.count) / float32(total) * 100
		}
		edge := ConfusionEdge{
			Target: key.target,
			Heard:  key.heard,
			Count:  data.count,
			Rate:   rate,
		}
		graph.Edges = append(graph.Edges, edge)
		graph.ByTarget[key.target] = append(graph.ByTarget[key.target], edge)
	}

	// Sort edges by count descending
	sort.Slice(graph.Edges, func(i, j int) bool {
		return graph.Edges[i].Count > graph.Edges[j].Count
	})

	// Sort per-target edges
	for _, edges := range graph.ByTarget {
		sort.Slice(edges, func(i, j int) bool {
			return edges[i].Count > edges[j].Count
		})
	}

	return graph
}

// SymmetricPairs returns confusion edges where both A→B and B→A exist.
func (g *ConfusionGraph) SymmetricPairs() []ConfusionEdge {
	seen := make(map[[2]string]bool)
	var pairs []ConfusionEdge

	for _, e := range g.Edges {
		// Check if the reverse exists
		for _, rev := range g.ByTarget[e.Heard] {
			if rev.Heard == e.Target {
				key := [2]string{e.Target, e.Heard}
				if e.Target > e.Heard {
					key = [2]string{e.Heard, e.Target}
				}
				if !seen[key] {
					seen[key] = true
					pairs = append(pairs, e)
				}
				break
			}
		}
	}
	return pairs
}

// TopConfusions returns the N highest-count confusion edges.
func (g *ConfusionGraph) TopConfusions(n int) []ConfusionEdge {
	if n >= len(g.Edges) {
		return g.Edges
	}
	return g.Edges[:n]
}
