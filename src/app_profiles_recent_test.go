package main

import (
	"fmt"
	"strings"
	"testing"
)

// Refocusing an app must MOVE it to the front, not add a second entry —
// otherwise a few alt-tabs between two apps fill the ring with two names and
// evict everything the user might actually want to set a profile for.
func TestRecentAppsDeduplicatesAndOrders(t *testing.T) {
	h := newTestHost()
	restore := h.recentApps
	t.Cleanup(func() { h.recentApps = restore })
	h.recentApps = nil

	for _, id := range []string{"a", "b", "c", "b"} {
		h.noteFocusedApp(id)
	}
	got := h.recentlyFocusedApps()
	want := []string{"b", "c", "a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ring = %v, want %v", got, want)
	}
}

func TestRecentAppsCapAndEmpty(t *testing.T) {
	h := newTestHost()
	restore := h.recentApps
	t.Cleanup(func() { h.recentApps = restore })
	h.recentApps = nil

	h.noteFocusedApp("")
	if len(h.recentlyFocusedApps()) != 0 {
		t.Error("empty bundle id was recorded")
	}
	for i := 0; i < recentAppsCap+5; i++ {
		h.noteFocusedApp(fmt.Sprintf("app%02d", i))
	}
	got := h.recentlyFocusedApps()
	if len(got) != recentAppsCap {
		t.Fatalf("ring length = %d, want cap %d", len(got), recentAppsCap)
	}
	if got[0] != fmt.Sprintf("app%02d", recentAppsCap+4) {
		t.Errorf("newest = %q, want the last one noted", got[0])
	}
}

// The table is the union of shipped rows, overrides and the ring, with recent
// apps first in ring order. An app that only appears because it was focused
// gets a row that resolves to prose — the point is that it can be CHANGED, not
// that anything already has an opinion about it.
func TestAppProfileRowsUnionAndOrder(t *testing.T) {
	h := newTestHost()
	seedTestAppTraits(h, t)
	restoreRing, restoreOv := h.recentApps, h.appProfileOverrides
	t.Cleanup(func() { h.recentApps, h.appProfileOverrides = restoreRing, restoreOv })
	h.appProfileOverrides = map[string]string{}
	h.recentApps = []string{"com.example.zzz", "com.hnc.Discord"}

	rows := h.appProfileRows()
	if len(rows) < 2 {
		t.Fatalf("only %d rows", len(rows))
	}
	if rows[0].BundleID != "com.example.zzz" || !rows[0].Recent {
		t.Errorf("row 0 = %+v, want the newest recent app first", rows[0])
	}
	if rows[1].BundleID != "com.hnc.Discord" || !rows[1].Recent {
		t.Errorf("row 1 = %+v, want the second recent app", rows[1])
	}
	// Ring order beats alphabetical: "zzz" sorts last by name and first here.
	if rows[0].Profile != "" {
		t.Errorf("unknown recent app resolved to %q, want prose", rows[0].Profile)
	}
	if !rows[1].Shipped || rows[1].Profile != profileChat {
		t.Errorf("Discord row = %+v, want shipped chat", rows[1])
	}
	// Non-recent shipped rows follow, sorted by display name.
	var tail []string
	for _, r := range rows {
		if !r.Recent {
			tail = append(tail, strings.ToLower(r.Name))
		}
	}
	for i := 1; i < len(tail); i++ {
		if tail[i-1] > tail[i] {
			t.Errorf("non-recent rows not name-sorted at %d: %q then %q", i, tail[i-1], tail[i])
		}
	}
}

// A shipped row for an app that is not installed is dropped — it can only
// render as a bare bundle identifier, which is a row nobody can act on. An
// override or a recent focus keeps it, because both are evidence the app
// matters to this user.
func TestShippedRowsFilteredToInstalled(t *testing.T) {
	h := newTestHost()
	seedTestAppTraits(h, t)
	restoreRing, restoreOv, restoreNames := h.recentApps, h.appProfileOverrides, h.appNamesByID
	t.Cleanup(func() {
		h.recentApps, h.appProfileOverrides, h.appNamesByID = restoreRing, restoreOv, restoreNames
	})
	h.recentApps = []string{"com.apple.Terminal"}
	h.appProfileOverrides = map[string]string{"com.googlecode.iterm2": profileProse}
	// Only Slack is "installed"; Terminal is recent, iTerm2 is overridden.
	h.appNamesByID = map[string]string{"com.tinyspeck.slackmacgap": "slack"}

	got := map[string]bool{}
	for _, r := range h.appProfileRows() {
		got[r.BundleID] = true
	}
	for _, want := range []string{
		"com.tinyspeck.slackmacgap", // installed
		"com.apple.Terminal",        // recent
		"com.googlecode.iterm2",     // overridden
	} {
		if !got[want] {
			t.Errorf("%s missing from rows", want)
		}
	}
	if got["com.hnc.Discord"] {
		t.Error("Discord is neither installed, recent nor overridden — should be dropped")
	}
}

// An empty name index means the collection has not loaded yet, not that the
// machine has no apps. Hiding the entire table on a slow load is worse than
// showing rows for apps the user lacks.
func TestEmptyNameIndexShowsEverything(t *testing.T) {
	h := newTestHost()
	restoreNames, restoreRing := h.appNamesByID, h.recentApps
	t.Cleanup(func() { h.appNamesByID, h.recentApps = restoreNames, restoreRing })
	h.appNamesByID = map[string]string{}
	h.recentApps = nil

	seedTestAppTraits(h, t)
	want := len(h.appsWithTraitOpinion())
	if n := len(h.appProfileRows()); n != want {
		t.Errorf("rows = %d, want all %d trait-derived rows", n, want)
	}
}
