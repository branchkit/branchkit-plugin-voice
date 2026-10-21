package main

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// modeWrites records each mode write and fails the first `failures` of them.
type modeWrites struct {
	mu       sync.Mutex
	failures int
	written  []string
}

func (w *modeWrites) write(mode string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = append(w.written, mode)
	if w.failures > 0 {
		w.failures--
		return errors.New("timed out")
	}
	return nil
}

func (w *modeWrites) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.written...)
}

var shortModeRetries = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}

func waitWrites(t *testing.T, w *modeWrites, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := w.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	return w.snapshot()
}

// A mode write that fails is re-asserted until it lands, so voice and the
// platform cannot stay in different modes.
func TestFailedModeWriteIsReasserted(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.modeRetryDelays = shortModeRetries
	w := &modeWrites{failures: 2}
	h.writeMode = w.write

	h.setVoiceMode("continuous_awake")
	got := waitWrites(t, w, 3)
	if len(got) != 3 {
		t.Fatalf("want the write, two failed retries' worth, then success: %v", got)
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(w.snapshot()); n != 3 {
		t.Fatalf("retries continued after the write landed: %d writes", n)
	}
}

// retryQueue stands in for time.AfterFunc: it holds each scheduled retry so a
// test runs them itself, in order, with no timers to wait out.
type retryQueue struct {
	mu      sync.Mutex
	pending []func()
	delays  []time.Duration
}

func (q *retryQueue) after(d time.Duration, f func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, f)
	q.delays = append(q.delays, d)
}

// runNext runs the oldest scheduled retry, reporting false when none is left.
func (q *retryQueue) runNext() bool {
	q.mu.Lock()
	if len(q.pending) == 0 {
		q.mu.Unlock()
		return false
	}
	f := q.pending[0]
	q.pending = q.pending[1:]
	q.mu.Unlock()
	f()
	return true
}

// A retry of a mode the user has already left must not write it: the newer
// transition's state stands.
func TestSupersededModeRetryDoesNotWrite(t *testing.T) {
	h := newTestHost()
	resetState(h)
	q := &retryQueue{}
	h.modeRetryAfter = q.after
	w := &modeWrites{failures: 1}
	h.writeMode = w.write

	h.setVoiceMode("continuous_awake") // fails, retry scheduled
	h.setVoiceMode("idle")             // lands
	if !q.runNext() {
		t.Fatal("the failed write scheduled no retry")
	}
	got := w.snapshot()
	if len(got) != 2 || got[1] != "idle" {
		t.Fatalf("the stale retry wrote after the newer mode: %v", got)
	}
	if q.runNext() {
		t.Fatal("a superseded retry scheduled another")
	}
}

// A write that never lands stops after the last retry rather than looping,
// waiting each configured delay in turn.
func TestModeRetriesGiveUp(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.modeRetryDelays = shortModeRetries
	q := &retryQueue{}
	h.modeRetryAfter = q.after
	w := &modeWrites{failures: 100}
	h.writeMode = w.write

	h.setVoiceMode("continuous_awake")
	runs := 0
	for q.runNext() {
		runs++
		if runs > 10 {
			t.Fatal("retries do not stop")
		}
	}
	if runs != len(shortModeRetries) {
		t.Fatalf("ran %d retries, want %d", runs, len(shortModeRetries))
	}
	if n := len(w.snapshot()); n != 1+len(shortModeRetries) {
		t.Fatalf("want 1 write + %d retries, got %d", len(shortModeRetries), n)
	}
	if !reflect.DeepEqual(q.delays, shortModeRetries) {
		t.Fatalf("retry delays = %v, want %v", q.delays, shortModeRetries)
	}
}
