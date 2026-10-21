package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/hud"
)

func TestRenderItemHTML_Basic(t *testing.T) {
	item := hud.HudItem{ID: "test", Title: "hello world"}
	html := hud.RenderItemHTML(item)
	if !strings.Contains(html, "class='hud-item'") {
		t.Errorf("expected hud-item class, got: %s", html)
	}
	if !strings.Contains(html, "hello world") {
		t.Errorf("expected title in output, got: %s", html)
	}
}

func TestRenderItemHTML_WithTag(t *testing.T) {
	// "Plugin" tags are suppressed (noise when everything is a plugin).
	// Use "User" to test that tags render correctly.
	item := hud.HudItem{ID: "test", Title: "cmd", Tag: strPtr("User")}
	html := hud.RenderItemHTML(item)
	if !strings.Contains(html, "tier-user") {
		t.Errorf("expected tier-user class, got: %s", html)
	}
	if !strings.Contains(html, "<span class='hud-tag") {
		t.Errorf("expected hud-tag span, got: %s", html)
	}
}

func TestRenderItemHTML_PluginNameTag(t *testing.T) {
	// Plugin names render with tier-plugin style
	item := hud.HudItem{ID: "test", Title: "cmd", Tag: strPtr("keyboard")}
	html := hud.RenderItemHTML(item)
	if !strings.Contains(html, "tier-plugin") {
		t.Errorf("expected tier-plugin class for plugin name, got: %s", html)
	}
	if !strings.Contains(html, "keyboard") {
		t.Errorf("expected plugin name in tag, got: %s", html)
	}
}

func TestRenderItemHTML_VarTag(t *testing.T) {
	item := hud.HudItem{ID: "test", Title: "{name}", Tag: strPtr("var")}
	html := hud.RenderItemHTML(item)
	if !strings.Contains(html, "item-var") {
		t.Errorf("expected item-var class, got: %s", html)
	}
	// var items should NOT have a tag badge
	if strings.Contains(html, "hud-tag") {
		t.Errorf("var items should not render tag badge, got: %s", html)
	}
}

func TestRenderItemHTML_SpeakableSubtitleGetsTheMicGlyph(t *testing.T) {
	// This branch shipped 2026-09-06 and had never executed. HudItem.Speakable
	// was only ever fed by decoding commands.list / collections.list straight
	// into []HudSection, and neither wire type carried the field, so the glyph
	// drew exactly zero times. `just check-deadfields` could not see it either:
	// the analyser models json.Unmarshal as a write, so the decode looked like
	// one. Converting those decodes to explicit constructions is what finally
	// reported it (2026-09-20), and collections.list now carries `speakable`.
	speakable := hud.HudItem{ID: "ape", Title: "ape", Subtitle: strPtr("shrug"), Speakable: true}
	if !strings.Contains(hud.RenderItemHTML(speakable), hud.MicGlyphSVG) {
		t.Errorf("speakable row should carry the mic glyph, got: %s", hud.RenderItemHTML(speakable))
	}

	plain := hud.HudItem{ID: "beam", Title: "beam", Subtitle: strPtr("plain")}
	if strings.Contains(hud.RenderItemHTML(plain), hud.MicGlyphSVG) {
		t.Errorf("non-speakable row must not carry the glyph")
	}

	// The glyph is appended to the subtitle span, so it has nowhere to sit
	// when there is no subtitle — the guard in RenderItemHTML.
	noSubtitle := hud.HudItem{ID: "x", Title: "x", Speakable: true}
	if strings.Contains(hud.RenderItemHTML(noSubtitle), hud.MicGlyphSVG) {
		t.Errorf("speakable with no subtitle has nothing to mark")
	}
}

func TestRenderItemHTML_WithSubtitle(t *testing.T) {
	// One row anatomy (2026-08-02): key-shaped subtitles keep the keycap
	// chip; longer ones render as the same-line ellipsizing note.
	item := hud.HudItem{ID: "test", Title: "go", Subtitle: strPtr("3 variations")}
	html := hud.RenderItemHTML(item)
	if !strings.Contains(html, "hud-item-note") {
		t.Errorf("expected note span for prose subtitle, got: %s", html)
	}
	if !strings.Contains(html, "3 variations") {
		t.Errorf("expected subtitle text, got: %s", html)
	}
	short := hud.HudItem{ID: "k", Title: "air", Subtitle: strPtr("a")}
	if !strings.Contains(hud.RenderItemHTML(short), "hud-item-subtitle") {
		t.Errorf("expected keycap span for key-shaped subtitle")
	}
}

func TestRenderSectionsHTML(t *testing.T) {
	sections := []hud.HudSection{
		{
			Title: "Navigation",
			Items: []hud.HudItem{
				{ID: "go", Title: "go", Tag: strPtr("Plugin")},
				{ID: "back", Title: "back", Tag: strPtr("User")},
			},
		},
		{
			Title: "System",
			Items: []hud.HudItem{
				{ID: "mute", Title: "mute"},
			},
		},
	}
	html := hud.RenderSectionsHTML(sections, hud.DefaultHudConfig())

	if !strings.Contains(html, "Navigation") {
		t.Errorf("expected section title (sentence case), got: %s", html)
	}
	if !strings.Contains(html, "System") {
		t.Errorf("expected section title (sentence case), got: %s", html)
	}
	if !strings.Contains(html, "hud-category") {
		t.Errorf("expected hud-category class, got: %s", html)
	}
	if !strings.Contains(html, "category-header") {
		t.Errorf("expected category-header class, got: %s", html)
	}
	if !strings.Contains(html, "category-items") {
		t.Errorf("expected category-items class, got: %s", html)
	}
	if strings.Count(html, "<li") != 3 {
		t.Errorf("expected 3 list items, got: %s", html)
	}
}

func TestRenderSectionsHTML_Empty(t *testing.T) {
	html := hud.RenderSectionsHTML(nil, hud.DefaultHudConfig())
	if html != "" {
		t.Errorf("expected empty string for nil sections, got: %s", html)
	}
}

func TestHandleRenderHud_Commands(t *testing.T) {
	h := newTestHost()
	// The mode is the whole request. With no platform to answer
	// commands.list there are no sections, so the response carries the mode as
	// its title and empty content — which is what the live caller gets too,
	// since PushMainHUD sends nothing but the mode.
	//
	// This used to supply Title, Footer and Sections and assert they were
	// rendered. Nothing ever supplied them, so it was asserting on a branch
	// production could not reach; the fields are gone with the platform hook.
	req := hud.RenderHUDRequest{HudMode: "commands"}

	result, err := h.handleRenderHud(&req)
	if err != nil {
		t.Fatalf("handleRenderHud error: %v", err)
	}

	resp := result.(hud.HudResponse)
	if resp.Title != "commands" {
		t.Errorf("expected title to fall back to the mode, got %s", resp.Title)
	}
	if resp.ContentHTML == nil {
		t.Fatal("expected content_html to be set")
	}
	if *resp.ContentHTML != "" {
		t.Errorf("expected empty content_html with no sections, got: %s", *resp.ContentHTML)
	}
}

func TestHandleRenderHud_UnknownMode(t *testing.T) {
	h := newTestHost()
	req := hud.RenderHUDRequest{HudMode: "custom-thing"}
	result, err := h.handleRenderHud(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resp := result.(hud.HudResponse)
	if resp.Title != "Unknown" {
		t.Errorf("expected Unknown title for unrecognized mode, got %s", resp.Title)
	}
}

func TestRenderSectionsHTML_BadgesHidden(t *testing.T) {
	sections := []hud.HudSection{
		{Title: "Test", Items: []hud.HudItem{{ID: "a", Title: "a", Tag: strPtr("Plugin")}}},
	}
	conf := hud.HudConfig{FontSize: "default", ShowBadges: false}
	html := hud.RenderSectionsHTML(sections, conf)
	if !strings.Contains(html, "<style>.hud-tag{display:none}</style>") {
		t.Errorf("expected badge-hiding style injection, got: %s", html)
	}
}

func TestRenderSectionsHTML_BadgesShown(t *testing.T) {
	sections := []hud.HudSection{
		{Title: "Test", Items: []hud.HudItem{{ID: "a", Title: "a", Tag: strPtr("Plugin")}}},
	}
	conf := hud.DefaultHudConfig()
	html := hud.RenderSectionsHTML(sections, conf)
	if strings.Contains(html, ".hud-tag{display:none}") {
		t.Errorf("expected no badge-hiding style when badges shown, got: %s", html)
	}
}

func TestRenderSectionsHTML_CompactMode(t *testing.T) {
	sections := []hud.HudSection{
		{Title: "Test", Items: []hud.HudItem{{ID: "a", Title: "a"}}},
	}
	conf := hud.HudConfig{FontSize: "default", ShowBadges: true, CompactMode: true}
	html := hud.RenderSectionsHTML(sections, conf)
	// The class on the wrapper, not the stylesheet that defines it.
	if !strings.Contains(html, " hud-compact'") {
		t.Errorf("expected hud-compact class, got: %s", html)
	}
}

func TestRenderSectionsHTML_NoCompactByDefault(t *testing.T) {
	sections := []hud.HudSection{
		{Title: "Test", Items: []hud.HudItem{{ID: "a", Title: "a"}}},
	}
	html := hud.RenderSectionsHTML(sections, hud.DefaultHudConfig())
	if strings.Contains(html, " hud-compact'") {
		t.Errorf("expected no hud-compact class by default, got: %s", html)
	}
}

// Handlers relay user gestures into the platform's user band (settings
// preset: the plugin never writes its own settings). Without a live
// platform (nil plugin / nil mirror) the relay is a no-op and the config
// stays at the shipped defaults — the same contract the mirror has before
// its first fetch. Mutation round-trips are covered platform-side
// (overrides.apply patch/unpatch tests) and by the SDK conformance case.
func TestHandleSetFontSize(t *testing.T) {
	h := newTestHost()
	req := struct {
		FontSize string `json:"font_size"`
	}{FontSize: "large"}
	err := h.handleSetFontSize(&req)
	// On a detached plugin the relay into the user band fails, and the
	// handler says so instead of pretending: an error, and no local
	// mutation.
	if err == nil {
		t.Fatalf("expected the relay to fail with no platform behind it")
	}
	if h.hud.Config().FontSize != "default" {
		t.Errorf("nil-platform relay must not mutate locally, got %s",
			h.hud.Config().FontSize)
	}
}

func TestHandleSetFontSize_Invalid(t *testing.T) {
	h := newTestHost()
	req := struct {
		FontSize string `json:"font_size"`
	}{FontSize: "huge"}
	err := h.handleSetFontSize(&req)
	if err == nil {
		t.Error("expected error for invalid font_size")
	}
}

func TestHandleSetShowBadges(t *testing.T) {
	h := newTestHost()
	req := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: false}
	err := h.handleSetShowBadges(&req)
	// On a detached plugin the relay into the user band fails, and the
	// handler says so instead of pretending: an error, and no local
	// mutation.
	if err == nil {
		t.Fatalf("expected the relay to fail with no platform behind it")
	}
	if h.hud.Config().ShowBadges != true {
		t.Error("nil-platform relay must not mutate locally")
	}
}

func TestHandleSetCompactMode(t *testing.T) {
	h := newTestHost()
	req := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: true}
	err := h.handleSetCompactMode(&req)
	// On a detached plugin the relay into the user band fails, and the
	// handler says so instead of pretending: an error, and no local
	// mutation.
	if err == nil {
		t.Fatalf("expected the relay to fail with no platform behind it")
	}
	if h.hud.Config().CompactMode != false {
		t.Error("nil-platform relay must not mutate locally")
	}
}

func TestRenderItemHTML_AllTierClasses(t *testing.T) {
	cases := []struct {
		tag      string
		class    string
		suppress bool
	}{
		{"Core", "tier-core", false},
		{"Plugin", "tier-plugin", false},
		{"Dynamic", "tier-plugin", false},
		{"User", "tier-user", false},
		{"OK", "tier-ok", false},
		{"MISS", "tier-miss", false},
		{"Custom", "tier-plugin", false},
	}
	for _, tc := range cases {
		item := hud.HudItem{ID: "test", Title: "x", Tag: &tc.tag}
		html := hud.RenderItemHTML(item)
		if tc.suppress {
			if strings.Contains(html, "hud-tag") {
				t.Errorf("tag %q: should be suppressed in: %s", tc.tag, html)
			}
		} else if tc.class != "" && !strings.Contains(html, tc.class) {
			t.Errorf("tag %q: expected class %q in: %s", tc.tag, tc.class, html)
		} else if tc.class == "" && strings.Contains(html, "tier-") {
			t.Errorf("tag %q: should not have tier class in: %s", tc.tag, html)
		}
	}
}

// Ensure json import is used (needed for test compilation)
var _ = json.Marshal

// Compact mode is a setting a person sees, so the stylesheet must define what
// its class means (it once set a class nothing defined). Text size is the
// platform's now, so voice's styles must not set sizes of their own.
func TestHudStylesDefineCompactAndLeaveSizeToThePlatform(t *testing.T) {
	css := hud.HudBaseStyles()
	if !strings.Contains(css, ".hud-compact .hud-item") {
		t.Error("HudStyles has no .hud-compact rule: the setting would do nothing")
	}
	if strings.Contains(css, ".hud-font-") {
		t.Error("voice sets its own font sizes; they would override the platform's text size")
	}
}
