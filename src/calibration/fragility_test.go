package calibration

import "testing"

// The re-decode probe is Sherpa-offline-only; under any other engine the runner
// must refuse (and persist nothing) rather than shell out pointlessly.
func TestRunFragilityAnalysis_GatedToSherpa(t *testing.T) {
	s := NewState()
	s.EngineName = func() string { return "other-engine" }

	_, err := s.RunFragilityAnalysis(map[string][]string{"COPY": {"/nope.wav"}})
	if err == nil {
		t.Fatal("expected an error under a non-Sherpa engine, got nil")
	}
}
