package main

import (
	"encoding/json"
	"sort"
	"time"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

// App traits: the FACT half of per-app dictation profiles.
//
// The apps plugin — the application registry, which already ships curated
// facts about applications and already tells us their spoken names — publishes
// what KIND of thing each app is. This file reads that and nothing else.
//
// The split it implements: apps says "Warp is a terminal", voice says
// "terminals take no trailing full stop". Fourteen bundle identifiers with a
// punctuation opinion each used to live in app_profiles.go; they were facts
// about applications sitting in the plugin with the least standing to assert
// them, including ten for software nobody here has ever run.
//
// The test that keeps the split honest, in both directions: could a second
// consumer agree an app IS that, and still disagree about what follows? If not,
// the name is a policy wearing a fact's clothes and does not belong upstream.
//
// TRAITS, not "groups". `group` is already taken in the collections subsystem,
// where it labels a writer-owned replace-set on a record envelope (ScopeGroup,
// plugin-sdk-go/collection.go) — about who may overwrite what, not what kind of
// thing an app is. Two meanings for one word in one subsystem is a homonym, and
// this one was still free to rename because nothing is stored: the collection is
// rebuilt from a shipped file at every boot. "Traits" is also plural by nature
// (an app has several) and implies no ranking, which is right — ranking is this
// plugin's business, below, not the publisher's.
const appTraitsCollection = "app_traits"

// traitProfiles is voice's ENTIRE opinion about applications — two rows where
// there used to be fourteen.
//
// A SLICE, not a map, and the order is load-bearing: an app can have several
// traits (Emacs is an editor, a terminal, a mail client and a browser, which is
// a joke that happens to be true), so two rules can fire for one app. First
// match wins. A map would make that resolution Go's map iteration order, which
// is a coin flip per process.
//
// Two entries today, so the ordering is not yet a real decision. Writing it as
// an ordered list is what makes it one when it becomes one.
var traitProfiles = []struct {
	Trait   string
	Profile string
}{
	// Named for its archetype, but what `chat` MEANS is "a trailing full stop
	// is wrong here" — as true of a shell as of a chat box. `ls -la.` is the
	// motivating complaint, and a fourth profile would differ from this in
	// nothing.
	{"terminal", profileChat},
	{"chat", profileChat},
}

var (
	// bundle id → traits, as published by system.
	appTraitsByID map[string][]string
	// loaded distinguishes "apps says this app has no traits" from "we have
	// not read the collection yet". Those are the same empty map and different
	// answers: the second is a boot race, not a fact.
	//
	// Not hypothetical. The apps plugin publishes once its connection is up,
	// and a platform slow to start can delay that for seconds — so the
	// collection can be genuinely absent for a while after boot. voice already learned this shape
	// once: appIsInstalled treats an empty name index as "not loaded" for the
	// same reason.
	appTraitsLoaded bool
)

// loadAppTraits reads the published memberships into the cache.
//
// Called at boot and again on every `collection.updated` for the collection, so
// a late push (or a system restart) converges without a voice restart. Reading
// per transcript would put a cross-plugin RPC on the paste path; the table
// changes when someone edits settings, not per utterance.
//
// To be explicit, because it is one letter away from a thing the design
// rejects: this caches the TRAIT TABLE. It does not cache focus. Which app was
// focused is stamped on the transcript by the platform at emit time, and that
// stays the source — a cached focus value goes stale in exactly the interesting
// case, when focus moves between the hold ending and the transcript arriving.
func (h *Host) loadAppTraits() {
	records, err := h.plugin.ListAll(appTraitsCollection)
	if err != nil {
		// Leaves the previous cache in force, and `loaded` untouched — a failed
		// read must not be mistaken for "no app has any traits".
		branchkit.Logf("voice", "app traits read error: %v", err)
		return
	}
	index := make(map[string][]string, len(records))
	for _, rec := range records {
		// The GENERATED shape, from this plugin's declared consumption of the
		// collection in plugin.json. Using it rather than an inline struct is
		// what makes a field rename upstream break the BUILD here, instead of
		// quietly unmarshalling to empty strings forever.
		var payload AppTraitsRecord
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			continue
		}
		if payload.Trait == "" || payload.AppID == "" {
			continue
		}
		index[payload.AppID] = append(index[payload.AppID], payload.Trait)
	}
	for id := range index {
		sort.Strings(index[id])
	}
	h.appTraitsMu.Lock()
	h.appTraitsByID = index
	h.appTraitsLoaded = true
	h.appTraitsMu.Unlock()
	branchkit.Logf("voice", "app traits loaded: %d apps", len(index))
}

// appTraitsLoadedOK reports whether the collection has been read successfully.
func (h *Host) appTraitsLoadedOK() bool {
	h.appTraitsMu.RLock()
	defer h.appTraitsMu.RUnlock()
	return h.appTraitsLoaded
}

// appTraitsRetryDelays backs off a failed boot read. Bounded on purpose,
// mirroring the apps plugin's own app-scan retry: a permanently absent
// apps plugin (disabled, or crash-looping) should settle into "no traits",
// which is prose everywhere — today's behaviour minus the standing defaults —
// rather than retrying forever behind every dictation.
var appTraitsRetryDelays = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

// retryAppTraits closes a narrow but real boot race.
//
// apps publishes at ITS boot and voice subscribes at its own, so if voice's
// first read fails transiently AND apps already pushed, no collection.updated
// is ever coming and the memberships stay invisible until a restart. Ordinary
// startup ordering does not save us: the dep graph gets apps started first,
// which is exactly what makes the push land before the subscription exists.
//
// Deliberately a background retry rather than a lazy load on the transcript
// path — that would put a cross-plugin RPC behind every dictation to fix a
// failure that resolves in seconds.
func (h *Host) retryAppTraits() {
	for i, delay := range appTraitsRetryDelays {
		time.Sleep(delay)
		h.loadAppTraits()
		if h.appTraitsLoadedOK() {
			branchkit.Logf("voice", "app traits: read succeeded on retry %d/%d",
				i+1, len(appTraitsRetryDelays))
			return
		}
	}
	branchkit.Logf("voice", "app traits: never readable — every app resolves to prose "+
		"unless overridden (is the apps plugin running?)")
}

// appTraits returns the traits the apps plugin publishes for one app.
func (h *Host) appTraits(bundleID string) []string {
	if bundleID == "" {
		return nil
	}
	h.appTraitsMu.RLock()
	defer h.appTraitsMu.RUnlock()
	return h.appTraitsByID[bundleID]
}

// traitProfileFor resolves an app to a profile through its traits, returning
// the profile and the trait that produced it.
//
// The second return value exists so the settings table can show its work.
// Resolution is consumer-local by design — system never ranks its facts, and a
// different plugin reading the same traits is free to order them differently —
// which means a surprising answer has nothing to inspect unless the consumer
// says which rule fired. "chat — via terminal" is the whole fix.
func (h *Host) traitProfileFor(bundleID string) (profile string, via string) {
	traits := h.appTraits(bundleID)
	if len(traits) == 0 {
		return "", ""
	}
	has := make(map[string]bool, len(traits))
	for _, t := range traits {
		has[t] = true
	}
	for _, rule := range traitProfiles {
		if has[rule.Trait] {
			return rule.Profile, rule.Trait
		}
	}
	return "", ""
}

// appsWithTraitOpinion lists every app whose traits resolve to a profile,
// sorted for a stable render. Feeds the settings table, which used to build the
// same list by iterating the shipped table this replaced.
func (h *Host) appsWithTraitOpinion() map[string]traitOpinion {
	h.appTraitsMu.RLock()
	ids := make([]string, 0, len(h.appTraitsByID))
	for id := range h.appTraitsByID {
		ids = append(ids, id)
	}
	h.appTraitsMu.RUnlock()

	out := make(map[string]traitOpinion, len(ids))
	for _, id := range ids {
		if profile, via := h.traitProfileFor(id); profile != "" {
			out[id] = traitOpinion{Profile: profile, Via: via}
		}
	}
	return out
}

// traitOpinion is what voice concluded about an app, and which trait got it there.
type traitOpinion struct {
	Profile string
	Via     string
}

// subscribeAppTraits keeps the cache fresh. apps publishes with a whole-scope
// replace, which emits collection.updated, so a late or repeated push lands here.
func (h *Host) subscribeAppTraits() {
	h.plugin.Subscribe(appTraitsCollection, func(branchkit.CollectionUpdatedEventParams) {
		h.loadAppTraits()
	})
}
