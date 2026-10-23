package session

import (
	"testing"
	"time"
)

// A command hold over continuous listening sets the persistent run aside
// once; a hold that finds it already aside (a re-press while the last one
// drains) stops nothing; releasing it restarts the run only while
// continuous listening still wants it.
func TestCmdRecAsideForHold(t *testing.T) {
	s := newTestState()
	if s.SetCmdRecAsideForHold() || s.IsCmdRecAside() {
		t.Fatal("outside continuous listening there is nothing to set aside")
	}

	s.SetCurrentMode("continuous_sleeping")
	s.SetCmdRecActive()
	s.StartCommandHold()
	if !s.SetCmdRecAsideForHold() {
		t.Fatal("the first hold must stop the persistent run")
	}
	if s.SetCmdRecAsideForHold() {
		t.Fatal("a run already aside must not be stopped again")
	}
	if s.ReleaseCmdRecAside() || !s.IsCmdRecAside() {
		t.Fatal("while a hold is down its claim is kept")
	}
	s.EndCommandHold()
	if !s.ReleaseCmdRecAside() {
		t.Fatal("releasing it while listening must restart it")
	}
	if s.ReleaseCmdRecAside() {
		t.Fatal("a second release must not restart it again")
	}

	// A dictation holding the microphone keeps it down; its own end
	// restarts it.
	s.SetCmdRecAsideForHold()
	s.SuppressCommandRecognition()
	if s.ReleaseCmdRecAside() {
		t.Fatal("released under a dictation, the run must stay down")
	}
}

// T down first: the dictation already stopped the persistent run, so the
// hold records the claim without stopping anything.
func TestCmdRecAsideUnderADictation(t *testing.T) {
	s := newTestState()
	s.SetCurrentMode("continuous_awake")
	s.SetCmdRecActive()
	s.SuppressCommandRecognition()
	if s.SetCmdRecAsideForHold() {
		t.Fatal("a run a dictation stopped must not be stopped again")
	}
	if !s.IsCmdRecAside() {
		t.Fatal("the hold's claim must still be recorded")
	}
	s.UnsuppressCommandRecognition()
	if !s.ReleaseCmdRecAside() {
		t.Fatal("the hold's end, after the dictation's, restarts the run")
	}
}

// `stopped` is matched to the oldest run started.
func TestCmdRecRunsMatchStoppedToTheOldestRun(t *testing.T) {
	s := newTestState()
	if known, _, _ := s.NoteCmdRecStopped(); known {
		t.Fatal("a stopped with no started is unknown")
	}
	s.NoteCmdRecStarted(false)
	s.NoteCmdRecStarted(true)
	if known, eph, _ := s.NoteCmdRecStopped(); !known || eph {
		t.Fatalf("first stopped = (%v, %v), want the persistent run", known, eph)
	}
	if known, eph, _ := s.NoteCmdRecStopped(); !known || !eph {
		t.Fatalf("second stopped = (%v, %v), want the ephemeral run", known, eph)
	}
	for range cmdRecRunsCap + 3 {
		s.NoteCmdRecStarted(true)
	}
	n := 0
	for {
		if known, _, _ := s.NoteCmdRecStopped(); !known {
			break
		}
		n++
	}
	if n != cmdRecRunsCap {
		t.Fatalf("runs kept = %d, want the cap %d", n, cmdRecRunsCap)
	}
}

// A released hold's cleanup waits for every run up at the release (the
// newest is the hold's own), and not for a run started after it.
func TestHoldDrainWaitsForTheRunsUpAtTheRelease(t *testing.T) {
	s := newTestState()
	s.NoteCmdRecStarted(false) // the persistent run, set aside
	s.StartCommandHold()
	s.NoteCmdRecStarted(true) // the hold's run
	s.BeginCommandHoldDrain(time.Minute, func() {})
	s.NoteCmdRecStarted(true) // a speak session's run, started at the flush
	if _, _, due := s.NoteCmdRecStopped(); due {
		t.Fatal("the persistent run's stop ended the hold's drain")
	}
	if _, eph, due := s.NoteCmdRecStopped(); !eph || !due {
		t.Fatal("the hold's own run's stop must end its drain")
	}
}

// At a hold's release the persistent run comes back at once, its
// transcripts held back from when the hold's run has said stopped until
// the cleanup lets them go; under a dictation the claim stays for the
// cleanup and nothing is held.
func TestCmdRecReturnedAtRelease(t *testing.T) {
	s := newTestState()
	s.SetCurrentMode("continuous_sleeping")
	s.SetCmdRecActive()
	s.StartCommandHold()
	s.SetCmdRecAsideForHold()
	s.NoteCmdRecStarted(true)
	if s.ReturnCmdRecAtRelease() {
		t.Fatal("while the key is down the claim is kept")
	}

	s.EndCommandHold()
	s.BeginCommandHoldDrain(time.Hour, func() {})
	if !s.ReturnCmdRecAtRelease() || s.IsCmdRecAside() {
		t.Fatal("the release must start the persistent run again")
	}
	if s.ReturnCmdRecAtRelease() || s.ReleaseCmdRecAside() {
		t.Fatal("the run was started twice")
	}
	if s.CmdRecHeldBack() != nil {
		t.Fatal("the hold's own flush must not be held back")
	}
	s.NoteCmdRecStarted(false)
	if _, ephemeral, due := s.NoteCmdRecStopped(); !ephemeral || !due {
		t.Fatal("want the hold's run's stopped, drain due")
	}
	gate := s.CmdRecHeldBack()
	if gate == nil {
		t.Fatal("after the hold's run stopped, the persistent run's transcripts wait")
	}
	s.OpenCmdRecHeldBack()
	select {
	case <-gate:
	default:
		t.Fatal("the cleanup did not let the held transcripts go")
	}
	if s.CmdRecHeldBack() != nil {
		t.Fatal("still held back after the cleanup")
	}

	// Under a dictation: the claim stays for the cleanup.
	s.StartCommandHold()
	s.SetCmdRecAsideForHold()
	s.SuppressCommandRecognition()
	s.EndCommandHold()
	if s.ReturnCmdRecAtRelease() || !s.IsCmdRecAside() {
		t.Fatal("released under a dictation, the run must stay down")
	}
}
