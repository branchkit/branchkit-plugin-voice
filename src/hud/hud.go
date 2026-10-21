package hud

import (
	"fmt"
	"html"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/branchkit/branchkit-plugin-voice/views"
	"github.com/branchkit/plugin-sdk-go"
)

// subtitleSpan renders an item subtitle. Short subtitles (hint letters, key
// names) keep the keycap chip; prose-length ones render as a full-width
// description line under the title (`hud-item-desc` + the item's `has-desc`
// wrap) — a paragraph inside the no-shrink keycap blew the row out past the
// window edge (bare-"copy" completion items, 2026-07-19). Returns the extra
// item class and the span HTML.
func subtitleSpan(s *string) (itemClass string, span string) {
	if s == nil {
		return "", ""
	}
	// ONE row anatomy (field 2026-08-02: two shapes — same-line keycaps vs
	// second-line paragraphs — read as unexplained inconsistency): every
	// subtitle sits on the SAME line, right-aligned. Key-shaped values (a
	// hint letter, "esc") keep the keycap chip; anything longer renders as
	// quiet text that ellipsizes — the full description lives in the help
	// overlay, this surface is a glance list.
	if utf8.RuneCountInString(*s) > 3 {
		return "", fmt.Sprintf("<span class='hud-item-note'>%s</span>", html.EscapeString(*s))
	}
	return "", fmt.Sprintf("<span class='hud-item-subtitle'>%s</span>", html.EscapeString(*s))
}

// HUD owns the main HUD channel's mode and open state, the discovery
// overlay, and the HUD appearance settings mirror. Build it with New: the
// platform handle and the content renderer arrive as arguments, so a HUD
// cannot exist unwired, and two can exist in a test.
type HUD struct {
	plugin *branchkit.Plugin
	// render answers the content for one main-HUD mode. It is the voice
	// plugin's own handler, handed in so this package need not import it.
	render func(req *RenderHUDRequest) (HudResponse, error)
	config *branchkit.SettingsMirror[HudConfig]

	// The main HUD channel's current mode and open state.
	mainMu    sync.Mutex
	mainMode  string
	hudOpen   bool
	discovery *discoveryState
}

// New wires the settings mirror (must run before plugin.Run()) whose
// OnChange repaints the main HUD, so an appearance edit from ANY surface —
// the voice tab, the generic settings form, the import — lands visually.
func New(p *branchkit.Plugin, render func(req *RenderHUDRequest) (HudResponse, error)) *HUD {
	h := &HUD{plugin: p, render: render, discovery: &discoveryState{}}
	h.mainMode = "commands"
	h.config = branchkit.Settings[HudConfig](p, configCollection)
	h.config.OnChange(func(HudConfig) { h.PushMainHUD() })
	return h
}

// IsHudOpen returns whether the main HUD is currently open.
func (h *HUD) IsHudOpen() bool {
	h.mainMu.Lock()
	defer h.mainMu.Unlock()
	return h.hudOpen
}

// SetHudOpen sets whether the main HUD is open.
func (h *HUD) SetHudOpen(open bool) {
	h.mainMu.Lock()
	defer h.mainMu.Unlock()
	h.hudOpen = open
}

// CurrentMainMode returns the mode the main HUD is currently showing. Used by
// the recent-command history to repaint only when its own panel is visible.
func (h *HUD) CurrentMainMode() string {
	h.mainMu.Lock()
	defer h.mainMu.Unlock()
	return h.mainMode
}

// buildTabsHTML generates clickable tab buttons for the HUD header.
func buildTabsHTML(activeMode string) string {
	type tab struct {
		mode  string
		label string
	}
	tabs := []tab{
		{"commands", "Command Editor"},
		{"entities", "Entities"},
		{"history", "History"},
		{"dictation", "Dictation"},
	}

	// The HUD kit's tabs (channel.html serves the kit): the selected one is
	// aria-selected, which is also what draws it, so state is never colour
	// alone.
	var b strings.Builder
	b.WriteString(`<div class="bk-tabs" role="tablist">`)
	for _, t := range tabs {
		fmt.Fprintf(&b,
			`<button class="bk-tab" role="tab" aria-selected="%t" data-on:click="%s">%s</button>`,
			t.mode == activeMode,
			html.EscapeString(branchkit.MethodPost("set_main_mode", "{mode: '"+jsString(t.mode)+"'}")),
			t.label)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// SetMainMode switches the main HUD to the given mode and pushes content.
func (h *HUD) SetMainMode(mode string) {
	func() {
		h.mainMu.Lock()
		defer h.mainMu.Unlock()
		h.mainMode = mode
	}()
	branchkit.Logf("voice", "hud: setMainMode: %s", mode)
	h.PushMainHUD()
}

// PushMainHUD renders content for the current mode and pushes to the "main" channel.
func (h *HUD) PushMainHUD() {
	h.mainMu.Lock()
	mode := h.mainMode
	h.mainMu.Unlock()

	conf := h.Config()
	if !conf.IsChannelEnabled(ChannelMain) {
		return
	}

	req := &RenderHUDRequest{HudMode: mode}
	resp, err := h.render(req)
	if err != nil {
		branchkit.Logf("voice", "hud: render failed for mode '%s': %v", mode, err)
		return
	}

	content := ""
	if resp.ContentHTML != nil {
		content = *resp.ContentHTML
	}

	tabsHTML := buildTabsHTML(mode)
	// The window is the panel (the shell draws the frost, corners and
	// shadow), so the content draws none of its own: the HUD kit's panel,
	// head and tabs.
	allHTML := fmt.Sprintf(
		`<div id="content" class="bk-panel">`+
			`<div class="bk-head">%s</div>`+
			`<div id="content-inner" class="mode-%s" style="padding:12px var(--hud-pad);display:flex;flex-direction:row;align-items:flex-start;gap:20px;width:fit-content;height:auto;overflow-x:auto">%s</div>`+
			`</div>`,
		tabsHTML, mode, content,
	)

	if err := h.plugin.HUDPushRaw("main", allHTML); err != nil {
		branchkit.Logf("voice", "hud: hud.push to 'main' failed: %v", err)
	}
	// The same content as meaning, for every renderer that is not this
	// window: speech, the window's accessibility element. Voice draws the
	// window itself (tabs, layout), so the platform is told not to draw.
	drawsOwn := true
	if _, err := h.plugin.OutputState(branchkit.OutputStateRequest{
		State:          MainMeaning(mode, resp),
		DrawsOwnWindow: &drawsOwn,
	}); err != nil {
		branchkit.Logf("voice", "hud: main output.state failed: %v", err)
	}
}

// MainMeaning is the main HUD's content as an output state: the groups and
// rows when the mode has them (commands), otherwise its title and summary
// line. Rows are information only: a command row's words can hold a slot
// ("scroll <direction>"), which is not something to inject as said.
func MainMeaning(mode string, resp HudResponse) branchkit.OutputState {
	return Meaning(ChannelMain, mode, resp)
}

// Meaning is a HUD response as an output state on `channel` — the main HUD,
// or a floating card on its own channel.
func Meaning(channel, mode string, resp HudResponse) branchkit.OutputState {
	var sections []branchkit.OutputSection
	count := 0
	for _, sec := range resp.Sections {
		items := make([]branchkit.OutputItem, 0, len(sec.Items))
		for _, it := range sec.Items {
			// The platform refuses an item with no id; a row with neither
			// id nor title says nothing anyway.
			id := it.ID
			if strings.TrimSpace(id) == "" {
				id = it.Title
			}
			if strings.TrimSpace(id) == "" {
				continue
			}
			items = append(items, branchkit.OutputItem{
				ID:       id,
				Title:    it.Title,
				Phrase:   it.Title,
				Subtitle: it.Subtitle,
			})
		}
		count += len(items)
		if len(items) > 0 {
			sections = append(sections, branchkit.OutputSection{Title: sec.Title, Items: items})
		}
	}
	title := resp.Title
	if title == "" {
		title = mode
	}
	doc := branchkit.OutputState{
		Channel: channel,
		Kind:    branchkit.OutputKindOutcome,
		Title:   title,
		Phrase:  resp.Footer,
		Urgency: branchkit.OutputUrgencyAmbient,
		Locale:  "en",
		V:       1,
	}
	if len(sections) > 0 {
		doc.Kind = branchkit.OutputKindChoices
		doc.Sections = sections
		doc.Phrase = fmt.Sprintf("%d %s", count, plural(count, "item", "items"))
		if resp.Footer != "" {
			footer := resp.Footer
			doc.Footer = &footer
		}
	}
	if doc.Phrase == "" {
		doc.Phrase = title
	}
	return doc
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// --- HUD rendering helpers (used by root package) ---

// HudBaseStyles returns the shared HUD CSS as an HTML string.
func HudBaseStyles() string {
	return views.Render(views.HudStyles())
}

// RenderItemHTML renders a single HUD item as an <li> element. Every string
// in the item is text — a command pattern (which may read "go to <number>"),
// a collection row a plugin filled, a plugin id — and is escaped here; no
// caller hands in markup.
func RenderItemHTML(item HudItem) string {
	isVar := item.Tag != nil && *item.Tag == "var"

	var tagHTML string
	if item.Tag != nil && !isVar {
		tag := *item.Tag
		var tierClass string
		switch tag {
		case "Core":
			tierClass = " tier-core"
		case "User":
			tierClass = " tier-user"
		case "OK":
			tierClass = " tier-ok"
		case "MISS":
			tierClass = " tier-miss"
		default:
			tierClass = " tier-plugin"
		}
		tagHTML = fmt.Sprintf("<span class='hud-tag%s'>%s</span>", tierClass, html.EscapeString(tag))
	}

	itemClass := "hud-item"
	if isVar {
		itemClass = "hud-item item-var"
	}

	descClass, subtitleHTML := subtitleSpan(item.Subtitle)

	// A speakable row's subtitle is itself a matchable spoken form (an alias
	// equal to the display name — a promoted selection record). The mic glyph
	// sits after the subtitle: the same "this is a way in" mark the browser
	// extension draws beside voice phrases.
	if item.Speakable && subtitleHTML != "" {
		subtitleHTML += MicGlyphSVG
	}

	return fmt.Sprintf("<li class='%s%s'>%s<span class='hud-item-text'>%s</span>%s</li>",
		itemClass, descClass, tagHTML, html.EscapeString(item.Title), subtitleHTML)
}

// MicGlyphSVG marks a speakable subtitle. Same strokes as the browser
// extension's micGlyph (render/mic-glyph.ts) so the mark reads identically
// across surfaces; sized and dimmed inline because HUD styling is inline.
const MicGlyphSVG = `<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" style="vertical-align:-1px;margin-left:4px;opacity:0.65;flex-shrink:0"><rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 11a7 7 0 0 0 14 0"/><line x1="12" y1="18" x2="12" y2="22"/></svg>`

// EntitySection is the collections.list section, not a copy of it. It was a
// hand-written mirror — its own comment said so — field for field identical
// to the generated type including the item shape, which meant a renamed
// field on the platform would have zero-filled here instead of failing the
// build. Aliased 2026-09-20 with the rest of the typed-call migration.
type EntitySection = branchkit.CollectionsListSection

// RenderEntitySectionsHTML renders entity collection sections.
func RenderEntitySectionsHTML(sections []EntitySection, conf HudConfig) string {
	var b strings.Builder
	for _, sec := range sections {
		sectionTitle := sec.Label
		if sec.Plugin != "" {
			sectionTitle = sec.Label + " · " + sec.Plugin
		}

		var items strings.Builder
		for _, it := range sec.Items {
			tooltip := it.Source
			descClass, subtitleHTML := subtitleSpan(it.Subtitle)
			fmt.Fprintf(&items,
				"<li class='hud-item%s' title='%s'><span class='hud-item-text'>%s</span>%s</li>",
				descClass, html.EscapeString(tooltip), html.EscapeString(it.Title), subtitleHTML)
		}
		// The pop-out icon floats this collection into the draggable `reference`
		// HUD via open_float (same path as the "open collection <name>" voice
		// command).
		fmt.Fprintf(&b,
			"<section class='hud-category' style='min-width:200px;width:fit-content;flex-shrink:0;display:flex;flex-direction:column;gap:4px'>"+
				"<div style='display:flex;align-items:center;gap:6px'>"+
				"<h2 class='category-header' style='flex-grow:1'>%s</h2>"+
				"%s"+
				"</div>"+
				"<ul class='category-items' style='display:flex;flex-direction:column;gap:2px'>%s</ul></section>",
			html.EscapeString(sectionTitle),
			FloatButtonHTML("entity", sec.Name),
			items.String(),
		)
	}

	inner := b.String()
	if inner == "" {
		return ""
	}

	wrapperClass := "hud-wrap"
	if conf.CompactMode {
		wrapperClass += " hud-compact"
	}

	var out strings.Builder
	out.WriteString(HudBaseStyles())
	fmt.Fprintf(&out, "<div class='%s' style='display:contents'>%s</div>", wrapperClass, inner)
	return out.String()
}

// PopOutIconSVG is the "pop out into a floating window" glyph (a panel with a
// mini-panel in the corner and an arrow into it). Uses currentColor so it
// inherits the button's text color.
const PopOutIconSVG = `<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7.5 7.5 L11 11 M11 11 L11 8 M11 11 L8 11"/><rect x="11.5" y="11" width="8.5" height="7" rx="1.5" fill="currentColor" stroke="none"/></svg>`

// FloatButtonHTML renders the pop-out icon button for a HUD group. `kind` is
// "entity" or "commands"; `id` identifies the group (collection name / command
// category title) that open_float re-renders into the floating card.
func FloatButtonHTML(kind, id string) string {
	expr := branchkit.MethodPost("open_float", "{kind: '"+jsString(kind)+"', id: '"+jsString(id)+"'}")
	return fmt.Sprintf(
		`<button class="bk-btn bk-icon-btn" title="Pop out as a floating card" aria-label="Pop out %s" data-on:click="%s">%s</button>`,
		html.EscapeString(id), html.EscapeString(expr), PopOutIconSVG)
}

// jsString makes s safe inside a single-quoted JS string literal. The whole
// expression is then HTML-escaped once for its attribute.
func jsString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "'", "\\'")
}

// RenderFloatCard renders a self-contained floating reference card from a title
// and pre-rendered <li> items: a draggable sticky header (data-hud-drag, which
// the shell's native drag grabs) with a close button, above a scrollable list.
// `channelID` is the panel's own HUD channel, so its ✕ closes just that panel.
func RenderFloatCard(channelID, title, itemsHTML string, conf HudConfig) string {
	wrapperClass := "hud-wrap"
	if conf.CompactMode {
		wrapperClass += " hud-compact"
	}

	// The card fills the window (position:fixed;inset:0) and scrolls when the
	// window is smaller than the content. Its NATURAL size is measurable via
	// #content.scrollHeight/scrollWidth (sticky header + natural-height list),
	// so the shell opens the window sized to fit the content (capped), then
	// leaves it alone once the user resizes. The 36px sticky header matches the
	// shell's native drag strip; the ✕ carries data-hud-nodrag so clicks on it
	// aren't swallowed by the drag region.
	// No panel of its own (the window is the panel). The sticky header keeps
	// a near-opaque fill only so rows scrolled under it stay unreadable
	// behind the title, not as a second panel.
	closeExpr := branchkit.MethodPost("close_float", "{channel: '"+jsString(channelID)+"'}")
	return fmt.Sprintf(
		`<div id="content" class="%s" style="position:fixed;inset:0;box-sizing:border-box;overflow:auto">`+
			`<style>html,body{height:100%%!important;margin:0!important;overflow:hidden!important;background:transparent!important}#content::-webkit-scrollbar{width:0;height:0;display:none}</style>`+
			`%s`+
			`<div class="bk-head" data-hud-drag style="position:sticky;top:0;z-index:2;height:36px;box-sizing:border-box;cursor:grab;padding:0 8px 0 var(--hud-pad);background:rgba(28,28,30,0.92);user-select:none;-webkit-user-select:none">`+
			`<h1 class="bk-title">%s</h1>`+
			`<button class="bk-btn bk-icon-btn" data-hud-nodrag aria-label="Close %s" data-on:click="%s">&times;</button>`+
			`</div>`+
			`<ul class="category-items" style="width:max-content;min-width:100%%;box-sizing:border-box;padding:8px;margin:0;display:flex;flex-direction:column;gap:2px;list-style:none">%s</ul>`+
			`</div>`,
		wrapperClass, HudBaseStyles(), html.EscapeString(title), html.EscapeString(title), html.EscapeString(closeExpr), itemsHTML,
	)
}

// RenderSectionsHTML renders a slice of HUD sections into HTML. When an
// optional floatKind ("commands") is passed, each titled group gets a pop-out
// icon that floats it as a reference card. Omitted for the discovery overlay
// (a live view, not poppable) and tests.
func RenderSectionsHTML(sections []HudSection, conf HudConfig, floatKind ...string) string {
	kind := ""
	if len(floatKind) > 0 {
		kind = floatKind[0]
	}
	var b strings.Builder
	for _, section := range sections {
		var items strings.Builder
		for _, item := range section.Items {
			items.WriteString(RenderItemHTML(item))
		}
		// Header shown once per group; skipped entirely for an untitled group
		// (e.g. literal next-word completions) so there's no empty header gap.
		// Title rendered as-is (no upper-casing) for a modern, sentence-case
		// look — the label already arrives cased (e.g. "Badge"). Titled groups
		// get a pop-out icon to float that command group as a reference card.
		header := ""
		if section.Title != "" {
			if kind != "" {
				header = fmt.Sprintf(
					"<div style='display:flex;align-items:center;gap:6px'><h2 class='category-header' style='flex-grow:1'>%s</h2>%s</div>",
					html.EscapeString(section.Title), FloatButtonHTML(kind, section.Title))
			} else {
				header = fmt.Sprintf("<h2 class='category-header'>%s</h2>", html.EscapeString(section.Title))
			}
		}
		// No explicit width: the flex container decides. In the discovery
		// column (align-items:stretch) the section fills the panel so keycaps
		// right-justify; in the main HUD's row it stays content-width so
		// categories sit side by side.
		fmt.Fprintf(&b,
			"<section class='hud-category' style='flex-shrink:0;display:flex;flex-direction:column;gap:4px'>%s<ul class='category-items' style='display:flex;flex-direction:column;gap:2px'>%s</ul></section>",
			header,
			items.String(),
		)
	}

	inner := b.String()
	if inner == "" {
		return ""
	}

	wrapperClass := "hud-wrap"
	if conf.CompactMode {
		wrapperClass += " hud-compact"
	}

	var out strings.Builder
	out.WriteString(HudBaseStyles())
	if !conf.ShowBadges {
		out.WriteString("<style>.hud-tag{display:none}</style>")
	}
	fmt.Fprintf(&out, "<div class='%s' style='display:contents'>%s</div>", wrapperClass, inner)
	return out.String()
}
