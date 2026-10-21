package main

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/plugin-sdk-go"
)

// The command hold (Option+G) against dictation (Option+T) and continuous
// listening: the key-edge orders that used to strand a command session or end
// continuous listening. The host-side key semantics these edges
// come from are the platform's hotkey_machine; this is what voice makes of
// them.

// pipelineCalls records which pipelines the handlers start and stop.
type pipelineCalls struct {
	mu    sync.Mutex
	calls []string
}

func (c *pipelineCalls) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	return out
}

func watchPipelines(h *Host) *pipelineCalls {
	c := &pipelineCalls{}
	h.pipelinesRun = func(req branchkit.PipelinesRunRequest) (*branchkit.PipelinesRunResponse, error) {
		c.mu.Lock()
		c.calls = append(c.calls, "run "+req.Name)
		c.mu.Unlock()
		return &branchkit.PipelinesRunResponse{Started: true}, nil
	}
	h.pipelinesStop = func(req branchkit.PipelinesStopRequest) (*branchkit.PipelinesStopResponse, error) {
		c.mu.Lock()
		c.calls = append(c.calls, "stop "+req.Name)
		c.mu.Unlock()
		return &branchkit.PipelinesStopResponse{}, nil
	}
	return c
}

// holdDictationLong backdates the recording so its release is a hold, not a
// tap waiting out the double-tap window.
func holdDictationLong(h *Host) {
	past := time.Now().Add(-time.Second)
	h.sess.SetDictationStartTime(&past)
}

// commandPipelineStopped is what main.go does on the command_recognition
// pipeline's `stopped` event: run the deferred end-of-hold cleanup.
func commandPipelineStopped(h *Host) {
	h.sess.FinishCommandHoldDrain(h.finishCommandHold)
}

func wantMode(t *testing.T, h *Host, want string) {
	t.Helper()
	if got := h.sess.GetCurrentMode(); got != want {
		t.Fatalf("mode = %q, want %q", got, want)
	}
}

// G down, T down, G up, T up: G's release ends the hold even though dictation
// covers it, and nothing is left running or claiming the mode afterwards.
func TestOverlappingChordsEndTheCommandHold(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)

	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.dictation", "start")
	wantMode(t, h, "dictation")
	calls.take()

	postActionWithPhase(h, t, "voice.command", "stop")
	if got := calls.take(); !slices.Contains(got, "stop command_recognition") {
		t.Fatalf("G up during dictation must stop the hold's pipeline; calls %v", got)
	}
	commandPipelineStopped(h)
	// The hold's cleanup must not take the mode away from the live dictation.
	wantMode(t, h, "dictation")

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	wantMode(t, h, "idle")
	if got := calls.take(); slices.Contains(got, "run command_recognition") {
		t.Fatalf("T up must not restart command recognition outside continuous mode; calls %v", got)
	}

	// The continuous toggle is no longer refused by a stranded hold.
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	wantMode(t, h, "continuous_sleeping")
}

// T down, G down, T up, G up: T's release returns to the hold the key is
// still making, and G's release ends it.
func TestDictationReleasedUnderAHoldReturnsToTheHold(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)

	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.command", "start")
	wantMode(t, h, "command_hold")

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	wantMode(t, h, "command_hold")
	calls.take()

	postActionWithPhase(h, t, "voice.command", "stop")
	if got := calls.take(); !slices.Contains(got, "stop command_recognition") {
		t.Fatalf("G up must stop the hold's pipeline; calls %v", got)
	}
	commandPipelineStopped(h)
	wantMode(t, h, "idle")
}

// T down, G down, G up, T up: the hold covers the dictation only while G is
// down. Its release returns to the dictation (Esc-cancel is bound to that
// mode) and never closes the dictation overlay or the base under it.
func TestHoldInsideDictationReturnsToDictation(t *testing.T) {
	for _, base := range []string{"idle", "continuous_awake"} {
		t.Run(base, func(t *testing.T) {
			h := newTestHost()
			resetState(h)
			calls := watchPipelines(h)
			if base != "idle" {
				h.setVoiceMode(base)
				h.sess.SetCmdRecActive()
			}

			postActionWithPhase(h, t, "voice.dictation", "start")
			postActionWithPhase(h, t, "voice.command", "start")
			wantMode(t, h, "command_hold")
			postActionWithPhase(h, t, "voice.command", "stop")
			commandPipelineStopped(h)
			wantMode(t, h, "dictation")

			calls.take()
			holdDictationLong(h)
			postActionWithPhase(h, t, "voice.dictation", "stop")
			wantMode(t, h, base)
			restarted := slices.Contains(calls.take(), "run command_recognition")
			if continuous := base != "idle"; restarted != continuous {
				t.Fatalf("T up restarted command_recognition = %v, want %v", restarted, continuous)
			}
		})
	}
}

// speakStopPhrase is what the command engine hears when the user ends a
// speak session by voice.
func speakStopPhrase(h *Host) {
	h.handleCommandTranscript(strings.Join(speech.StopPhrases[0], " "), true, nil, nil, nil, nil)
}

// Push-to-talk: hold G, say "speak", release G. The speak session listens
// for its stop phrase on the hold's command_recognition run, so G up must
// leave that run to it; the stop phrase then ends both.
func TestSpeakStartedUnderAHoldOutlivesTheRelease(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)

	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.speak", "start")
	wantMode(t, h, "dictation")
	calls.take()

	postActionWithPhase(h, t, "voice.command", "stop")
	if got := calls.take(); slices.Contains(got, "stop command_recognition") {
		t.Fatalf("G up stopped the run a live speak session listens on; calls %v", got)
	}
	wantMode(t, h, "dictation")

	speakStopPhrase(h)
	if h.sess.GetSpeakDictating() {
		t.Fatal("the stop phrase did not end the speak session")
	}
	if got := calls.take(); !slices.Contains(got, "stop command_recognition") {
		t.Fatalf("ending speak must stop the run it inherited; calls %v", got)
	}
	wantMode(t, h, "idle")
}

// Hold G, say "speak", say the stop phrase while G is still down: the run is
// still the hold's, so speak leaves it running, and G up stops it.
func TestSpeakEndedUnderAHoldLeavesTheHoldsRun(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)

	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.speak", "start")
	calls.take()
	speakStopPhrase(h)
	if got := calls.take(); slices.Contains(got, "stop command_recognition") {
		t.Fatalf("speak stopped the run of a hold still down; calls %v", got)
	}
	wantMode(t, h, "command_hold")

	postActionWithPhase(h, t, "voice.command", "stop")
	if got := calls.take(); !slices.Contains(got, "stop command_recognition") {
		t.Fatalf("G up must stop the hold's run; calls %v", got)
	}
	commandPipelineStopped(h)
	wantMode(t, h, "idle")
}

// A speak session started outside a hold, then a G tap: the release must not
// take the speak session's run, and the mode returns to the dictation.
func TestHoldDuringSpeakLeavesTheSpeakRun(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)

	postActionWithPhase(h, t, "voice.speak", "start")
	postActionWithPhase(h, t, "voice.command", "start")
	calls.take()
	postActionWithPhase(h, t, "voice.command", "stop")
	if got := calls.take(); slices.Contains(got, "stop command_recognition") {
		t.Fatalf("G up stopped the speak session's run; calls %v", got)
	}
	commandPipelineStopped(h)
	wantMode(t, h, "dictation")
	if !h.sess.GetSpeakDictating() {
		t.Fatal("speak session ended")
	}
}
