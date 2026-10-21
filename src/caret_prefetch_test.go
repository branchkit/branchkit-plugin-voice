package main

import (
	"testing"
	"time"
)

func setPendingCaret(p *caretPrefetch) {
	caretMu.Lock()
	caretPending = p
	caretMu.Unlock()
}

func TestCaretPrefetchIsTakenOnce(t *testing.T) {
	h := &Host{}
	p := &caretPrefetch{started: time.Now(), done: make(chan struct{})}
	setPendingCaret(p)
	if got := h.takeCaretPrefetch(); got != p {
		t.Fatalf("first take = %v, want the pending read", got)
	}
	if got := h.takeCaretPrefetch(); got != nil {
		t.Fatalf("second take = %v, want nil: one read serves one transcript", got)
	}
}

func TestStaleCaretPrefetchIsNotUsed(t *testing.T) {
	h := &Host{}
	setPendingCaret(&caretPrefetch{
		started: time.Now().Add(-caretPrefetchMaxAge - time.Second),
		done:    make(chan struct{}),
	})
	if got := h.takeCaretPrefetch(); got != nil {
		t.Fatalf("take = %v, want nil: a read from a dictation whose transcript never came", got)
	}
	if got := h.takeCaretPrefetch(); got != nil {
		t.Fatal("a stale read must also be cleared")
	}
}
