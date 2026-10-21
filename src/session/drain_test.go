package session

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// The command-hold drain defers end-of-hold cleanup (session boundary + idle)
// until the pipeline's flush has been matched. These tests pin the generation
// guard that keeps a stale drain from a superseded hold from clobbering a new
// one — the fast-re-press case from the jar/day bridge bug.

func TestCommandHoldDrain_FinishFiresOnce(t *testing.T) {
	s := newTestState()
	s.StartCommandHold()
	var mu sync.Mutex
	n := 0
	fire := func() { mu.Lock(); n++; mu.Unlock() }

	s.BeginCommandHoldDrain(time.Minute, fire) // long backstop; we finish manually
	s.FinishCommandHoldDrain(fire)
	s.FinishCommandHoldDrain(fire) // idempotent — second call is a no-op

	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("expected cleanup to fire exactly once, got %d", n)
	}
}

func TestCommandHoldDrain_FastRepressInvalidatesStaleCleanup(t *testing.T) {
	s := newTestState()
	s.StartCommandHold() // hold 1
	fired := false
	fire := func() { fired = true }

	s.BeginCommandHoldDrain(time.Minute, fire)
	prevPending, _ := s.StartCommandHold() // hold 2 (fast re-press) supersedes hold 1
	if !prevPending {
		t.Fatal("StartCommandHold should report the previous hold was still draining")
	}
	// Hold 1's pipeline.stopped arrives late — it must NOT run cleanup, or it
	// would clear hold 2's bridge and drop hold 2 to idle.
	s.FinishCommandHoldDrain(fire)
	if fired {
		t.Fatal("a stale drain from a superseded hold must not run cleanup")
	}
}

func TestCommandHoldDrain_CancelSuppresses(t *testing.T) {
	s := newTestState()
	s.StartCommandHold()
	fired := false
	s.BeginCommandHoldDrain(time.Minute, func() { fired = true })
	s.CancelCommandHoldDrain()
	s.FinishCommandHoldDrain(func() { fired = true })
	if fired {
		t.Fatal("a cancelled drain must not fire")
	}
}

func TestCommandHoldDrain_BackstopFires(t *testing.T) {
	s := newTestState()
	s.StartCommandHold()
	done := make(chan struct{})
	s.BeginCommandHoldDrain(20*time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("backstop timer did not fire the deferred cleanup")
	}
	// After the backstop fired, a late FinishCommandHoldDrain is a no-op.
	again := false
	s.FinishCommandHoldDrain(func() { again = true })
	if again {
		t.Fatal("cleanup fired twice (backstop + finish)")
	}
}

// A dictation that ends without a transcript is reported as lost; one whose
// transcript arrived (FinishDictation) is not, and neither is a stopped
// pipeline's late event for a session already finished.
func TestDictationLostOnlyWithoutATranscript(t *testing.T) {
	s := newTestState()
	lost := 0
	s.SetOnDictationLost(func() { lost++ })

	s.StartDictationRecording()
	s.DrainDictation()
	s.FinishDictation() // transcript landed
	s.FinishDictationIfDraining()
	if lost != 0 {
		t.Fatalf("a dictation with a transcript was reported lost (%d)", lost)
	}

	s.StartDictationRecording()
	s.DrainDictation()
	s.FinishDictationIfDraining() // pipeline stopped, no transcript
	if lost != 1 {
		t.Fatalf("lost = %d, want 1 for a dictation that ended with no transcript", lost)
	}
}

// The backstop must outlast the dictation pipeline's own deadline, or it
// gives up on transcripts the pipeline would still have delivered. Read from
// the manifest, so changing either one without the other fails here.
func TestDrainBackstopOutlastsThePipelineDeadline(t *testing.T) {
	raw, err := os.ReadFile("../../plugin.json")
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var manifest struct {
		CollectionData struct {
			Pipelines []struct {
				Name          string `json:"name"`
				CancelDelayMs int    `json:"cancel_delay_ms"`
			} `json:"_platform.pipelines"`
		} `json:"collection_data"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse plugin.json: %v", err)
	}
	deadline := time.Duration(-1)
	for _, p := range manifest.CollectionData.Pipelines {
		if p.Name == "dictation" {
			deadline = time.Duration(p.CancelDelayMs) * time.Millisecond
		}
	}
	if deadline <= 0 {
		t.Fatal("plugin.json has no dictation pipeline with a cancel_delay_ms")
	}
	if drainBackstop <= deadline {
		t.Fatalf("drainBackstop %v must exceed the dictation pipeline deadline %v", drainBackstop, deadline)
	}
}
