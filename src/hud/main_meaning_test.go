package hud

import (
	"testing"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

// The main HUD's content reaches non-visual renderers: command groups as a
// choices document, a sectionless mode (history) as its title and summary.
func TestMainMeaningCarriesTheHUDsContent(t *testing.T) {
	note := "Undo the last edit."
	doc := MainMeaning("commands", HudResponse{
		Title:  "Commands",
		Footer: "54 live",
		Sections: []HudSection{
			{Title: "Editing", Items: []HudItem{{ID: "undo", Title: "undo", Subtitle: &note}, {ID: "save", Title: "save"}}},
			{Title: "Empty"},
		},
	})
	if doc.Channel != ChannelMain || doc.Kind != branchkit.OutputKindChoices {
		t.Fatalf("channel/kind = %q/%q", doc.Channel, doc.Kind)
	}
	if len(doc.Sections) != 1 || len(doc.Sections[0].Items) != 2 || doc.Phrase != "2 items" {
		t.Fatalf("sections/phrase = %+v / %q", doc.Sections, doc.Phrase)
	}
	if it := doc.Sections[0].Items[0]; it.Phrase != "undo" || it.Action != nil {
		t.Fatalf("a command row is information only: %+v", it)
	}
	if doc.Footer == nil || *doc.Footer != "54 live" {
		t.Fatalf("footer = %v", doc.Footer)
	}

	card := Meaning("ref_entity_apps", "entity", HudResponse{
		Title:    "Apps",
		Sections: []HudSection{{Items: []HudItem{{Title: "Safari"}, {}}}},
	})
	if card.Channel != "ref_entity_apps" || len(card.Sections) != 1 || len(card.Sections[0].Items) != 1 || card.Sections[0].Items[0].ID != "Safari" {
		t.Fatalf("a card's rows need ids and skip blanks: %+v", card.Sections)
	}

	hist := MainMeaning("history", HudResponse{Title: "History", Footer: "Recent commands"})
	if hist.Kind != branchkit.OutputKindOutcome || hist.Phrase != "Recent commands" || len(hist.Sections) != 0 {
		t.Fatalf("history = %+v", hist)
	}
}
