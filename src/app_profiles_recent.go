package main

import (
	"encoding/json"
	"sort"
	"strings"

	branchkit "github.com/branchkit/plugin-sdk-go"

	"github.com/branchkit/branchkit-plugin-voice/views"
)

// The recently-focused ring, and app display names — both exist for the
// SETTINGS TABLE, not for deciding a transcript's profile.
//
// That distinction matters because the design doc explicitly REJECTS having
// this plugin subscribe to focus and cache it as the source for the profile
// decision: the cached value goes stale in exactly the interesting case, when
// focus moves between the hold ending and the transcript arriving. The emit
// path answers that question exactly, and does.
//
// What is left over is a genuine UI problem with no other answer: nobody can
// type `com.tinyspeck.slackmacgap`, and "add a rule for the app you are in"
// cannot work, because while you are reading Settings the focused app IS the
// browser showing Settings. So the choices have to be apps you were in a moment
// ago, which needs a ring, and a stale entry here costs nothing — it is a
// menu item, not a decision.

const recentAppsCap = 12

var (
	// Most-recently-focused first. Bundle identifiers, deduplicated: refocusing
	// an app moves it to the front rather than filling the ring with one name.
	recentApps []string
)

// noteFocusedApp records a focus change for the settings picker.
func (h *Host) noteFocusedApp(bundleID string) {
	if bundleID == "" {
		return
	}
	h.recentAppsMu.Lock()
	defer h.recentAppsMu.Unlock()
	out := make([]string, 0, recentAppsCap)
	out = append(out, bundleID)
	for _, id := range h.recentApps {
		if id == bundleID {
			continue
		}
		if len(out) == recentAppsCap {
			break
		}
		out = append(out, id)
	}
	h.recentApps = out
}

// recentlyFocusedApps returns the ring, newest first.
func (h *Host) recentlyFocusedApps() []string {
	h.recentAppsMu.Lock()
	defer h.recentAppsMu.Unlock()
	out := make([]string, len(h.recentApps))
	copy(out, h.recentApps)
	return out
}

// subscribeFocusedApps wires the ring. Registered alongside the other
// subscriptions; the handler is deliberately tiny because nothing but the
// settings picker reads what it writes.
func (h *Host) subscribeFocusedApps() {
	h.plugin.OnAppFocused(func(payload branchkit.AppFocusedEventParams) {
		h.noteFocusedApp(payload.BundleID)
	})
}

// appDisplayName turns a bundle identifier into something a person recognises,
// using the `apps` collection the open-app command already consumes. Falls back
// to the identifier, which is ugly but never wrong — better than an empty cell
// for an app the collection has not heard of.
func (h *Host) appDisplayName(bundleID string) string {
	h.appNamesMu.RLock()
	name, ok := h.appNamesByID[bundleID]
	h.appNamesMu.RUnlock()
	if ok && name != "" {
		return name
	}
	return bundleID
}

// loadAppNames builds the bundle-id → spoken-name index once.
//
// Read lazily at first settings render rather than at boot: the collection is
// large, it is populated asynchronously by an app scan, and nothing outside the
// settings page needs it. Reloading on each render also means an app installed
// since boot appears without a restart.
func (h *Host) loadAppNames() {
	records, err := h.plugin.ListAll("apps")
	if err != nil {
		branchkit.Logf("voice", "app names load error: %v", err)
		return
	}
	index := appNameIndex(records)
	h.appNamesMu.Lock()
	h.appNamesByID = index
	h.appNamesMu.Unlock()
}

// appNameIndex maps each bundle id in the `apps` records to the name to show
// for it.
func appNameIndex(records []branchkit.CollectionRecord) map[string]string {
	index := make(map[string]string, len(records))
	named := map[string]bool{}
	for _, rec := range records {
		// The GENERATED shape, from this plugin's declared consumption of the
		// collection, so a field rename upstream breaks the build here.
		var payload AppsRecord
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			continue
		}
		if payload.AppID == "" || payload.Spoken == "" {
			continue
		}
		// The collection is keyed by spoken name, so two entries can share a
		// bundle id (aliases). Among spoken fallbacks the first wins: they are
		// equally correct, and a deterministic pick beats a changing name.
		// The app's display name ("Firefox") when a record has one, else the
		// spoken form ("firefox"). A named record wins over a spoken fallback
		// whatever the order: the registry's records carry the name, but a
		// row a person added ("browser") does not, and must not label the app.
		if payload.Name != nil && *payload.Name != "" {
			if !named[payload.AppID] {
				index[payload.AppID] = *payload.Name
				named[payload.AppID] = true
			}
			continue
		}
		if _, seen := index[payload.AppID]; !seen {
			index[payload.AppID] = payload.Spoken
		}
	}
	return index
}

// installedApps is every app the `apps` collection knows, by display name.
func (h *Host) installedApps() []views.AppOptionView {
	h.appNamesMu.RLock()
	out := make([]views.AppOptionView, 0, len(h.appNamesByID))
	for id, name := range h.appNamesByID {
		out = append(out, views.AppOptionView{BundleID: id, Name: name})
	}
	h.appNamesMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// appIsInstalled reports whether the `apps` collection knows this bundle id.
//
// Answers "is it worth showing a row for this app", nothing more. When the
// index is EMPTY the answer is yes for everything: an empty index means the
// collection has not loaded, not that the machine has no applications, and
// hiding the whole table on a slow load is a worse failure than showing a few
// rows for apps someone does not have.
func (h *Host) appIsInstalled(bundleID string) bool {
	h.appNamesMu.RLock()
	defer h.appNamesMu.RUnlock()
	if len(h.appNamesByID) == 0 {
		return true
	}
	_, ok := h.appNamesByID[bundleID]
	return ok
}
