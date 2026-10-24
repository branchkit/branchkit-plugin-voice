package main

import (
	"strings"
	"testing"
)

// The OS's microphone switch, as the host reports it: no row until it has,
// "Ready" when on, and when off a row and a title that say so, with the fix
// in this OS's words. Typing's row is not created by it.
func TestMicrophoneRowFollowsTheHostsReport(t *testing.T) {
	h := newTestHost()

	if _, ok := h.microphoneItem(); ok {
		t.Fatal("no report yet must produce no microphone row")
	}
	if got := h.microphoneBlockReason(); got != "" {
		t.Fatalf("unreported = not blocked, got %q", got)
	}

	h.handlePermissionChanged("microphone", true)
	if row, ok := h.microphoneItem(); !ok || row.Subtitle == nil || *row.Subtitle != "Ready ✓" {
		t.Fatalf("granted = Ready, got %+v", row)
	}
	if _, ok := h.typingItem(); ok {
		t.Fatal("the microphone report must not create the Typing row")
	}

	h.handlePermissionChanged("microphone", false)
	if row, ok := h.microphoneItem(); !ok || row.Subtitle == nil || !strings.HasPrefix(*row.Subtitle, "Not allowed") {
		t.Fatalf("denied = Not allowed, got %+v", row)
	}
	if got := h.readyTitle(); got != "Voice ready — microphone not allowed" {
		t.Fatalf("blocked title, got %q", got)
	}
	found := false
	for _, it := range h.readinessSections()[0].Items {
		if it.ID == "microphone" {
			found = true
		}
	}
	if !found {
		t.Fatal("the readiness panel must carry the microphone row")
	}
	// A hold that starts blocked says so (and does not panic without a HUD).
	h.holdBegan()
}

func TestMicrophoneBlockedTextNamesEachOSsSwitch(t *testing.T) {
	for goos, want := range map[string]string{
		"windows": "Let desktop apps access your microphone",
		"darwin":  "when macOS asks",
		"linux":   "microphone privacy setting",
	} {
		if got := microphoneBlockedText(goos); !strings.Contains(got, want) {
			t.Errorf("%s: %q lacks %q", goos, got, want)
		}
	}
}
