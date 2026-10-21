package main

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/branchkit/branchkit-plugin-voice/hud"
	"github.com/branchkit/plugin-sdk-go"
)

// buildFloatableCatalog derives the plugin.voice.floatable_collections catalog from the
// live entity collections and pushes it. Each entry's spoken key is the
// collection's own (single-word) name and the value is the same collection id,
// so "open collection <name>" captures directly to the collection to float.
// Namespaced collections (containing ".") are skipped: their names aren't
// cleanly speakable and they're internal machinery, not reference lists.
func (h *Host) buildFloatableCatalog() {
	entity := "entity"
	sections, err := h.plugin.CollectionsList(branchkit.CollectionsListRequest{Kind: &entity})
	if err != nil {
		branchkit.Logf("voice", "floatable: collections.list failed: %v", err)
		return
	}

	type fcEntry struct {
		Spoken     string `json:"spoken"`
		Collection string `json:"collection"`
	}
	names := make([]string, 0, len(sections))
	records := make([]branchkit.CollectionPutEntry, 0, len(sections))
	for _, sec := range sections {
		if strings.Contains(sec.Name, ".") { // the catalog itself is namespaced, so this skips it too
			continue
		}
		raw, err := json.Marshal(fcEntry{Spoken: sec.Name, Collection: sec.Name})
		if err != nil {
			branchkit.Logf("voice", "floatable: marshal %q: %v", sec.Name, err)
			return
		}
		names = append(names, sec.Name)
		records = append(records, branchkit.CollectionPutEntry{ID: sec.Name, Payload: raw})
	}
	sort.Strings(names)
	snapshot := strings.Join(names, ",")

	// Skip the push when the eligible set matches the last CONFIRMED push, so
	// a high-frequency collection (browser hints) rebuilding on every change
	// doesn't cost a replace each time. The snapshot is recorded only after
	// the replace succeeds: recorded before it, a failed push read as done and
	// the catalog stayed stale until restart. A timed-out push may have
	// landed; the next rebuild retries it, and a replace that changes nothing
	// writes and emits nothing.
	h.floatableMu.Lock()
	unchanged := snapshot == h.floatableNames
	h.floatableMu.Unlock()
	if unchanged {
		return
	}

	if _, err := h.plugin.Replace("plugin.voice.floatable_collections", records, branchkit.ScopeCollection()); err != nil {
		branchkit.Logf("voice", "floatable: failed to push catalog: %v", err)
		return
	}
	h.floatableMu.Lock()
	h.floatableNames = snapshot
	h.floatableMu.Unlock()
	branchkit.Logf("voice", "floatable: catalog now %d collections: [%s]", len(records), snapshot)
}

// openFloat renders a HUD group into the draggable "reference" channel and
// makes it visible. `kind` selects the source: "commands" floats a command
// category (by title) from commands.list; anything else ("entity") floats a
// named entity collection from collections.list. Shared by the pop-out icon
// buttons (open_float method) and the "open collection <name>" voice command.
func (h *Host) openFloat(kind, id string) {
	if id == "" {
		return
	}

	var title, itemsHTML string
	var rows []hud.HudItem
	switch kind {
	case "commands":
		resp, err := h.plugin.CommandsList()
		if err != nil {
			branchkit.Logf("voice", "open_float: commands.list failed: %v", err)
			return
		}
		for _, sec := range hud.SectionsFromCommandList(resp.Sections) {
			if sec.Title == id {
				title = sec.Title
				var b strings.Builder
				for _, it := range sec.Items {
					b.WriteString(hud.RenderItemHTML(it))
				}
				itemsHTML = b.String()
				rows = sec.Items
				break
			}
		}
	default: // "entity"
		entity := "entity"
		sections, err := h.plugin.CollectionsList(branchkit.CollectionsListRequest{Kind: &entity})
		if err != nil {
			branchkit.Logf("voice", "open_float: collections.list failed: %v", err)
			return
		}
		for _, sec := range sections {
			if sec.Name == id {
				title = sec.Label
				if title == "" {
					title = sec.Name
				}
				var b strings.Builder
				for _, it := range sec.Items {
					row := hud.HudItem{
						ID: it.Title, Title: it.Title, Subtitle: it.Subtitle,
						Speakable: it.Speakable,
					}
					rows = append(rows, row)
					b.WriteString(hud.RenderItemHTML(row))
				}
				itemsHTML = b.String()
				break
			}
		}
	}

	if itemsHTML == "" {
		branchkit.Logf("voice", "open_float: nothing to render for kind=%q id=%q", kind, id)
		return
	}

	// Each group floats into its OWN channel (keyed by kind+id), so panels are
	// independent: multiple can be open at once, and each remembers its own
	// size/position (the shell persists per-channel). A brand-new group has no
	// saved frame and auto-fits to its content.
	channelID := floatChannelID(kind, id)
	h.floatMu.Lock()
	created := h.floatPanels[channelID]
	h.floatMu.Unlock()
	if !created {
		anchor := branchkit.AnchorTopLeft
		width, minHeight := 340, 240
		acceptsInput, followsFocus, draggable := true, false, true
		if err := h.plugin.HUDCreateChannel(branchkit.HUDCreateChannelRequest{
			Channel:      channelID,
			AcceptsInput: &acceptsInput,
			Anchor:       &anchor,
			Description:  &title,
			Draggable:    &draggable,
			FollowsFocus: &followsFocus,
			MinHeight:    &minHeight,
			Width:        &width,
		}); err != nil {
			// Likely the per-plugin channel cap — surface it and bail.
			branchkit.Logf("voice", "open_float: create_channel %q failed (cap reached?): %v", channelID, err)
			return
		}
		h.floatMu.Lock()
		h.floatPanels[channelID] = true
		h.floatMu.Unlock()
	}

	conf := h.hud.Config()
	html := hud.RenderFloatCard(channelID, title, itemsHTML, conf)
	if err := h.plugin.HUDPushRaw(channelID, html); err != nil {
		branchkit.Logf("voice", "open_float: hud.push failed: %v", err)
		return
	}
	if err := h.plugin.HUDShow(branchkit.HUDShowRequest{Channel: channelID}); err != nil {
		branchkit.Logf("voice", "open_float: hud.show failed: %v", err)
	}
	// The card's rows as meaning, for speech and the window's accessibility
	// element; voice draws the card itself, so the platform draws nothing.
	drawsOwn := true
	doc := hud.Meaning(channelID, kind, hud.HudResponse{
		Title:    title,
		Sections: []hud.HudSection{{Items: rows}},
	})
	if _, err := h.plugin.OutputState(branchkit.OutputStateRequest{State: doc, DrawsOwnWindow: &drawsOwn}); err != nil {
		branchkit.Logf("voice", "open_float: output.state failed: %v", err)
	}
}

// floatChannelID derives a stable, valid channel id for a floated group, so the
// same group always maps to the same window (and its persisted frame).
func floatChannelID(kind, id string) string {
	var b strings.Builder
	b.WriteString("ref_")
	b.WriteString(kind)
	b.WriteByte('_')
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// handleOpenFloatMethod backs the pop-out icon buttons in the Option+B HUD.
func (h *Host) handleOpenFloatMethod(req *struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}) error {
	h.openFloat(req.Kind, req.ID)
	return nil
}

// handleCloseFloat closes a single floating panel (its ✕ button) and frees its
// dynamic channel so the slot is reclaimed.
func (h *Host) handleCloseFloat(req *struct {
	Channel string `json:"channel"`
}) error {
	if req.Channel != "" {
		h.plugin.HUDHide(branchkit.HUDHideRequest{Channel: req.Channel})
		h.plugin.HUDRemoveChannel(branchkit.HUDRemoveChannelRequest{Channel: req.Channel})
		h.floatMu.Lock()
		delete(h.floatPanels, req.Channel)
		h.floatMu.Unlock()
	}
	return nil
}

// handleCloseFloatsAction backs "close cards": every floating reference card
// closes. Each card's own close button needs a pointer, so without this a
// person working by voice alone had no way to put one away.
func (h *Host) handleCloseFloatsAction(_ *branchkit.OnActionRequest) (any, error) {
	h.floatMu.Lock()
	open := make([]string, 0, len(h.floatPanels))
	for ch := range h.floatPanels {
		open = append(open, ch)
	}
	h.floatMu.Unlock()
	for _, ch := range open {
		_ = h.handleCloseFloat(&struct {
			Channel string `json:"channel"`
		}{Channel: ch})
	}
	return h.handled(), nil
}

// handleOpenFloatAction backs the "open collection <name>" voice command.
func (h *Host) handleOpenFloatAction(req *branchkit.OnActionRequest) (any, error) {
	var p struct {
		Collection string `json:"collection"`
	}
	if err := req.UnmarshalParams(&p); err != nil {
		return nil, err
	}
	h.openFloat("entity", p.Collection)
	return h.handled(), nil
}
