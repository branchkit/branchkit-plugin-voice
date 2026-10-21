package hud

import (
	"sync"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// OnExpire declares what happens when a discovery countdown reaches zero.
type OnExpire struct {
	Close bool `json:"close"`
}

// discoveryState is what is true on the discovery channel right now — the
// document `output.state` carries, held here so the countdown, the live
// transcript and the model-loading status can re-push it as they change.
// The platform renders it (and any other renderer consumes it); this
// package no longer produces markup for the channel.
type discoveryState struct {
	mu sync.Mutex

	isOpen   bool
	kind     string
	title    string
	phrase   string
	sections []branchkit.OutputSection
	footer   string

	countdownTotal     int
	countdownRemaining int
	cancelCountdown    chan struct{}
	generation         int

	isListening  bool
	transcript   string
	preparing    bool
	silenceTimer *time.Timer
}

// IsDiscoveryOpen returns whether the discovery overlay is open.
func (h *HUD) IsDiscoveryOpen() bool {
	h.discovery.mu.Lock()
	defer h.discovery.mu.Unlock()
	return h.discovery.isOpen
}

// OpenDiscovery opens the discovery overlay on a `choices` document —
// `sections` are what the person can say, `phrase` is the same in words
// ("twelve commands") for a renderer that has no list — with an optional
// countdown.
func (h *HUD) OpenDiscovery(title string, sections []branchkit.OutputSection, phrase, footer string, countdownSecs int, onExpire OnExpire) {
	if !h.Config().IsChannelEnabled(ChannelDiscovery) {
		return
	}
	h.discovery.mu.Lock()
	if h.discovery.cancelCountdown != nil {
		close(h.discovery.cancelCountdown)
		h.discovery.cancelCountdown = nil
	}
	if h.discovery.silenceTimer != nil {
		h.discovery.silenceTimer.Stop()
		h.discovery.silenceTimer = nil
	}

	h.discovery.isOpen = true
	h.discovery.kind = branchkit.OutputKindChoices
	h.discovery.title = title
	h.discovery.phrase = phrase
	h.discovery.sections = sections
	h.discovery.footer = footer
	h.discovery.countdownTotal = countdownSecs
	h.discovery.countdownRemaining = countdownSecs
	h.discovery.generation++
	gen := h.discovery.generation
	h.discovery.mu.Unlock()

	// The title's length, not the title: on a miss it is the words heard,
	// upper-cased, and the log must not hold what the user said.
	branchkit.Logf("voice", "discovery: open title=%d chars sections=%d countdown=%d gen=%d",
		len(title), len(sections), countdownSecs, gen)
	h.pushDiscoveryState()
	h.signalShell("hud:open:discovery")

	if countdownSecs > 0 {
		cancel := make(chan struct{})
		h.discovery.mu.Lock()
		h.discovery.cancelCountdown = cancel
		h.discovery.mu.Unlock()
		go h.runCountdown(cancel, countdownSecs, onExpire, gen)
	}
}

// OpenOverlay opens the discovery overlay on a transient document of any
// kind — the boot-time warm-up is `progress`, a refused keystroke is
// `problem`. Returns the overlay generation so later updates can bail if a
// real Option+G/T interaction has taken the overlay over. No-op (returns 0)
// if the discovery channel is disabled.
func (h *HUD) OpenOverlay(kind, title, phrase string, sections []branchkit.OutputSection) int {
	if !h.Config().IsChannelEnabled(ChannelDiscovery) {
		return 0
	}
	h.discovery.mu.Lock()
	if h.discovery.cancelCountdown != nil {
		close(h.discovery.cancelCountdown)
		h.discovery.cancelCountdown = nil
	}
	if h.discovery.silenceTimer != nil {
		h.discovery.silenceTimer.Stop()
		h.discovery.silenceTimer = nil
	}
	h.discovery.isOpen = true
	h.discovery.kind = kind
	h.discovery.title = title
	h.discovery.phrase = phrase
	h.discovery.sections = sections
	h.discovery.footer = ""
	h.discovery.countdownTotal = 0
	h.discovery.countdownRemaining = 0
	h.discovery.generation++
	gen := h.discovery.generation
	h.discovery.mu.Unlock()

	h.pushDiscoveryState()
	h.signalShell("hud:open:discovery")
	return gen
}

// UpdateOverlay supersedes the overlay's document, but only while it still
// owns the overlay (generation unchanged) — if the user opened
// commands/dictation in the meantime, their view wins and this is a no-op.
func (h *HUD) UpdateOverlay(gen int, kind, title, phrase string, sections []branchkit.OutputSection) {
	h.discovery.mu.Lock()
	if gen == 0 || h.discovery.generation != gen || !h.discovery.isOpen {
		h.discovery.mu.Unlock()
		return
	}
	h.discovery.kind = kind
	h.discovery.title = title
	h.discovery.phrase = phrase
	h.discovery.sections = sections
	h.discovery.mu.Unlock()
	h.pushDiscoveryState()
}

// LeadDiscoveryPhrase puts a sentence in front of the open choices
// document's phrase, keeping its list, countdown and generation, and reports
// whether it did. It is how something that happened is said while a mode's
// choices are showing, as an outcome is ("snap left. 6 commands"): replacing
// the document would take the list away mid-mode. Any other open document
// (a problem, a live transcript) is left alone and this returns false.
func (h *HUD) LeadDiscoveryPhrase(lead string) bool {
	h.discovery.mu.Lock()
	if !h.discovery.isOpen || h.discovery.kind != branchkit.OutputKindChoices ||
		h.discovery.preparing || (h.discovery.isListening && h.discovery.transcript != "") {
		h.discovery.mu.Unlock()
		return false
	}
	if h.discovery.phrase == "" {
		h.discovery.phrase = lead
	} else {
		h.discovery.phrase = lead + ". " + h.discovery.phrase
	}
	h.discovery.mu.Unlock()
	h.pushDiscoveryState()
	return true
}

// CloseOverlay closes the overlay panel iff it still owns the overlay.
func (h *HUD) CloseOverlay(gen int) {
	h.discovery.mu.Lock()
	stale := gen == 0 || h.discovery.generation != gen
	h.discovery.mu.Unlock()
	if stale {
		return
	}
	h.CloseDiscovery()
}

// CloseDiscovery closes the discovery overlay. Purely visual — tag
// lifecycle is handled by _platform.input.session_boundary.
func (h *HUD) CloseDiscovery() {
	if !h.closeDiscoveryState() {
		return
	}
	// Nothing is true on the channel now: clear the state (every renderer
	// stops conveying it, generation-stamped) and hide the window.
	if _, err := h.plugin.OutputClear(branchkit.OutputClearRequest{Channel: ChannelDiscovery}); err != nil {
		branchkit.Logf("voice", "discovery: output.clear failed: %v", err)
	}
	h.signalShell("hud:close:discovery")
}

// EndDiscoveryWithOutcome closes discovery at a hold's end when the hold
// executed a command while its mode stayed open: the channel's final state
// is that command's words as an `outcome`, not a clear — a renderer that
// speaks at the boundary says "snap right" — and the window hides. The
// outcome stands until the next state; nothing about the closed mode is
// still claimed, because the choices are gone with it.
func (h *HUD) EndDiscoveryWithOutcome(phrase string) {
	if !h.closeDiscoveryState() {
		return
	}
	h.PushOutcome(phrase)
	h.signalShell("hud:close:discovery")
}

// closeDiscoveryState tears down the local discovery state machine and
// reports whether discovery was open. The caller decides what the channel's
// state becomes.
func (h *HUD) closeDiscoveryState() bool {
	h.discovery.mu.Lock()
	if !h.discovery.isOpen {
		h.discovery.mu.Unlock()
		return false
	}
	if h.discovery.cancelCountdown != nil {
		close(h.discovery.cancelCountdown)
		h.discovery.cancelCountdown = nil
	}
	if h.discovery.silenceTimer != nil {
		h.discovery.silenceTimer.Stop()
		h.discovery.silenceTimer = nil
	}
	h.discovery.isOpen = false
	h.discovery.isListening = false
	h.discovery.transcript = ""
	h.discovery.preparing = false
	h.discovery.mu.Unlock()
	return true
}

// SetPreparing toggles the "Loading model…" status shown in the dictation
// overlay while the STT model is still loading (before it can transcribe).
// Cleared automatically when a real transcript arrives or the overlay closes.
func (h *HUD) SetPreparing(preparing bool) {
	h.discovery.mu.Lock()
	h.discovery.preparing = preparing
	h.discovery.mu.Unlock()
	h.pushDiscoveryState()
}

// UpdateTranscript updates the listening indicator and live transcript text.
func (h *HUD) UpdateTranscript(transcript string, isListening bool) {
	h.discovery.mu.Lock()
	h.discovery.isListening = isListening
	h.discovery.transcript = transcript
	// A live transcript means the model is up and running — drop any loading status.
	if isListening {
		h.discovery.preparing = false
	}

	if h.discovery.silenceTimer != nil {
		h.discovery.silenceTimer.Stop()
	}
	if isListening {
		h.discovery.silenceTimer = time.AfterFunc(2*time.Second, func() {
			h.discovery.mu.Lock()
			h.discovery.isListening = false
			h.discovery.transcript = ""
			h.discovery.mu.Unlock()
			h.pushDiscoveryState()
		})
	}
	h.discovery.mu.Unlock()

	h.pushDiscoveryState()
}

// runCountdown waits out the countdown and applies on_expire. The document
// was pushed ONCE with its full `progress`; renderers keep time themselves
// (the visual bar runs down on its own, a speech renderer would not read a
// countdown at all). Re-pushing every second would say nothing new while
// stamping a new generation each time — and a renderer mid-utterance must
// only be interrupted by a new statement, not by a clock.
func (h *HUD) runCountdown(cancel chan struct{}, totalSecs int, onExpire OnExpire, gen int) {
	timer := time.NewTimer(time.Duration(totalSecs) * time.Second)
	defer timer.Stop()
	select {
	case <-cancel:
		return
	case <-timer.C:
	}

	h.discovery.mu.Lock()
	if h.discovery.generation != gen || !h.discovery.isOpen {
		h.discovery.mu.Unlock()
		return
	}
	h.discovery.countdownRemaining = 0
	h.discovery.mu.Unlock()

	branchkit.Logf("voice", "discovery: countdown expired gen=%d — applying on_expire", gen)
	if onExpire.Close {
		h.CloseDiscovery()
	}
}

// PushOutcome states what just happened on the discovery channel — the
// words of the command that was acted on — as a `notable` outcome. The
// window is usually hidden by then (a match closes discovery); this is for
// every renderer that is not the window. It supersedes the choices that led
// to it, which is what a person who cannot see the list needs to hear.
func (h *HUD) PushOutcome(phrase string) {
	if phrase == "" || !h.Config().IsChannelEnabled(ChannelDiscovery) {
		return
	}
	doc := branchkit.OutputState{
		Channel: ChannelDiscovery,
		Kind:    branchkit.OutputKindOutcome,
		Title:   "Command",
		Phrase:  phrase,
		Urgency: branchkit.OutputUrgencyNotable,
		Locale:  "en",
		V:       1,
	}
	if _, err := h.plugin.OutputState(branchkit.OutputStateRequest{State: doc}); err != nil {
		branchkit.Logf("voice", "discovery: outcome output.state failed: %v", err)
	}
}

// pushDiscoveryState makes the current discovery document the channel's
// state. The platform's visual renderer draws it into the discovery window;
// every other renderer — speech, accessibility, a plugin on
// `_platform.output.state` — receives the same document. A push replaces
// the previous state; it is never appended.
//
// While the window is closed only the dictation statuses push (a loading
// model, a live transcript): they describe what is under way whether or not
// anything is drawing it, and the window hides regardless.
func (h *HUD) pushDiscoveryState() {
	h.discovery.mu.Lock()
	doc := branchkit.OutputState{
		Channel:  ChannelDiscovery,
		Kind:     h.discovery.kind,
		Title:    h.discovery.title,
		Phrase:   h.discovery.phrase,
		Sections: h.discovery.sections,
		Urgency:  urgencyFor(h.discovery.kind),
		Locale:   "en",
		V:        1,
	}
	if h.discovery.footer != "" {
		footer := h.discovery.footer
		doc.Footer = &footer
	}
	if h.discovery.countdownTotal > 0 {
		doc.Progress = &branchkit.OutputProgress{
			RemainingMs: h.discovery.countdownRemaining * 1000,
			TotalMs:     h.discovery.countdownTotal * 1000,
		}
	}
	isOpen := h.discovery.isOpen
	if h.discovery.isListening && h.discovery.transcript != "" {
		// The live transcript is the whole state while it lasts.
		doc.Kind = branchkit.OutputKindProgress
		doc.Phrase = h.discovery.transcript
		doc.Sections = nil
		doc.Footer = nil
		doc.Progress = nil
		if doc.Title == "" {
			doc.Title = "Dictation"
		}
	} else if h.discovery.preparing {
		// First dictation before the model finishes loading: say so, so the
		// hold doesn't read as broken (the audio can't transcribe yet).
		doc.Kind = branchkit.OutputKindProgress
		doc.Title = "Dictation"
		doc.Phrase = "Loading model…"
		doc.Sections = nil
		doc.Footer = nil
		doc.Progress = nil
	} else if !isOpen {
		h.discovery.mu.Unlock()
		return
	}
	if doc.Kind == "" {
		doc.Kind = branchkit.OutputKindChoices
	}
	h.discovery.mu.Unlock()

	if _, err := h.plugin.OutputState(branchkit.OutputStateRequest{State: doc}); err != nil {
		branchkit.Logf("voice", "discovery: output.state failed: %v", err)
	}
}

// urgencyFor is the producer's claim about how much a document matters:
// a problem needs the person now, an outcome is worth their attention at
// the next boundary, choices and progress are background truth. What each
// means is every renderer's own policy.
func urgencyFor(kind string) string {
	switch kind {
	case branchkit.OutputKindProblem:
		return branchkit.OutputUrgencyInterrupt
	case branchkit.OutputKindOutcome:
		return branchkit.OutputUrgencyNotable
	default:
		return branchkit.OutputUrgencyAmbient
	}
}

func (h *HUD) signalShell(action string) {
	switch action {
	case "hud:open:discovery":
		h.plugin.HUDShow(branchkit.HUDShowRequest{Channel: "discovery"})
	case "hud:close:discovery":
		h.plugin.HUDHide(branchkit.HUDHideRequest{Channel: "discovery"})
	default:
		branchkit.Logf("voice", "hud: no HUD action %q", action)
	}
}
