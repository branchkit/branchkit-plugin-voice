package calibration

import (
	"testing"
	"time"
)

func TestComputeConfusion(t *testing.T) {
	ts := time.Now().UTC().Format(time.RFC3339)

	// "gust" confused with "just" 3 times, once with "gush"
	// "just" confused with "gust" once (symmetric pair)
	attempts := []VerificationAttempt{
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "gust", Heard: "just", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "gust", Heard: "just", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "gust", Heard: "just", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "gust", Heard: "gush", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "gust", Heard: "gust", Matched: true, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "just", Heard: "gust", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "just", Heard: "just", Matched: true, Group: "alphabet", Engine: "sherpa-offline-nemo"},
	}

	graph := ComputeConfusion(attempts, time.Time{})

	if len(graph.Edges) != 3 {
		t.Fatalf("expected 3 edges, got %d", len(graph.Edges))
	}

	// Top edge should be gust→just (count 3)
	top := graph.Edges[0]
	if top.Target != "gust" || top.Heard != "just" {
		t.Errorf("top edge = %s→%s, want gust→just", top.Target, top.Heard)
	}
	if top.Count != 3 {
		t.Errorf("top count = %d, want 3", top.Count)
	}

	// Rate: 3 confusions out of 5 total attempts for "gust" = 60%
	if top.Rate < 59.9 || top.Rate > 60.1 {
		t.Errorf("top rate = %v, want ~60", top.Rate)
	}

	// Check ByTarget lookup
	gustEdges := graph.ByTarget["gust"]
	if len(gustEdges) != 2 {
		t.Errorf("gust edges = %d, want 2 (just + gush)", len(gustEdges))
	}
}

func TestSymmetricPairs(t *testing.T) {
	ts := time.Now().UTC().Format(time.RFC3339)

	// gust→just and just→gust both exist
	attempts := []VerificationAttempt{
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "gust", Heard: "just", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "gust", Heard: "gust", Matched: true, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "just", Heard: "gust", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "just", Heard: "just", Matched: true, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		// snap→nap exists but nap→snap does not (one-way)
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "snap", Heard: "nap", Matched: false, Group: "commands", Engine: "sherpa-offline-nemo"},
		{ID: NewID(), SessionID: "s1", Timestamp: ts, Target: "snap", Heard: "snap", Matched: true, Group: "commands", Engine: "sherpa-offline-nemo"},
	}

	graph := ComputeConfusion(attempts, time.Time{})
	pairs := graph.SymmetricPairs()

	if len(pairs) != 1 {
		t.Fatalf("expected 1 symmetric pair, got %d", len(pairs))
	}

	// Should be the gust↔just pair
	p := pairs[0]
	if (p.Target == "gust" && p.Heard == "just") || (p.Target == "just" && p.Heard == "gust") {
		// good
	} else {
		t.Errorf("symmetric pair = %s↔%s, want gust↔just", p.Target, p.Heard)
	}
}

func TestConfusionGraphWithTimeWindow(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().UTC().Format(time.RFC3339)

	attempts := []VerificationAttempt{
		// Old confusion
		{ID: NewID(), SessionID: "s1", Timestamp: old, Target: "gust", Heard: "just", Matched: false, Group: "alphabet", Engine: "sherpa-offline-nemo"},
		// Recent: gust works fine
		{ID: NewID(), SessionID: "s2", Timestamp: recent, Target: "gust", Heard: "gust", Matched: true, Group: "alphabet", Engine: "sherpa-offline-nemo"},
	}

	// Query last 24h: no confusions
	since := time.Now().Add(-24 * time.Hour)
	graph := ComputeConfusion(attempts, since)

	if len(graph.Edges) != 0 {
		t.Errorf("expected 0 edges in recent window, got %d", len(graph.Edges))
	}
}
