package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

// Per-app dictation profiles — the standing default for "what is an utterance
// in THIS application", as opposed to the momentary one a mode declares.
//
// The platform stamps `focused_app` (a bundle identifier) on the transcript and
// stops there, deliberately: it reports WHICH app had focus and this file — the
// engine owner — decides what that means. An app-to-behaviour table inside the
// platform would be plugin policy in generic infrastructure.
//
// Storage is OVERRIDES ONLY. The shipped table below stays in code and is never
// copied into the user's record, which is the difference from the corrections
// list and is deliberate:
//
//   - Corrections materialise because a user edits, toggles and deletes
//     individual shipped rules, so each one needs an identity that survives.
//   - Here the edit IS the override, and "reset" is deleting it. Nothing needs
//     a per-row identity.
//
// The payoff is that a shipped table corrected in a later release actually
// reaches existing users, instead of being frozen at whatever shipped the day
// they first ran the app. It also removes the seed-version migration entirely.
const appProfilesCollection = "plugin.voice.app_profiles"

// The shipped `bundle_id → profile` table that used to live here is GONE, and
// its fourteen rows did not shrink — they moved. What kind of application each
// one is now lives in the apps plugin's core_apps.json as app traits; what
// that means for punctuation is the two-row `traitProfiles` list in
// app_traits.go.
//
// The reason is not tidiness. Those rows were factual claims about fourteen
// applications, asserted by the plugin with the least standing to make them —
// ten of them about software that was not installed. Relocating them puts app
// facts in the application registry, which already ships curated app knowledge
// nobody finds presumptuous, because facts are what it is for.
//
// What REMAINS here is the user's per-app overrides, unchanged.

// resolveDictationProfile implements the precedence the design doc fixes:
//
//	gate-declared profile  >  per-app mapping  >  prose
//
// A gate wins because it is more specific IN TIME. "A palette is open right
// now" is a stronger claim about this particular utterance than "you are
// usually writing prose in this app", which is the standing default the gate
// overrides. Returns "" for prose so the caller's existing default stands and
// `applyDictationProfile` keeps its single fallback.
func (h *Host) resolveDictationProfile(gateProfile, focusedApp string) string {
	profile, _ := h.dictationProfileWithSource(gateProfile, focusedApp)
	return profile
}

// dictationProfileWithSource is resolveDictationProfile plus what decided it,
// for the dictation record: "mode" (a gate declared it), "app" (the user's
// own per-app setting), a trait name (the app's kind), or "" (the default).
func (h *Host) dictationProfileWithSource(gateProfile, focusedApp string) (profile, from string) {
	if gateProfile != "" {
		return gateProfile, "mode"
	}
	if h.appProfileIsOverridden(focusedApp) {
		return h.appProfile(focusedApp), "app"
	}
	return h.appProfileWithReason(focusedApp)
}

// appProfile is the mapping half alone: user override, then the trait opinion,
// then "" (prose).
func (h *Host) appProfile(bundleID string) string {
	profile, _ := h.appProfileWithReason(bundleID)
	return profile
}

// appProfileWithReason is appProfile plus the trait that decided it, or "" when
// the answer came from the user's own override (which needs no explanation —
// they set it) or from the prose default (which explains itself by being the
// default). Only a trait-derived answer is surprising enough to need a reason.
func (h *Host) appProfileWithReason(bundleID string) (profile string, via string) {
	if bundleID == "" {
		return "", ""
	}
	h.appProfilesMu.RLock()
	override, ok := h.appProfileOverrides[bundleID]
	h.appProfilesMu.RUnlock()
	if ok {
		return override, ""
	}
	return h.traitProfileFor(bundleID)
}

// appProfileIsOverridden reports whether the user has set this row explicitly,
// which the settings table needs in order to offer "reset" only where there is
// something to reset.
func (h *Host) appProfileIsOverridden(bundleID string) bool {
	h.appProfilesMu.RLock()
	defer h.appProfilesMu.RUnlock()
	_, ok := h.appProfileOverrides[bundleID]
	return ok
}

// setAppProfileOverride records one row. An empty profile CLEARS the override,
// restoring whatever the shipped table says — that is the "reset" path, and it
// is why clearing is not a separate handler.
func (h *Host) setAppProfileOverride(bundleID, profile string) error {
	if bundleID == "" {
		return nil
	}
	// A failed read left the map empty; read again before editing, so this
	// row lands on top of the saved ones instead of replacing them.
	h.appProfilesMu.RLock()
	failed := h.appProfilesLoadErr != nil
	h.appProfilesMu.RUnlock()
	if failed {
		h.loadAppProfiles()
		h.appProfilesMu.RLock()
		err := h.appProfilesLoadErr
		h.appProfilesMu.RUnlock()
		if err != nil {
			return fmt.Errorf("your saved app profiles could not be read (%v), so this "+
				"change was not saved", err)
		}
	}
	h.appProfilesMu.Lock()
	if profile == "" {
		delete(h.appProfileOverrides, bundleID)
	} else {
		h.appProfileOverrides[bundleID] = profile
	}
	h.appProfilesMu.Unlock()
	return h.saveAppProfiles()
}

// knownAppProfiles returns every bundle id with a resolved non-prose profile —
// apps whose TRAITS give them one, plus the user's overrides, union'd and
// sorted for a stable render. An override to prose still appears, because the
// user needs to see (and undo) the row they set.
func (h *Host) knownAppProfiles() []appProfileRow {
	rows := make(map[string]appProfileRow)
	for id, op := range h.appsWithTraitOpinion() {
		rows[id] = appProfileRow{
			BundleID: id, Profile: op.Profile, Shipped: true, Via: op.Via,
		}
	}
	h.appProfilesMu.RLock()
	for id, p := range h.appProfileOverrides {
		r := rows[id]
		r.BundleID, r.Profile, r.Overridden = id, p, true
		rows[id] = r
	}
	h.appProfilesMu.RUnlock()

	out := make([]appProfileRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BundleID < out[j].BundleID })
	return out
}

// appProfileRow is one line of the settings table.
type appProfileRow struct {
	BundleID string `json:"bundle_id"`
	Profile  string `json:"profile"`
	// Shipped means the platform has a standing opinion about this app —
	// today, that its traits resolve to a profile — whether or not the user has
	// overridden it. The name predates traits and still reads correctly: what
	// the software ships, as opposed to what you set.
	Shipped bool `json:"shipped"`
	// Via names the trait that produced the profile, for the settings table to
	// show its work. Empty when the answer did not come from a trait.
	Via string `json:"via,omitempty"`
	// Overridden means the user set this row explicitly, so it can be reset.
	Overridden bool `json:"overridden"`
}

func (h *Host) saveAppProfiles() error {
	h.appProfilesMu.RLock()
	overrides := make(map[string]string, len(h.appProfileOverrides))
	for k, v := range h.appProfileOverrides {
		overrides[k] = v
	}
	h.appProfilesMu.RUnlock()
	return h.persistAppProfiles(overrides)
}

// loadAppProfiles reads the user's overrides. Absent record and parse failure
// both leave the shipped table in force, which is the behaviour we want: a
// corrupt overrides blob should cost the user their customisations, never their
// dictation.
func (h *Host) loadAppProfiles() {
	rec, err := h.plugin.Get(appProfilesCollection, "singleton")
	h.appProfilesMu.Lock()
	h.appProfilesLoadErr = err
	h.appProfilesMu.Unlock()
	if err != nil {
		branchkit.Logf("voice", "app profiles read error: %v", err)
		return
	}
	if rec == nil {
		return
	}
	var wrapper struct {
		Overrides map[string]string `json:"overrides"`
	}
	if err := json.Unmarshal(rec.Payload, &wrapper); err != nil {
		branchkit.Logf("voice", "app profiles parse error: %v", err)
		return
	}
	clean := make(map[string]string, len(wrapper.Overrides))
	for id, p := range wrapper.Overrides {
		// An unknown profile name is dropped rather than stored. It would
		// otherwise fall through `applyDictationProfile`'s default to prose
		// while the settings table showed the invalid name as if it were live.
		if p := strings.TrimSpace(p); isKnownProfile(p) {
			clean[id] = p
		} else {
			branchkit.Logf("voice", "app profiles: dropping unknown profile %q for %s", p, id)
		}
	}
	h.appProfilesMu.Lock()
	h.appProfileOverrides = clean
	h.appProfilesMu.Unlock()
}

// isKnownProfile guards the override store against a name no build understands.
func isKnownProfile(p string) bool {
	switch p {
	case profileProse, profileQuery, profileChat:
		return true
	}
	return false
}

// appOrUnknown names the focused app for a log line. An unstamped transcript
// predates the platform stamp or arrived from `ingest_transcript`, and saying
// so beats an empty field — the same reasoning as profileOrProse.
func appOrUnknown(bundleID string) string {
	if bundleID == "" {
		return "unknown"
	}
	return bundleID
}

// HandleAppProfileSet records or clears one row from the settings table.
//
// ONE handler for both, because they are one operation: an empty profile means
// "no opinion of my own here", which is exactly what deleting the override
// says. A separate reset handler would be a second name for `delete`.
func (h *Host) HandleAppProfileSet(req *struct {
	BundleID string `json:"bundle_id"`
	Profile  string `json:"profile"`
}) error {
	if req.BundleID == "" {
		return errNoBundleID
	}
	profile := strings.TrimSpace(req.Profile)
	if profile != "" && !isKnownProfile(profile) {
		return fmt.Errorf("unknown dictation profile %q", profile)
	}
	if err := h.setAppProfileOverride(req.BundleID, profile); err != nil {
		return fmt.Errorf("failed to save app profile: %w", err)
	}
	return nil
}

var errNoBundleID = errors.New("bundle_id is required")
