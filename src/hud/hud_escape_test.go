package hud

import (
	"html"
	"strings"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// Every string these renderers interpolate arrives from outside voice: a
// command pattern, a collection a plugin (or the user) filled, a plugin id.
// The HUD is a live HTML surface with Datastar behind it, so a row title that
// reaches the page unescaped is markup — and an attribute value that breaks
// out of its quotes is script. These tests hold the escaping at each site.

const hostile = "<img src=x onerror=1>"

func strPtr(s string) *string { return &s }

func TestRenderItemHTMLEscapesText(t *testing.T) {
	cases := map[string]HudItem{
		"title":         {Title: hostile},
		"short note":    {Title: "t", Subtitle: strPtr("<b>")},
		"long subtitle": {Title: "t", Subtitle: strPtr(hostile)},
		"tag":           {Title: "t", Tag: strPtr(hostile)},
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			got := RenderItemHTML(item)
			if strings.Contains(got, "<img") || strings.Contains(got, "<b>") {
				t.Errorf("raw markup reached the HUD: %s", got)
			}
			if !strings.Contains(got, "&lt;") {
				t.Errorf("text was dropped instead of escaped: %s", got)
			}
		})
	}
}

// A command pattern's capture placeholder is text the user should read, not
// an unknown element the page swallows.
func TestRenderItemHTMLShowsAngleBracketPlaceholders(t *testing.T) {
	got := RenderItemHTML(HudItem{Title: "go to <number>"})
	if !strings.Contains(got, "go to &lt;number&gt;") {
		t.Errorf("placeholder not rendered as text: %s", got)
	}
}

func TestRenderEntitySectionsHTMLEscapesText(t *testing.T) {
	sections := []EntitySection{{
		Name:   "s",
		Label:  "<b>",
		Plugin: "<i>",
		Items: []branchkit.CollectionsListItem{{
			Title:    hostile,
			Source:   "x' onmouseover='y",
			Subtitle: strPtr("<u>long"),
		}},
	}}
	got := RenderEntitySectionsHTML(sections, HudConfig{})
	for _, raw := range []string{"<B>", "<b>", "<I>", "<img", "<u>", "' onmouseover"} {
		if strings.Contains(got, raw) {
			t.Errorf("unescaped %q in entity HUD:\n%s", raw, got)
		}
	}
	if !strings.Contains(got, "title='x&#39; onmouseover=&#39;y'") {
		t.Errorf("tooltip not escaped as one attribute value:\n%s", got)
	}
}

func TestRenderSectionsHTMLEscapesSectionTitle(t *testing.T) {
	sections := []HudSection{{Title: hostile, Items: []HudItem{{Title: "t"}}}}
	for _, kind := range [][]string{nil, {"commands"}} {
		got := RenderSectionsHTML(sections, HudConfig{}, kind...)
		if strings.Contains(got, "<img") {
			t.Errorf("floatKind=%v: raw section title in HUD:\n%s", kind, got)
		}
	}
}

func TestRenderFloatCardEscapesTitle(t *testing.T) {
	got := RenderFloatCard("ch", hostile, "<li>kept</li>", HudConfig{})
	if strings.Contains(got, "<IMG") {
		t.Errorf("raw float title in HUD:\n%s", got)
	}
	if !strings.Contains(got, "<li>kept</li>") {
		t.Errorf("pre-rendered items must pass through unchanged:\n%s", got)
	}
}

// jsString output sits in a single-quoted JS literal, and the whole Datastar
// expression is HTML-escaped once for its double-quoted attribute (the
// composition below is what FloatButtonHTML and the float card do). The browser decodes the attribute's character
// references BEFORE Datastar evaluates the expression, so what matters is the
// decoded text: no quote may survive decoding without its backslash.
func TestJSStringSurvivesAttributeDecoding(t *testing.T) {
	jsAttrEscape := func(s string) string { return html.EscapeString(jsString(s)) }
	for _, in := range []string{
		"a&#39;b", "a&apos;b", "a'b", `a"b`, "a&quot;b", `a\'b`, "a<b", "plain",
	} {
		decoded := html.UnescapeString(jsAttrEscape(in))
		if unescapedQuote(decoded) {
			t.Errorf("jsAttrEscape(%q) decodes to %q: a quote ends the JS literal", in, decoded)
		}
		if raw := jsAttrEscape(in); strings.ContainsAny(raw, `<"`) {
			t.Errorf("jsAttrEscape(%q) = %q: a raw '<' or '\"' in the attribute", in, raw)
		}
	}
	// Round trip: the JS literal, once decoded and unescaped, is the input.
	if got := html.UnescapeString(jsAttrEscape("a&#39;b")); got != "a&#39;b" {
		t.Errorf("decoded %q, want the literal text back", got)
	}
}

// unescapedQuote reports a ' not preceded by an odd run of backslashes.
func unescapedQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}
		n := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			n++
		}
		if n%2 == 0 {
			return true
		}
	}
	return false
}
