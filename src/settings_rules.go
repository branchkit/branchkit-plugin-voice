package main

// Rules domain: enhanced command browser with structured rule cards,
// variable cross-references, and category grouping. Corresponds to
// the "Commands" tab in settings (Phase 2 of the Voice Rule Editor).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/settings"
	"github.com/branchkit/branchkit-plugin-voice/views"
)

// templatePlaceholderRe matches `{name}`, `{name.field}`, `{name:transform("arg")}`
// — the named placeholder syntax used in action templates. Kept aligned with the
// platform's regex in actions.rs.
var templatePlaceholderRe = regexp.MustCompile(`\{[a-zA-Z_][a-zA-Z0-9_]*(\.\w+)?(:[a-z]+\("?[^)"]*"?\))?\}`)

// unwrapRoutedActionJSON converts the platform's routed action form —
// {"type":"plugin","action_type":"input.type","params":{...},"phase"?} —
// to the authored one-params dialect: {"type":"input.type","params":{...},
// "phase"?}. The authored form is the editor's ONLY internal shape;
// everything downstream of this call reads and writes it. Returns the input unchanged if it's not routed.
func unwrapRoutedActionJSON(raw json.RawMessage) json.RawMessage {
	var check struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &check) != nil || check.Type != "plugin" {
		return raw
	}
	var pa struct {
		ActionType string          `json:"action_type"`
		Params     json.RawMessage `json:"params"`
		Phase      string          `json:"phase"`
	}
	if json.Unmarshal(raw, &pa) != nil || pa.ActionType == "" {
		return raw
	}
	authored := map[string]any{"type": pa.ActionType}
	var paramsMap map[string]any
	if json.Unmarshal(pa.Params, &paramsMap) == nil && len(paramsMap) > 0 {
		authored["params"] = paramsMap
	}
	if pa.Phase != "" {
		authored["phase"] = pa.Phase
	}
	out, err := json.Marshal(authored)
	if err != nil {
		return raw
	}
	return out
}

// authoredFromAction renders a resolved Action in the AUTHORED dialect that
// `settings.DescribeEffect` reads — `{"type":"wm.snap","params":{…}}`.
//
// This is the typed twin of unwrapRoutedActionJSON, and both exist on
// purpose. The two dialects are genuinely different: the WIRE action is the
// generated `Action` (a routed envelope — kind, action_type, params), while
// the AUTHORED action is the flattened shape stored in a command record and
// parsed by DescribeEffect. Six of unwrapRoutedActionJSON's callers hold
// real record bytes and keep using it; this one holds a typed Action and
// should not marshal it just to parse it straight back.
func authoredFromAction(a *branchkit.Action) json.RawMessage {
	if a == nil {
		return nil
	}
	if a.Type != branchkit.ActionKindPlugin || a.ActionType == nil || *a.ActionType == "" {
		// Native and sequence actions are already in the authored dialect.
		raw, err := json.Marshal(a)
		if err != nil {
			return nil
		}
		return raw
	}
	authored := map[string]any{"type": *a.ActionType}
	var params map[string]any
	if len(a.Params) > 0 && json.Unmarshal(a.Params, &params) == nil && len(params) > 0 {
		authored["params"] = params
	}
	if a.Phase != nil && *a.Phase != "" {
		authored["phase"] = *a.Phase
	}
	out, err := json.Marshal(authored)
	if err != nil {
		return nil
	}
	return out
}

// parseActionForEdit extracts action type and value from raw action JSON.
// Returns ("", "") for complex actions that can't be edited.
func (h *Host) parseActionForEdit(raw json.RawMessage) (string, string) {
	// Check for sequence action
	var seqCheck struct {
		Type    string            `json:"type"`
		Actions []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(raw, &seqCheck); err == nil && seqCheck.Type == "sequence" && len(seqCheck.Actions) > 0 {
		return "sequence", ""
	}
	// Check if it's a bare JSON array (legacy sequence format)
	if len(raw) > 0 && raw[0] == '[' {
		return "", ""
	}
	// Check for template placeholders — any `{name}` reference in the raw JSON.
	// Match identifiers (letter/underscore start) optionally followed by `.field`
	// or `:transform("arg")`. Keep this aligned with the platform's template_re.
	rawStr := string(raw)
	if templatePlaceholderRe.MatchString(rawStr) {
		return "template", rawStr
	}
	var action struct {
		Type    string          `json:"type"`
		Command string          `json:"command"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &action); err != nil {
		return "", ""
	}
	switch action.Type {
	case "shell":
		return "shell", action.Command
	case "plugin":
		// Routed form: convert to the authored dialect and re-parse. This
		// recurses exactly once — the unwrapped type (e.g. "input.type")
		// cannot be "plugin" since that's a routing discriminator, not an
		// action_type value any plugin declares.
		authored := unwrapRoutedActionJSON(raw)
		if string(authored) == string(raw) {
			return "", "" // unwrap failed, avoid infinite recursion
		}
		return h.parseActionForEdit(authored)
	default:
		// Check plugin-declared action type schemas for generic extraction
		schemas := h.actionSchemas()
		if schema, ok := schemas[action.Type]; ok && len(schema.Fields) > 0 {
			var params map[string]any
			if len(action.Params) > 0 {
				_ = json.Unmarshal(action.Params, &params)
			}
			return extractPluginActionValue(action.Type, schema, params)
		}
		return "", ""
	}
}

// extractPluginActionValue extracts (actionType, actionVal) from an action's
// params object using schema field definitions. Handles compound values like
// shortcut modifiers.
func extractPluginActionValue(actionType string, schema ActionTypeSchema, params map[string]any) (string, string) {
	// Handle shortcut-like types with modifiers array
	if mods, ok := params["modifiers"]; ok {
		if modArr, ok := mods.([]any); ok && len(modArr) > 0 {
			// Find the non-modifiers field (e.g. "name")
			name := ""
			for _, fs := range schema.Fields {
				if fs.Key == "modifiers" {
					continue
				}
				if val, ok := params[fs.Key]; ok {
					name = fmt.Sprintf("%v", val)
					break
				}
			}
			parts := make([]string, 0, len(modArr)+1)
			for _, m := range modArr {
				parts = append(parts, fmt.Sprintf("%v", m))
			}
			if name != "" {
				parts = append(parts, name)
			}
			return actionType, strings.Join(parts, "+")
		}
	}
	// Single-field: use first field's value
	primaryKey := schema.Fields[0].Key
	if val, ok := params[primaryKey]; ok {
		return actionType, fmt.Sprintf("%v", val)
	}
	return actionType, ""
}

// parseSequenceSteps extracts step views from a sequence action for the edit form.
func (h *Host) parseSequenceSteps(raw json.RawMessage) []views.SeqStepView {
	var seq struct {
		Actions []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(raw, &seq); err != nil {
		return nil
	}
	var steps []views.SeqStepView
	for _, stepRaw := range seq.Actions {
		t, v := h.parseActionForEdit(stepRaw)
		if t == "" {
			t = "shell"
		}
		steps = append(steps, views.SeqStepView{Type: t, Val: v})
	}
	return steps
}

// --- Render ---

func (h *Host) renderRules(search string, commands []CommandRowData, commandFilter string, testPhrase string, editor *EditorState, listSchemas map[string]ListSchemaInfo) string {
	details := settings.LoadCommandDetails(branchkit.PluginDir())

	// Build rule cards grouped by category
	categoryMap := make(map[string][]views.RuleCardView)
	var categoryOrder []string

	for _, c := range commands {
		if search != "" && !strings.Contains(strings.ToLower(c.Canonical), search) && !strings.Contains(strings.ToLower(c.Pattern), search) {
			continue
		}

		mc := ""
		if c.IsUser {
			mc = "modified"
		}

		// Look up structural details from commands.json
		var description string
		var nl views.RuleDescription
		displayPhrase := c.Pattern

		var detail settings.CommandDetail
		var found bool
		if detail, found = details[c.Pattern]; !found {
			detail, found = details[c.Canonical]
		}
		if found {
			description = detail.Description
			dp, _ := settings.ParsePhraseForDeps(detail.Phrase)
			displayPhrase = dp
			nl = settings.DescribeRule(detail)
		}
		// Prefer the platform-provided description (covers every plugin's
		// commands, not just the voice plugin's locally-parsed commands.json).
		if c.Description != nil && *c.Description != "" {
			description = *c.Description
		}

		category := c.Category
		if category == "" {
			category = "Other"
		}

		hasNL := nl.Trigger != "" || nl.Effect != ""

		card := views.RuleCardView{
			Canonical:      c.Canonical,
			DisplayPhrase:  displayPhrase,
			PluginName:     c.PluginName,
			IsUser:         c.IsUser,
			ModifiedClass:  mc,
			Description:    description,
			NL:             nl,
			HasNL:          hasNL,
			IsEditable:     true,
			IsBeingEdited:  editor != nil && editor.Open && editor.EditCanonical != "" && editor.EditCanonical == c.Canonical,
			BlockedReasons: c.BlockedReasons,
		}

		if _, exists := categoryMap[category]; !exists {
			categoryOrder = append(categoryOrder, category)
		}
		categoryMap[category] = append(categoryMap[category], card)
	}

	// Sort categories: known categories first in a stable order, then the rest
	knownOrder := []string{"Editing", "Navigation", "Keyboard", "Dictation", "System", "Applications", "Alphabet"}
	filtering := commandFilter != "" && !strings.EqualFold(commandFilter, "all")
	var groups []views.RuleCategoryGroup
	seen := make(map[string]bool)

	appendCategory := func(name string, rules []views.RuleCardView) {
		if filtering {
			var filtered []views.RuleCardView
			for _, r := range rules {
				pn := r.PluginName
				if pn == "" {
					pn = "User"
				}
				if pn == commandFilter {
					filtered = append(filtered, r)
				}
			}
			if len(filtered) == 0 {
				return
			}
			rules = filtered
		}
		groups = append(groups, views.RuleCategoryGroup{Name: name, Count: len(rules), Rules: rules})
	}

	for _, name := range knownOrder {
		if rules, ok := categoryMap[name]; ok {
			appendCategory(name, rules)
			seen[name] = true
		}
	}
	for _, name := range categoryOrder {
		if seen[name] {
			continue
		}
		appendCategory(name, categoryMap[name])
	}

	// Build plugin tabs (same as before)
	pluginGroupMap := make(map[string]bool)
	var pluginGroupOrder []string
	for _, c := range commands {
		group := c.PluginName
		if group == "" {
			group = "User"
		}
		if !pluginGroupMap[group] {
			pluginGroupMap[group] = true
			pluginGroupOrder = append(pluginGroupOrder, group)
		}
	}

	isAll := commandFilter == "" || strings.EqualFold(commandFilter, "all")
	var tabs []views.PluginTab
	if len(pluginGroupOrder) > 1 {
		tabs = append(tabs, views.PluginTab{Name: "All", Active: isAll})
		for _, name := range pluginGroupOrder {
			tabs = append(tabs, views.PluginTab{Name: name, Active: commandFilter == name})
		}
	}

	// Collect known tags for autocomplete
	knownTagsSet := make(map[string]bool)
	for _, detail := range details {
		for _, t := range detail.RequiresTags {
			knownTagsSet[t] = true
		}
		for _, t := range detail.SetsTags {
			knownTagsSet[t] = true
		}
		for _, t := range detail.ClearsTags {
			knownTagsSet[t] = true
		}
	}
	var knownTags []string
	for t := range knownTagsSet {
		knownTags = append(knownTags, t)
	}
	sort.Strings(knownTags)

	// Build test result if there's an active test phrase
	testResult := h.buildTestResult(testPhrase)

	// Build editor view from server state
	var ev views.EditorView
	if editor != nil {
		ev = h.buildEditorView(editor)
	}

	// Resolve the inspector's selection against the (filtered) groups.
	// A selection hidden by the current plugin filter simply shows no
	// highlight; the inspector still shows it — the record outlives the view.
	selCanonical := h.getSelectedRule()
	var selected *views.RuleCardView
	if selCanonical != "" {
		for gi := range groups {
			for ri := range groups[gi].Rules {
				if groups[gi].Rules[ri].Canonical == selCanonical {
					selected = &groups[gi].Rules[ri]
					break
				}
			}
			if selected != nil {
				break
			}
		}
	}

	// The inspector IS the form: a selected command renders through the same
	// loader and view builder as the editor, locked. Nothing appears on
	// Customize that was not already shown.
	var locked views.EditorView
	hasLocked := false
	if selected != nil && (editor == nil || !editor.Open) {
		st := h.loadEditorState(selected.Canonical, details)
		locked = h.buildEditorView(&st)
		locked.Locked = true
		locked.EditCanonical = selected.Canonical
		locked.PluginName = selected.PluginName
		locked.IsUser = selected.IsUser
		if locked.Description == "" {
			locked.Description = selected.Description
		}
		hasLocked = true
	}

	return views.Render(views.Rules(views.RulesViewData{
		Listening:         listeningLabel(h.sess.ContinuousMode()),
		Groups:            groups,
		Tabs:              tabs,
		TestResult:        testResult,
		KnownTags:         knownTags,
		Editor:            ev,
		Selected:          selected,
		SelectedCanonical: selCanonical,
		Locked:            locked,
		HasLocked:         hasLocked,
	}))
}

func (h *Host) buildTestResult(phrase string) views.TestResultView {
	if phrase == "" {
		return views.TestResultView{Empty: true}
	}

	words := strings.Fields(strings.ToLower(phrase))

	// preview: the test box only ASKS. Without it a plugin-caller resolve is
	// a real match — typing "snap" here used to put the live session in snap
	// mode and write telemetry.
	preview := true
	resp, err := h.plugin.CommandsResolve(branchkit.CommandsResolveRequest{Preview: &preview, Words: words})
	if err != nil {
		branchkit.Logf("voice", "settings: Test resolve API error: %v", err)
		return views.TestResultView{Phrase: phrase}
	}

	view := views.TestResultView{
		Phrase:  phrase,
		Matched: resp.Matched,
	}

	if resp.Matched {
		view.ActionDisplay = h.describeMatchEffect(resp)

		if resp.ConsumedCount > 0 && resp.ConsumedCount <= len(words) {
			view.ConsumedWords = strings.Join(words[:resp.ConsumedCount], " ")
			// The rest of the sentence: a chained phrase ("snap left") matches
			// its entry word first, then the words after it match under the
			// mode the entry opened. Follow the chain the way the matcher
			// would, one preview resolve per hop, so the box reads the whole
			// sentence instead of stopping at the first word.
			view.Chain = h.followChain(resp, words)
			view.HasChain = len(view.Chain) > 0
		}

		// Stable display order — iterate sorted names for deterministic UI.
		argNames := make([]string, 0, len(resp.Args))
		for name := range resp.Args {
			argNames = append(argNames, name)
		}
		sort.Strings(argNames)
		for _, name := range argNames {
			view.Args = append(view.Args, views.TestArgView{Name: name, Value: string(resp.Args[name])})
		}
		view.HasArgs = len(view.Args) > 0

		view.SetsTags = resp.SetsTags
		view.ClearsTags = resp.ClearsTags
		view.RequiresTags = resp.RequiresTags
		view.HasSetsTags = len(resp.SetsTags) > 0
		view.HasClearsTags = len(resp.ClearsTags) > 0
		view.HasRequires = len(resp.RequiresTags) > 0

		if resp.OwnerPlugin != nil {
			view.OwnerPlugin = *resp.OwnerPlugin
		}

		// The why line: what context this match depended on.
		switch {
		case len(resp.RequiresTags) == 0:
			view.WhyLine = "no mode required — this matches anywhere"
		default:
			active := append(append([]string{}, resp.ScopedTags...), resp.ActivePluginGates...)
			met := 0
			for _, t := range resp.RequiresTags {
				for _, a := range active {
					if a == t {
						met++
						break
					}
				}
			}
			if met == len(resp.RequiresTags) {
				view.WhyLine = "its required mode is active right now — outside it, this phrase would not match"
			} else {
				view.WhyLine = "matched under the current context"
			}
		}
	} else if resp.HasCompletions {
		// Partial-match feedback is in the same envelope — no extra RPC needed.
		view.HasPartial = true
		if resp.Title != "" {
			view.NextList = resp.Title
			view.HasNextList = true
		}
	}

	if !resp.Matched {
		// The best why-not answer we can give without guessing: this exact
		// phrase IS a command, gated behind a mode — name the entry word and
		// the one-breath form the chain grammar already accepts.
		if snap := h.cachedCatalog(); snap != nil {
			if child, entry, ok := snap.whyGated(phrase); ok {
				view.GatedChild = child
				view.GatedEntry = entry
				view.HasGatedWhy = true
			}
		}
	}

	return view
}

// describeMatchEffect says what a match DOES in a phrase: the action's verb
// when it has one; for a mode entry (a `_noop` or no action at all) the mode it
// enters or leaves, so the box never shows an empty "Action:" line.
func (h *Host) describeMatchEffect(resp *resolveAPIResponse) string {
	effect := ""
	if resp.Action != nil {
		// A match arrives in the routed form ({"type":"plugin","action_type":
		// "wm.snap",...}); describe the authored one, which names the type.
		authored := authoredFromAction(resp.Action)
		effect = settings.DescribeEffect(authored)
		var a struct {
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(authored, &a)
		if a.Type == "_noop" {
			effect = ""
		} else if effect == a.Type {
			// DescribeEffect knows the native verbs; a plugin's action type
			// falls through as its id. The registry's label reads better —
			// "snap window left" rather than "wm.snap".
			if schema, ok := h.actionSchemas()[a.Type]; ok && schema.Label != "" {
				effect = strings.ToLower(schema.Label)
				keys := make([]string, 0, len(a.Params))
				for k := range a.Params {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if v, ok := a.Params[k].(string); ok && v != "" {
						effect += " " + v
					}
				}
			}
		}
	}
	if effect == "" {
		effect = strings.TrimPrefix(settings.DescribeSideEffects(resp.SetsTags, resp.ClearsTags), "then ")
	}
	if effect == "" && resp.Action != nil {
		effect = string(authoredFromAction(resp.Action))
	}
	return effect
}

// maxChainHops bounds the sentence walk: a phrase is a few words, and a
// resolve that consumes nothing would otherwise loop.
const maxChainHops = 4

// followChain resolves the words AFTER the first match under the tags that
// match would set, repeating while words remain and something matches. Every
// hop is a preview: nothing is written.
func (h *Host) followChain(first *resolveAPIResponse, words []string) []views.TestChainStep {
	var steps []views.TestChainStep
	active := map[string]bool{}
	for _, t := range first.ScopedTags {
		active[t] = true
	}
	for _, t := range first.ActivePluginGates {
		active[t] = true
	}
	applyTags := func(r *resolveAPIResponse) {
		for _, t := range r.SetsTags {
			active[t] = true
		}
		for _, t := range r.ClearsTags {
			delete(active, t)
		}
	}
	applyTags(first)
	rest := words[first.ConsumedCount:]
	for hop := 0; hop < maxChainHops && len(rest) > 0; hop++ {
		tags := make([]string, 0, len(active))
		for t := range active {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		preview := true
		r, err := h.plugin.CommandsResolve(branchkit.CommandsResolveRequest{ActiveTags: tags, Preview: &preview, Words: rest})
		if err != nil || !r.Matched || r.ConsumedCount == 0 || r.ConsumedCount > len(rest) {
			break
		}
		step := views.TestChainStep{
			Words:  strings.Join(rest[:r.ConsumedCount], " "),
			Effect: h.describeMatchEffect(r),
		}
		if r.OwnerPlugin != nil {
			step.OwnerPlugin = *r.OwnerPlugin
		}
		steps = append(steps, step)
		applyTags(r)
		rest = rest[r.ConsumedCount:]
	}
	return steps
}

// --- Live Test Panel ---

// resolveAPIResponse IS the platform's resolve result, not a copy of it.
// It was a hand-written partial mirror — its own comment said "mirrors the
// platform's ResolveResult" — carrying eleven of the response's fields and
// typing `action` as raw JSON where the generated response types it as
// *Action. No consumer read that field, so aliasing costs nothing and a
// platform rename now breaks the build instead of zero-filling a settings
// view. Aliased 2026-09-20.
type resolveAPIResponse = branchkit.CommandsResolveResponse

func (h *Host) handleTestPhrase(req *struct {
	TestPhrase string `json:"testPhrase"`
}) error {
	// Store the test phrase so renderRules can pick it up on SSE re-render
	h.edState.SetTestPhrase(strings.TrimSpace(req.TestPhrase))
	return nil
}

// listeningLabel is the Commands tab's line on always-listening.
func listeningLabel(continuous string) string {
	switch continuous {
	case "continuous_awake":
		return "Always listening: on, awake."
	case "continuous_sleeping":
		return "Always listening: on, asleep."
	default:
		return "Always listening: off. Commands are heard while you hold the command key."
	}
}
