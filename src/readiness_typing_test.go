package main

import (
	"strings"
	"testing"
)

// The Typing row on the readiness panel is derived from what the shell has
// reported: nothing until it has, "Ready" when both delivery bits are good,
// and the specific block otherwise — post-event trust first, because that is
// the one a user can fix, then secure input.
func TestTypingRowFollowsTheShellsReport(t *testing.T) {
	h := newTestHost()
	reset := func() {
		h.typingMu.Lock()
		h.typingKnown = map[string]bool{}
		h.typingMu.Unlock()
	}
	reset()
	defer reset()

	if row, ok := h.typingItem(); ok {
		t.Fatalf("no report yet must produce no row, got %+v", row)
	}
	if got := h.readyTitle(); got != "Voice ready" {
		t.Fatalf("unreported = plain ready title, got %q", got)
	}

	h.handlePermissionChanged("microphone", false) // not ours
	if row, ok := h.typingItem(); ok {
		t.Fatalf("an unrelated permission must not create the row, got %+v", row)
	}

	h.handlePermissionChanged("post_event", true)
	h.handlePermissionChanged("secure_input", true)
	if row, ok := h.typingItem(); !ok || row.Title != "Typing" || row.Subtitle == nil || *row.Subtitle != "Ready ✓" {
		t.Fatalf("both good = Ready, got %+v", row)
	}

	h.handlePermissionChanged("secure_input", false)
	if r := h.typingBlockReason(); !strings.Contains(r, "password field") {
		t.Fatalf("secure input names the password field, got %q", r)
	}
	h.handlePermissionChanged("post_event", false)
	if r := h.typingBlockReason(); !strings.Contains(r, "Accessibility") {
		t.Fatalf("post-event refusal wins and names the fix, got %q", r)
	}
	if got := h.readyTitle(); got != "Voice ready — typing blocked" {
		t.Fatalf("blocked title, got %q", got)
	}
	if row, ok := h.typingItem(); !ok || row.Subtitle == nil || !strings.Contains(*row.Subtitle, "Blocked") {
		t.Fatalf("blocked row, got %+v", row)
	}
}
