package hud

import "github.com/branchkit/plugin-sdk-go"

// Voice's own HUD content vocabulary.
//
// These used to come from the SDK, generated from `render_hud`'s request and
// result schemas. That method was a platform hook the platform never called,
// and removing it took its schemas — and therefore these types — with it.
//
// Voice still needs them, which is the point worth recording: it drives an
// internal pull model (PushMainHUD asks the render callback New was given for content, which is
// voice's own handler) and then renders the result to HTML and ships it with
// `hud.push`. The platform contract is the HTML; these shapes are how voice
// gets there, so they belong to voice.
//
// If a pull-model HUD ever returns to the platform, these are what its
// result schema looked like.

// RenderHUDRequest asks for the content of one HUD mode.
//
// Mode is all of it. The platform version carried Title, Footer and Sections
// so a caller could pre-populate or override what the plugin returned — and
// since the platform never called it, the only caller was voice asking itself,
// which passes the mode and nothing else. Those three were read by the handler
// on every render and set by no one; they went with the hook.
type RenderHUDRequest struct {
	HudMode string `json:"hud_mode"`
}

// HudResponse is one mode's rendered content. ContentHTML and Sections are
// alternatives: Sections is the structured form, ContentHTML the escape hatch.
type HudResponse struct {
	ContentHTML *string      `json:"content_html,omitempty"`
	Footer      string       `json:"footer"`
	Sections    []HudSection `json:"sections,omitempty"`
	Title       string       `json:"title"`
}

// HudSection is a titled group of items.
type HudSection struct {
	Items []HudItem `json:"items"`
	Title string    `json:"title"`
}

// HudItem is one row: a command, an entity, a model.
type HudItem struct {
	Icon     *string `json:"icon,omitempty"`
	ID       string  `json:"id"`
	Subtitle *string `json:"subtitle,omitempty"`
	Tag      *string `json:"tag,omitempty"`
	Title    string  `json:"title"`
	// Speakable marks a row whose SUBTITLE is itself a matchable spoken form
	// (an alias equal to the display name — a promoted selection record).
	// Rendered as a small mic glyph after the subtitle.
	Speakable bool `json:"speakable,omitempty"`
}

// SectionsFromCommandList converts the platform's commands.list sections into
// the local render shape.
//
// HudItem is NOT a mirror of ListCommandItem — it carries Icon that the wire
// has no notion of, and ignores Dynamic/DynamicOwner that the wire does — so this is a real conversion rather than an alias. Before
// 2026-09-20 the call sites decoded the wire response straight into
// []HudSection and relied on JSON field names lining up, which meant a
// platform rename would have silently blanked subtitles rather than failing.
func SectionsFromCommandList(in []branchkit.ListCommandSection) []HudSection {
	out := make([]HudSection, 0, len(in))
	// Speakable is deliberately not set here: a `commands.list` subtitle is a
	// variation count ("3 variations"), never a spoken alias, so no row on
	// this path can be speakable-by-name. The entity browse (collections.list,
	// float.go) is the surface that carries the flag.
	for _, sec := range in {
		items := make([]HudItem, 0, len(sec.Items))
		for _, it := range sec.Items {
			items = append(items, HudItem{
				ID:       it.ID,
				Title:    it.Title,
				Subtitle: it.Subtitle,
				Tag:      it.Tag,
			})
		}
		out = append(out, HudSection{Title: sec.Title, Items: items})
	}
	return out
}
