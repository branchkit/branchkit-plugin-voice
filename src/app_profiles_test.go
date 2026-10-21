package main

import "testing"

// seedTestAppTraits installs the traits the apps plugin publishes, so the
// profile tests exercise the real resolution path (traits → opinion) rather
// than a table that no longer exists. Mirrors core_apps.json for the handful of
// apps these tests name.
func seedTestAppTraits(h *Host, t *testing.T) {
	t.Helper()
	restore, restoreLoaded := h.appTraitsByID, h.appTraitsLoaded
	t.Cleanup(func() { h.appTraitsByID, h.appTraitsLoaded = restore, restoreLoaded })
	h.appTraitsByID = map[string][]string{
		"com.tinyspeck.slackmacgap": {"chat"},
		"com.hnc.Discord":           {"chat"},
		"com.apple.Terminal":        {"terminal"},
		"dev.warp.Warp-Stable":      {"terminal"},
	}
	h.appTraitsLoaded = true
}

// Precedence is the whole point of this file:
//
//	gate-declared profile  >  per-app mapping  >  prose
func TestResolveDictationProfilePrecedence(t *testing.T) {
	h := newTestHost()
	seedTestAppTraits(h, t)
	tests := []struct {
		name, gate, app, want string
	}{
		{"nothing declared is prose", "", "", ""},
		{"gate alone wins", profileQuery, "", profileQuery},
		{"app mapping alone applies", "", "com.tinyspeck.slackmacgap", profileChat},
		{"unknown app is prose", "", "com.example.nothing", ""},
		// The case the precedence exists for: a palette open inside Slack is a
		// query, not a chat message. The momentary claim beats the standing one.
		{"gate beats app mapping", profileQuery, "com.tinyspeck.slackmacgap", profileQuery},
		// And a gate declaring prose inside a chat app must still win, or
		// "prose" would be indistinguishable from "unstamped".
		{"explicit prose gate beats app mapping", profileProse, "com.hnc.Discord", profileProse},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.resolveDictationProfile(tc.gate, tc.app); got != tc.want {
				t.Errorf("resolveDictationProfile(%q, %q) = %q, want %q",
					tc.gate, tc.app, got, tc.want)
			}
		})
	}
}

// An override replaces the shipped row; clearing it restores the shipped row
// rather than falling to prose. That distinction is the reason "reset" is
// modelled as deleting the override instead of writing prose into it.
func TestAppProfileOverrideAndReset(t *testing.T) {
	h := newTestHost()
	const slack = "com.tinyspeck.slackmacgap"
	seedTestAppTraits(h, t)
	restore := h.appProfileOverrides
	t.Cleanup(func() { h.appProfileOverrides = restore })
	h.appProfileOverrides = map[string]string{}

	if got := h.appProfile(slack); got != profileChat {
		t.Fatalf("shipped default = %q, want %q", got, profileChat)
	}
	if h.appProfileIsOverridden(slack) {
		t.Error("no override set, but reported as overridden")
	}

	if err := h.setAppProfileOverride(slack, profileProse); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if got := h.appProfile(slack); got != profileProse {
		t.Errorf("after override = %q, want %q", got, profileProse)
	}
	if !h.appProfileIsOverridden(slack) {
		t.Error("override set, but not reported as overridden")
	}

	if err := h.setAppProfileOverride(slack, ""); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if got := h.appProfile(slack); got != profileChat {
		t.Errorf("after reset = %q, want shipped %q", got, profileChat)
	}
	if h.appProfileIsOverridden(slack) {
		t.Error("override cleared, but still reported as overridden")
	}
}

// Every rule must name a profile this build understands. A typo here would
// silently resolve to prose through applyDictationProfile's default, which is
// exactly the kind of quiet nothing that survives review.
func TestTraitProfilesAreKnown(t *testing.T) {
	if len(traitProfiles) == 0 {
		t.Fatal("traitProfiles is empty — voice has no opinion about any trait")
	}
	seen := map[string]bool{}
	for _, rule := range traitProfiles {
		if !isKnownProfile(rule.Profile) {
			t.Errorf("trait %q maps to unknown profile %q", rule.Trait, rule.Profile)
		}
		// A prose rule is a no-op that reads like a decision; prose is already
		// what an app with no matching group resolves to.
		if rule.Profile == profileProse {
			t.Errorf("trait %q maps to prose, which is the default — drop the rule", rule.Trait)
		}
		if seen[rule.Trait] {
			t.Errorf("trait %q appears twice — the second rule is unreachable", rule.Trait)
		}
		seen[rule.Trait] = true
	}
}

// An app can have several traits voice has rules for, so the ORDER of
// traitProfiles decides the answer. This is the test that would fail if
// someone turned the slice back into a map.
func TestTraitProfileFirstMatchWins(t *testing.T) {
	h := newTestHost()
	restore, restoreLoaded := h.appTraitsByID, h.appTraitsLoaded
	t.Cleanup(func() { h.appTraitsByID, h.appTraitsLoaded = restore, restoreLoaded })
	// Emacs: an editor, a terminal, a mail client and a browser.
	h.appTraitsByID = map[string][]string{
		"org.gnu.Emacs": {"browser", "chat", "editor", "mail", "terminal"},
	}
	h.appTraitsLoaded = true

	profile, via := h.traitProfileFor("org.gnu.Emacs")
	if profile != profileChat {
		t.Errorf("profile = %q, want %q", profile, profileChat)
	}
	// "terminal" is first in traitProfiles, so it must be the reason reported —
	// even though "chat" would produce the same profile. The settings table
	// shows this string, and naming the wrong rule is a wrong explanation.
	if via != "terminal" {
		t.Errorf("via = %q, want %q (first matching rule in traitProfiles)", via, "terminal")
	}
}

// An app with no trait voice cares about resolves to prose with no reason given.
func TestTraitProfileNoOpinion(t *testing.T) {
	h := newTestHost()
	restore, restoreLoaded := h.appTraitsByID, h.appTraitsLoaded
	t.Cleanup(func() { h.appTraitsByID, h.appTraitsLoaded = restore, restoreLoaded })
	h.appTraitsByID = map[string][]string{"com.example.thing": {"editor"}}
	h.appTraitsLoaded = true

	if profile, via := h.traitProfileFor("com.example.thing"); profile != "" || via != "" {
		t.Errorf("traitProfileFor = (%q, %q), want empty — voice has no editor rule", profile, via)
	}
}

// A user override answers without consulting traits, and reports no reason:
// the user set it, so there is nothing to explain.
func TestOverrideReportsNoTraitReason(t *testing.T) {
	h := newTestHost()
	seedTestAppTraits(h, t)
	restore := h.appProfileOverrides
	t.Cleanup(func() { h.appProfileOverrides = restore })
	h.appProfileOverrides = map[string]string{"com.apple.Terminal": profileProse}

	profile, via := h.appProfileWithReason("com.apple.Terminal")
	if profile != profileProse {
		t.Errorf("profile = %q, want the override %q", profile, profileProse)
	}
	if via != "" {
		t.Errorf("via = %q, want empty — the user set this row, not a trait", via)
	}
}

// The settings table shows shipped rows and user rows together, with the
// override flag set only where there is something to reset.
func TestKnownAppProfilesUnion(t *testing.T) {
	h := newTestHost()
	seedTestAppTraits(h, t)
	restore := h.appProfileOverrides
	t.Cleanup(func() { h.appProfileOverrides = restore })
	h.appProfileOverrides = map[string]string{
		"com.tinyspeck.slackmacgap": profileProse, // override of a trait-derived row
		"com.example.custom":        profileChat,  // a row no trait produced
	}
	rows := h.knownAppProfiles()
	byID := map[string]appProfileRow{}
	for _, r := range rows {
		byID[r.BundleID] = r
	}
	if r := byID["com.tinyspeck.slackmacgap"]; !r.Shipped || !r.Overridden || r.Profile != profileProse {
		t.Errorf("overridden shipped row = %+v", r)
	}
	if r := byID["com.example.custom"]; r.Shipped || !r.Overridden || r.Profile != profileChat {
		t.Errorf("user-only row = %+v", r)
	}
	if r := byID["com.hnc.Discord"]; !r.Shipped || r.Overridden || r.Via != "chat" {
		t.Errorf("untouched trait-derived row = %+v (want Via=chat)", r)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].BundleID >= rows[i].BundleID {
			t.Errorf("rows not sorted at %d: %q then %q", i, rows[i-1].BundleID, rows[i].BundleID)
		}
	}
}

// After a FAILED read of the saved overrides the map is empty, and every edit
// saves the whole map, so saving would erase the user's other rows. The edit
// reads again first, and refuses (persisting nothing) if that fails too.
func TestAppProfileEditRefusedWhileSavedOverridesAreUnreadable(t *testing.T) {
	h := newTestHost() // detached: every read fails
	persisted := 0
	h.persistAppProfiles = func(map[string]string) error { persisted++; return nil }
	h.loadAppProfiles()

	if err := h.setAppProfileOverride("com.tinyspeck.slackmacgap", profileProse); err == nil {
		t.Fatal("an edit on top of an unread map must be refused")
	}
	if persisted != 0 {
		t.Fatalf("nothing may be persisted, got %d writes", persisted)
	}
}
