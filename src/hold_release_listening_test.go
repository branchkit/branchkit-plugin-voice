package main

import (
	"slices"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Over continuous listening, an Option+G release starts the persistent run
// again at once, so the platform captures what is said from the release on.
// What that run hears is held back until the hold's cleanup has restored
// continuous listening's mode, then resolved in that mode: speech after the
// release is never the hold's command, and none of it is lost.

// heldSay delivers a command_recognition transcript on its own goroutine, as
// the platform's request is handled, and returns a channel that yields the
// actions voice answered it with.
func heldSay(t *testing.T, h *Host, text string) <-chan []branchkit.Action {
	t.Helper()
	out := make(chan []branchkit.Action, 1)
	go func() {
		resp, err := h.handleOnTranscript(&branchkit.OnTranscriptRequest{Pipeline: "command_recognition", Text: text})
		if err != nil {
			t.Errorf("handleOnTranscript: %v", err)
			out <- nil
			return
		}
		out <- resp.(*branchkit.OnTranscriptResponse).Actions
	}()
	return out
}

// wantWaiting fails if the transcript was answered already.
func wantWaiting(t *testing.T, got <-chan []branchkit.Action) {
	t.Helper()
	select {
	case a := <-got:
		t.Fatalf("a transcript heard after the release was resolved before the hold's cleanup (%d actions)", len(a))
	case <-time.After(50 * time.Millisecond):
	}
}

// answered waits for the transcript's answer.
func answered(t *testing.T, got <-chan []branchkit.Action) []branchkit.Action {
	t.Helper()
	select {
	case a := <-got:
		return a
	case <-time.After(heldBackLimit + time.Second):
		t.Fatal("the held transcript was never answered")
		return nil
	}
}

// releasedHold presses and releases Option+G over continuous listening and
// delivers the set-aside persistent run's `stopped`, leaving the hold's own
// run draining and the persistent run started again at the release.
func releasedHold(t *testing.T, h *Host, p *pipelineSim) {
	t.Helper()
	postActionWithPhase(h, t, "voice.command", "start")
	if p.deliverStopped() {
		t.Fatal("want the persistent run's stopped first")
	}
	postActionWithPhase(h, t, "voice.command", "stop")
	if got := p.take(); !slices.Equal(got, []string{
		"stop command_recognition", "run command_recognition ephemeral",
		"stop command_recognition", "run command_recognition persistent",
	}) {
		t.Fatalf("calls %v, want the hold's run, then the persistent run back at the release", got)
	}
	wantLive(t, p, "persistent")
	wantMode(t, h, "command_hold")
}

// hearHoldRunStopped is the first half of the hold's run's `stopped` as
// handleCommandRecognitionStopped takes it: the run is matched and the
// drain is due, but the cleanup that restores the mode has not run yet.
// That is the moment a transcript of the persistent run can race it.
func hearHoldRunStopped(t *testing.T, h *Host, p *pipelineSim) {
	t.Helper()
	p.mu.Lock()
	p.stopping = p.stopping[1:]
	p.mu.Unlock()
	known, ephemeral, drainDue := h.sess.NoteCmdRecStopped()
	if !known || !ephemeral || !drainDue {
		t.Fatalf("NoteCmdRecStopped = known %v ephemeral %v drainDue %v, want the hold's run, drain due", known, ephemeral, drainDue)
	}
}

// Asleep: words said right after the release reach voice before the hold's
// cleanup. They wait, and are resolved asleep, where a command phrase runs
// nothing. Resolved at once they would have run as the hold's command.
func TestSpeechAfterReleaseWaitsForTheBaseMode(t *testing.T) {
	h, p, m := listeningHost(t)
	releasedHold(t, h, p)

	hearHoldRunStopped(t, h, p)
	before := m.resolveCalls()
	got := heldSay(t, h, "close tab")
	wantWaiting(t, got)
	if n := m.resolveCalls(); n != before {
		t.Fatalf("the held transcript was resolved while it waited (%d resolves)", n-before)
	}

	h.sess.FinishCommandHoldDrain(h.finishCommandHold)
	if a := answered(t, got); len(a) != 0 {
		t.Fatalf("speech after the release ran as a command while asleep: %#v", a)
	}
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}

// Awake: "snap left" said right after the release is heard whole by the run
// started at the release, and runs once the mode is back, in that mode.
func TestSpeechAfterReleaseIsHeardWhenAwake(t *testing.T) {
	h, p, m := listeningHost(t)
	h.setVoiceMode("continuous_awake")
	releasedHold(t, h, p)

	hearHoldRunStopped(t, h, p)
	got := heldSay(t, h, "snap left")
	wantWaiting(t, got)

	h.sess.FinishCommandHoldDrain(h.finishCommandHold)
	if a := answered(t, got); len(a) != 1 {
		t.Fatalf("speech after the release: %d actions, want 1", len(a))
	}
	if n := m.resolveCalls(); n == 0 {
		t.Fatal("the held transcript was never resolved")
	}
	wantMode(t, h, "continuous_awake")

	// Nothing is held once the cleanup has run.
	if gate := h.sess.CmdRecHeldBack(); gate != nil {
		t.Fatal("transcripts are still held back after the cleanup")
	}
	if a := say(t, h, "snap left"); len(a) != 1 {
		t.Fatalf("a later command: %d actions, want 1", len(a))
	}
}

// The hold's own last words, decoded at its end-of-hold flush, arrive
// before its run says stopped: they are the hold's command, not held back
// for the cleanup.
func TestHoldsFlushIsNotHeldBack(t *testing.T) {
	h, p, _ := listeningHost(t)
	releasedHold(t, h, p)

	if gate := h.sess.CmdRecHeldBack(); gate != nil {
		t.Fatal("the hold's flush would be held back")
	}
	if a := say(t, h, "close tab"); len(a) != 1 {
		t.Fatalf("the hold's flushed command: %d actions, want 1", len(a))
	}
	p.deliverStopped()
	wantMode(t, h, "continuous_sleeping")
}

// A re-press while the last release's transcripts wait: that hold's cleanup
// will never run, so they go on at the press, under the new hold, rather
// than wait out the limit while the new hold's run waits on theirs.
func TestRePressLetsHeldTranscriptsGo(t *testing.T) {
	h, p, _ := listeningHost(t)
	releasedHold(t, h, p)

	hearHoldRunStopped(t, h, p)
	got := heldSay(t, h, "close tab")
	wantWaiting(t, got)

	postActionWithPhase(h, t, "voice.command", "start")
	if a := answered(t, got); len(a) != 1 {
		t.Fatalf("a held transcript under the new hold: %d actions, want 1", len(a))
	}
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition", "run command_recognition ephemeral"}) {
		t.Fatalf("re-press: calls %v, want the persistent run set aside again and the new hold's run", got)
	}

	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 3 {
		t.Fatalf("persistent runs started = %d, want 3 (listening on, then once per release)", got)
	}
}

// An abort after the release takes the cleanup's place: it restores the mode
// and lets held transcripts go, and nothing is held back after it.
func TestAbortAfterReleaseLetsHeldTranscriptsGo(t *testing.T) {
	h, p, _ := listeningHost(t)
	releasedHold(t, h, p)

	postActionWithPhase(h, t, "voice.command", "abort")
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	p.deliverAllStopped()
	if gate := h.sess.CmdRecHeldBack(); gate != nil {
		t.Fatal("transcripts are held back after an abort ended the hold")
	}
	if a := say(t, h, "close tab"); len(a) != 0 {
		t.Fatalf("speech while asleep ran as a command: %#v", a)
	}
}

// A dictation holding the microphone at the release keeps the persistent
// run down: nothing starts at the release and nothing is held back, and the
// dictation's end brings the run back.
func TestReleaseUnderDictationStartsNothing(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.dictation", "start")
	p.take()

	postActionWithPhase(h, t, "voice.command", "stop")
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition"}) {
		t.Fatalf("G up under a dictation: calls %v, want only the hold's run stopped", got)
	}
	p.deliverAllStopped()
	if gate := h.sess.CmdRecHeldBack(); gate != nil {
		t.Fatal("transcripts held back with no run started at the release")
	}
	wantMode(t, h, "dictation")
	wantLive(t, p, "none")

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
}
