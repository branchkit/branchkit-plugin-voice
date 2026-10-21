package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// A shortcut recording abandoned mid-capture (Settings tab closed) must not
// hold every hotkey paused: it ends itself and releases the pause, once.
func TestAnAbandonedRecordingReleasesThePause(t *testing.T) {
	prev := captureTimeout
	captureTimeout = 30 * time.Millisecond
	t.Cleanup(func() { captureTimeout = prev })
	h := newHost(branchkit.NewDetachedPlugin())
	var holds, releases atomic.Int32
	h.holdPause = func() { holds.Add(1) }
	h.releasePause = func() { releases.Add(1) }

	if err := h.handleEditorRecordStart(nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	if holds.Load() != 1 || releases.Load() != 1 {
		t.Fatalf("holds=%d releases=%d, want 1 and 1", holds.Load(), releases.Load())
	}
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()
	if h.edState.editor.Recording {
		t.Fatal("the expired recording is still recording")
	}
}
