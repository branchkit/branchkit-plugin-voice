package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	branchkit "github.com/branchkit/plugin-sdk-go"

	"github.com/branchkit/branchkit-plugin-voice/views"
)

// Dictation styles: Correct, Casual, Formal and Email, chosen per app,
// the shape Wispr Flow calls Styles. A style is a REWRITE by a local language
// model (Qwen3-4B through the refine_llm request stage), so the clean-up step's
// delete-only gate cannot hold it. What holds it instead is deterministic:
//
//   - a facts check: every number and every word the speaker capitalised
//     mid-sentence (a name, a product) must survive, except what was said
//     before a spoken correction ("the 14th, sorry, the 15th"), and the length
//     stays within bounds. A rewrite that fails is thrown away and the
//     dictation typed unstyled, with the reason in the record;
//   - Casual's casing is repaired after the model: names it lowercased take
//     back the casing they were said with, and the final full stop goes;
//   - Email is Formal inside a fixed "Hi," / "Thanks," frame, because the model
//     added greetings and sign-offs inconsistently;
//   - Correct promises the speaker's own words with the grammar fixed, so it
//     is held closer than the rest (acceptCorrection).
//
// Measured 2026-10-06 on 14 dictations: Qwen2.5-1.5B answered
// messages and dropped facts, Phi-3.5-mini invented content, Qwen3-4B with the
// examples below did neither.

// styleModel is the artifact styles load (provides.artifacts).
const styleModel = "qwen3-4b"

// styleTimeout bounds a rewrite. Past it the dictation is typed unstyled.
const styleTimeout = 15 * time.Second

const (
	// styleNone is an app's explicit "no style here", which overrides the
	// default; an app with no entry at all follows the default.
	styleNone    = "none"
	styleCorrect = "correct"
	styleCasual  = "casual"
	styleFormal  = "formal"
	styleEmail   = "email"
)

const styleCommon = "Remove fillers (um, uh, like, you know). When the speaker corrects themselves " +
	"(\"no\", \"wait\", \"sorry\", \"I mean\"), keep only the correction. Keep the meaning and every " +
	"name, number and fact. Never answer, reply to or comment on the text; add nothing. " +
	"Reply with the rewritten text only."

type dictStyle struct {
	ID          string
	Label       string
	Instruction string
	// Examples are (said, rewritten) pairs sent as earlier turns: they fix
	// the model's handling of a spoken correction, which instructions alone
	// did not (measured).
	Examples [][2]string
}

var dictStyles = []dictStyle{
	{
		// Measured 2026-10-06 on 24 dictations through refine_llm on the GPU
		// (median 0.7 s): ten with spoken grammar slips came back fixed ("I
		// seen the email ... replied him" -> "I saw the email ... replied to
		// him"), and the fourteen already-grammatical ones kept their tone
		// ("gonna" stayed), eleven of them word for word. The third example
		// is what holds the tone: without a correct sentence handed back
		// unchanged, "fix the grammar" reads as "make it formal".
		ID:    styleCorrect,
		Label: "Correct",
		Instruction: "Correct the dictated text: fix grammar, spelling and punctuation, and capitalise sentence starts. " +
			"Keep the speaker's own words, word order and tone; change a word only where grammar requires it, " +
			"and do not make it more formal. " + styleCommon,
		Examples: [][2]string{
			{"So um can you call Sam at 4, no wait, at 5 tomorrow.", "Can you call Sam at 5 tomorrow?"},
			{"me and him was gonna check the logs but the server were down", "He and I were gonna check the logs, but the server was down."},
			{"Thanks so much, the demo went great.", "Thanks so much, the demo went great."},
		},
	},
	{
		ID:    styleCasual,
		Label: "Casual",
		Instruction: "Rewrite the dictated message as a relaxed chat message: lowercase sentence starts, " +
			"light punctuation, no full stop at the end. " + styleCommon,
		Examples: [][2]string{
			{"So um can you call Sam at 4, no wait, at 5 tomorrow.", "can you call Sam at 5 tomorrow"},
			{"Thanks so much, the demo went great.", "thanks so much, the demo went great"},
		},
	},
	{
		ID:          styleFormal,
		Label:       "Formal",
		Instruction: "Rewrite the dictated text in a clear, professional tone: complete sentences, correct grammar and punctuation. " + styleCommon,
		Examples: [][2]string{
			{"So um can you call Sam at 4, no wait, at 5 tomorrow.", "Could you call Sam at 5 tomorrow?"},
			{"Thanks so much, the demo went great.", "Thank you very much; the demo went well."},
		},
	},
}

// styleByID returns the style with that id; Email rewrites as Formal.
func styleByID(id string) (dictStyle, bool) {
	want := id
	if id == styleEmail {
		want = styleFormal
	}
	for _, s := range dictStyles {
		if s.ID == want {
			return s, true
		}
	}
	return dictStyle{}, false
}

// styleLabel is a style's name as the person sees it.
func styleLabel(id string) string {
	if id == styleEmail {
		return "Email"
	}
	if s, ok := styleByID(id); ok {
		return s.Label
	}
	return id
}

// isKnownStyle guards the per-app store against a name no build understands.
func isKnownStyle(id string) bool {
	return id == styleCorrect || id == styleCasual || id == styleFormal || id == styleEmail
}

// styleRequestBody is what the stage is asked. "/no_think" turns off Qwen3's
// reasoning, which otherwise costs seconds of tokens nobody reads.
func styleRequestBody(text string, st dictStyle) json.RawMessage {
	msgs := []map[string]string{{"role": "system", "content": st.Instruction}}
	for _, ex := range st.Examples {
		msgs = append(msgs,
			map[string]string{"role": "user", "content": ex[0] + " /no_think"},
			map[string]string{"role": "assistant", "content": ex[1]})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": text + " /no_think"})
	body, _ := json.Marshal(map[string]any{
		"messages":   msgs,
		"max_tokens": 4*len(strings.Fields(text)) + 40,
	})
	return body
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// finishStyle turns the model's answer into the text to type: the reasoning
// block removed, Casual's casing repaired, Email framed. The facts check runs
// on the body, before the frame.
func finishStyle(said, answer, style string) (string, bool, string) {
	out := strings.TrimSpace(thinkBlock.ReplaceAllString(answer, ""))
	if style == styleCasual {
		out = strings.TrimSuffix(restoreSaidCase(said, out), ".")
	}
	if ok, why := acceptStyle(said, out); !ok {
		return said, false, why
	}
	if style == styleCorrect {
		if ok, why := acceptCorrection(said, out); !ok {
			return said, false, why
		}
	}
	if style == styleEmail {
		out = "Hi,\n\n" + out + "\n\nThanks,"
	}
	return out, true, ""
}

// abbreviations end with a full stop that does not end a sentence.
var abbreviations = map[string]bool{
	"dr.": true, "mr.": true, "mrs.": true, "ms.": true, "st.": true, "prof.": true,
	"p.m.": true, "a.m.": true, "e.g.": true, "i.e.": true, "etc.": true, "vs.": true, "no.": true,
}

func styleCore(tok string) string {
	return strings.TrimFunc(tok, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	})
}

// saidForms are the words whose casing is the speaker's, not the sentence's:
// capitalised mid-sentence ("Priya", "Friday", "Linux") or mixed case ("iOS",
// "BranchKit", "Q3"), by lower-case form.
func saidForms(said string) map[string]string {
	forms := map[string]string{}
	toks := strings.Fields(said)
	for i, t := range toks {
		c := strings.TrimRight(styleCore(t), "'’")
		if c == "" || !unicode.IsLetter([]rune(c)[0]) {
			continue
		}
		start := i == 0
		if i > 0 {
			prev := toks[i-1]
			last := prev[len(prev)-1:]
			start = strings.ContainsAny(last, ".!?") && !abbreviations[strings.ToLower(prev)]
		}
		runes := []rune(c)
		mixed := false
		for _, r := range runes[1:] {
			if unicode.IsUpper(r) {
				mixed = true
			}
		}
		if (unicode.IsUpper(runes[0]) && !start) || mixed {
			forms[strings.ToLower(c)] = c
		}
	}
	return forms
}

// restoreSaidCase gives the words of out back the casing they were said
// with, and the pronoun I its capital wherever it stands (a relaxed style
// still writes "I think", never "i think").
func restoreSaidCase(said, out string) string {
	forms := saidForms(said)
	toks := strings.Fields(out)
	for i, t := range toks {
		c := strings.TrimRight(styleCore(t), "'’")
		if f, ok := forms[strings.ToLower(c)]; ok && c != f {
			toks[i] = strings.Replace(t, c, f, 1)
			continue
		}
		if lc := strings.ToLower(c); lc == "i" || strings.HasPrefix(lc, "i'") || strings.HasPrefix(lc, "i’") {
			toks[i] = strings.Replace(t, c, "I"+c[1:], 1)
		}
	}
	return strings.Join(toks, " ")
}

var styleCueRE = regexp.MustCompile(`(?i)(?:^|[\s,.;])(?:no|wait|sorry|actually|rather)[,.;]|\bi mean\b|\bscratch that\b`)
var digitsRE = regexp.MustCompile(`\d`)

// acceptStyle is the facts check: every number and speaker-cased word said
// after the last spoken correction cue must appear in the rewrite, and the
// rewrite's length stays between 0.4x and 1.6x (+4 words) of what was said.
func acceptStyle(said, out string) (bool, string) {
	if out == "" {
		return false, "the model answered nothing"
	}
	nIn, nOut := len(strings.Fields(said)), len(strings.Fields(out))
	if float64(nOut) < 0.4*float64(nIn) || float64(nOut) > 1.6*float64(nIn)+4 {
		return false, fmt.Sprintf("the rewrite is %d words for %d said", nOut, nIn)
	}
	lastCue := -1
	for _, m := range styleCueRE.FindAllStringIndex(said, -1) {
		lastCue = m[0]
	}
	if ov := contentOverlap(said, out); ov < 0.6 {
		return false, fmt.Sprintf("the rewrite keeps too little of what was said (%.0f%%)", ov*100)
	}
	forms := saidForms(said)
	outLower := strings.ToLower(out)
	outDigits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, out)
	pos := 0
	for _, t := range strings.Fields(said) {
		at := strings.Index(said[pos:], t) + pos
		pos = at + len(t)
		if at < lastCue {
			continue // said before a correction: may be the abandoned half
		}
		c := strings.TrimRight(styleCore(t), "'’")
		switch {
		case digitsRE.MatchString(c):
			d := strings.Map(func(r rune) rune {
				if unicode.IsDigit(r) {
					return r
				}
				return -1
			}, c)
			if !strings.Contains(outDigits, d) {
				return false, fmt.Sprintf("the rewrite lost %q", c)
			}
		case forms[strings.ToLower(c)] != "" && !isIContraction(c):
			if !containsWord(outLower, strings.ToLower(c)) {
				return false, fmt.Sprintf("the rewrite lost %q", c)
			}
		}
	}
	return true, ""
}

// acceptCorrection holds Correct to its promise, the speaker's words with
// the grammar fixed, more tightly than acceptStyle holds a rewrite: at least
// 70% of the content words kept (every measured correction kept 75% or more;
// a style may drop to 60%), and no more than 1.25x the words said, plus two
// (measured corrections ran 0.6x to 1.08x; a style may reach 1.6x plus four).
// A correction that grew into a rewrite is refused and the words go in as
// dictated.
func acceptCorrection(said, out string) (bool, string) {
	if ov := contentOverlap(said, out); ov < 0.7 {
		return false, fmt.Sprintf("the correction changed too many of your words (kept %.0f%%)", ov*100)
	}
	nIn, nOut := len(strings.Fields(said)), len(strings.Fields(out))
	if float64(nOut) > 1.25*float64(nIn)+2 {
		return false, fmt.Sprintf("the correction is %d words for %d said", nOut, nIn)
	}
	return true, ""
}

// overlapStop are frequent words of four letters or more that carry no
// content of their own.
var overlapStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("that this with from have will would could should there their " +
		"they them then than what when where which while your about just like really gonna wanna " +
		"kind sort been were also into onto over very some sorry wait actually rather") {
		overlapStop[w] = true
	}
}

// contentOverlap is the share of said's content words (four letters or more,
// compared on their first five) that appear in out. An answer to a message
// shares little with it ("no problem, glad to help" keeps 17% of "thanks for
// the quick turnaround..."); every faithful rewrite measured kept 67% or more.
func contentOverlap(said, out string) float64 {
	words := func(s string) []string {
		var w []string
		for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !unicode.IsLetter(r) && r != '\''
		}) {
			if len([]rune(f)) >= 4 && !overlapStop[f] {
				if len(f) > 5 {
					f = f[:5]
				}
				w = append(w, f)
			}
		}
		return w
	}
	a := words(said)
	if len(a) == 0 {
		return 1
	}
	have := map[string]bool{}
	for _, w := range words(out) {
		have[w] = true
	}
	kept := 0
	for _, w := range a {
		if have[w] {
			kept++
		}
	}
	return float64(kept) / float64(len(a))
}

func isIContraction(w string) bool {
	return w == "I" || strings.HasPrefix(w, "I'") || strings.HasPrefix(w, "I’")
}

// containsWord reports whether w occurs in s as a whole word.
func containsWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isWordRune(rune(s[j-1]))
		after := j+len(w) == len(s) || !isWordRune(rune(s[j+len(w)]))
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// --- per-app styles ---

const appStylesCollection = "plugin.voice.app_styles"

// appStyles is the style choice: def, the default for every app ("" for
// none), and by, the per-app exceptions (bundle id -> style id, or styleNone
// for "no style in this app"). An app absent from by follows def.
type appStyles struct {
	mu  sync.RWMutex
	def string
	by  map[string]string
	err error
	// write serialises the two setters across read, Put and commit: each
	// writes the whole record, so overlapping writes (requests run
	// concurrently) would otherwise store one with the other's stale field
	// while memory kept both.
	write sync.Mutex
}

// appStyleFor is an app's exception, "" when it has none (it follows the
// default).
func (h *Host) appStyleFor(bundleID string) string {
	h.styles.mu.RLock()
	defer h.styles.mu.RUnlock()
	return h.styles.by[bundleID]
}

// effectiveStyle is the style a dictation into this app gets: its exception
// if it has one (styleNone meaning none), else the default. "" means none.
func (h *Host) effectiveStyle(bundleID string) string {
	h.styles.mu.RLock()
	defer h.styles.mu.RUnlock()
	return resolveStyle(h.styles.by[bundleID], h.styles.def)
}

// resolveStyle is an exception applied over a default.
func resolveStyle(exception, def string) string {
	switch exception {
	case "":
		return def
	case styleNone:
		return ""
	}
	return exception
}

// defaultStyle is the style every app gets unless it is an exception.
func (h *Host) defaultStyle() string {
	h.styles.mu.RLock()
	defer h.styles.mu.RUnlock()
	return h.styles.def
}

func (h *Host) loadAppStyles() {
	rec, err := h.plugin.Get(appStylesCollection, "singleton")
	h.styles.mu.Lock()
	h.styles.err = err
	h.styles.mu.Unlock()
	if err != nil || rec == nil {
		return
	}
	var wrapper struct {
		Default string            `json:"default"`
		Styles  map[string]string `json:"styles"`
	}
	if err := json.Unmarshal(rec.Payload, &wrapper); err != nil {
		branchkit.Logf("voice", "app styles parse error: %v", err)
		return
	}
	clean := map[string]string{}
	for id, s := range wrapper.Styles {
		if isKnownStyle(s) || s == styleNone {
			clean[id] = s
		}
	}
	def := ""
	if isKnownStyle(wrapper.Default) {
		def = wrapper.Default
	}
	h.styles.mu.Lock()
	h.styles.by = clean
	h.styles.def = def
	h.styles.mu.Unlock()
}

// setAppStyle records one app's exception: a style, or styleNone for none;
// "" removes the exception, so the app follows the default again.
func (h *Host) setAppStyle(bundleID, style string) error {
	if bundleID == "" {
		return errNoBundleID
	}
	if style != "" && style != styleNone && !isKnownStyle(style) {
		return fmt.Errorf("unknown style %q", style)
	}
	h.styles.write.Lock()
	defer h.styles.write.Unlock()
	h.styles.mu.Lock()
	if h.styles.err != nil {
		h.styles.mu.Unlock()
		h.loadAppStyles()
		return fmt.Errorf("your saved styles could not be read, so this change was not saved; try again")
	}
	next := map[string]string{}
	for k, v := range h.styles.by {
		next[k] = v
	}
	if style == "" {
		delete(next, bundleID)
	} else {
		next[bundleID] = style
	}
	def := h.styles.def
	h.styles.mu.Unlock()
	if err := h.putAppStyles(map[string]any{"styles": next, "default": def}); err != nil {
		return err
	}
	h.styles.mu.Lock()
	h.styles.by = next
	h.styles.mu.Unlock()
	return nil
}

// setDefaultStyle records the style every app gets unless it is an
// exception; "" is none.
func (h *Host) setDefaultStyle(style string) error {
	if style != "" && !isKnownStyle(style) {
		return fmt.Errorf("unknown style %q", style)
	}
	h.styles.write.Lock()
	defer h.styles.write.Unlock()
	h.styles.mu.Lock()
	if h.styles.err != nil {
		h.styles.mu.Unlock()
		h.loadAppStyles()
		return fmt.Errorf("your saved styles could not be read, so this change was not saved; try again")
	}
	by := map[string]string{}
	for k, v := range h.styles.by {
		by[k] = v
	}
	h.styles.mu.Unlock()
	if err := h.putAppStyles(map[string]any{"styles": by, "default": style}); err != nil {
		return err
	}
	h.styles.mu.Lock()
	h.styles.def = style
	h.styles.mu.Unlock()
	return nil
}

// styleModelReady reports whether the style model is on disk.
func styleModelReady() bool {
	_, ok := installedModelSet()[styleModel]
	return ok
}

// styleDictation rewrites shaped text in the app's style. It returns the text
// to type and a note for the record.
func (h *Host) styleDictation(text, style, profile string) (string, string) {
	if why := refineSkip(text, profile); why != "" {
		logLLMCall("style", "", outcomeSkipped, 0, false, refineAnswer{}, h.traceID())
		return text, "skipped: " + why
	}
	out, _, note := h.styleText(text, style)
	return out, note
}

// styleText asks the model for text in a style and checks the answer: the
// rewrite, whether it was accepted, and a note saying how or why not. On
// refusal the text comes back unchanged.
func (h *Host) styleText(text, style string) (string, bool, string) {
	st, ok := styleByID(style)
	if !ok {
		return text, false, "unknown style " + style
	}
	started := time.Now()
	timeout := int(styleTimeout / time.Millisecond)
	resp, err := h.plugin.StagesRequest(branchkit.StagesRequestRequest{
		Stage:     refineStage,
		Params:    llmStageParams(styleModel),
		Body:      styleRequestBody(text, st),
		TimeoutMs: &timeout,
	})
	if err != nil {
		branchkit.Logf("voice", "style: %v", err)
		logLLMCall("style", style, outcomeError, time.Since(started), false, refineAnswer{}, h.traceID())
		return text, false, "not styled: " + err.Error()
	}
	var answer refineAnswer
	if err := json.Unmarshal(resp.Body, &answer); err != nil {
		logLLMCall("style", style, outcomeError, time.Since(started), false, refineAnswer{}, h.traceID())
		return text, false, "not styled: unreadable answer"
	}
	out, ok, why := finishStyle(text, answer.Text, style)
	took := time.Since(started).Round(10 * time.Millisecond)
	if !ok {
		branchkit.Logf("voice", "style: rewrite refused: %s", why)
		logLLMCall("style", style, outcomeRefused, took, false, answer, h.traceID())
		return text, false, "style refused: " + why
	}
	outcome := outcomeAccepted
	if strings.TrimSpace(out) == strings.TrimSpace(text) {
		outcome = outcomeUnchanged
	}
	logLLMCall("style", style, outcome, took, false, answer, h.traceID())
	return out, true, fmt.Sprintf("written as %s (%s)", styleLabel(style), took)
}

// rewriteMaxWords bounds a selection rewrite: the stage's context holds the
// instruction, the examples, the text and the answer (4096 tokens), and a
// longer rewrite would also take many seconds with nothing on screen.
const rewriteMaxWords = 400

// handleRewriteSelection rewrites the selected text in a style ("make that
// formal"), with the same model and checks as a dictation in a styled app,
// and types the rewrite over the selection. When it cannot, the selection is
// left as it was and a notification says why.
func (h *Host) handleRewriteSelection(req *branchkit.OnActionRequest) (any, error) {
	var p RewriteSelectionParams
	if err := req.UnmarshalParams(&p); err != nil {
		return nil, err
	}
	notice, typed := h.rewriteSelection(p.Style)
	if notice != "" {
		go func() {
			if err := h.plugin.SystemNotify(branchkit.SystemNotifyRequest{
				Title: "Text not rewritten", Body: notice,
			}); err != nil {
				branchkit.Logf("voice", "rewrite: notice failed: %v", err)
			}
		}()
	}
	if typed != "" {
		if err := h.plugin.InputTypeText(branchkit.InputTypeTextRequest{Text: typed}); err != nil {
			h.reportTypingFailure(err)
		}
	}
	return h.handled(), nil
}

// rewriteSelection reads the selection and decides its rewrite.
func (h *Host) rewriteSelection(style string) (notice, typed string) {
	sel := ""
	ready := styleModelReady()
	if isKnownStyle(style) && ready {
		if resp, err := h.plugin.NativeSelectedText(); err == nil {
			sel = resp.Text
		}
	}
	return decideRewrite(style, ready, sel, func(core string) (string, bool, string) {
		out, ok, note := h.styleText(core, style)
		branchkit.Logf("voice", "rewrite selection: %d words, %s tr=%s", len(strings.Fields(core)), note, h.traceID())
		return out, ok, note
	})
}

// decideRewrite is a selection rewrite's outcome: the text to type over the
// selection, or a notice saying why there is none. rewrite asks the model.
func decideRewrite(style string, ready bool, sel string, rewrite func(string) (string, bool, string)) (notice, typed string) {
	if !isKnownStyle(style) {
		return "There is no style called " + style + ".", ""
	}
	if !ready {
		return "Download the style model first, on the Dictation tab in Settings.", ""
	}
	core := strings.TrimSpace(sel)
	if core == "" {
		return "Select the text to rewrite first.", ""
	}
	if n := len(strings.Fields(core)); n > rewriteMaxWords {
		return fmt.Sprintf("The selection is %d words; a rewrite takes up to %d.", n, rewriteMaxWords), ""
	}
	out, ok, note := rewrite(core)
	if !ok {
		note = strings.TrimPrefix(strings.TrimPrefix(note, "style refused: "), "not styled: ")
		return "Kept as it was: " + note + ".", ""
	}
	// Typing replaces the selection, so keep the whitespace it had around
	// the words (a selected line often carries its newline).
	lead := sel[:strings.Index(sel, core)]
	trail := sel[len(lead)+len(core):]
	return "", lead + out + trail
}

// errStyleModelMissing refuses a style while its model is not downloaded: it
// would take no effect, and the page offers no choice then either.
var errStyleModelMissing = errors.New("download the style model first, on the Dictation tab")

// HandleAppStyleDefault records the default style from the settings page; an
// empty style is none, and is always allowed.
func (h *Host) HandleAppStyleDefault(req *struct {
	Style string `json:"style"`
}) error {
	style := strings.TrimSpace(req.Style)
	if style != "" && !styleModelReady() {
		return errStyleModelMissing
	}
	if err := h.setDefaultStyle(style); err != nil {
		return fmt.Errorf("failed to save the default style: %w", err)
	}
	return nil
}

// HandleAppStyleSet records or clears one app's exception from the settings
// page: a style, "none" for no style in that app, or "" to follow the default
// again. "" and "none" are always allowed.
func (h *Host) HandleAppStyleSet(req *struct {
	BundleID string `json:"bundle_id"`
	Style    string `json:"style"`
}) error {
	if req.BundleID == "" {
		return errNoBundleID
	}
	style := strings.TrimSpace(req.Style)
	if style != "" && style != styleNone && !styleModelReady() {
		return errStyleModelMissing
	}
	if err := h.setAppStyle(req.BundleID, style); err != nil {
		return fmt.Errorf("failed to save the app's style: %w", err)
	}
	return nil
}

// appStyleRows are the apps the exceptions table lists: every app with its
// own style (or its own None), by name. An app joins through the Add row
// (addAppOptions) and leaves with its remove button, following the default
// again.
func (h *Host) appStyleRows() []views.AppStyleRowView {
	h.loadAppNames()
	h.styles.mu.RLock()
	var out []views.AppStyleRowView
	for id, st := range h.styles.by {
		out = append(out, views.AppStyleRowView{BundleID: id, Style: st})
	}
	h.styles.mu.RUnlock()
	for i := range out {
		out[i].Name = h.appDisplayName(out[i].BundleID)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// addAppOptions are the apps the Add row offers: those without a style, the
// recently used ones first (most recent first: the app you just came from is
// usually the one you are here to set), then every other installed app by
// name. Settings cannot offer "the app you are in", since while you read it
// the focused app is Settings itself.
func (h *Host) addAppOptions() []views.AppOptionView {
	h.loadAppNames()
	h.styles.mu.RLock()
	styled := make(map[string]bool, len(h.styles.by))
	for id := range h.styles.by {
		styled[id] = true
	}
	h.styles.mu.RUnlock()
	seen := map[string]bool{}
	var out []views.AppOptionView
	for _, id := range h.recentlyFocusedApps() {
		if id == "" || styled[id] || seen[id] || !h.appIsInstalled(id) {
			continue
		}
		seen[id] = true
		out = append(out, views.AppOptionView{BundleID: id, Name: h.appDisplayName(id), Recent: true})
	}
	for _, a := range h.installedApps() {
		if styled[a.BundleID] || seen[a.BundleID] {
			continue
		}
		out = append(out, a)
	}
	return out
}
