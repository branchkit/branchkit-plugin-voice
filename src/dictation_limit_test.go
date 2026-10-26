package main

import (
	"slices"
	"testing"
	"time"
)

// limitTimers captures each scheduled dictation limit, to fire by hand.
func limitTimers(h *Host) *[]func() {
	var fired []func()
	h.dictationLimitAfter = func(d time.Duration, f func()) {
		if d != dictationLimit {
			panic("the limit is scheduled at dictationLimit")
		}
		fired = append(fired, f)
	}
	return &fired
}

// A dictation still recording at the limit is stopped, what it holds goes to
// be typed, and the user is told why. The key's later release ends nothing.
func TestDictationStopsAtTheLimit(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	limits := limitTimers(h)
	notes := notices(h)

	postActionWithPhase(h, t, "voice.dictation", "start")
	if len(*limits) != 1 {
		t.Fatalf("a recording schedules one limit, got %d", len(*limits))
	}
	calls.take()

	(*limits)[0]()
	if got := calls.take(); !slices.Contains(got, "stop dictation") {
		t.Fatalf("the limit must stop the dictation pipeline; calls %v", got)
	}
	if len(*notes) != 1 {
		t.Fatalf("the limit must say why dictation stopped; notices %v", *notes)
	}
	wantMode(t, h, "idle")

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	if got := calls.take(); len(got) != 0 {
		t.Fatalf("the release after the limit must end nothing; calls %v", got)
	}
}

// The limit of a recording that already ended does not touch the next one.
func TestDictationLimitIgnoresALaterRecording(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	limits := limitTimers(h)
	notes := notices(h)

	postActionWithPhase(h, t, "voice.dictation", "start")
	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	h.sess.FinishDictation()
	postActionWithPhase(h, t, "voice.dictation", "start")
	calls.take()

	(*limits)[0]()
	if got := calls.take(); len(got) != 0 || len(*notes) != 0 {
		t.Fatalf("an old limit acted on a new recording; calls %v, notices %v", got, *notes)
	}
	wantMode(t, h, "dictation")
}

// A speak session, which has no key to release, stops at the limit too.
func TestSpeakStopsAtTheLimit(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	limits := limitTimers(h)
	notes := notices(h)

	postActionWithPhase(h, t, "voice.speak", "start")
	calls.take()
	(*limits)[len(*limits)-1]()
	if h.sess.GetSpeakDictating() {
		t.Fatal("the limit did not end the speak session")
	}
	if got := calls.take(); !slices.Contains(got, "stop dictation") {
		t.Fatalf("the limit must stop the dictation pipeline; calls %v", got)
	}
	if len(*notes) != 1 {
		t.Fatalf("the limit must say why speak stopped; notices %v", *notes)
	}
	wantMode(t, h, "idle")
}
