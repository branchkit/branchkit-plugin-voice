package main

import (
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	branchkit "github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/plugin-sdk-go/ui"

	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/branchkit-plugin-voice/views"
)

// Recent dictations — the Option+B "Dictation" HUD tab, and the answer to
// "where did my text go?"
//
// A dictated transcript is typed at the cursor and then, as far as the user is
// concerned, gone. If focus moved mid-transcribe, the app swallowed the paste,
// or it landed in the wrong window, the words exist only in platform.log. That
// is a recovery problem, not a nostalgia one, and it is the single most
// annoying way to lose a long dictation.
//
// IN-MEMORY ONLY, deliberately. Command history has a durable half
// (`recognition_log`) because margins are training data; prose is not. Writing
// the user's dictated sentences to a new on-disk store is a fresh category of
// data-at-rest and wants an explicit decision, not a default — and it would buy
// little, since recovery is a "just now" need. Nor does voice log them: the
// platform's own transcript lines keep the words only under its log-recording
// setting (on by default in a dev build, off in a release build), so that is
// the one place to dig further back.
//
// A pure VIEW of transcripts the plugin already handles: no new subscription,
// timer, or sensor, so it does not trip the one-in-one-out sensing freeze.

const dictationHistoryCap = 15

// dictationEntry is one dictation's trip through the text pipeline:
// what each step produced, and the facts that decided it. The
// record is what makes cleanup visible and reversible: the panel shows what a
// step changed, "Fix a word" picks from what Whisper actually heard, and
// "type original" can put the unedited transcript back.
type dictationEntry struct {
	ID string
	At time.Time
	// Heard is the speech stage's transcript, before any step touched it.
	Heard string
	// Repaired is Heard after the repair step: corrections (the
	// hallucination filter among them), the all-caps fix, and for a speak
	// session the stop-phrase backstop.
	Repaired string
	// Shaped is Repaired after the dictation profile.
	Shaped string
	// Refined is Shaped after the refine step (refine.go): the local model's
	// clean-up when it ran and its edit was accepted, else Shaped.
	Refined string
	// RefineNote says what the refine step did: cleaned up, unchanged,
	// refused (and why), skipped (and why), or "" when it is switched off.
	RefineNote string
	// Text is what was sent to the user: typed at the caret (seam
	// included), or kept on the clipboard when focus moved. Empty when a step
	// removed everything, which is itself worth seeing. It reached them
	// unless TypeError is set.
	Text string
	// TypeError is why typing Text failed (input.type_text refused or
	// errored), so nothing landed. The overlay saying so closes after a few
	// seconds; this is what stays.
	TypeError string
	// Profile is the profile applied ("" = prose), and ProfileFrom what chose
	// it: "mode" (an active mode declared it), "app" (your per-app setting),
	// a trait name (the app's kind, e.g. "terminal"), or "" for the default.
	Profile     string
	ProfileFrom string
	// App is the app focused when the hold ended; Kept means focus had moved
	// by the time the text arrived, so it went to the clipboard instead.
	App  string
	Kept bool
	// Caret says whether the text around the caret could be read ("read") or
	// not ("none"), i.e. whether the seam was fitted.
	Caret string
	Speak bool
	// Confidence and WordScores are the speech stage's own scores for Heard
	// (word scores aligned to its words; positive = the decoder was better
	// than even odds on the word). Absent when the stage sent none. Kept as
	// evidence for later steps (learned corrections, refinement), not shown.
	Confidence *float64
	WordScores []float64
}

// recordDictation appends a dictation to the ring. Skipped only when nothing
// was heard: a transcript that a step reduced to nothing (the hallucination
// filter blanking "So.") is recorded, because "where did my word go?" has
// to be answerable.
func (h *Host) recordDictation(e dictationEntry) {
	e.Text = strings.TrimSpace(e.Text)
	if strings.TrimSpace(e.Heard) == "" && e.Text == "" {
		return
	}
	e.ID = speech.SimpleID()
	e.At = time.Now()
	h.dictationMu.Lock()
	h.dictationEntries = append(h.dictationEntries, e)
	if len(h.dictationEntries) > dictationHistoryCap {
		h.dictationEntries = h.dictationEntries[len(h.dictationEntries)-dictationHistoryCap:]
	}
	h.dictationMu.Unlock()
}

// snapshotDictations returns the ring newest-first.
func (h *Host) snapshotDictations() []dictationEntry {
	h.dictationMu.Lock()
	defer h.dictationMu.Unlock()
	out := make([]dictationEntry, len(h.dictationEntries))
	for i, e := range h.dictationEntries {
		out[len(h.dictationEntries)-1-i] = e
	}
	return out
}

// findDictation returns the entry with this id, or nil once it has rolled off.
func (h *Host) findDictation(id string) *dictationEntry {
	h.dictationMu.Lock()
	defer h.dictationMu.Unlock()
	for i := range h.dictationEntries {
		if h.dictationEntries[i].ID == id {
			e := h.dictationEntries[i]
			return &e
		}
	}
	return nil
}

// NOTE ON RECOVERY, and why there is no one-click Copy button.
//
// The obvious affordance is a button that puts the text back on the clipboard.
// `input.clipboard_write` requires the `clipboard` capability, and voice
// does not hold it — its privileges are input, dispatch, microphone,
// events.speech, events.*, system.indicator, notifications and
// stage.neural_engine. Adding one for a convenience button is a real expansion
// of what this plugin may do, it would surface as a new grant the user has to
// approve, and it is not a call to make quietly on the way past.
//
// Re-inserting with `input.type_text` (which voice DOES hold) was the other
// option and is worse: it types wherever focus happens to be now, which is
// exactly how the text went missing in the first place.
//
// So the panel shows the text, selectable, and the user copies it. That solves
// the actual problem — the words are not lost — without touching the
// permission surface. If a Copy button is wanted later it is a one-line
// manifest change plus a grant, and it should be decided on its own merits.

// renderDictationHistoryHTML renders the Dictation HUD tab.
func (h *Host) renderDictationHistoryHTML() string {
	entries := h.snapshotDictations()
	if len(entries) == 0 {
		return `<div style="padding:14px;opacity:.6;font-size:12px">` +
			`No dictations yet this session. Hold ⌥T and speak.</div>`
	}
	var b strings.Builder
	b.WriteString(`<div data-signals:fix-row__ifmissing="''" ` +
		`data-signals:fix-heard__ifmissing="''" data-signals:fix-correct__ifmissing="''" ` +
		`style="display:flex;flex-direction:column;gap:6px;padding:8px">`)
	b.WriteString(`<div style="font-size:10px;opacity:.5;padding:0 2px 2px">` +
		`Select text to copy it back, or fix a word it misheard. Say “type original” for the last one as it was heard.</div>`)
	for i, e := range entries {
		preview := clip(e.Text, 260)
		if preview == "" {
			preview = clip(e.Heard, 260)
		}
		// Words to pick for "Fix a word" come from what was HEARD: a
		// correction matches the transcript before any step touched it.
		heard := clip(e.Heard, 260)
		if heard == "" {
			heard = preview
		}
		id := jsEscapeID(e.ID)
		save := branchkit.MethodPost("dictation_correct", "{heard: $fixHeard, correct: $fixCorrect}")

		fmt.Fprintf(&b, `<div style="padding:8px 10px;background:var(--scrim-2);border-radius:6px">`)

		// Normal reading: plain, selectable, so a lost dictation can be copied
		// straight back out.
		fmt.Fprintf(&b,
			`<div data-show="$fixRow !== '%s'" style="font-size:12px;line-height:1.45;`+
				`word-wrap:break-word;user-select:text;-webkit-user-select:text;cursor:text">%s</div>`,
			id, html.EscapeString(preview))

		// Picking mode: the SAME text as clickable words.
		//
		// Typing out what it misheard was the worst part of this flow — you had
		// to read the transcript carefully and retype a word you did not choose
		// and might spell differently, to make a rule that only fires on an
		// exact match. The word is already on screen; clicking it is the whole
		// interaction. Only the correction still has to be typed.
		//
		// Two renderings rather than one clickable one, because plain text has
		// to stay SELECTABLE for the recovery case — word chips and a text
		// selection fight each other for the same drag.
		fmt.Fprintf(&b,
			`<div data-show="$fixRow === '%s'" style="display:none;font-size:12px;line-height:1.8;word-wrap:break-word">`, id)
		for _, w := range strings.Fields(heard) {
			// Trailing punctuation is part of the sentence, not of the word a
			// rule should match: "Whisperflow?" must yield "Whisperflow".
			word := strings.Trim(w, `.,!?;:"'()[]{}`)
			if word == "" {
				fmt.Fprintf(&b, `%s `, html.EscapeString(w))
				continue
			}
			// BOTH escapes, in this order, and the order is the bug this
			// nearly shipped with. `jsEscapeID` makes the value safe as a
			// JavaScript string literal; it does nothing about HTML. A word
			// containing a quote became `\"` — which HTML still reads as the
			// attribute's closing quote, breaking out of the attribute
			// entirely. Escaping the JS-escaped text for HTML closes it: the
			// browser decodes `&#34;` back to `"` only after the attribute
			// boundary is settled, so the expression sees what it should.
			fmt.Fprintf(&b,
				`<span role="button" tabindex="0" data-on:click="$fixHeard = '%s'" `+
					`style="cursor:pointer;padding:1px 3px;border-radius:3px;`+
					`background:var(--scrim-4)" `+
					`title="use this word">%s</span> `,
				html.EscapeString(jsEscapeID(word)), html.EscapeString(w))
		}
		b.WriteString(`</div>`)

		b.WriteString(dictationTrailHTML(e, i == 0))

		fmt.Fprintf(&b,
			`<div style="display:flex;gap:8px;align-items:center;margin-top:3px">`+
				`<span style="font-size:10px;opacity:.5">%s</span>`+
				`<button class="small" data-show="$fixRow !== '%s'" `+
				`data-on:click="$fixRow = '%s'; $fixHeard = ''; $fixCorrect = ''" `+
				`title="Teach it a word it got wrong here">Fix a word</button>`+
				`%s</div>`,
			html.EscapeString(relTime(e.At)), id, id, h.styleControlHTML(e))

		fmt.Fprintf(&b,
			`<div data-show="$fixRow === '%s'" style="display:none;gap:6px;margin-top:6px;align-items:center">`+
				`<span style="font-size:11px;opacity:.6">heard:</span> `+
				// VALUE form, not the `data-bind:name` key form. HTML lowercases
				// attribute names, so a camelCase bind KEY yields an all-lowercase signal, while
				// the click handler and `data-signals:fix-heard` both address
				// `$fixHeard` — two different signals, and the symptom is exactly
				// what it looked like: clicking a word did nothing visible. The
				// docs recommend this form for templated markup for this reason.
				`<input type="text" data-bind="fixHeard" placeholder="click a word above" `+
				`style="width:30%%;padding:4px 6px;border-radius:4px;border:1px solid var(--border,#333);`+
				`background:var(--bg-input);color:var(--_ink);font-size:12px"> `+
				`<span style="font-size:11px;opacity:.6">→</span> `+
				`<input type="text" data-bind="fixCorrect" placeholder="should be…" `+
				`style="width:30%%;padding:4px 6px;border-radius:4px;border:1px solid var(--border,#333);`+
				`background:var(--bg-input);color:var(--_ink);font-size:12px"> `+
				`<button class="small" data-on:click="%s; $fixRow = ''">Save</button> `+
				`<button class="small" data-on:click="$fixRow = ''">Cancel</button>`+
				`</div>`,
			id, html.EscapeString(save))

		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// handleRetypeLast types the newest Recent dictation again at the cursor
// ("type again"): the rescue for text that went to the
// wrong place, was kept on the clipboard because focus moved, or was typed
// into an app that dropped it. With form "raw" ("type original") it types
// what was heard instead, before corrections and the profile: the way back
// from a cleanup that made it worse. Either way the text is fitted to the
// caret it lands at NOW, not the one it was first typed at. History stays in
// memory for the session only, so after a restart there is nothing to retype.
func (h *Host) handleRetypeLast(req *branchkit.OnActionRequest) (any, error) {
	var p RetypeLastParams
	if err := req.UnmarshalParams(&p); err != nil {
		return nil, err
	}
	entries := h.snapshotDictations()
	if len(entries) == 0 {
		branchkit.Logf("voice", "retype last: no dictation this session")
		return h.handled(), nil
	}
	raw := p.Form != nil && *p.Form == "raw"
	text := retypeSource(entries[0], raw)
	if text == "" {
		branchkit.Logf("voice", "retype last: nothing to type (raw=%v)", raw)
		return h.handled(), nil
	}
	text, caret := h.fitDictationToCaret(text)
	branchkit.Logf("voice", "retype last: %d chars (raw=%v caret=%s) tr=%s", len(text), raw, caret, h.traceID())
	err := h.plugin.InputTypeText(branchkit.InputTypeTextRequest{Text: text})
	if err != nil {
		h.reportTypingFailure(err)
	}
	if err == nil && !raw {
		h.clearTypingFailure(entries[0].ID)
	}
	return h.handled(), nil
}

// clearTypingFailure marks a dictation whose text "type again" just typed
// as having landed. Without it the entry keeps saying "not typed" after the
// retype worked, and a second retry types it twice. Only that case changes
// the record: a failed retype says nothing about the original attempt (the
// overlay reports it), and "type original" types different text.
func (h *Host) clearTypingFailure(id string) {
	h.dictationMu.Lock()
	defer h.dictationMu.Unlock()
	for i := range h.dictationEntries {
		if h.dictationEntries[i].ID == id {
			h.dictationEntries[i].TypeError = ""
			return
		}
	}
}

// retypeSource is the text a retype starts from, before the caret fit: what
// was heard for "raw", else the shaped text (the seam is refitted, so the
// typed text's own leading space is not reused).
func retypeSource(e dictationEntry, raw bool) string {
	if raw {
		return strings.TrimSpace(e.Heard)
	}
	if e.Refined != "" {
		return e.Refined
	}
	if e.Shaped != "" {
		return e.Shaped
	}
	return e.Text
}

// clip shortens s to n bytes on a rune boundary, with an ellipsis.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// dictationTrailHTML says what the steps did to one dictation, when they did
// anything: what was heard if it differs from what reached the user, which
// profile shaped it and why, and where the text went if not to the caret.
// Nothing at all for the common case, a transcript typed as heard.
//
// newest says this is the most recent dictation: the only one "type again"
// retypes, so the only one whose failure note offers it.
func dictationTrailHTML(e dictationEntry, newest bool) string {
	var notes []string
	heard := strings.TrimSpace(e.Heard)
	switch {
	case heard != "" && e.Text == "":
		notes = append(notes, "removed by a correction rule")
	case heard != "" && heard != e.Text:
		notes = append(notes, "heard: “"+clip(heard, 200)+"”")
	}
	if e.Repaired != e.Shaped && e.Profile != "" {
		why := ""
		switch e.ProfileFrom {
		case "mode":
			why = " (from the active mode)"
		case "app":
			why = " (your setting for this app)"
		case "":
		default:
			why = " (" + e.ProfileFrom + " app)"
		}
		notes = append(notes, "formatted as "+e.Profile+why)
	}
	if e.RefineNote != "" && !strings.HasPrefix(e.RefineNote, "skipped:") {
		notes = append(notes, e.RefineNote)
	}
	if e.Kept {
		notes = append(notes, "focus moved, so it went to the clipboard")
	}
	if e.TypeError != "" {
		note := "not typed: " + clip(e.TypeError, 160)
		if newest {
			note += " (say “type again”)"
		}
		notes = append(notes, note)
	}
	if e.Speak {
		notes = append(notes, "speak session")
	}
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div style="font-size:10px;opacity:.6;margin-top:3px;word-wrap:break-word">`)
	for i, n := range notes {
		if i > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(html.EscapeString(n))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// styleControlHTML is the style control on one recent dictation: the moment a
// dictation came out wrong for an app is the moment to choose that app's
// style. Nothing for a speak session or a dictation with no known app.
func (h *Host) styleControlHTML(e dictationEntry) string {
	if e.App == "" || e.Speak {
		return ""
	}
	name := h.appDisplayName(e.App)
	// The app's exception, "" when it follows the default. The picker marks
	// that, not the resolved style: every button sets the exception, so
	// marking the resolved style would make clicking the marked button pin
	// the app (and stop it following later default changes). Default sets
	// it back to following the default.
	current := h.appStyleFor(e.App)
	// No model: no style takes effect and nothing can be chosen, so nothing
	// is marked, as in the Styles section.
	ready := styleModelReady()
	if !ready {
		current = "-"
	}
	defLabel := "Default"
	if d := h.defaultStyle(); d != "" {
		defLabel = "Default (" + styleLabel(d) + ")"
	}
	choices := append([]views.StyleChoice{{ID: "", Label: defLabel}}, views.RowStyleChoices...)
	var b strings.Builder
	fmt.Fprintf(&b, `<span style="margin-left:auto;font-size:10px;opacity:.6">Style in %s:</span>`+
		`<span role="group" aria-label="Style in %s" style="display:inline-flex;gap:4px">`,
		html.EscapeString(name), html.EscapeString(name))
	for _, c := range choices {
		if !ready {
			fmt.Fprintf(&b, `<button class="%s" aria-pressed="%t" aria-disabled="true" title="Download the style model first" style="opacity:.45;pointer-events:none">%s</button>`,
				views.StyleBtnClass(current, c.ID), current == c.ID, html.EscapeString(c.Label))
			continue
		}
		set := branchkit.MethodPost("app_styles/set", ui.Args("bundle_id", e.App, "style", c.ID))
		fmt.Fprintf(&b, `<button class="%s" aria-pressed="%t" data-on:click="%s">%s</button>`,
			views.StyleBtnClass(current, c.ID), current == c.ID, html.EscapeString(set), html.EscapeString(c.Label))
	}
	b.WriteString(`</span>`)
	return b.String()
}
