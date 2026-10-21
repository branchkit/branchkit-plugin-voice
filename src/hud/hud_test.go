package hud

import (
	"reflect"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// The HUD's first tests. They exist because a HUD is now an instance: two
// can be built in one process, on a plugin with no platform behind it, and
// their state cannot leak into each other.

func newTestHUD() *HUD {
	return New(branchkit.NewDetachedPlugin(), func(*RenderHUDRequest) (HudResponse, error) {
		return HudResponse{}, nil
	})
}

func TestTwoHUDsKeepSeparateState(t *testing.T) {
	a, b := newTestHUD(), newTestHUD()
	a.SetMainMode("history")
	a.SetHudOpen(true)
	if got := b.CurrentMainMode(); got != "commands" {
		t.Fatalf("b's mode = %q; a's SetMainMode leaked", got)
	}
	if b.IsHudOpen() {
		t.Fatal("b reports open; a's SetHudOpen leaked")
	}
	if got := a.CurrentMainMode(); got != "history" || !a.IsHudOpen() {
		t.Fatalf("a = %q open=%v, want history/open", got, a.IsHudOpen())
	}
}

func TestDiscoveryOpensAndCloses(t *testing.T) {
	h := newTestHUD()
	if h.IsDiscoveryOpen() {
		t.Fatal("fresh HUD reports discovery open")
	}
	h.OpenDiscovery("Say a command", nil, "no commands", "", 0, OnExpire{})
	if !h.IsDiscoveryOpen() {
		t.Fatal("OpenDiscovery did not open")
	}
	h.CloseDiscovery()
	if h.IsDiscoveryOpen() {
		t.Fatal("CloseDiscovery did not close")
	}
}

func TestOverlayGenerationsGuardStaleUpdates(t *testing.T) {
	h := newTestHUD()
	gen := h.OpenOverlay(branchkit.OutputKindProgress, "Preparing", "warming", nil)
	if gen == 0 {
		t.Fatal("OpenOverlay returned generation 0")
	}
	h.UpdateOverlay(gen, branchkit.OutputKindProgress, "Preparing", "ready", nil)
	if got := h.discovery.phrase; got != "ready" {
		t.Fatalf("current-generation update ignored: %q", got)
	}
	h.UpdateOverlay(gen-1, branchkit.OutputKindProgress, "Stale", "stale", nil)
	if got := h.discovery.phrase; got != "ready" {
		t.Fatalf("stale-generation update applied: %q", got)
	}
	h.CloseOverlay(gen)
	if h.IsDiscoveryOpen() {
		t.Fatal("CloseOverlay with the live generation did not close")
	}
}

func TestConfigAnswersDefaultsBeforeTheFirstFetch(t *testing.T) {
	h := newTestHUD()
	if got := h.Config(); !reflect.DeepEqual(got, DefaultHudConfig()) {
		t.Fatalf("Config before ready = %+v, want the manifest defaults", got)
	}
}

// Something said while a mode's choices are showing leads their phrase and
// keeps the list and the overlay's generation.
func TestLeadDiscoveryPhraseKeepsTheList(t *testing.T) {
	h := newTestHUD()
	if h.LeadDiscoveryPhrase("x failed") {
		t.Fatal("closed discovery has no phrase to lead")
	}
	sections := []branchkit.OutputSection{{Title: "Directions"}}
	h.OpenDiscovery("Snap", sections, "6 commands", "", 0, OnExpire{})
	h.discovery.mu.Lock()
	gen := h.discovery.generation
	h.discovery.mu.Unlock()
	if !h.LeadDiscoveryPhrase("“left” was not handled by placement") {
		t.Fatal("open discovery should take the lead")
	}
	h.discovery.mu.Lock()
	defer h.discovery.mu.Unlock()
	if h.discovery.phrase != "“left” was not handled by placement. 6 commands" {
		t.Errorf("phrase = %q", h.discovery.phrase)
	}
	if len(h.discovery.sections) != 1 || h.discovery.generation != gen || !h.discovery.isOpen {
		t.Errorf("the list and the overlay must stay: sections=%d gen=%d→%d open=%v",
			len(h.discovery.sections), gen, h.discovery.generation, h.discovery.isOpen)
	}
}

// Only a mode's choices take a lead: a problem overlay or a live transcript
// is left alone, and the caller opens its own document instead.
func TestLeadDiscoveryPhraseOnlyLeadsChoices(t *testing.T) {
	h := newTestHUD()
	h.OpenOverlay(branchkit.OutputKindProblem, "Didn't work", "“a” failed", nil)
	if h.LeadDiscoveryPhrase("“b” failed") {
		t.Error("a problem overlay must not take a lead")
	}
	h.discovery.mu.Lock()
	phrase := h.discovery.phrase
	h.discovery.mu.Unlock()
	if phrase != "“a” failed" {
		t.Errorf("problem phrase changed: %q", phrase)
	}
}
