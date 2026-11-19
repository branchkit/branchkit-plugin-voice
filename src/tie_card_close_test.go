package main

import (
	"encoding/json"
	"os"
	"testing"
)

// Closing the "Which command?" card must end the tie behind it: the card
// names an on_close action, the action takes no fields (on_close sends
// only the channel), and voice handles it.
func TestTheTieCardEndsTheTieWhenClosed(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var manifest struct {
		HudCards []struct {
			Channel string `json:"channel"`
			OnClose string `json:"on_close"`
		} `json:"hud_cards"`
		ActionTypes map[string]struct {
			Fields []struct {
				Required bool `json:"required"`
			} `json:"fields"`
		} `json:"action_types"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse plugin.json: %v", err)
	}
	onClose := ""
	for _, c := range manifest.HudCards {
		if c.Channel == "voice.which" {
			onClose = c.OnClose
		}
	}
	if onClose != "voice.dismiss_tie" {
		t.Fatalf("voice.which closes through %q, want voice.dismiss_tie", onClose)
	}
	decl, ok := manifest.ActionTypes["dismiss_tie"]
	if !ok {
		t.Fatalf("dismiss_tie is not a declared action type")
	}
	for _, f := range decl.Fields {
		if f.Required {
			t.Errorf("on_close sends only the channel: dismiss_tie must require nothing")
		}
	}
	h := newTestHost()
	if _, ok := actionHandlersForTest(h)[onClose]; !ok {
		t.Errorf("no handler for %s", onClose)
	}
}
