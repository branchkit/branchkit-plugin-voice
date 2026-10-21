package session

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/hud"
)

// OrchestrateRequest is the payload from the platform's speech ingest handler.
// isScopedTag checks if a tag belongs to any scoped namespace prefix.
func isScopedTag(tag string, scopedPrefixes []string) bool {
	for _, prefix := range scopedPrefixes {
		if strings.HasPrefix(tag, prefix) {
			return true
		}
	}
	return false
}

type OrchestrateRequest struct {
	Transcript string   `json:"transcript"`
	IsFinal    bool     `json:"is_final"`
	Mode       string   `json:"mode"`
	Words      []string `json:"words"`
	ActiveTags []string `json:"active_tags"`
}

// EventEntry is a structured event log entry returned to the platform.
type EventEntry struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// TagChanges describes tag mutations from a command match.
//
// Set/Clear are string tag names surfaced to the shell for observability
// (logs, audit). The platform applies the actual state writes
// (collection.put/delete_record) inside commands.resolve as _platform
// before returning the result — the plugin no longer dispatches them.
type TagChanges struct {
	Set   []string `json:"set,omitempty"`
	Clear []string `json:"clear,omitempty"`
}

// DiscoveryMutation describes discovery HUD state changes. Orchestrate returns
// it and State.applyDiscoveryMutationLocally applies it in-process through
// HUD.OpenDiscovery / HUD.CloseDiscovery — no RPC to the platform.
type DiscoveryMutation struct {
	Open *bool `json:"open,omitempty"`
	// What the person can say, as a semantic document's sections — the
	// platform renders them; this plugin no longer produces the markup.
	Sections []branchkit.OutputSection `json:"sections,omitempty"`
	// The same in words, for a renderer that has no list.
	Phrase           *string       `json:"phrase,omitempty"`
	Title            *string       `json:"title,omitempty"`
	Footer           *string       `json:"footer,omitempty"`
	CountdownSeconds *uint8        `json:"countdown_seconds,omitempty"`
	OnExpire         *hud.OnExpire `json:"on_expire,omitempty"`
	RequireTag       *string       `json:"require_tag,omitempty"`
	Words            []string      `json:"words,omitempty"`
	Countdown        *bool         `json:"countdown,omitempty"`
}

// OrchestrateResponse is what one orchestration produced.
//
// Everything here except Actions is applied by voice itself. Only Actions
// crosses back to the platform, on the `on_transcript` response — see
// ApplyOrchestrateResults.
type OrchestrateResponse struct {
	Result      string  `json:"result"`
	ResetEngine bool    `json:"reset_engine"`
	PasteText   *string `json:"paste_text,omitempty"`
	Mode        *string `json:"mode,omitempty"`
	// Actions the platform should execute, in order.
	//
	// Voice does not dispatch these. It returns them on the response to the
	// `on_transcript` call the platform made, and the platform executes them
	// as itself. That is the whole difference from what this used to be: the
	// actions no longer travel back as a second, separately authenticated
	// `dispatch` call, so there is nothing for the platform to authorise and
	// no credential for voice to carry. An attacker cannot answer a question
	// that was never put to it.
	Actions    []branchkit.Action `json:"actions,omitempty"`
	TagChanges *TagChanges        `json:"tag_changes,omitempty"`
	Discovery  *DiscoveryMutation `json:"discovery,omitempty"`
	// What just happened, in the person's own words — the phrase of a
	// command this orchestration handed the platform to act on. Pushed as
	// an `outcome` document after the discovery mutation is applied, so a
	// renderer that cannot show the list still conveys the result.
	Outcome     *string          `json:"outcome,omitempty"`
	Events      []EventEntry     `json:"events,omitempty"`
	Completions *resolveResponse `json:"-"`
	// Why the utterance stopped resolving before its last word, for the
	// History record: "" when every word was used (or dropped as silence by
	// a scoped mode), else "miss", "partial" or "tie". Independent of
	// Actions: "snap left blah" runs snap left and stops on "blah".
	Stop string `json:"-"`
	// The words left when it stopped.
	Unresolved []string `json:"-"`
}

// --- Platform API types for /v1/commands/resolve ---
//
// `commands.resolve` is the unified matcher-consumption primitive: one
// RPC, one round-trip, dispatch decision + partial-match feedback +
// tiebreaker telemetry in a single envelope. Replaces the prior
// `commands.match` + `commands.completions` pair.

// tiedCandidate mirrors the platform's TiedCandidate — the dispatchable
// subset of one command in a surfaced tie, plus a human-readable label.
// The platform's matcher surfaces it when a phrase ties.
type tiedCandidate struct {
	OwnerPlugin   string            `json:"owner_plugin"`
	Action        *branchkit.Action `json:"action"`
	ConsumedCount int               `json:"consumed_count"`
	SetsTags      []string          `json:"sets_tags"`
	ClearsTags    []string          `json:"clears_tags"`
	RequiresTags  []string          `json:"requires_tags"`
	Label         string            `json:"label"`
}

type resolveResponse struct {
	// --- Dispatch decision ---
	Matched bool              `json:"matched"`
	Action  *branchkit.Action `json:"action"`
	// Blocked names the command the words would have run, and why the
	// platform has it switched off, when nothing matched for that reason.
	Blocked *branchkit.BlockedCommand `json:"blocked,omitempty"`
	// TraceID identifies this resolve, for correlating its downstream
	// events. Observability only — it briefly doubled as the credential
	// that told the platform a dispatch was command routing, which is no
	// longer a thing that needs telling: routed actions ride home on the
	// `on_transcript` response and the platform executes them itself.
	TraceID string `json:"trace_id,omitempty"`
	// Phase B: args is a name-keyed object (was a positional array).
	// The platform pre-resolves action templates against named captures
	// before returning, so orchestrate doesn't read Args — but it must
	// still be the right JSON shape or the whole response fails to
	// unmarshal.
	Args          map[string]json.RawMessage `json:"args"`
	ConsumedCount int                        `json:"consumed_count"`
	SetsTags      []string                   `json:"sets_tags"`
	ClearsTags    []string                   `json:"clears_tags"`
	RequiresTags  []string                   `json:"requires_tags"`
	OwnerPlugin   *string                    `json:"owner_plugin,omitempty"`
	ScopedTags    []string                   `json:"scoped_tags,omitempty"`
	// ActivePluginGates is the platform's authoritative list of currently
	// active gates from `plugin.<X>.*` namespaces other than
	// `plugin.voice.*`. Used by session-end cleanup to decide whether to
	// leave Discovery open in continuous mode without shadowing platform
	// state in a local map.
	ActivePluginGates []string `json:"active_plugin_gates,omitempty"`
	// ScopedPrefixes is the platform-wide list of namespace prefixes that
	// mark a tag as "scoped." Used by matchWords to classify a matched
	// command's sets_tags entries as mode tags without the shell needing
	// to forward the prefix list through req.ScopedPrefixes (which the
	// pipeline transcript path never populates).
	ScopedPrefixes []string `json:"scoped_prefixes,omitempty"`
	// --- Partial-match feedback ---
	HasCompletions bool `json:"has_completions"`
	// DrawnByPlatform: the required tag is a select mode's, whose list the
	// platform draws as a palette card itself. Discovery leaves it alone.
	DrawnByPlatform bool           `json:"drawn_by_platform,omitempty"`
	NextWords       []string       `json:"next_words"`
	Title           string         `json:"title"`
	Items           []discoverItem `json:"items"`

	// BridgeActive is true when an active PendingPartial bridge survived
	// this resolve (advanced one token, or rejected the new utterance
	// without dropping). The discovery HUD must be left as-is: the
	// bridge's previously-rendered items remain the correct view of what
	// completes the in-progress capture. Without this flag, the empty
	// Items + HasCompletions=false from a bridge-survives resolve would
	// trigger the "no match, no partial" branch and close the HUD,
	// destroying the user's anchored prefix context.
	BridgeActive bool `json:"bridge_active,omitempty"`

	// TiedCandidates is the genuinely-tied set: 2+ equally-eligible
	// commands the matcher couldn't separate. Non-empty means Matched is
	// false and NO tag writes were applied — rather than pick arbitrarily,
	// the platform hands us the full set to disambiguate. We stand up a
	// transient choice grammar (see tie.go). Empty on the common path.
	TiedCandidates []tiedCandidate `json:"tied_candidates,omitempty"`
}

// OrchestrateInternal decides what one utterance means.
//
// There used to be a second entry point beside this one — a registered
// `speech_orchestrate` RPC handler, reachable by name and called by nothing
// anywhere in the tree. It was the plugin half of a request/response channel
// whose platform half was never built. That channel now exists, but as
// `on_transcript` with a generic contract (transcript in, actions out), so
// the dead handler is gone rather than revived: its OrchestrateRequest
// carried `is_session_ending`, which nothing on the live path has ever set,
// and its response carried paste_text, discovery and channel_visibility —
// speech vocabulary that has no business on a platform contract.
func (s *State) OrchestrateInternal(req *OrchestrateRequest) OrchestrateResponse {
	// If calibration ended mid-session, suppress normal command processing
	// for the remainder of this session. Without this, the final transcript
	// gets routed through the command pipeline after the game ends (e.g.
	// typing codeword letters into the active app).
	if s.GetCalibrationConsumed() {
		branchkit.Logf("voice", "orchestrate: calibration consumed session — suppressing")
		return OrchestrateResponse{Result: "handled"}
	}
	// No transcript dedup. It skipped a transcript identical to the previous
	// one, against a streaming engine that could emit the same tail fragment
	// twice (force-finalize, then audio_stop). The offline engine decodes each
	// segment exactly once, so all the dedup still did was swallow a genuine
	// retry after a miss — a command said again because the first attempt
	// landed before its context (app focus, a loaded list) had settled.
	return s.orchestrate(req)
}

// discoverItem mirrors the platform's DiscoverItem schema. Used both by
// the resolve response envelope and the discovery HUD renderer below.
type discoverItem struct {
	ID       string  `json:"id"`
	Tag      string  `json:"tag"`
	Title    string  `json:"title"`
	Subtitle *string `json:"subtitle,omitempty"`
	// The subtitle is itself a matchable spoken form (an alias equal to the
	// display name — a promoted selection record). Rendered as a mic glyph
	// on the row.
	Speakable bool `json:"speakable,omitempty"`
}

// callResolveDefault calls the platform's commands.resolve RPC method.
// One round-trip returns dispatch + partial-match data + telemetry.
func (s *State) callResolveDefault(words []string, requireTag *string, activeTags []string) *resolveResponse {
	if s.Platform == nil {
		return &resolveResponse{}
	}
	// ActiveTags passes through as given: nil means "use the live session
	// tags" (the matcher's snapshot), a non-nil slice — even an empty one —
	// is an explicit set. Callers pass nil or a populated slice.
	//
	// Never pass a tag set built locally in voice. A voice-side mirror only
	// ever held the SCOPED subset of the platform's gates, so non-scoped gates
	// (plugin.browser.hints, app.<bundle>, _platform.input.*) vanished from it
	// and the resolve treated the set as authoritative: the hint command went
	// ineligible, and a bridge gate-active check could never see hints
	// clear. Ask the platform (nil here, or
	// ActivePluginGates on the response).
	req := branchkit.CommandsResolveRequest{Words: words, RequireTag: requireTag, ActiveTags: activeTags, Source: s.resolveSource()}
	// Wire-format diagnostic so we can correlate voice's resolve calls
	// against the platform's MATCHER_DIAG log lines. Confirms whether
	// `active_tags` lands omitted (use snapshot) or with a value. The words
	// are counted, not printed — they are what the user said,
	// and the matcher's own lines show them under the platform's recording
	// rule.
	wire := req
	wire.Words = nil
	if payload, err := json.Marshal(wire); err == nil {
		branchkit.Logf("voice", "resolve wire: words=%d %s", len(req.Words), string(payload))
	} else {
		branchkit.Logf("voice", "resolve wire: words=%d marshal failed: %v", len(req.Words), err)
	}
	resp, err := s.resolve(req)
	if err != nil {
		branchkit.Logf("voice", "orchestrate: resolve call failed: %v", err)
		return &resolveResponse{}
	}
	return resp
}

// resolveSource tells the matcher which input mode a resolve is under, so it
// can pick its bridge-continuation policy (no inter-word timeout in
// command_hold; long fallback in continuous): the session's mode mapped onto
// the platform's generic `source` vocabulary. nil when neither applies.
func (s *State) resolveSource() *string {
	switch mode := s.GetCurrentMode(); {
	case mode == "command_hold":
		src := "command_hold"
		return &src
	case strings.HasPrefix(mode, "continuous"):
		src := "continuous"
		return &src
	}
	return nil
}

// phraseStep is one command an utterance named: where it starts, the words it
// consumed, and the resolve that matched them.
type phraseStep struct {
	Start    int
	Words    []string
	Resolved *resolveResponse
}

// phraseResponse is the session's view of commands.resolve_phrase: the
// commands the utterance named, in order; the positions of words dropped as
// silence inside a scoped mode; and, when resolution stopped short of the
// last word, where and why (a tie, the start of a longer command, a codeword
// in progress, or nothing at all).
type phraseResponse struct {
	Steps     []phraseStep
	Dropped   []int
	StoppedAt int
	Stopped   *resolveResponse
}

// callResolvePhraseDefault resolves a whole utterance on the platform,
// committing each step's tag writes as it matches.
func (s *State) callResolvePhraseDefault(words []string) *phraseResponse {
	if s.Platform == nil {
		return &phraseResponse{}
	}
	r, err := s.Platform.CommandsResolvePhrase(branchkit.CommandsResolvePhraseRequest{
		Words:  words,
		Source: s.resolveSource(),
	})
	if err != nil {
		branchkit.Logf("voice", "orchestrate: resolve_phrase call failed: %v", err)
		return &phraseResponse{}
	}
	out := &phraseResponse{Dropped: r.Dropped}
	for _, step := range r.Steps {
		out.Steps = append(out.Steps, phraseStep{
			Start:    step.Start,
			Words:    step.Words,
			Resolved: fromNestedResolveResult(step.Resolved),
		})
	}
	if r.Stopped != nil {
		out.StoppedAt = r.Stopped.At
		out.Stopped = fromNestedResolveResult(r.Stopped.Resolved)
	}
	return out
}

// fromNestedResolveResult maps one resolve inside a phrase result onto the
// session's view. It is the same wire object commands.resolve returns; the
// generator names the nested copy separately and renders its optional fields
// as values, so it goes through the top-level shape to share one mapping.
func fromNestedResolveResult(r branchkit.ResolveResult) *resolveResponse {
	bridge, trace, drawn := r.BridgeActive, r.TraceID, r.DrawnByPlatform
	return fromResolveResult(&branchkit.CommandsResolveResponse{
		Action:            r.Action,
		ActivePluginGates: r.ActivePluginGates,
		Args:              r.Args,
		Blocked:           r.Blocked,
		BridgeActive:      &bridge,
		ClearsTags:        r.ClearsTags,
		ConsumedCount:     r.ConsumedCount,
		DrawnByPlatform:   &drawn,
		HasCompletions:    r.HasCompletions,
		Items:             r.Items,
		Matched:           r.Matched,
		NextWords:         r.NextWords,
		OwnerPlugin:       r.OwnerPlugin,
		RequiresTags:      r.RequiresTags,
		ScopedPrefixes:    r.ScopedPrefixes,
		ScopedTags:        r.ScopedTags,
		SetsTags:          r.SetsTags,
		Telemetry:         r.Telemetry,
		TiedCandidates:    r.TiedCandidates,
		Title:             r.Title,
		TraceID:           &trace,
	})
}

// resolve issues commands.resolve through the generated wrapper and maps the
// typed response onto the session's view of it. The wrapper is positional;
// taking the request struct keeps call sites named.
func (s *State) resolve(req branchkit.CommandsResolveRequest) (*resolveResponse, error) {
	r, err := s.Platform.CommandsResolve(branchkit.CommandsResolveRequest{
		ActiveTags:  req.ActiveTags,
		Collections: req.Collections,
		PreferOwner: req.PreferOwner,
		Preview:     req.Preview,
		RequireTag:  req.RequireTag,
		SessionID:   req.SessionID,
		Source:      req.Source,
		Words:       req.Words,
	})
	if err != nil {
		return nil, err
	}
	return fromResolveResult(r), nil
}

// fromResolveResult copies the fields the session reads off the generated
// response. Every field of resolveResponse has a same-named, same-wire
// counterpart there; pointer-typed optionals collapse to their zero value.
func fromResolveResult(r *branchkit.CommandsResolveResponse) *resolveResponse {
	out := &resolveResponse{
		Matched:           r.Matched,
		Action:            r.Action,
		Args:              r.Args,
		ConsumedCount:     r.ConsumedCount,
		SetsTags:          r.SetsTags,
		ClearsTags:        r.ClearsTags,
		RequiresTags:      r.RequiresTags,
		OwnerPlugin:       r.OwnerPlugin,
		ScopedTags:        r.ScopedTags,
		ActivePluginGates: r.ActivePluginGates,
		ScopedPrefixes:    r.ScopedPrefixes,
		HasCompletions:    r.HasCompletions,
		NextWords:         r.NextWords,
		Title:             r.Title,
	}
	if r.TraceID != nil {
		out.TraceID = *r.TraceID
	}
	if r.BridgeActive != nil {
		out.BridgeActive = *r.BridgeActive
	}
	if r.DrawnByPlatform != nil {
		out.DrawnByPlatform = *r.DrawnByPlatform
	}
	if r.Items != nil {
		out.Items = make([]discoverItem, len(r.Items))
		for i, it := range r.Items {
			out.Items[i] = discoverItem{ID: it.ID, Tag: it.Tag, Title: it.Title, Subtitle: it.Subtitle, Speakable: it.Speakable}
		}
	}
	out.Blocked = r.Blocked
	if r.TiedCandidates != nil {
		out.TiedCandidates = make([]tiedCandidate, len(r.TiedCandidates))
		for i, c := range r.TiedCandidates {
			out.TiedCandidates[i] = tiedCandidate{
				OwnerPlugin:   c.OwnerPlugin,
				Action:        c.Action,
				ConsumedCount: c.ConsumedCount,
				SetsTags:      c.SetsTags,
				ClearsTags:    c.ClearsTags,
				RequiresTags:  c.RequiresTags,
				Label:         c.Label,
			}
		}
	}
	return out
}

// modeTitleFromTag renders a gate tag's last segment as a discovery HUD
// title: "plugin.placement.snap_mode" → "SNAP MODE". Identifier separators
// become spaces so the panel never leaks a raw variable-style name.
func modeTitleFromTag(tag string) string {
	parts := strings.Split(tag, ".")
	last := parts[len(parts)-1]
	if last == "" {
		return "COMMANDS"
	}
	last = strings.ReplaceAll(last, "_", " ")
	last = strings.ReplaceAll(last, "-", " ")
	return strings.ToUpper(last)
}

// buildEmptyDiscoveryMutation creates a discovery mutation with empty content_html.
func buildEmptyDiscoveryMutation(words []string, requireTag *string, countdown bool) *DiscoveryMutation {
	title := "COMMANDS"
	if len(words) > 0 {
		title = strings.ToUpper(strings.Join(words, " "))
	} else if requireTag != nil {
		title = modeTitleFromTag(*requireTag)
	}
	var countdownSecs *uint8
	var onExpire *hud.OnExpire
	if countdown {
		secs := uint8(8)
		countdownSecs = &secs
		onExpire = &hud.OnExpire{Close: true}
	}
	return &DiscoveryMutation{
		Open:             boolPtr(true),
		Title:            &title,
		Phrase:           strPtr("No matching commands"),
		CountdownSeconds: countdownSecs,
		OnExpire:         onExpire,
		Countdown:        boolPtr(countdown),
	}
}

// buildDiscoveryFromCompletions builds a discovery mutation from a resolve
// response's partial-match feedback. HUD items come from the same data that
// generates grammar words — one source of truth.
func (s *State) buildDiscoveryFromCompletions(comp *resolveResponse, requireTag *string, countdown bool) *DiscoveryMutation {
	if comp == nil || len(comp.Items) == 0 {
		return buildEmptyDiscoveryMutation(nil, requireTag, countdown)
	}
	sections := buildDiscoverySections(comp.Items)
	phrase := discoveryPhrase(comp.Items)
	title := comp.Title
	if title == "" && requireTag != nil {
		title = modeTitleFromTag(*requireTag)
	}
	var countdownSecs *uint8
	var onExpire *hud.OnExpire
	if countdown {
		secs := uint8(8)
		countdownSecs = &secs
		onExpire = &hud.OnExpire{Close: true}
	}
	return &DiscoveryMutation{
		Open:             boolPtr(true),
		Title:            &title,
		Sections:         sections,
		Phrase:           &phrase,
		Footer:           strPtr(""),
		CountdownSeconds: countdownSecs,
		OnExpire:         onExpire,
		Countdown:        boolPtr(countdown),
	}
}

// buildDiscoverySections turns discover items into the sections of a
// `choices` document — the platform renders them (and any other renderer
// reads them); voice states what can be said and nothing about how it looks.
//
// Items are grouped by their tag (the collection label, e.g. "Badge") into one
// section per tag, with the tag shown once as the section title rather than
// repeated on every row. Literal next words (the elided "Next" tag) group
// under "Commands" — an unheaded group below a headed one read as orphans of
// the section above it (field, 2026-08-02). First-seen order is preserved.
//
// Every item's phrase is its spoken form, and confirming it says exactly that
// (`action: {say}`), so a renderer that offers items in turn — a scanning
// menu — routes through the same matcher a voice would. A subtitle that is
// itself sayable (a promoted selection record) rides as an alternate phrase.
func buildDiscoverySections(items []discoverItem) []branchkit.OutputSection {
	if len(items) == 0 {
		return nil
	}
	var sections []branchkit.OutputSection
	byTitle := map[string]int{}
	for _, item := range items {
		title := "Commands"
		if item.Tag != "" && item.Tag != "Next" {
			title = item.Tag
		}
		idx, seen := byTitle[title]
		if !seen {
			idx = len(sections)
			byTitle[title] = idx
			sections = append(sections, branchkit.OutputSection{Title: title})
		}
		out := branchkit.OutputItem{
			ID:       item.ID,
			Title:    item.Title,
			Phrase:   item.Title,
			Subtitle: item.Subtitle,
			Action:   branchkit.SayAction(item.Title),
		}
		if item.Speakable && item.Subtitle != nil {
			out.AltPhrases = []string{*item.Subtitle}
		}
		sections[idx].Items = append(sections[idx].Items, out)
	}
	return sections
}

// discoveryPhrase is the `choices` document in words — what a renderer
// with no list conveys: "twelve commands".
func discoveryPhrase(items []discoverItem) string {
	switch len(items) {
	case 0:
		return "no commands"
	case 1:
		return "one command"
	default:
		return fmt.Sprintf("%d commands", len(items))
	}
}

// isRepeatAction reports whether a RESOLVED action is `voice.repeat`.
func isRepeatAction(action *branchkit.Action) bool {
	return resolvedActionType(action) == "voice.repeat"
}

// isPickTieAction reports whether a RESOLVED action is `voice.pick_tie`.
func isPickTieAction(action *branchkit.Action) bool {
	return resolvedActionType(action) == "voice.pick_tie"
}

// isEscapeAction reports whether a RESOLVED action is the platform's
// mode-global escape (`_escape`, the "over" command).
func isEscapeAction(action *branchkit.Action) bool {
	return resolvedActionType(action) == "_escape"
}

// resolvedActionType reads the action type off a RESOLVED action.
//
// This was a hand-rolled unmarshal into an anonymous struct until
// 2026-09-19, carrying a long comment about how commands.json authors
// actions flat (`{"type":"voice.repeat"}`) while a `commands.resolve`
// response carries serde's internally-tagged enum
// (`{"type":"plugin","action_type":"voice.repeat",…}`) — because reading
// `type` alone had silently returned "" for every action production ever
// handed it, and the tests passed by feeding the authoring shape.
// `commands.resolve` is typed now, so the two shapes are two Go types and
// the confusion is not expressible: the tag is `Type`, the dotted name is
// `ActionType`, and a test cannot feed the wrong one.
//
// Empty for a sequence or an absent action — neither can be one of the
// in-place expansions above.
func resolvedActionType(action *branchkit.Action) string {
	if action == nil || action.Type != branchkit.ActionKindPlugin || action.ActionType == nil {
		return ""
	}
	return *action.ActionType
}

// pickTieChoice reads the codeword the user spoke off a resolved
// `voice.pick_tie` action.
//
// `Params` stays raw: it is per-plugin by design (the platform cannot name
// the shape), and these are voice's own action's params.
func pickTieChoice(action *branchkit.Action) string {
	if action == nil || len(action.Params) == 0 {
		return ""
	}
	var typed struct {
		Choice string `json:"choice"`
	}
	if err := json.Unmarshal(action.Params, &typed); err != nil {
		return ""
	}
	return typed.Choice
}

// RepeatCount reads how many times a resolved `voice.repeat` action asks for
// the last command to run.
//
// `mode: "total"` counts the original utterance as the first of N, so
// "do that three times total" adds two more. Out-of-range, unparseable and
// zero-or-negative all collapse to the same answer: run it once, or not at
// all. The 99 ceiling is a guard against a misheard number turning into a
// thousand keystrokes.
func RepeatCount(action *branchkit.Action) int {
	if action == nil {
		return 0
	}
	var typed struct {
		Count string `json:"count"`
		Mode  string `json:"mode"`
	}
	if len(action.Params) > 0 {
		if err := json.Unmarshal(action.Params, &typed); err != nil {
			return 0
		}
	}
	count := 1
	if typed.Count != "" {
		n, err := strconv.Atoi(typed.Count)
		if err != nil || n <= 0 || n > 99 {
			return 0
		}
		count = n
	}
	if typed.Mode == "total" {
		count--
	}
	if count <= 0 {
		return 0
	}
	return count
}

// expandRepeat turns one resolved `voice.repeat` into N copies of the last
// matched action, plus the events that record it happened.
//
// This is the whole of repeat now. It used to be a round trip: the platform
// executed a `voice.repeat` action, which routed back to voice's on_action,
// which called `dispatch` once per copy — a cross-plugin call carrying no
// trace, since no resolve had produced those actions, surviving only on
// first-party trust.
//
// Nothing about the decision moved. `lastMatch` and the count are both
// already in hand right here, one round trip earlier than the handler that
// used to read them. Ordering for a one-breath "snap left again" is
// preserved because both matches happen in the same pass.
//
// `voice.repeat` remains a declared action type: it is user-authorable in
// the command editor, and a keybind can still target it.
func (s *State) expandRepeat(action *branchkit.Action) ([]branchkit.Action, []EventEntry) {
	last := s.GetLastMatch()
	if last == nil || last.Action == nil {
		branchkit.Logf("voice", "repeat: no previous command")
		return nil, nil
	}
	count := RepeatCount(action)
	if count == 0 {
		return nil, nil
	}

	branchkit.Logf("voice", "repeat: expanding to %d action(s)", count)
	label := ActionLabel(last.Action)
	actions := make([]branchkit.Action, 0, count)
	events := make([]EventEntry, 0, count)
	for i := 0; i < count; i++ {
		// Each entry is its own struct value. `Params` and `Actions` still
		// share backing arrays with the remembered action, which is fine
		// because nothing downstream mutates them — the platform marshals
		// and executes. This used to copy bytes because the whole action
		// was one buffer handed over N times.
		actions = append(actions, *last.Action)
		events = append(events, EventEntry{
			Type: "command_repeat",
			Data: map[string]interface{}{
				"repeat_index": i + 1,
				"repeat_total": count,
				"action":       label,
			},
		})
	}
	return actions, events
}

// ActionLabels renders a list of actions for a log or test message.
func ActionLabels(actions []branchkit.Action) string {
	labels := make([]string, 0, len(actions))
	for i := range actions {
		labels = append(labels, ActionLabel(&actions[i]))
	}
	return "[" + strings.Join(labels, " ") + "]"
}

// ActionLabel renders an action for a log or event field, where the old
// `string(rawJSON)` used to go.
func ActionLabel(action *branchkit.Action) string {
	if action == nil {
		return ""
	}
	if encoded, err := json.Marshal(action); err == nil {
		return string(encoded)
	}
	return resolvedActionType(action)
}

func tagsToSlice(tags map[string]bool) []string {
	result := make([]string, 0, len(tags))
	for t := range tags {
		result = append(result, t)
	}
	return result
}

// ---------------------------------------------------------------------------
// matchResult captures everything the matching phase produces.
// ---------------------------------------------------------------------------

type matchResult struct {
	actions      []branchkit.Action
	setTags      []string
	clearTags    []string
	events       []EventEntry
	matched      bool     // at least one command matched
	outcomes     []string // the words of each command that produced an action
	scopeDropped int      // words dropped by scope filtering (treat as silence)

	// Discovery hints — matchWords doesn't decide discovery state, but it
	// produces the data decideDiscovery needs.
	newScopedTag  *string // non-nil if the match sets a scoped tag
	pluginModeTag *string // non-nil if the match requires/sets a plugin mode tag
	escaped       bool    // the platform's escape ran: the mode is gone, whatever scoped_tags said

	// Partial match state
	partial           bool             // words are a prefix of a longer command
	partialWords      []string         // the partial prefix words
	partialRequireTag *string          // scoped tag for require_tag on partial
	completions       *resolveResponse // resolve envelope carrying partial-match data

	// Why resolving stopped early, and the words it left; see
	// OrchestrateResponse.Stop.
	stop       string
	unresolved []string

	// tie is non-nil when the matcher surfaced a genuine tie (2+ equally-
	// eligible commands). Disambiguation supersedes dispatch: orchestrate
	// stands up the transient choice grammar instead of acting. Carries the
	// exact words that produced the tie (re-resolved with prefer_owner on
	// pick) and the candidate set.
	tie *tieMatch

	// blocked is non-nil when the words name a command the platform has
	// switched off (an operation this machine refuses; a plugin's action that
	// is gone or changed). Nothing runs; the person is told why instead of
	// hearing nothing.
	blocked *branchkit.BlockedCommand
}

// tieMatch captures a surfaced tie: the words that produced it and the
// tied candidate set, both needed to disambiguate and re-resolve.
type tieMatch struct {
	words      []string
	candidates []tiedCandidate
}

// ---------------------------------------------------------------------------
// Phase 1: matchWords — pure matching + tag computation
// ---------------------------------------------------------------------------

// matchWords turns an utterance into what it asks for.
//
// Resolving the words into commands is the platform's (commands.resolve_phrase):
// it resolves, applies the winner's tag writes, and resolves what is left, so
// "snap left" comes back as the snap-mode entry and then `left`. That loop
// used to live here, and nothing in it was about speech; every other way of
// issuing commands needs it too. What stays here is what IS
// voice's: its own commands (repeat, a spoken tie pick), the escape's effect
// on discovery, and the events and outcome it narrates.
//
// One ordering change came with the move: a spoken tie pick used to
// re-resolve its winner (applying the winner's tag writes) BEFORE the rest of
// the utterance resolved; now the whole utterance resolves first. That only
// differs for a pick followed by words that depend on the winner's mode, in
// one breath, while the disambiguating gate (exclusive) is still open — the
// choice is spoken alone.
func (s *State) matchWords(words []string) matchResult {
	var result matchResult
	if len(words) == 0 {
		return result
	}
	phrase := s.callResolvePhraseFn(words)
	if phrase == nil {
		return result
	}

	// Platform-wide config, the same on every resolve; adopt it from any.
	var scopedPrefixes []string
	adopt := func(resp *resolveResponse) {
		if resp != nil && len(resp.ScopedPrefixes) > 0 {
			scopedPrefixes = resp.ScopedPrefixes
		}
	}
	for _, step := range phrase.Steps {
		adopt(step.Resolved)
	}
	adopt(phrase.Stopped)

	// Scope filtering: a word with no match and no partial completion inside
	// a scoped mode was dropped as silence.
	for _, at := range phrase.Dropped {
		branchkit.Logf("voice", "orchestrate: scope-dropped word %d of %d", at+1, len(words))
		result.scopeDropped++
	}

	for _, step := range phrase.Steps {
		resp := step.Resolved
		spoken := strings.Join(step.Words, " ")
		branchkit.Logf("voice", "orchestrate: matched: consumed=%d setTags=%v clearTags=%v owner=%v",
			resp.ConsumedCount, resp.SetsTags, resp.ClearsTags, resp.OwnerPlugin)

		result.matched = true
		result.events = append(result.events, EventEntry{
			Type: "match",
			Data: map[string]interface{}{
				"phrase":       spoken,
				"action_count": 1,
			},
		})

		if resp.Action != nil {
			// The command's words are the outcome — what the person will
			// hear happened. Mode entries carry no action and set tags
			// only; their outcome is the choices they open.
			if !isEscapeAction(resp.Action) {
				result.outcomes = append(result.outcomes, spoken)
			}
			switch {
			case isEscapeAction(resp.Action):
				// The platform's mode-global escape ("over"): its whole
				// effect — clearing the exclusive tag — happened inside
				// the platform's resolve. There is no plugin to dispatch it
				// to (`_escape` owns no prefix), nothing to repeat, and the
				// outcome is the mode closing, which discovery reports.
				branchkit.Logf("voice", "orchestrate: escape cleared=%v", resp.ClearsTags)
				result.escaped = true
			case isPickTieAction(resp.Action):
				// Same reason as repeat below: a `voice.pick_tie` action
				// would route back to voice, and voice would re-resolve
				// and dispatch the winner itself. The re-resolve happens
				// here instead, and its winner goes home on this response.
				//
				// The choice command's `clears_tags` dropped the
				// disambiguating gate during its own step, inside the
				// platform, so the prefer_owner re-resolve already sees the
				// original tied commands.
				if winner := s.ResolveTie(pickTieChoice(resp.Action)); winner != nil {
					result.actions = append(result.actions, *winner)
					s.SetLastMatch(&LastMatch{Action: winner})
				}
			case isRepeatAction(resp.Action):
				// Expanded here rather than returned for the platform to
				// execute: a `voice.repeat` action would route back to
				// voice, and voice would have to dispatch the remembered
				// action itself — the one remaining path where command
				// routing left the platform and came back as a
				// cross-plugin call. `lastMatch` is already in hand, set by
				// the steps before this one ("snap left again").
				//
				// Deliberately NOT recorded as the new lastMatch: "again
				// again" must repeat the original command, not itself.
				repeated, repeatEvents := s.expandRepeat(resp.Action)
				result.actions = append(result.actions, repeated...)
				result.events = append(result.events, repeatEvents...)
			default:
				result.actions = append(result.actions, *resp.Action)
				s.SetLastMatch(&LastMatch{Action: resp.Action})
			}
		}

		setTags := resp.SetsTags
		for i, t := range setTags {
			if isScopedTag(t, scopedPrefixes) {
				result.newScopedTag = &setTags[i]
				break
			}
		}
		result.setTags = append(result.setTags, setTags...)
		result.clearTags = append(result.clearTags, resp.ClearsTags...)

		// The resolve reports the scoped tags that were active when it
		// matched. After the escape those tags are gone — reading them as
		// "still in a mode" rebuilt the six choices right after "over".
		if !result.escaped {
			result.pluginModeTag = findPluginModeTag(resp.ScopedTags, setTags, scopedPrefixes)
		}
	}

	stop := phrase.Stopped
	if stop == nil {
		return result
	}
	remaining := words[min(phrase.StoppedAt, len(words)):]
	result.unresolved = remaining
	result.stop = "miss"

	// Genuine tie: the matcher surfaced 2+ equally-eligible commands and
	// refused to pick (nothing written). Disambiguation supersedes
	// everything else this pass: orchestrate stands up the transient choice
	// grammar. See tie.go.
	if len(stop.TiedCandidates) > 0 {
		tied := make([]string, len(remaining))
		copy(tied, remaining)
		result.tie = &tieMatch{words: tied, candidates: stop.TiedCandidates}
		result.stop = "tie"
		return result
	}

	// The words name a command the platform switched off: say why below.
	if stop.Blocked != nil {
		result.blocked = stop.Blocked
		result.stop = "blocked"
	}

	// No match — the captured prefix lives server-side in the matcher's
	// pending_partial bridge (seeded when the matcher returns a gated
	// Partial carrying `sets_on_partial`). The bridge survives until a
	// successful continuation, an explicit cancel ("over"), a whole command
	// of two or more words led by its own word, the session
	// boundary, or the mode tag clearing.
	if stop.HasCompletions {
		result.stop = "partial"
		result.partial = true
		result.partialWords = remaining
		result.completions = stop
		for _, t := range stop.ScopedTags {
			if isScopedTag(t, scopedPrefixes) {
				tag := t
				result.partialRequireTag = &tag
				break
			}
		}
	} else if stop.BridgeActive {
		// Bridge survived but the matcher correctly returned no fresh
		// completions (suppressed under suppress_fresh_progress). Still
		// thread the response through so the discovery decision logic
		// can read BridgeActive and leave the HUD as-is.
		result.stop = "partial"
		result.completions = stop
	}
	return result
}

// findPluginModeTag returns the first scoped non-voice plugin tag from
// setTags (newly entered mode) or scopedTags (existing mode). A tag must
// be both `plugin.<non-voice>.*` AND in a scoped namespace to qualify as
// a "mode" — ambient status flags like `plugin.browser.hints` are
// non-scoped and shouldn't trigger Discovery's mode-narrowing path.
func findPluginModeTag(scopedTags, setTags, scopedPrefixes []string) *string {
	isModeCandidate := func(t string) bool {
		return strings.HasPrefix(t, "plugin.") &&
			!strings.HasPrefix(t, "plugin.voice.") &&
			isScopedTag(t, scopedPrefixes)
	}
	for i, t := range setTags {
		if isModeCandidate(t) {
			return &setTags[i]
		}
	}
	for i, t := range scopedTags {
		if isModeCandidate(t) {
			return &scopedTags[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase 2: decideDiscovery — what to do with the discovery HUD
// ---------------------------------------------------------------------------

type discoveryDecision struct {
	mutation *DiscoveryMutation
	isOpen   bool // resulting discovery state
}

func (s *State) fetchModeCompletions(requireTag *string) *resolveResponse {
	if requireTag == nil || s.callResolveFn == nil {
		return nil
	}
	// Pass the mode tag as an active tag override — the tag hasn't been applied
	// yet when we build discovery (Phase 2 runs before Phase 4 sets tags).
	resp := s.callResolveFn(nil, requireTag, []string{*requireTag})
	if resp == nil || !resp.HasCompletions {
		return nil
	}
	return resp
}

func (s *State) decideDiscovery(result matchResult, isDiscoveryOpen bool, hasWords bool, mode string) discoveryDecision {
	// isVoiceEngaged: an active voice session that could receive another utterance.
	// True for both hold-to-talk (mid-hold) and continuous awake.
	isVoiceEngaged := mode == "continuous_awake" || mode == "command_hold"

	// needsCountdown: session has no natural terminator, so the discovery HUD
	// needs a countdown safety net. Hold-to-talk terminates on key release via
	// _platform.input.session_boundary, so its discovery shouldn't auto-expire
	// mid-session — that race produced HUD/hint desync on rapid two-word commands.
	needsCountdown := mode == "continuous_awake"

	if result.matched {
		// The escape closed the mode: discovery closes with it, and nothing
		// below may reopen it from the tags the resolve saw on the way in.
		if result.escaped {
			if isDiscoveryOpen {
				return discoveryDecision{
					mutation: &DiscoveryMutation{Open: boolPtr(false)},
					isOpen:   false,
				}
			}
			return discoveryDecision{isOpen: false}
		}
		// Command executed — decide based on what tags it set
		if result.newScopedTag != nil {
			comp := s.fetchModeCompletions(result.newScopedTag)
			// A select list is drawn by the platform as a palette card,
			// which stays after the hold; a Discovery copy would be a
			// second list of the same words.
			if comp != nil && comp.DrawnByPlatform {
				if isDiscoveryOpen {
					return discoveryDecision{
						mutation: &DiscoveryMutation{Open: boolPtr(false)},
						isOpen:   false,
					}
				}
				return discoveryDecision{isOpen: false}
			}
			d := s.buildDiscoveryFromCompletions(comp, result.newScopedTag, needsCountdown)
			if !isVoiceEngaged {
				d.Footer = strPtr("Say \"over\" to exit")
			}
			return discoveryDecision{mutation: d, isOpen: true}
		}
		if result.pluginModeTag != nil {
			branchkit.Logf("voice", "orchestrate: mode tag detected: %s", *result.pluginModeTag)
			comp := s.fetchModeCompletions(result.pluginModeTag)
			d := s.buildDiscoveryFromCompletions(comp, result.pluginModeTag, needsCountdown)
			if !isVoiceEngaged {
				d.Footer = strPtr("Say \"over\" to exit")
			}
			return discoveryDecision{mutation: d, isOpen: true}
		}
		// Plain match, no mode — close discovery if it was open
		if isDiscoveryOpen {
			return discoveryDecision{
				mutation: &DiscoveryMutation{Open: boolPtr(false)},
				isOpen:   false,
			}
		}
		return discoveryDecision{isOpen: false}
	}

	// Bridge survived the resolve (matcher's PendingPartial bridge advanced
	// or rejected the new utterance without dropping). The discovery HUD
	// must NOT change — the bridge's previously-rendered items remain the
	// correct view of what completes the in-progress capture. Returning a
	// no-mutation decision preserves the current HUD state. Skip both the
	// partial-build path (which would replace HUD content with the empty
	// items the matcher correctly returns under bridge survival) AND the
	// "no match, no partial" close branch below. See platform commit
	// history (suppress_fresh_progress) for the matching change.
	if result.completions != nil && result.completions.BridgeActive {
		return discoveryDecision{isOpen: isDiscoveryOpen}
	}

	// A partial inside a select list (a drill's category word): the
	// platform's card shows the list; Discovery would be a second copy.
	if result.partial && result.completions != nil && result.completions.DrawnByPlatform {
		if isDiscoveryOpen {
			return discoveryDecision{mutation: &DiscoveryMutation{Open: boolPtr(false)}, isOpen: false}
		}
		return discoveryDecision{isOpen: false}
	}

	if result.partial {
		// Partial match — build discovery from completions data
		var d *DiscoveryMutation
		if result.completions != nil && len(result.completions.Items) > 0 {
			phrase := discoveryPhrase(result.completions.Items)
			d = &DiscoveryMutation{
				Open:      boolPtr(true),
				Title:     &result.completions.Title,
				Sections:  buildDiscoverySections(result.completions.Items),
				Phrase:    &phrase,
				Footer:    strPtr(""),
				Countdown: boolPtr(needsCountdown),
			}
			if needsCountdown {
				secs := uint8(8)
				d.CountdownSeconds = &secs
				d.OnExpire = &hud.OnExpire{Close: true}
			}
		}
		if d == nil {
			d = buildEmptyDiscoveryMutation(result.partialWords, result.partialRequireTag, needsCountdown)
		}
		return discoveryDecision{mutation: d, isOpen: true}
	}

	// No match, no partial — only close if words were actually spoken.
	// Empty words (e.g., session-end with no speech) shouldn't close discovery;
	// session-end cleanup handles that with its persistent-tag check.
	if isDiscoveryOpen && hasWords {
		return discoveryDecision{
			mutation: &DiscoveryMutation{Open: boolPtr(false)},
			isOpen:   false,
		}
	}
	return discoveryDecision{isOpen: isDiscoveryOpen}
}

// ---------------------------------------------------------------------------
// orchestrate — the main entry point, now structured as sequential phases
// ---------------------------------------------------------------------------

func (s *State) orchestrate(req *OrchestrateRequest) OrchestrateResponse {
	// Use local discovery state — the platform's channel_visibility may be
	// stale due to async signal race. The voice plugin's discovery.isOpen is
	// the authoritative source.
	// The plugin's own discovery state is authoritative. It used to be OR-ed
	// with the platform's reported channel visibility, which was always nil
	// because nothing ever populated it — so the OR could only ever be a
	// no-op, and the `main` channel it reported was always false.
	isDiscoveryOpen := s.hud.IsDiscoveryOpen()

	// req_tags is what the platform reported active when it handed us this
	// transcript. It is now genuinely populated — it was empty on every
	// call for as long as this log line existed, which made "req_tags=[]"
	// unreadable as evidence and cost real time during at least one
	// investigation.
	//
	// Still advisory, not authoritative: the matcher consults full platform
	// state during commands.resolve, so a tag absent here can still gate
	// matching.
	branchkit.Logf("voice", "orchestrate: words=%d req_tags=%v", len(req.Words), req.ActiveTags)

	// --- Cancel mid-disambiguation (early return) ---
	// The disambiguating gate is plugin.voice.* and scoped+exclusive, so the
	// matcher would scope-drop "dismiss"/"over" while a tie is open. This
	// string check bypasses it so the user can always back out of a tie.
	//
	// It used to fire for ANY active scoped plugin tag too (snap mode, desk
	// mode, …), and that swallowed the matcher's mode-global `_escape` — the
	// one mechanism that clears the exclusive tag. It set a ClearPluginTags
	// flag that nothing had read since the discovery/tag decoupling of
	// 2026-05-18; with a held key the release's session boundary cleared the
	// tags anyway, which hid it. Without one — continuous mode, or a
	// synthetic transcript — "over" closed the overlay and left the mode and
	// its engine narrowing standing (found live 2026-09-19; the selection
	// surface case, 2026-09-06, was the same bug one namespace over). Now
	// "over" reaches the matcher whenever there is no tie, and the platform's
	// escape clears the mode inside `commands.resolve` before this returns.
	if s.IsTieActive() && (req.Transcript == "dismiss" || req.Transcript == "over") {
		s.clearTie()
		return OrchestrateResponse{
			Result:      "handled",
			ResetEngine: true,
			Discovery:   &DiscoveryMutation{Open: boolPtr(false)},
		}
	}

	// --- Phase 1: Match words against commands ---
	result := s.matchWords(req.Words)

	// Genuine tie — stand up the disambiguation choice grammar and surface
	// the chooser HUD instead of dispatching. The next utterance (a spoken
	// codeword or app name) resolves it via the voice.pick_tie handler.
	// Supersedes the alternatives/partial/discovery flow below.
	if result.tie != nil {
		disc := s.beginTie(result.tie, req.Mode)
		return OrchestrateResponse{
			Result:     "handled",
			Discovery:  disc,
			Stop:       result.stop,
			Unresolved: result.unresolved,
		}
	}

	// A command the platform switched off: tell the person why, rather than
	// leave them with silence. Nothing runs; the rest of the pass continues
	// as for any miss.
	if result.blocked != nil {
		s.showBlockedCard(result.blocked)
	}

	// --- Phase 2: Decide discovery state ---
	// Scope-dropped words are treated as silence — they shouldn't close discovery.
	effectiveWords := len(req.Words) > 0 && (result.matched || result.partial || result.scopeDropped == 0)
	disc := s.decideDiscovery(result, isDiscoveryOpen, effectiveWords, req.Mode)

	isDiscoveryOpen = disc.isOpen

	// --- Phase 4: Build response ---
	var tagChanges *TagChanges
	if len(result.setTags) > 0 || len(result.clearTags) > 0 {
		tagChanges = &TagChanges{
			Set:   result.setTags,
			Clear: result.clearTags,
		}
	}

	if len(req.Words) > 0 && len(result.actions) == 0 && len(result.events) == 0 {
		result.events = append(result.events, EventEntry{
			Type: "miss",
			Data: map[string]interface{}{
				"words":       req.Words,
				"active_tags": req.ActiveTags,
			},
		})
	}

	// Fallthrough detection: a multi-word utterance that produced multiple
	// separate single-word matches (instead of a single multi-word command
	// match) usually means a gated command the user intended didn't fire,
	// and the matcher fell through to per-word commands (e.g., spelling
	// alphabet typing letters). Surface this so it's not a silent footgun.
	if len(req.Words) > 1 && len(result.actions) > 1 {
		branchkit.Logf("voice", "orchestrate: FALLTHROUGH words=%d actions=%d req_tags=%v (multi-word input split into per-word matches; expected gated command may not be active)", len(req.Words), len(result.actions), req.ActiveTags)
	}

	// The outcome is the utterance that produced actions, in the person's
	// words — "snap left". One state per channel: when discovery ends
	// CLOSED the outcome is the state and is pushed on its own; when it
	// stays open (a sticky mode, where "snapped left" and the six commands
	// are both true), the outcome leads the choices' phrase — "snap left.
	// 6 commands" — so a renderer with no list still hears what happened,
	// and the list the person is looking at is not superseded.
	var outcome *string
	if len(result.outcomes) > 0 && len(result.actions) > 0 {
		joined := strings.Join(result.outcomes, " ")
		if disc.isOpen && disc.mutation != nil && disc.mutation.Phrase != nil {
			led := joined + ". " + *disc.mutation.Phrase
			disc.mutation.Phrase = &led
			// Remembered for the hold's end: the release clears the
			// choices, and a renderer that speaks at the boundary would
			// otherwise say nothing about what the hold did (found
			// 2026-09-19: "snap right" on a hold was never spoken).
			s.SetLastOutcome(joined)
		} else if !disc.isOpen {
			outcome = &joined
			s.SetLastOutcome("")
		}
	}

	return OrchestrateResponse{
		Result:      "handled",
		ResetEngine: result.matched,
		Actions:     result.actions,
		TagChanges:  tagChanges,
		Discovery:   disc.mutation,
		Events:      result.events,
		Completions: result.completions,
		Outcome:     outcome,
		Stop:        result.stop,
		Unresolved:  result.unresolved,
	}
}
