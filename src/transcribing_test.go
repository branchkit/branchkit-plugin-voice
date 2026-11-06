package main

import (
	"sync"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// outputRecorder is the discovery channel as the platform would see it:
// every output.state document and output.clear, in order.
type outputRecorder struct {
	mu     sync.Mutex
	events []string // "state:<kind>:<phrase>" or "clear"
}

func (r *outputRecorder) state(req branchkit.OutputStateRequest) (*branchkit.OutputStateResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "state:"+req.State.Kind+":"+req.State.Phrase)
	return &branchkit.OutputStateResponse{}, nil
}

func (r *outputRecorder) clear(branchkit.OutputClearRequest) (*branchkit.OutputClearResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "clear")
	return &branchkit.OutputClearResponse{}, nil
}

func (r *outputRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

const transcribingState = "state:" + branchkit.OutputKindProgress + ":Transcribing…"

// releasedDictation holds Option+T past a tap and lets go: the recording is
// draining, its transcript still to come.
func releasedDictation(t *testing.T) (*Host, *outputRecorder) {
	t.Helper()
	h := newTestHost()
	// The model is up, so the hold opens no "starting dictation" overlay.
	h.dictationReady.Store(true)
	rec := &outputRecorder{}
	h.hud.SetOutputSink(rec.state, rec.clear)
	postActionWithPhase(h, t, "voice.dictation", "start")
	waitPast(tapMS)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	got := rec.snapshot()
	if len(got) == 0 || got[len(got)-1] != transcribingState {
		t.Fatalf("release did not publish the transcribing document: %v", got)
	}
	return h, rec
}

// assertSuperseded checks the transcribing document was said once and is no
// longer the channel's state: cleared, or replaced by a later document.
func assertSuperseded(t *testing.T, rec *outputRecorder) {
	t.Helper()
	got := rec.snapshot()
	if got[len(got)-1] == transcribingState {
		t.Fatalf("the transcribing document was left standing: %v", got)
	}
	n := 0
	for _, e := range got {
		if e == transcribingState {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("transcribing published %d times, want once (a renderer that speaks would repeat it): %v", n, got)
	}
}

func assertCleared(t *testing.T, rec *outputRecorder) {
	t.Helper()
	assertSuperseded(t, rec)
	if got := rec.snapshot(); got[len(got)-1] != "clear" {
		t.Fatalf("want the channel cleared, got %v", got)
	}
}

// The transcript landing ends the transcribing document. Here typing fails
// (a detached plugin types nothing) and that problem is what stands.
func TestReleasedDictationSaysTranscribingUntilTheTranscript(t *testing.T) {
	h, rec := releasedDictation(t)
	h.handlePipelineTranscript(dictationTranscriptWith(t, map[string]any{"text": "Hello."}))
	assertSuperseded(t, rec)
}

// A transcript a correction rule empties types nothing and leaves nothing
// standing.
func TestReleasedDictationSaysTranscribingUntilARuleEmptiesIt(t *testing.T) {
	h, rec := releasedDictation(t)
	h.handlePipelineTranscript(dictationTranscriptWith(t, map[string]any{"text": "So."}))
	assertCleared(t, rec)
}

// An empty transcript (silence, a tap) is an answer too.
func TestReleasedDictationSaysTranscribingUntilAnEmptyResult(t *testing.T) {
	h, rec := releasedDictation(t)
	h.handlePipelineTranscript(dictationTranscriptWith(t, map[string]any{"text": ""}))
	assertCleared(t, rec)
}

// A pipeline that stops with no transcript at all: the dictation is lost,
// and its own notice says so; the transcribing document must not outlive it.
func TestReleasedDictationSaysTranscribingUntilItIsLost(t *testing.T) {
	h, rec := releasedDictation(t)
	h.sess.FinishDictationIfDraining()
	assertCleared(t, rec)
}

// Pressing Option+T again while the last transcript is still on its way: the
// new recording is what is under way.
func TestNewDictationEndsTheLastOnesTranscribing(t *testing.T) {
	h, rec := releasedDictation(t)
	postActionWithPhase(h, t, "voice.dictation", "start")
	assertCleared(t, rec)
}
