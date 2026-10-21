package main

// Server-rendered rule editor state and handlers.
// Follows Datastar's server-side state philosophy: editor state lives on the
// server, every structural change (action type, phrase, raw toggle) POSTs to
// the server which re-renders the full editor form via SSE patchElements.

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

// --- Editor state (lives on PluginState) ---

type EditorState struct {
	Open          bool
	EditCanonical string // "" for new rule
	IsOverride    bool   // true when editing a plugin command (save creates user override)
	Phrase        string
	Category      string
	Description   string
	ActionType    string // shell, type, key_by_name, shortcut_by_name, clipboard, sequence, template
	ActionVal     string // value for simple action types
	ActionJSON    string // raw JSON for template type
	RequiresTags  string
	SetsTags      string
	ClearsTags    string
	Discovery     string // "" | "prefix" | "exclusive" — bare-prefix Discovery HUD opt-in
	NewListSlot   int    // index of the slot for which an inline "＋ New list…" form is open; -1 = none
	ShowRaw       bool   // true = raw JSON textarea, false = structured editor
	Recording     bool   // true = capturing next keypress for action value
	// Sequence steps
	SeqSteps []EditorSeqStep
	// Structured template fields
	TmplFields []EditorTmplField
	// Phrase builder slots
	PhraseSlots   []PhraseSlot
	ShowRawPhrase bool
	// SaveError holds the platform's rejection reason from the last save
	// attempt (e.g. a terminal-command-rule collision). Shown in the editor;
	// cleared on a fresh open or a successful save.
	SaveError string
	// The form's folds and reveals. Server-held so a re-render (every slot
	// keystroke morphs the form) cannot close what the user opened.
	ShowDetails bool
	RevealApp   bool
	RevealMode  bool
	RevealAfter bool
	// ShowStart: a new command is showing its "Start from…" cards.
	ShowStart bool
}

type PhraseSlot struct {
	Type   string   // "literal" or "list"
	Value  string   // literal text or primary list name (e.g. "direction")
	Lists  []string // all list names for multi-list captures (e.g. ["keys", "alphabet"])
	Repeat string   // "" (exactly one), "+" (one or more), "*" (zero or more)
}

type EditorSeqStep struct {
	Type string
	Val  string
}

type EditorTmplField struct {
	Mode string // "literal" or "{name}" referencing a pattern capture
	Val  string
}

// placeholderStr extracts a string from the SDK's json.RawMessage placeholder field.
func placeholderStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// actionSchemas returns the action type schemas to use for editor rendering.
func (h *Host) actionSchemas() map[string]ActionTypeSchema {
	schemas := h.edState.actionSchemas
	if len(schemas) > 0 {
		return schemas
	}
	return map[string]ActionTypeSchema{}
}

// --- Phrase slot parsing/serialization ---

// altsList splits an alternatives value into its trimmed, non-empty options
// ("new line, return" → ["new line", "return"]).
func altsList(value string) []string {
	var out []string
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parsePhraseToSlots converts a phrase string into editor slots, recognizing
// `<captures>` (with optional */+), `(a, b)` parenthesized alternatives (whose
// options may contain spaces, e.g. "(new line, return)"), bare-comma
// alternatives ("go,move"), and runs of literal words. Mirrors the platform's
// parse_pattern_slots tokenization so the round-trip stays faithful.
func parsePhraseToSlots(phrase string) []PhraseSlot {
	var slots []PhraseSlot
	var lit []string
	flushLit := func() {
		if len(lit) > 0 {
			slots = append(slots, PhraseSlot{Type: "literal", Value: strings.Join(lit, " ")})
			lit = nil
		}
	}
	addAlts := func(inside string) {
		if a := altsList(inside); len(a) > 0 {
			flushLit()
			slots = append(slots, PhraseSlot{Type: "alternatives", Value: strings.Join(a, ", ")})
		}
	}

	runes := []rune(phrase)
	n := len(runes)
	for i := 0; i < n; {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(':
			j := i + 1
			for j < n && runes[j] != ')' {
				j++
			}
			addAlts(string(runes[i+1 : j]))
			if j < n {
				j++ // consume ')'
			}
			i = j
		case c == '<':
			j := i + 1
			for j < n && runes[j] != '>' {
				j++
			}
			inner := string(runes[i+1 : j])
			if j < n {
				j++ // consume '>'
			}
			repeat := ""
			if j < n && (runes[j] == '+' || runes[j] == '*') {
				repeat = string(runes[j])
				j++
			}
			flushLit()
			lists := strings.Split(inner, "|")
			for k := range lists {
				lists[k] = strings.TrimSpace(lists[k])
			}
			slots = append(slots, PhraseSlot{Type: "list", Value: lists[0], Lists: lists, Repeat: repeat})
			i = j
		default:
			j := i
			for j < n && runes[j] != ' ' && runes[j] != '\t' && runes[j] != '(' && runes[j] != '<' {
				j++
			}
			word := string(runes[i:j])
			if strings.Contains(word, ",") {
				addAlts(word)
			} else {
				lit = append(lit, word)
			}
			i = j
		}
	}
	flushLit()
	if len(slots) == 0 {
		slots = append(slots, PhraseSlot{Type: "literal", Value: ""})
	}
	return slots
}

// slotsToPhrase converts slots back to a phrase string.
func slotsToPhrase(slots []PhraseSlot) string {
	var parts []string
	for _, s := range slots {
		switch s.Type {
		case "literal":
			if v := strings.TrimSpace(s.Value); v != "" {
				parts = append(parts, v)
			}
		case "alternatives":
			alts := altsList(s.Value)
			switch len(alts) {
			case 0:
				// empty — skip
			case 1:
				// a single option is just a literal phrase
				parts = append(parts, alts[0])
			default:
				// Parens group the options so multi-word alternatives keep
				// their spaces ("(new line, return)"); the platform expands
				// the group into pattern alternatives.
				parts = append(parts, "("+strings.Join(alts, ", ")+")")
			}
		case "list":
			if s.Value != "" {
				name := s.Value
				if len(s.Lists) > 1 {
					name = strings.Join(s.Lists, "|")
				}
				parts = append(parts, "<"+name+">"+s.Repeat)
			}
		}
	}
	return strings.Join(parts, " ")
}

// --- Build editor view from state ---

var captureRe = regexp.MustCompile(`<([^>]+)>`)

// captureRef is one <...> blank in a phrase: its position, the binding name
// a template reads it by ({name}), and the joined list label(s).
type captureRef struct {
	Index int
	Name  string
	Label string
}

// defaultBindingFor mirrors the platform's rule for a blank the author did
// not name: the collection name, or a dotted name's last segment
// (`commands::default_binding_for`). Binding names cannot contain dots.
func defaultBindingFor(collection string) string {
	if i := strings.LastIndex(collection, "."); i >= 0 && i+1 < len(collection) {
		return collection[i+1:]
	}
	return collection
}

// parseCaptures lists the phrase's blanks. Templates bind a blank BY NAME —
// `<name:audio_outputs>` as {name}, `<keys|alphabet>` as {keys} — so the
// editor's "from a blank" choice carries the name; the positional {0} it
// wrote before never resolved, since the platform binds blanks by name.
func parseCaptures(phrase string) []captureRef {
	matches := captureRe.FindAllStringSubmatch(phrase, -1)
	var captures []captureRef
	for i, m := range matches {
		inner := strings.TrimRight(m[1], "*+")
		name := ""
		if j := strings.Index(inner, ":"); j > 0 {
			name = strings.TrimSpace(inner[:j])
			inner = inner[j+1:]
		}
		names := strings.Split(inner, "|")
		for j := range names {
			names[j] = strings.TrimSpace(names[j])
		}
		if name == "" && len(names) > 0 {
			name = defaultBindingFor(names[0])
		}
		captures = append(captures, captureRef{
			Index: i, Name: name, Label: strings.Join(names, "/"),
		})
	}
	return captures
}

// autoConnectMatch checks if a capture list name is a natural match for a field key.
// Used to pre-select "From spoken [X]" instead of "Fixed value" when the action type changes.
func autoConnectMatch(fieldKey string, captureLabel string) bool {
	for _, name := range strings.Split(captureLabel, "/") {
		name = strings.TrimSpace(name)
		if name == fieldKey {
			return true
		}
		// keys → name (key action's "name" field matches "keys" collection)
		if fieldKey == "name" && (name == "keys" || strings.HasSuffix(name, "_name")) {
			return true
		}
		// modifiers → modifiers
		if fieldKey == "modifiers" && (name == "modifiers" || strings.HasPrefix(name, "modifier")) {
			return true
		}
	}
	return false
}

// buildEditorView assembles the full editor view model from the editor
// state. Each section has a focused builder below; this function is the
// table of contents. Shared inputs (action schemas, filtered list
// names, parsed captures) are computed once and passed explicitly.
func (h *Host) buildEditorView(es *EditorState) views.EditorView {
	v := baseEditorView(es)
	allSchemas := h.actionSchemas()
	listNames := h.editorListNames()

	v.Categories = buildCategoryOptions(es.Category)
	v.ActionTypes = buildActionTypeOptions(allSchemas, es.ActionType)
	v.PhraseSlots = h.buildPhraseSlotViews(es, listNames)
	v.TagFields = h.buildTagFields(es)
	v.AppScope = currentAppScope(es.RequiresTags)
	v.AppOptions = buildAppOptions(h.edState.apps, v.AppScope)
	v.Preview = h.buildEditorPreview(es)
	v.Confusables = h.buildConfusables(es)
	v.ShowNewList = es.NewListSlot >= 0 && es.NewListSlot < len(es.PhraseSlots)

	captures := parseCaptures(es.Phrase)
	applySimpleField(&v, es, allSchemas, captures)
	h.applySequenceSteps(&v, es)
	applyTemplateFields(&v, es, allSchemas, captures)
	v.Picker = buildActionPicker(es, allSchemas)
	v.CanAddStep = v.ShowSimple
	v.Block = v.Preview.Show && v.Preview.Kind == "conflict" && es.EditCanonical == ""
	if v.ShowSequence {
		v.SeqTypeGroups = buildSeqTypeGroups(v.Picker)
	}
	if es.ActionType == "apps.launch" && v.ShowSimple && len(h.edState.apps) > 0 {
		v.ShowLaunchPicker = true
		v.LaunchOptions = buildLaunchOptions(h.edState.apps, es.ActionVal)
	}

	// The plugin whose command this is, for the Customize notice.
	if es.EditCanonical != "" {
		for _, c := range h.edState.lastCommands {
			if c.Canonical == es.EditCanonical && !c.IsUser {
				v.PluginName = c.PluginName
				break
			}
		}
	}
	v.ShowStart = es.ShowStart
	if es.ShowStart {
		v.StartCopyOf = h.getSelectedRule()
		for _, e := range v.Picker.Everyday {
			if e.Value != "sequence" {
				v.StartShapes = append(v.StartShapes, e)
			}
		}
	}
	// What the locked inspector prints where the editor has controls.
	v.ValueText = es.ActionVal
	for _, o := range v.LaunchOptions {
		if o.Selected && o.Value != "" {
			v.ValueText = o.Label
		}
	}
	v.AppLabel = strings.TrimPrefix(v.AppScope, "app.")
	if len(h.edState.apps) > 0 {
		for _, o := range v.AppOptions {
			if o.Selected && o.Value != "" {
				v.AppLabel = o.Label
			}
		}
	}

	v.ShowDetails = es.ShowDetails
	v.ShowApp = es.RevealApp || v.AppScope != ""
	v.ShowMode = es.RevealMode || (len(v.TagFields) > 0 && len(v.TagFields[0].Chips) > 0)
	v.ShowAfter = es.RevealAfter || (len(v.TagFields) > 2 && (len(v.TagFields[1].Chips) > 0 || len(v.TagFields[2].Chips) > 0))
	v.OnlyWhenEmpty = !v.ShowApp && !v.ShowMode && !v.ShowAfter

	// Discovery only applies to `literal word(s) + trailing capture` phrases, so
	// only surface the control there. This is a UI affordance gate, not the shape
	// contract — the platform's `compile_discovery_commands` is the authority and
	// rejects a bad shape loudly at save.
	if phraseEndsInCapture(es) {
		v.ShowDiscovery = true
		v.DiscoveryOptions = buildDiscoveryOptions(es.Discovery)
	}
	return v
}

// phraseEndsInCapture reports whether the phrase matches the discovery shape:
// non-capture lead word(s) followed by a SINGLE trailing capture. This mirrors
// the platform's compile_discovery_commands contract, so the control isn't
// offered on shapes (e.g. multi-capture) it would silently reject at save.
func phraseEndsInCapture(es *EditorState) bool {
	n := len(es.PhraseSlots)
	if n < 2 {
		return false
	}
	// Last slot must be a capture ("list"); no earlier slot may be one.
	if es.PhraseSlots[n-1].Type != "list" {
		return false
	}
	for _, s := range es.PhraseSlots[:n-1] {
		if s.Type == "list" {
			return false
		}
	}
	return true
}

// baseEditorView copies the editor state's scalar fields into a fresh
// view model.
func baseEditorView(es *EditorState) views.EditorView {
	return views.EditorView{
		Open:          es.Open,
		IsNew:         es.EditCanonical == "",
		IsOverride:    es.IsOverride,
		EditCanonical: es.EditCanonical,
		Phrase:        es.Phrase,
		Category:      es.Category,
		Description:   es.Description,
		ActionType:    es.ActionType,
		ActionVal:     es.ActionVal,
		ActionJSON:    es.ActionJSON,
		Discovery:     es.Discovery,
		ShowRaw:       es.ShowRaw,
		SaveError:     es.SaveError,
		ShowRawPhrase: es.ShowRawPhrase,
	}
}

// literalPreviewPhrase returns the leading run of literal words in the phrase
// (stopping at the first capture) — the input for the live conflict check. A
// capture command's literal lead-in is what determines whether its prefix
// collides with, or continues, an existing command.
func literalPreviewPhrase(slots []PhraseSlot) string {
	var words []string
	for _, s := range slots {
		switch s.Type {
		case "literal":
			words = append(words, strings.Fields(s.Value)...)
		case "alternatives":
			opt := strings.SplitN(s.Value, ",", 2)[0]
			words = append(words, strings.Fields(strings.TrimSpace(opt))...)
		default:
			return strings.Join(words, " ") // stop at the first capture slot
		}
	}
	return strings.Join(words, " ")
}

// classifyPreview turns a resolve result into the author-time preview signal.
// Pure (no I/O) so it's unit-testable. `wordCount` is the number of literal
// words previewed, used to distinguish an exact duplicate from a shorter command
// shadowing the start.
func classifyPreview(resp resolveAPIResponse, wordCount int) views.EditorPreview {
	if resp.Matched {
		owner := ""
		if resp.OwnerPlugin != nil && *resp.OwnerPlugin != "" {
			owner = " (" + *resp.OwnerPlugin + ")"
		}
		if resp.ConsumedCount > 0 && resp.ConsumedCount < wordCount {
			// A warning, not a refusal: the record is valid, it may just lose.
			return views.EditorPreview{Show: true, Kind: "shadow",
				Message: "A shorter existing command already matches the start of this phrase" + owner + " — this one may never fire."}
		}
		return views.EditorPreview{Show: true, Kind: "conflict",
			Message: "This exact phrase already belongs to an existing command" + owner + "."}
	}
	if resp.HasCompletions {
		msg := "This extends an existing command"
		if resp.Title != "" {
			msg += " (" + resp.Title + ")"
		}
		msg += "."
		return views.EditorPreview{Show: true, Kind: "info", Message: msg}
	}
	return views.EditorPreview{Show: true, Kind: "free", Message: "No conflict — this phrase is available."}
}

// allLiteralWords returns every literal (and alternatives-first) word in the
// phrase — the candidate words the confusability check phonemizes. Unlike
// literalPreviewPhrase it does not stop at the first capture.
func allLiteralWords(slots []PhraseSlot) []string {
	var words []string
	for _, s := range slots {
		switch s.Type {
		case "literal":
			words = append(words, strings.Fields(strings.ToLower(s.Value))...)
		case "alternatives":
			opt := strings.SplitN(s.Value, ",", 2)[0]
			words = append(words, strings.Fields(strings.ToLower(strings.TrimSpace(opt)))...)
		}
	}
	return words
}

// buildConfusables asks the platform which existing command words the phrase may
// be misheard as (and that compete in its context), for the author-time
// "may be misheard" warnings. Best-effort: nil on error.
func (h *Host) buildConfusables(es *EditorState) []views.ConfusableWarning {
	words := allLiteralWords(es.PhraseSlots)
	if len(words) == 0 {
		return nil
	}
	var requires []string
	for _, t := range strings.Split(es.RequiresTags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			requires = append(requires, t)
		}
	}
	findings, err := h.plugin.CommandsConfusability(branchkit.CommandsConfusabilityRequest{RequiresTags: requires, Words: words})
	if err != nil {
		return nil
	}
	var out []views.ConfusableWarning
	for _, f := range findings {
		msg := fmt.Sprintf("%q may be misheard as %q", f.Target, f.Confuser)
		switch {
		case f.Example != "":
			msg += " (" + f.Example + ")"
		case f.Owner != "":
			msg += " (" + f.Owner + ")"
		}
		out = append(out, views.ConfusableWarning{Message: msg})
	}
	return out
}

// buildEditorPreview runs a preview resolve of the phrase's literal lead-in and
// classifies it. Only for new rules — an edit/override matches itself, which
// isn't a conflict. Best-effort: empty (no panel) on error or empty phrase.
func (h *Host) buildEditorPreview(es *EditorState) views.EditorPreview {
	if es.EditCanonical != "" {
		return views.EditorPreview{}
	}
	phrase := literalPreviewPhrase(es.PhraseSlots)
	if phrase == "" {
		return views.EditorPreview{}
	}
	words := strings.Fields(strings.ToLower(phrase))
	// preview: this is a question, not a match. Without it, typing "snap"
	// as a phrase put the live session in snap mode.
	preview := true
	resp, err := h.plugin.CommandsResolve(branchkit.CommandsResolveRequest{Preview: &preview, Words: words})
	if err != nil {
		return views.EditorPreview{}
	}
	p := classifyPreview(*resp, len(words))
	if p.Kind == "conflict" {
		p.Existing = h.findExactCommand(phrase)
	}
	return p
}

// findExactCommand returns the canonical of the live command whose phrase is
// exactly `phrase` (case-insensitive), or "" — the target of "Customize it
// instead" when a new phrase collides.
func (h *Host) findExactCommand(phrase string) string {
	want := strings.ToLower(strings.TrimSpace(phrase))
	for _, c := range h.edState.lastCommands {
		if strings.ToLower(c.Canonical) == want || strings.ToLower(c.Pattern) == want {
			return c.Canonical
		}
	}
	return ""
}

// AppEntry is one installed app for the "Only in app" scope dropdown.
type AppEntry struct {
	BundleID string
	Name     string
}

// fetchApps reads the platform `apps` collection (bundle_id + spoken name) for
// the command editor's app-scope dropdown. Best-effort: nil on error, so the
// editor still renders (the dropdown just offers "Any app").
func (h *Host) fetchApps() []AppEntry {
	// `AppsRecord` is generated from this plugin's declared read of `apps` and
	// typed from system's schema, so a field rename over there breaks the
	// build here instead of decoding to "" and emptying this dropdown.
	//
	// ListAll, not List: `collection.list` substitutes a default limit of 1000
	// when none is passed and WARNS rather than erroring, so the old unlimited
	// `collection.get` would have quietly become a capped read. ListAll is the
	// SDK's exhaustive form — two round trips at most, since `total` comes back
	// with the first page — and it is what "the whole app list" means.
	records, err := h.plugin.ListAll("apps")
	if err != nil {
		return nil
	}
	out := make([]AppEntry, 0, len(records))
	for _, r := range records {
		var app AppsRecord
		if json.Unmarshal(r.Payload, &app) != nil {
			continue
		}
		if app.AppID == "" {
			continue
		}
		name := app.Spoken
		if name == "" {
			// The record id is on the ENVELOPE, not in the payload — the old
			// `collection.get` shape flattened the two together.
			name = r.ID
		}
		out = append(out, AppEntry{BundleID: app.AppID, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// currentAppScope returns the single `app.*` requires tag in a comma-joined
// requires string (the app the command is scoped to), or "" for "any app".
func currentAppScope(requiresTags string) string {
	for _, t := range strings.Split(requiresTags, ",") {
		t = strings.TrimSpace(t)
		if strings.HasPrefix(t, "app.") {
			return t
		}
	}
	return ""
}

// setAppScope replaces the single `app.*` scope tag in a comma-joined requires
// string, preserving every other (non-app) requires tag. An empty appTag clears
// the app scope ("Any app"). This is what lets the friendly dropdown own the
// app scope while the advanced tag picker owns the rest.
func setAppScope(requiresTags, appTag string) string {
	var kept []string
	for _, t := range strings.Split(requiresTags, ",") {
		t = strings.TrimSpace(t)
		if t == "" || strings.HasPrefix(t, "app.") {
			continue
		}
		kept = append(kept, t)
	}
	if appTag != "" {
		kept = append(kept, appTag)
	}
	return strings.Join(kept, ",")
}

// buildAppOptions builds the "Only in app" dropdown: "Any app" plus one option
// per installed app (value = the `app.<bundle_id>` requires tag). A current
// scope not in the installed list (uninstalled app, hand-authored) is kept as a
// selectable option so it round-trips instead of being silently cleared.
func buildAppOptions(apps []AppEntry, current string) []views.SelectOption {
	opts := []views.SelectOption{{Value: "", Label: "Any app", Selected: current == ""}}
	found := false
	for _, a := range apps {
		tag := "app." + a.BundleID
		sel := tag == current
		found = found || sel
		opts = append(opts, views.SelectOption{Value: tag, Label: a.Name, Selected: sel})
	}
	if current != "" && !found {
		opts = append(opts, views.SelectOption{
			Value: current, Label: strings.TrimPrefix(current, "app.") + " (not installed)", Selected: true,
		})
	}
	return opts
}

// buildDiscoveryOptions builds the discovery dropdown. "None" is the default;
// "prefix" keeps targets live in free context, "exclusive" flips a mode while
// the prefix holds (best for large/dynamic target sets), "select" points at
// items with alphabet codewords instead of putting their names in the
// grammar (best for churning sets — snippets, prompts, anything imported).
func buildDiscoveryOptions(current string) []views.SelectOption {
	opts := []struct{ Value, Label string }{
		{"", "None"},
		{"prefix", "Prefix — say the word alone to see choices"},
		{"exclusive", "Exclusive — say the word to enter a focused mode"},
		{"select", "Selection — items get letter codes; say the code to pick one"},
	}
	out := make([]views.SelectOption, 0, len(opts))
	for _, o := range opts {
		out = append(out, views.SelectOption{
			Value: o.Value, Label: o.Label, Selected: o.Value == current,
		})
	}
	return out
}

// buildCategoryOptions builds the category dropdown.
func buildCategoryOptions(current string) []views.SelectOption {
	categories := []string{"User", "Editing", "Navigation", "Keyboard", "Dictation", "System", "Applications"}
	out := make([]views.SelectOption, 0, len(categories))
	for _, cat := range categories {
		out = append(out, views.SelectOption{
			Value: cat, Label: cat, Selected: cat == current,
		})
	}
	return out
}

// buildActionTypeOptions builds the action-type dropdown from the
// platform-provided schemas, plus the structural "sequence" entry.
func buildActionTypeOptions(allSchemas map[string]ActionTypeSchema, current string) []views.SelectOption {
	var typeKeys []string
	for k := range allSchemas {
		typeKeys = append(typeKeys, k)
	}
	sort.Strings(typeKeys)
	var out []views.SelectOption
	for _, k := range typeKeys {
		s := allSchemas[k]
		label := s.Label
		if label == "" {
			label = k
		}
		out = append(out, views.SelectOption{
			Value: k, Label: label, Selected: k == current,
		})
	}
	// Sequence is structural, not in the schema registry
	out = append(out, views.SelectOption{
		Value: "sequence", Label: "Sequence (multiple actions)", Selected: current == "sequence",
	})
	// The select must never render blank: a current type with no schema (a
	// legacy record, or a plugin that is not running) still gets its own
	// option, so the bound value has something to land on.
	if current != "" && current != "sequence" {
		if _, ok := allSchemas[current]; !ok {
			out = append(out, views.SelectOption{Value: current, Label: current, Selected: true})
		}
	}
	return out
}

// defaultActionType picks the action type a NEW command opens with: the
// everyday shortcut shape when the registry declares it, else the first
// declared type. It used to be the literal "shell", which no schema declares,
// so the Action Type select opened with no selected option — blank.
func defaultActionType(allSchemas map[string]ActionTypeSchema) string {
	if _, ok := allSchemas["input.shortcut_by_name"]; ok {
		return "input.shortcut_by_name"
	}
	var keys []string
	for k := range allSchemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return keys[0]
	}
	return "shell"
}

// everydayShapes are the five things most commands do, offered first in the
// picker under plain names; the same action types sit in their plugin's group
// below under the registry label. Only the ones the registry declares appear.
var everydayShapes = []struct{ Type, Label, Example string }{
	{"input.shortcut_by_name", "Press a shortcut", "cmd+shift+a"},
	{"input.type", "Type text", "hello there"},
	{"input.key_by_name", "Press a key", "escape"},
	{"apps.launch", "Open an app", "Warp"},
	{"apps.open", "Open a link or file", "https://"},
}

// buildActionPicker assembles the "It does" picker: the everyday shapes,
// then every declared action type grouped under its plugin's name.
func buildActionPicker(es *EditorState, allSchemas map[string]ActionTypeSchema) views.ActionPickerView {
	var p views.ActionPickerView
	everydayLabel := map[string]string{}
	for _, sh := range everydayShapes {
		if _, ok := allSchemas[sh.Type]; !ok {
			continue
		}
		everydayLabel[sh.Type] = sh.Label
		p.Everyday = append(p.Everyday, views.ActionPickEntry{
			Value: sh.Type, Label: sh.Label, Example: sh.Example,
			Match: strings.ToLower(sh.Label + " " + sh.Example + " " + sh.Type), Selected: sh.Type == es.ActionType,
		})
	}
	groupMap := map[string][]views.ActionPickEntry{}
	for key, s := range allSchemas {
		name := actionGroupName(key)
		label := s.Label
		if label == "" {
			label = key
		}
		groupMap[name] = append(groupMap[name], views.ActionPickEntry{
			Value: key, Label: label, Match: strings.ToLower(label + " " + name + " " + key), Selected: key == es.ActionType,
		})
		p.Total++
	}
	var groupNames []string
	for n := range groupMap {
		groupNames = append(groupNames, n)
	}
	sort.Strings(groupNames)
	for _, n := range groupNames {
		entries := groupMap[n]
		sort.Slice(entries, func(i, j int) bool { return entries[i].Label < entries[j].Label })
		p.Groups = append(p.Groups, views.ActionPickGroup{Plugin: n, Entries: entries})
	}

	// The closed picker's face.
	switch {
	case es.ActionType == "sequence":
		p.Current = "Several steps, in order"
		p.CurrentSub = fmt.Sprintf("%d steps", len(es.SeqSteps))
	case everydayLabel[es.ActionType] != "":
		p.Current = everydayLabel[es.ActionType]
	default:
		if s, ok := allSchemas[es.ActionType]; ok && s.Label != "" {
			p.Current = s.Label
		} else {
			p.Current = es.ActionType
		}
		p.CurrentSub = actionGroupName(es.ActionType)
	}
	if p.Current == "" {
		p.Current = "Choose what it does…"
	}
	return p
}

// buildSeqTypeGroups turns the picker into optgroups for a step's compact
// select: Everyday first, then each prefix group.
func buildSeqTypeGroups(p views.ActionPickerView) []views.SelectGroup {
	var groups []views.SelectGroup
	if len(p.Everyday) > 0 {
		g := views.SelectGroup{Label: "Everyday"}
		for _, e := range p.Everyday {
			g.Options = append(g.Options, views.SelectOption{Value: e.Value, Label: e.Label})
		}
		groups = append(groups, g)
	}
	for _, pg := range p.Groups {
		g := views.SelectGroup{Label: pg.Plugin}
		for _, e := range pg.Entries {
			g.Options = append(g.Options, views.SelectOption{Value: e.Value, Label: e.Label})
		}
		groups = append(groups, g)
	}
	return groups
}

// actionGroupName names the picker group an action type belongs to. The
// registry does not say which plugin declared a type, and a command's routed
// action names only the type, so the prefix — the namespace the plugin chose
// for its actions — is the honest group name: "Input", "Placement", "System". A
// registry field naming the owner would replace this.
func actionGroupName(actionType string) string {
	prefix := actionType
	if i := strings.Index(actionType, "."); i > 0 {
		prefix = actionType[:i]
	}
	if prefix == "" {
		return ""
	}
	return strings.ToUpper(prefix[:1]) + prefix[1:]
}

// buildLaunchOptions offers the installed apps for a launch action (value =
// bundle id). A current value not in the list is kept so it round-trips.
func buildLaunchOptions(apps []AppEntry, current string) []views.SelectOption {
	opts := []views.SelectOption{{Value: "", Label: "Choose an app…", Selected: current == ""}}
	found := false
	for _, a := range apps {
		sel := a.BundleID == current
		found = found || sel
		opts = append(opts, views.SelectOption{Value: a.BundleID, Label: a.Name, Selected: sel})
	}
	if current != "" && !found {
		opts = append(opts, views.SelectOption{Value: current, Label: current, Selected: true})
	}
	return opts
}

// editorListNames returns the sorted names of lists worth surfacing in
// the editor (named-entity lists another plugin provides, or ones with
// a real label). Feeds both the phrase-help hints and the per-slot
// list dropdowns.
func (h *Host) editorListNames() []string {
	var listNames []string
	for name, ls := range h.edState.listSchemas {
		if ls.SourcePlugin != "" || (ls.Label != "" && ls.Label != name) {
			listNames = append(listNames, name)
		}
	}
	sort.Strings(listNames)
	return listNames
}

// newListSentinel is the list-dropdown value that opens the inline
// "＋ New list…" create form instead of selecting an existing list.
const newListSentinel = "__new_list__"

// buildPhraseSlotViews builds the per-slot editors: slot-type selector,
// list dropdown, and repeat options (repeat only offered on the last
// slot — a repeat makes a slot an open tail, which the grammar compiler
// requires to be last).
func (h *Host) buildPhraseSlotViews(es *EditorState, listNames []string) []views.PhraseSlotView {
	repeatOpts := []views.SelectOption{
		{Value: "", Label: "Exactly one"},
		{Value: "+", Label: "One or more"},
		{Value: "*", Label: "Zero or more"},
	}
	var out []views.PhraseSlotView
	for i, slot := range es.PhraseSlots {
		sv := views.PhraseSlotView{
			Index:          i,
			Value:          slot.Value,
			Repeat:         slot.Repeat,
			IsLiteral:      slot.Type == "literal",
			IsList:         slot.Type == "list",
			IsRaw:          slot.Type == "list" && (strings.Contains(slot.Value, ":") || len(slot.Lists) > 1),
			Display:        slot.Value,
			IsAlternatives: slot.Type == "alternatives",
		}
		if len(slot.Lists) > 1 {
			sv.Display = strings.Join(slot.Lists, "|")
		}
		// Slot-type selector — what can be said at this position. Framed by
		// intent: an exact word, one of a few options, or one from a saved list.
		sv.TypeOptions = []views.SelectOption{
			{Value: "literal", Label: "Word", Selected: !sv.IsList && !sv.IsAlternatives},
			{Value: "alternatives", Label: "Options", Selected: sv.IsAlternatives},
			{Value: "list", Label: "List", Selected: sv.IsList},
		}
		// List dropdown options
		sv.ListOptions = append(sv.ListOptions, views.SelectOption{Value: "", Label: "Choose a list..."})
		// If this is a multi-list capture, add a combined option first
		if len(slot.Lists) > 1 {
			var labels []string
			for _, ln := range slot.Lists {
				if ls, ok := h.edState.listSchemas[ln]; ok && ls.Label != "" {
					labels = append(labels, ls.Label)
				} else {
					labels = append(labels, ln)
				}
			}
			combinedVal := strings.Join(slot.Lists, "|")
			sv.ListOptions = append(sv.ListOptions, views.SelectOption{
				Value: combinedVal, Label: strings.Join(labels, " or "), Selected: true,
			})
		}
		for _, name := range listNames {
			ls := h.edState.listSchemas[name]
			label := ls.Label
			if label == "" {
				label = name
			}
			selected := name == slot.Value && len(slot.Lists) <= 1
			sv.ListOptions = append(sv.ListOptions, views.SelectOption{
				Value: name, Label: fmt.Sprintf("%s (%d)", label, ls.EntryCount), Selected: selected,
			})
		}
		// A just-created list (or one whose schema hasn't re-pushed yet) may not
		// be in listNames — keep the current value selectable so it shows and
		// round-trips instead of silently reverting to "Choose a list...".
		if slot.Value != "" && len(slot.Lists) <= 1 {
			found := false
			for _, o := range sv.ListOptions {
				if o.Value == slot.Value {
					found = true
					break
				}
			}
			if !found {
				sv.ListOptions = append(sv.ListOptions, views.SelectOption{
					Value: slot.Value, Label: slot.Value, Selected: true,
				})
			}
		}
		// Inline "make your own list" entry point (Part 2a).
		sv.ListOptions = append(sv.ListOptions, views.SelectOption{
			Value: newListSentinel, Label: "＋ New list…",
		})
		// Repeat options. A repeat (`+`/`*`) makes a slot an open tail, which the
		// grammar compiler requires to be last — so only the final slot may
		// repeat. Non-last slots are offered "exactly one" only.
		isLast := i == len(es.PhraseSlots)-1
		for _, ro := range repeatOpts {
			if !isLast && ro.Value != "" {
				continue
			}
			sv.RepeatOptions = append(sv.RepeatOptions, views.SelectOption{
				Value: ro.Value, Label: ro.Label, Selected: ro.Value == slot.Repeat,
			})
		}
		out = append(out, sv)
	}
	return out
}

// humanizeTag names a tag that declares no label the way a person would
// read it: the last segment, spaced and capitalised — plugin.voice.mode.spelling
// → "Spelling", plugin.placement.snap_mode → "Snap mode".
func humanizeTag(tag string) string {
	short := tag
	if i := strings.LastIndex(short, "."); i >= 0 {
		short = short[i+1:]
	}
	short = strings.ReplaceAll(short, "_", " ")
	if short == "" {
		return tag
	}
	return strings.ToUpper(short[:1]) + short[1:]
}

// clipNote shortens a tag's description to one picker line. Some tags carry a
// paragraph of design rationale; the picker wants the first sentence of it.
func clipNote(text string, max int) string {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, ". "); i > 0 && i < max {
		return text[:i+1]
	}
	if len(text) <= max {
		return text
	}
	cut := text[:max]
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// buildTagFields builds the three tag pickers (requires/sets/clears)
// and wires their Datastar signal names.
func (h *Host) buildTagFields(es *EditorState) []views.TagFieldView {
	fields := []views.TagFieldView{
		h.buildTagField("requires", "Other conditions", "advanced — also require another context/mode tag", es.RequiresTags),
		h.buildTagField("sets", "Sets tags", "activated when command matches", es.SetsTags),
		h.buildTagField("clears", "Clears tags", "deactivated when command matches", es.ClearsTags),
	}
	for i := range fields {
		fields[i].Signal = fmt.Sprintf("tagPicker%d", i)
		fields[i].SearchSignal = fmt.Sprintf("tagSearch%d", i)
	}
	return fields
}

// buildTagField builds one tag picker: selected chips plus available
// tags grouped by source plugin.
func (h *Host) buildTagField(fieldName, fieldLabel, helpText, currentTags string) views.TagFieldView {
	selected := map[string]bool{}
	var chips []views.TagChipView
	for _, t := range strings.Split(currentTags, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		selected[t] = true
		// The "Only in app" dropdown owns app.* scope; don't also show it as a
		// raw chip here (it would render as an ugly bare bundle id).
		if strings.HasPrefix(t, "app.") {
			continue
		}
		label := humanizeTag(t)
		if ts, ok := h.edState.tagSchemas[t]; ok && ts.Label != "" {
			label = ts.Label
		}
		chips = append(chips, views.TagChipView{Name: t, Label: label})
	}

	// Group available tags by source owner, excluding already-selected
	groupMap := map[string][]views.TagPickerEntry{}
	var groupOrder []string
	var tagNames []string
	for name := range h.edState.tagSchemas {
		tagNames = append(tagNames, name)
	}
	sort.Strings(tagNames)
	for _, name := range tagNames {
		ts := h.edState.tagSchemas[name]
		if ts.Label == "" || selected[name] {
			continue
		}
		// App-focus contexts (app.*) are the "In app" row's — it owns the one
		// app scope with a dropdown — and a command can't meaningfully set or
		// clear a platform-managed focus tag. So no picker offers them: the
		// mode picker lists modes, not four hundred installed apps.
		if strings.HasPrefix(name, "app.") {
			continue
		}
		owner := ts.SourcePlugin
		if owner == "" {
			owner = "Platform"
		} else if owner == "_user" {
			owner = "You"
		} else {
			// SourcePlugin is the owner's id; the group header reads as a name.
			owner = strings.ToUpper(owner[:1]) + owner[1:]
		}
		if _, exists := groupMap[owner]; !exists {
			groupOrder = append(groupOrder, owner)
		}
		groupMap[owner] = append(groupMap[owner], views.TagPickerEntry{
			Name: name, Label: ts.Label, Description: clipNote(ts.Description, 110),
			Match: strings.ToLower(ts.Label),
		})
	}
	sort.Strings(groupOrder)
	var groups []views.TagGroupView
	total := 0
	for _, p := range groupOrder {
		groups = append(groups, views.TagGroupView{Plugin: p, Tags: groupMap[p]})
		total += len(groupMap[p])
	}

	return views.TagFieldView{
		FieldName: fieldName, FieldLabel: fieldLabel, HelpText: helpText,
		Chips: chips, Groups: groups,
		// Show a filter box only when the list is long enough to need one.
		ShowSearch: total > 8,
	}
}

// applySimpleField sets the single-value action input: visibility,
// label/placeholder per action type, and the record affordance for
// key/shortcut types.
func applySimpleField(v *views.EditorView, es *EditorState, allSchemas map[string]ActionTypeSchema, captures []captureRef) {
	hasCaptures := len(captures) > 0
	v.ShowSimple = !hasCaptures && es.ActionType != "sequence"
	v.ShowRecord = isKeyByNameAction(es.ActionType)
	v.Recording = es.Recording
	switch es.ActionType {
	case "shell":
		v.SimpleLabel, v.SimplePh = "Shell command", "e.g. open -a Terminal"
	case "input.type":
		v.SimpleLabel, v.SimplePh = "Text to type", "hello there"
	case "input.key_by_name":
		v.SimpleLabel, v.SimplePh = "Key", "escape, tab, return…"
	case "input.shortcut_by_name":
		v.SimpleLabel, v.SimplePh = "Shortcut", "cmd+shift+a"
	case "input.clipboard":
		v.SimpleLabel, v.SimplePh = "Action (copy, paste, cut)", ""
	case "apps.launch":
		v.SimpleLabel, v.SimplePh = "App", "Safari"
	case "apps.open":
		v.SimpleLabel, v.SimplePh = "Link or file", "https://example.com"
	default:
		// Plugin action types: derive label from schema
		if schema, ok := allSchemas[es.ActionType]; ok && len(schema.Fields) > 0 {
			v.SimpleLabel = schema.Fields[0].Label
			if ph := placeholderStr(schema.Fields[0].Placeholder); ph != "" {
				v.SimplePh = ph
			}
		}
	}
}

// applySequenceSteps builds the sequence-step editors when the action
// type is the structural "sequence".
func (h *Host) applySequenceSteps(v *views.EditorView, es *EditorState) {
	v.ShowSequence = es.ActionType == "sequence"
	if !v.ShowSequence {
		return
	}
	// A step can be any shape the registry declares with a value field: the
	// platform builds each step the way it builds a single action. The
	// everyday names lead, the registry label follows.
	allSchemas := h.actionSchemas()
	everyday := map[string]string{}
	for _, sh := range everydayShapes {
		everyday[sh.Type] = sh.Label
	}
	for i, step := range es.SeqSteps {
		typeLabel := step.Type
		if l, ok := everyday[step.Type]; ok {
			typeLabel = l
		} else if s, ok := allSchemas[step.Type]; ok && s.Label != "" {
			typeLabel = s.Label
		}
		placeholder := "value"
		if s, ok := allSchemas[step.Type]; ok && len(s.Fields) > 0 {
			if ph := placeholderStr(s.Fields[0].Placeholder); ph != "" {
				placeholder = ph
			}
		}
		for _, sh := range everydayShapes {
			if sh.Type == step.Type {
				placeholder = sh.Example
			}
		}
		v.SeqSteps = append(v.SeqSteps, views.SeqStepEditView{
			Index: i, Val: step.Val, TypeLabel: typeLabel, Placeholder: placeholder,
			TypeOptions: []views.SelectOption{{Value: step.Type, Label: typeLabel, Selected: true}},
			IsFirst:     i == 0, IsLast: i == len(es.SeqSteps)-1,
		})
	}
	v.SeqCanAdd = len(es.SeqSteps) < 5
}

// applyTemplateFields builds the structured per-field template editor
// for captured phrases with a schema-known action type, and falls back
// to the raw JSON editor when the type has no schema.
func applyTemplateFields(v *views.EditorView, es *EditorState, allSchemas map[string]ActionTypeSchema, captures []captureRef) {
	hasCaptures := len(captures) > 0
	_, hasSchema := allSchemas[es.ActionType]
	v.ShowTemplate = hasCaptures && hasSchema && !es.ShowRaw
	if hasCaptures && hasSchema {
		isKeyAction := isKeyByNameAction(es.ActionType)
		schema := allSchemas[es.ActionType]
		for i, fs := range schema.Fields {
			field := views.TmplFieldView{
				Index:       i,
				Label:       fs.Label,
				Placeholder: placeholderStr(fs.Placeholder),
				ShowInput:   true,
			}
			if i < len(es.TmplFields) {
				field.Mode = es.TmplFields[i].Mode
				field.Val = es.TmplFields[i].Val
				field.ShowInput = field.Mode == "literal"
			} else {
				field.Mode = "literal"
			}
			field.ModeOptions = append(field.ModeOptions, views.SelectOption{
				Value: "literal", Label: "fixed", Selected: field.Mode == "literal",
			})
			for _, c := range captures {
				placeholder := "{" + c.Name + "}"
				field.ModeOptions = append(field.ModeOptions, views.SelectOption{
					Value:    placeholder,
					Label:    "from ‹" + c.Name + "›",
					Selected: field.Mode == placeholder,
				})
			}
			field.HideModeSelect = len(field.ModeOptions) <= 1
			for _, o := range field.ModeOptions {
				if o.Selected {
					field.ModeLabel = o.Label
				}
			}
			if field.ModeLabel == "" && field.Mode != "" && field.Mode != "literal" {
				// A binding the builder cannot draw — a transform such as
				// {alphabet:join("")} — stays as one locked piece, verbatim.
				field.ModeLabel = "from " + field.Mode
				field.IsRawBinding = true
			}
			// Show Record button on the "name" field for key/shortcut action types
			if isKeyAction && fs.Key == "name" && field.ShowInput {
				field.ShowRecord = true
				field.Recording = es.Recording
			}
			v.TmplFields = append(v.TmplFields, field)
		}
	}
	if hasCaptures && !hasSchema {
		v.ShowRaw = true
	}
}

// --- Decompose action JSON into structured template fields ---

func (h *Host) decomposeActionJSON(actionJSON string) (tmplActionType string, fields []EditorTmplField) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(actionJSON), &obj); err != nil {
		return "input.key_by_name", nil
	}
	at, _ := obj["type"].(string)
	if at == "" {
		at = "shell"
	}
	schema, ok := h.actionSchemas()[at]
	if !ok {
		return at, nil
	}
	// One params dialect: fields live under "params" and nowhere else. Flat
	// input decomposes to empty fields — the platform refuses flat at load,
	// so the editor tolerating it would be drift by design.
	params, _ := obj["params"].(map[string]any)
	for _, fs := range schema.Fields {
		val := params[fs.Key]
		field := EditorTmplField{Mode: "literal"}
		if val == nil {
			fields = append(fields, field)
			continue
		}
		var strVal string
		if fs.FieldType == "string[]" {
			if arr, ok := val.([]any); ok {
				parts := make([]string, len(arr))
				for i, v := range arr {
					parts[i] = fmt.Sprintf("%v", v)
				}
				strVal = strings.Join(parts, ",")
				if len(arr) == 1 {
					if s, ok := arr[0].(string); ok && strings.HasPrefix(s, "{") && strings.Contains(s, "}") {
						field.Mode = s
						fields = append(fields, field)
						continue
					}
				}
			} else {
				strVal = fmt.Sprintf("%v", val)
			}
		} else {
			strVal = fmt.Sprintf("%v", val)
		}
		if strings.HasPrefix(strVal, "{") && strings.Contains(strVal, "}") && len(strVal) < 20 {
			field.Mode = strVal
		} else {
			field.Val = strVal
		}
		fields = append(fields, field)
	}
	return at, fields
}

func (h *Host) buildActionJSONFromFields(tmplActionType string, fields []EditorTmplField) string {
	schema, ok := h.actionSchemas()[tmplActionType]
	if !ok {
		return fmt.Sprintf(`{"type":"%s"}`, tmplActionType)
	}
	// One params dialect: the payload nests under "params".
	obj := map[string]any{"type": tmplActionType}
	params := map[string]any{}
	obj["params"] = params
	for i, fs := range schema.Fields {
		if i >= len(fields) {
			break
		}
		f := fields[i]
		if f.Mode == "literal" {
			if fs.FieldType == "string[]" {
				parts := strings.Split(f.Val, ",")
				var arr []string
				for _, p := range parts {
					p = strings.TrimSpace(p)
					if p != "" {
						arr = append(arr, p)
					}
				}
				params[fs.Key] = arr
			} else {
				params[fs.Key] = f.Val
			}
		} else {
			if fs.FieldType == "string[]" {
				params[fs.Key] = []string{f.Mode}
			} else {
				params[fs.Key] = f.Mode
			}
		}
	}
	b, _ := json.MarshalIndent(obj, "", "  ")
	return string(b)
}

// --- RPC handlers ---

// blankEditorState is a new command before anything is typed: one empty
// word, the everyday shortcut shape, the User category.
func (h *Host) blankEditorState() EditorState {
	return EditorState{
		Category:    "User",
		ActionType:  defaultActionType(h.actionSchemas()),
		PhraseSlots: []PhraseSlot{{Type: "literal", Value: ""}},
		NewListSlot: -1,
	}
}

// loadEditorState reconstructs a command as the editor's state. One loader
// feeds both the open editor and the locked inspector, so the two can never
// disagree about a record: the inspector is the form. `details` is the commands.json read the caller already
// has; it may be nil for a command that lives only in the live list.
//
// Discovery does not round-trip yet: the live command list carries the
// post-desugar form (a `_noop` mode-entry + gated capture), not the original
// declaration, so an existing command can't be reconstructed as a discovery
// command here. Declaring discovery is a create-time affordance, compiled in one place for plugin and user
// commands alike.
func (h *Host) loadEditorState(canonical string, details map[string]settings.CommandDetail) EditorState {
	state := h.blankEditorState()
	es := &state
	es.EditCanonical = canonical
	es.Phrase = ""
	es.PhraseSlots = nil
	// Load existing command
	detail, found := details[canonical]
	if !found {
		for _, cmd := range h.edState.lastCommands {
			if cmd.Canonical == canonical || cmd.Pattern == canonical {
				es.IsOverride = !cmd.IsUser
				es.Phrase = cmd.Canonical
				es.Category = cmd.Category
				if es.Category == "" {
					es.Category = "User"
				}
				es.RequiresTags = strings.Join(cmd.RequiresTags, ",")
				es.SetsTags = strings.Join(cmd.SetsTags, ",")
				es.ClearsTags = strings.Join(cmd.ClearsTags, ",")
				if len(cmd.ActionJson) > 0 {
					actionType, actionVal := h.parseActionForEdit(cmd.ActionJson)
					es.ActionType = actionType
					es.ActionVal = actionVal
					switch actionType {
					case "sequence":
						steps := h.parseSequenceSteps(cmd.ActionJson)
						es.SeqSteps = nil
						for _, s := range steps {
							es.SeqSteps = append(es.SeqSteps, EditorSeqStep{Type: s.Type, Val: s.Val})
						}
						es.ActionVal = ""
					case "template":
						// A template raw from the platform arrives in the
						// routed form; unwrap so the textarea and the
						// field decomposition both show the authored
						// dialect (which the platform accepts on save).
						authored := string(unwrapRoutedActionJSON([]byte(actionVal)))
						es.ActionJSON = authored
						es.ActionVal = ""
						var parsed any
						if err := json.Unmarshal([]byte(authored), &parsed); err == nil {
							if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
								es.ActionJSON = string(pretty)
							}
						}
						innerType, fields := h.decomposeActionJSON(authored)
						es.ActionType = innerType
						es.TmplFields = fields
						if _, ok := h.actionSchemas()[innerType]; !ok {
							es.ShowRaw = true
						}
					case "":
						es.ActionType = "shell"
						es.ActionJSON = string(cmd.ActionJson)
						var parsed any
						if err := json.Unmarshal(cmd.ActionJson, &parsed); err == nil {
							if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
								es.ActionJSON = string(pretty)
							}
						}
						es.ShowRaw = true
					default:
						// Plugin action type with schema — set up template fields
						if _, ok := h.actionSchemas()[actionType]; ok {
							authored := unwrapRoutedActionJSON(cmd.ActionJson)
							innerType, fields := h.decomposeActionJSON(string(authored))
							if innerType != "" {
								es.TmplFields = fields
								es.ActionJSON = h.buildActionJSONFromFields(innerType, fields)
							}
						}
					}
				} else {
					es.ActionType = "shell"
					es.ActionVal = cmd.Action
				}
				break
			}
		}
		if es.Phrase == "" {
			es.Phrase = canonical
			es.Category = "User"
			es.ActionType = "shell"
		}
	}
	if found {
		isUserOverride := false
		for _, cmd := range h.edState.lastCommands {
			if cmd.Canonical == canonical && cmd.IsUser {
				isUserOverride = true
				break
			}
		}
		es.IsOverride = !isUserOverride
		// Full-fidelity phrase (preserves alternatives as commas) so
		// editing an alternatives command doesn't drop options on save.
		es.Phrase = settings.RawPhraseFull(detail.Phrase)
		if detail.Category != "" {
			es.Category = detail.Category
		} else {
			es.Category = "User"
		}
		es.Description = detail.Description
		es.RequiresTags = strings.Join(detail.RequiresTags, ",")
		es.SetsTags = strings.Join(detail.SetsTags, ",")
		es.ClearsTags = strings.Join(detail.ClearsTags, ",")

		actionType, actionVal := h.parseActionForEdit(detail.Action)
		es.ActionType = actionType
		es.ActionVal = actionVal

		switch actionType {
		case "sequence":
			steps := h.parseSequenceSteps(detail.Action)
			es.SeqSteps = nil
			for _, s := range steps {
				es.SeqSteps = append(es.SeqSteps, EditorSeqStep{Type: s.Type, Val: s.Val})
			}
			es.ActionVal = ""
		case "template":
			// Same unwrap as the enumerate path: show and decompose the
			// authored dialect regardless of which form the raw arrived in.
			authored := string(unwrapRoutedActionJSON([]byte(actionVal)))
			es.ActionJSON = authored
			es.ActionVal = ""
			var parsed any
			if err := json.Unmarshal([]byte(authored), &parsed); err == nil {
				if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
					es.ActionJSON = string(pretty)
				}
			}
			innerType, fields := h.decomposeActionJSON(authored)
			es.ActionType = innerType
			es.TmplFields = fields
			if _, ok := h.actionSchemas()[innerType]; !ok {
				es.ShowRaw = true
			}
		default:
			// Plugin action type with schema — set up template fields
			if _, ok := h.actionSchemas()[actionType]; ok {
				authored := unwrapRoutedActionJSON(detail.Action)
				innerType, fields := h.decomposeActionJSON(string(authored))
				if innerType != "" {
					es.TmplFields = fields
					es.ActionJSON = h.buildActionJSONFromFields(innerType, fields)
				}
			}
		}
	}
	es.PhraseSlots = parsePhraseToSlots(es.Phrase)
	return state
}

func (h *Host) handleEditorOpen(req *struct {
	Canonical string `json:"canonical"`
}) error {
	// Fetch the installed-app list for the "In app" row before taking the
	// lock (it's an RPC — don't hold the editor mutex across it).
	apps := h.fetchApps()
	var details map[string]settings.CommandDetail
	if req.Canonical != "" {
		details = settings.LoadCommandDetails(branchkit.PluginDir())
	}

	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()
	h.edState.apps = apps

	var es EditorState
	if req.Canonical == "" {
		// A new command starts from something: a copy, a shape, or blank.
		es = h.blankEditorState()
		es.ShowStart = true
	} else {
		es = h.loadEditorState(req.Canonical, details)
	}
	es.Open = true
	h.edState.editor = es
	return nil
}

// handleEditorStart resolves the "Start from…" choice of a new command:
// "copy" takes everything but the phrase from an existing command (its
// blanks stay when it has them — they are the phrase's shape), "shape" opens
// on one of the everyday action types, "blank" just dismisses the cards.
func (h *Host) handleEditorStart(req *struct {
	From      string `json:"from"`
	Canonical string `json:"canonical"`
	Type      string `json:"type"`
}) error {
	var details map[string]settings.CommandDetail
	if req.From == "copy" && req.Canonical != "" {
		details = settings.LoadCommandDetails(branchkit.PluginDir())
	}
	apps := h.edState.apps
	if len(apps) == 0 {
		apps = h.fetchApps()
	}

	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()
	h.edState.apps = apps

	es := h.blankEditorState()
	switch req.From {
	case "copy":
		if req.Canonical != "" {
			es = h.loadEditorState(req.Canonical, details)
			es.EditCanonical = ""
			es.IsOverride = false
			es.Discovery = ""
			if len(parseCaptures(es.Phrase)) == 0 {
				es.Phrase = ""
				es.PhraseSlots = []PhraseSlot{{Type: "literal", Value: ""}}
			}
		}
	case "shape":
		if _, ok := h.actionSchemas()[req.Type]; ok {
			es.ActionType = req.Type
		} else if req.Type == "sequence" {
			es.ActionType = "sequence"
			es.SeqSteps = []EditorSeqStep{{Type: "input.key_by_name"}}
		}
	}
	es.Open = true
	es.ShowStart = false
	h.edState.editor = es
	return nil
}

type editorUpdateRequest struct {
	Trigger        string `json:"trigger"`
	Phrase         string `json:"newrulephrase"`
	ActionType     string `json:"newruleactiontype"`
	ActionVal      string `json:"newruleactionval"`
	ActionJSON     string `json:"newruleactionjson"`
	Category       string `json:"newrulecategory"`
	Description    string `json:"newruledescription"`
	Discovery      string `json:"newrulediscovery"`
	App            string `json:"newruleapp"`
	TmplActionType string `json:"tmplactiontype"`
	TmplF0Mode     string `json:"tmplf0mode"`
	TmplF0Val      string `json:"tmplf0val"`
	TmplF1Mode     string `json:"tmplf1mode"`
	TmplF1Val      string `json:"tmplf1val"`
	TmplF2Mode     string `json:"tmplf2mode"`
	TmplF2Val      string `json:"tmplf2val"`
	TmplF3Mode     string `json:"tmplf3mode"`
	TmplF3Val      string `json:"tmplf3val"`
	Seq0Type       string `json:"seq0type"`
	Seq0Val        string `json:"seq0val"`
	Seq1Type       string `json:"seq1type"`
	Seq1Val        string `json:"seq1val"`
	Seq2Type       string `json:"seq2type"`
	Seq2Val        string `json:"seq2val"`
	Seq3Type       string `json:"seq3type"`
	Seq3Val        string `json:"seq3val"`
	Seq4Type       string `json:"seq4type"`
	Seq4Val        string `json:"seq4val"`
	SeqRemoveIdx   int    `json:"seqRemoveIdx"`
	SeqMoveDir     int    `json:"seqMoveDir"`
}

// syncEditorFromSignals copies the plain typed fields from the form's signals
// into the server state: category, description, the action's value or raw
// JSON, discovery, the app scope, the raw phrase, and — when the shape has
// them — the template fields and sequence steps. Every structural post runs
// it, and so does Save: the value inputs have no handler of their own, so a
// value typed and then saved without any other round trip reached the server
// only here. (Before 2026-09-16 Save wrote the STALE server copy over the
// signals, which is how a user command could be saved with an empty action.)
func syncEditorFromSignals(es *EditorState, req *editorUpdateRequest) {
	if es.ShowRawPhrase {
		es.Phrase = req.Phrase
		es.PhraseSlots = parsePhraseToSlots(es.Phrase)
	}
	es.Category = req.Category
	es.Description = req.Description
	es.ActionVal = req.ActionVal
	es.ActionJSON = req.ActionJSON
	// Discovery rides along; drop it if the phrase no longer ends in a capture
	// so a stale selection can't be saved on a shape it doesn't apply to.
	es.Discovery = req.Discovery
	if !phraseEndsInCapture(es) {
		es.Discovery = ""
	}
	// App scope: the dropdown owns the single app.* requires tag; other requires
	// tags (set via the mode picker) are preserved.
	es.RequiresTags = setAppScope(es.RequiresTags, req.App)
}

func (h *Host) handleEditorUpdate(req *editorUpdateRequest) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open {
		return nil
	}

	syncEditorFromSignals(es, req)

	switch req.Trigger {
	case "action_type":
		es.ActionType = req.ActionType
		es.ActionVal = ""
		es.ActionJSON = ""
		es.SeqSteps = nil
		es.TmplFields = nil
		es.ShowRaw = false
		if req.ActionType == "sequence" {
			es.SeqSteps = append(es.SeqSteps, EditorSeqStep{Type: "input.key_by_name"})
		}
		captures := parseCaptures(es.Phrase)
		if len(captures) > 0 {
			if schema, ok := h.actionSchemas()[req.ActionType]; ok {
				for _, fs := range schema.Fields {
					field := EditorTmplField{Mode: "literal"}
					for _, c := range captures {
						if autoConnectMatch(fs.Key, c.Label) {
							field.Mode = "{" + c.Name + "}"
							break
						}
					}
					es.TmplFields = append(es.TmplFields, field)
				}
				es.ActionJSON = h.buildActionJSONFromFields(req.ActionType, es.TmplFields)
			}
		}

	case "phrase":
		h.syncTmplFieldsFromRequest(es, req)

	case "raw_toggle":
		es.ShowRaw = !es.ShowRaw
		if es.ShowRaw {
			h.syncTmplFieldsFromRequest(es, req)
			es.ActionJSON = h.buildActionJSONFromFields(es.ActionType, es.TmplFields)
		} else {
			es.ActionJSON = req.ActionJSON
			at, fields := h.decomposeActionJSON(es.ActionJSON)
			if _, ok := h.actionSchemas()[at]; ok {
				es.ActionType = at
				es.TmplFields = fields
			} else {
				es.ShowRaw = true
			}
		}

	case "tmpl_field":
		h.syncTmplFieldsFromRequest(es, req)
		es.ActionJSON = h.buildActionJSONFromFields(es.ActionType, es.TmplFields)

	case "seq_from_simple":
		// "+ add a step": the single action becomes step 1 of a sequence and
		// an empty step 2 opens under it.
		first := EditorSeqStep{Type: "input.key_by_name"}
		if _, ok := h.actionSchemas()[es.ActionType]; ok {
			first = EditorSeqStep{Type: es.ActionType, Val: es.ActionVal}
		}
		es.ActionType = "sequence"
		es.ActionVal = ""
		es.ActionJSON = ""
		es.TmplFields = nil
		es.ShowRaw = false
		es.SeqSteps = []EditorSeqStep{first, {Type: "input.key_by_name"}}

	case "seq_add":
		syncSeqStepsFromRequest(es, req)
		if len(es.SeqSteps) < 5 {
			es.SeqSteps = append(es.SeqSteps, EditorSeqStep{Type: "input.key_by_name"})
		}

	case "seq_remove":
		syncSeqStepsFromRequest(es, req)
		idx := req.SeqRemoveIdx
		if idx >= 0 && idx < len(es.SeqSteps) {
			es.SeqSteps = append(es.SeqSteps[:idx], es.SeqSteps[idx+1:]...)
		}

	case "seq_move":
		// Reorder: seqRemoveIdx names the step, seqMoveDir is -1 (up) or +1.
		syncSeqStepsFromRequest(es, req)
		i, j := req.SeqRemoveIdx, req.SeqRemoveIdx+req.SeqMoveDir
		if i >= 0 && i < len(es.SeqSteps) && j >= 0 && j < len(es.SeqSteps) {
			es.SeqSteps[i], es.SeqSteps[j] = es.SeqSteps[j], es.SeqSteps[i]
		}

	case "seq_field":
		syncSeqStepsFromRequest(es, req)

	}

	return nil
}

func (h *Host) handlePhraseSlotUpdate(req *struct {
	Index  int    `json:"index"`
	Type   string `json:"type"` // "literal", "alternatives", or "list"
	Value  string `json:"value"`
	Repeat string `json:"repeat"` // "", "+", "*"
}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open || req.Index < 0 || req.Index >= len(es.PhraseSlots) {
		return nil
	}

	slot := &es.PhraseSlots[req.Index]
	// "＋ New list…" — open the inline create form for this slot rather than
	// setting an invalid list value. The form's Create posts slot-new-list.
	if req.Value == newListSentinel {
		es.NewListSlot = req.Index
		return nil
	}
	// Any real slot edit dismisses an open new-list form.
	es.NewListSlot = -1
	if req.Type != "" {
		slot.Type = req.Type
	}
	switch slot.Type {
	case "alternatives":
		// Store raw so typing "refresh, reload" isn't reflowed mid-keystroke;
		// slotsToPhrase normalizes to the space-free wire token.
		slot.Value = req.Value
		slot.Lists = nil
		slot.Repeat = ""
	case "list":
		lists := strings.Split(req.Value, "|")
		slot.Value = lists[0]
		slot.Lists = lists
		slot.Repeat = req.Repeat
	default: // literal
		slot.Value = req.Value
		slot.Lists = nil
		slot.Repeat = ""
	}

	es.Phrase = slotsToPhrase(es.PhraseSlots)
	return nil
}

func (h *Host) handlePhraseSlotAdd(req *struct {
	After int    `json:"after"` // insert after this index, -1 for append
	Type  string `json:"type"`  // "literal" (default), "alternatives", "list"
}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open {
		return nil
	}

	newSlot := PhraseSlot{Type: "literal", Value: ""}
	switch req.Type {
	case "alternatives":
		newSlot = PhraseSlot{Type: "alternatives", Value: ""}
	case "list":
		// A blank starts on the first list so the phrase is whole at once;
		// the slot's dropdown changes it.
		if names := h.editorListNames(); len(names) > 0 {
			newSlot = PhraseSlot{Type: "list", Value: names[0], Lists: []string{names[0]}}
		} else {
			newSlot = PhraseSlot{Type: "list", Value: ""}
		}
	}
	if req.After < 0 || req.After >= len(es.PhraseSlots) {
		es.PhraseSlots = append(es.PhraseSlots, newSlot)
	} else {
		idx := req.After + 1
		es.PhraseSlots = append(es.PhraseSlots[:idx], append([]PhraseSlot{newSlot}, es.PhraseSlots[idx:]...)...)
	}

	es.Phrase = slotsToPhrase(es.PhraseSlots)
	return nil
}

func (h *Host) handlePhraseSlotRemove(req *struct {
	Index int `json:"index"`
}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open || req.Index < 0 || req.Index >= len(es.PhraseSlots) {
		return nil
	}

	es.PhraseSlots = append(es.PhraseSlots[:req.Index], es.PhraseSlots[req.Index+1:]...)
	if len(es.PhraseSlots) == 0 {
		es.PhraseSlots = []PhraseSlot{{Type: "literal", Value: ""}}
	}

	es.Phrase = slotsToPhrase(es.PhraseSlots)
	return nil
}

// handleEditorReveal opens or closes one of the form's folds: "details", or
// the "Only when" pieces "app", "mode", "after". Without `show` it toggles.
// Closing the app piece also clears the app scope — the row is the value.
func (h *Host) handleEditorReveal(req *struct {
	Section string `json:"section"`
	Show    *bool  `json:"show"`
}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open {
		return nil
	}
	set := func(p *bool) {
		if req.Show == nil {
			*p = !*p
		} else {
			*p = *req.Show
		}
	}
	switch req.Section {
	case "details":
		set(&es.ShowDetails)
	case "app":
		set(&es.RevealApp)
		if !es.RevealApp {
			es.RequiresTags = setAppScope(es.RequiresTags, "")
		}
	case "mode":
		set(&es.RevealMode)
	case "after":
		set(&es.RevealAfter)
	}
	return nil
}

func (h *Host) handlePhraseRawToggle(_ *struct{}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open {
		return nil
	}

	es.ShowRawPhrase = !es.ShowRawPhrase
	if !es.ShowRawPhrase {
		// Switching back to builder — re-parse the phrase
		es.PhraseSlots = parsePhraseToSlots(es.Phrase)
	}

	return nil
}

func (h *Host) handleEditorTagAdd(req *struct {
	Field string `json:"field"` // "requires", "sets", "clears"
	Tag   string `json:"tag"`
}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open || req.Tag == "" {
		return nil
	}

	fieldPtr := tagFieldPtr(es, req.Field)
	if fieldPtr == nil {
		return nil
	}

	// Append tag if not already present
	existing := parseTagList(*fieldPtr)
	for _, t := range existing {
		if t == req.Tag {
			return nil
		}
	}
	existing = append(existing, req.Tag)
	*fieldPtr = strings.Join(existing, ",")

	return nil
}

func (h *Host) handleEditorTagRemove(req *struct {
	Field string `json:"field"`
	Tag   string `json:"tag"`
}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()

	es := &h.edState.editor
	if !es.Open || req.Tag == "" {
		return nil
	}

	fieldPtr := tagFieldPtr(es, req.Field)
	if fieldPtr == nil {
		return nil
	}

	existing := parseTagList(*fieldPtr)
	var filtered []string
	for _, t := range existing {
		if t != req.Tag {
			filtered = append(filtered, t)
		}
	}
	*fieldPtr = strings.Join(filtered, ",")

	return nil
}

func tagFieldPtr(es *EditorState, field string) *string {
	switch field {
	case "requires":
		return &es.RequiresTags
	case "sets":
		return &es.SetsTags
	case "clears":
		return &es.ClearsTags
	}
	return nil
}

func parseTagList(s string) []string {
	var tags []string
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

func (h *Host) syncTmplFieldsFromRequest(es *EditorState, req *editorUpdateRequest) {
	modes := []string{req.TmplF0Mode, req.TmplF1Mode, req.TmplF2Mode, req.TmplF3Mode}
	vals := []string{req.TmplF0Val, req.TmplF1Val, req.TmplF2Val, req.TmplF3Val}
	schema, ok := h.actionSchemas()[es.ActionType]
	if !ok {
		return
	}
	es.TmplFields = nil
	for i := range schema.Fields {
		mode := "literal"
		val := ""
		if i < len(modes) {
			mode = modes[i]
			val = vals[i]
		}
		if mode == "" {
			mode = "literal"
		}
		es.TmplFields = append(es.TmplFields, EditorTmplField{Mode: mode, Val: val})
	}
}

func syncSeqStepsFromRequest(es *EditorState, req *editorUpdateRequest) {
	types := []string{req.Seq0Type, req.Seq1Type, req.Seq2Type, req.Seq3Type, req.Seq4Type}
	vals := []string{req.Seq0Val, req.Seq1Val, req.Seq2Val, req.Seq3Val, req.Seq4Val}
	es.SeqSteps = nil
	for i := 0; i < 5; i++ {
		t := types[i]
		if t == "" {
			break
		}
		es.SeqSteps = append(es.SeqSteps, EditorSeqStep{Type: t, Val: vals[i]})
	}
}

func (h *Host) handleEditorClose(_ *struct{}) error {
	h.edState.mu.Lock()
	h.edState.editor.Open = false
	h.edState.editor.SaveError = ""
	h.edState.mu.Unlock()
	return nil
}

// handleEditorSave saves a rule (create or update) via the platform, then closes
// the editor on success.
// handleSlotNewList creates a simple user list (via the platform
// collections.create_user RPC) and selects it for the slot whose inline
// "＋ New list…" form is open (Part 2a of the list-builder work).
func (h *Host) handleSlotNewList(req *struct {
	Name      string `json:"name"`
	WordsText string `json:"words_text"`
}) error {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil
	}
	// Create first — don't hold the editor lock across the RPC.
	resp, err := h.plugin.CollectionsCreateUser(branchkit.CollectionsCreateUserRequest{Name: name, WordsText: &req.WordsText})

	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()
	es := &h.edState.editor
	if err != nil {
		es.SaveError = "Couldn't create list: " + err.Error()
		return nil
	}
	created := resp.Name
	if created == "" {
		created = name
	}
	if es.NewListSlot >= 0 && es.NewListSlot < len(es.PhraseSlots) {
		slot := &es.PhraseSlots[es.NewListSlot]
		slot.Type = "list"
		slot.Value = created
		slot.Lists = []string{created}
		slot.Repeat = ""
		es.Phrase = slotsToPhrase(es.PhraseSlots)
	}
	es.NewListSlot = -1
	es.SaveError = ""
	return nil
}

// handleSlotNewListCancel dismisses the inline "＋ New list…" form.
func (h *Host) handleSlotNewListCancel(_ *struct{}) error {
	h.edState.mu.Lock()
	defer h.edState.mu.Unlock()
	h.edState.editor.NewListSlot = -1
	h.edState.editor.SaveError = "" // don't leave a failed-create error after cancel
	return nil
}

func (h *Host) handleEditorSave(params json.RawMessage) (any, error) {
	// Determine create vs update based on whether editcanonical is set
	var signals map[string]any
	if err := json.Unmarshal(params, &signals); err != nil {
		return nil, fmt.Errorf("invalid JSON")
	}

	canonical, _ := signals["editcanonical"].(string)
	method := "settings.rules_create"
	if canonical != "" {
		signals["canonical"] = canonical
		method = "settings.rules_update"
	}

	// The typed fields arrive as signals; take them before reading the state
	// (see syncEditorFromSignals). Template fields and sequence steps have the
	// same no-handler inputs, so they sync here too.
	var upd editorUpdateRequest
	_ = json.Unmarshal(params, &upd)
	h.edState.mu.Lock()
	if h.edState.editor.Open {
		syncEditorFromSignals(&h.edState.editor, &upd)
		if len(h.edState.editor.TmplFields) > 0 {
			h.syncTmplFieldsFromRequest(&h.edState.editor, &upd)
			h.edState.editor.ActionJSON = h.buildActionJSONFromFields(h.edState.editor.ActionType, h.edState.editor.TmplFields)
		}
		if h.edState.editor.ActionType == "sequence" {
			syncSeqStepsFromRequest(&h.edState.editor, &upd)
		}
	}
	es := h.edState.editor
	h.edState.mu.Unlock()

	// Inject server-side editor state into signals — the server is the source
	// of truth for phrase (built from slots), tags (from chip picker), and
	// action fields (synced via editor-update).
	signals["newrulephrase"] = es.Phrase
	signals["newruleactiontype"] = es.ActionType
	signals["newruleactionval"] = es.ActionVal
	signals["newruleactionjson"] = es.ActionJSON
	signals["newrulecategory"] = es.Category
	signals["newruledescription"] = es.Description
	signals["newrulerequirestags"] = es.RequiresTags
	signals["newrulesetstags"] = es.SetsTags
	signals["newruleclearstags"] = es.ClearsTags
	signals["newrulediscovery"] = es.Discovery
	for i, step := range es.SeqSteps {
		signals[fmt.Sprintf("seq%dtype", i)] = step.Type
		signals[fmt.Sprintf("seq%dval", i)] = step.Val
	}

	phrase := es.Phrase
	captures := parseCaptures(phrase)
	if len(captures) > 0 && len(es.TmplFields) > 0 {
		actionJSON := h.buildActionJSONFromFields(es.ActionType, es.TmplFields)
		signals["newruleactiontype"] = "template"
		signals["newruleactionjson"] = actionJSON
	}

	// Dry-run check first: the platform REPORTS any terminal-command-rule
	// conflict (it does not block). The block decision lives here — the voice
	// plugin owns the command-builder UI, and the rule is that no command may
	// make a longer command unreachable.
	signals["check_only"] = true
	var chk struct {
		Conflict string `json:"conflict"`
	}
	if err := h.plugin.Call(method, signals, &chk); err != nil {
		branchkit.Logf("voice", "editor: check failed: %v", err)
		return nil, fmt.Errorf("%s", err)
	}
	delete(signals, "check_only")

	if chk.Conflict != "" {
		// Conflict reported — block the save here and show why; keep the editor
		// open. The re-render fired on return draws SaveError inline (a bare
		// error would surface as a banner and lose the editor state).
		h.edState.mu.Lock()
		h.edState.editor.SaveError = chk.Conflict
		h.edState.mu.Unlock()
		return nil, nil
	}

	// No conflict — actually save.
	if err := h.plugin.Call(method, signals, nil); err != nil {
		branchkit.Logf("voice", "editor: save failed: %v", err)
		return nil, fmt.Errorf("%s", err)
	}

	// Saved — clear any prior error and close the editor.
	h.edState.mu.Lock()
	h.edState.editor.SaveError = ""
	h.edState.editor.Open = false
	h.edState.mu.Unlock()
	return nil, nil
}

// --- Record mode: server-side key capture for action value ---

type recordKeydownRequest struct {
	Code  string `json:"code"`
	Key   string `json:"key"`
	Ctrl  bool   `json:"ctrl"`
	Alt   bool   `json:"alt"`
	Shift bool   `json:"shift"`
	Meta  bool   `json:"meta"`
}

func (h *Host) handleEditorRecordStart(_ *struct{}) error {
	h.edState.mu.Lock()
	h.edState.editor.Recording = true
	h.edState.mu.Unlock()

	h.pauseKeybinds()
	return nil
}

func (h *Host) handleEditorRecordStop(_ *struct{}) error {
	h.edState.mu.Lock()
	h.edState.editor.Recording = false
	h.edState.mu.Unlock()

	h.resumeKeybinds()
	return nil
}

func (h *Host) handleEditorRecordKeydown(req *recordKeydownRequest) error {
	// Escape → cancel recording
	if req.Key == "Escape" {
		h.edState.mu.Lock()
		h.edState.editor.Recording = false
		h.edState.mu.Unlock()
		h.resumeKeybinds()
		return nil
	}

	// `input.parse_key_event` is a platform operation. It used to be
	// `plugin.keyboard.parse_key_event` — plugin-to-plugin RPC, which the
	// platform does not route, so this failed on every press and recording a
	// shortcut here did nothing.
	parsed, err := h.plugin.InputParseKeyEvent(branchkit.InputParseKeyEventRequest{
		Alt:   &req.Alt,
		Code:  &req.Code,
		Ctrl:  &req.Ctrl,
		Key:   &req.Key,
		Meta:  &req.Meta,
		Shift: &req.Shift,
	})
	if err != nil {
		branchkit.Logf("voice", "record-keydown: parse error: %v", err)
		return nil
	}

	// Bare modifier or unknown → ignore
	if parsed.IsBareModifier {
		return nil
	}

	h.edState.mu.Lock()
	h.edState.editor.ActionVal = parsed.Combo
	if parsed.HasModifiers && h.edState.editor.ActionType == "input.key_by_name" {
		h.edState.editor.ActionType = "input.shortcut_by_name"
	}
	// Also fill template fields (name + modifiers) when in template mode
	if schema, ok := h.actionSchemas()[h.edState.editor.ActionType]; ok {
		// Grow TmplFields to match schema
		for len(h.edState.editor.TmplFields) < len(schema.Fields) {
			h.edState.editor.TmplFields = append(h.edState.editor.TmplFields, EditorTmplField{Mode: "literal"})
		}
		for i, fs := range schema.Fields {
			if h.edState.editor.TmplFields[i].Mode != "literal" {
				continue // don't overwrite capture-bound fields
			}
			switch fs.Key {
			case "name":
				h.edState.editor.TmplFields[i].Val = parsed.KeyName
			case "modifiers":
				h.edState.editor.TmplFields[i].Val = modifiersFromCombo(parsed.Combo, parsed.KeyName)
			}
		}
	}
	h.edState.editor.Recording = false
	h.edState.mu.Unlock()

	h.resumeKeybinds()
	return nil
}

// isKeyByNameAction returns true if the action type is key_by_name or shortcut_by_name.
func isKeyByNameAction(actionType string) bool {
	return actionType == "input.key_by_name" || actionType == "input.shortcut_by_name"
}

// modifiersFromCombo extracts the modifier portion from a combo string.
// e.g., "cmd+shift+a" with keyName "a" → "cmd+shift", "escape" with keyName "escape" → ""
func modifiersFromCombo(combo, keyName string) string {
	suffix := "+" + keyName
	if strings.HasSuffix(combo, suffix) {
		return combo[:len(combo)-len(suffix)]
	}
	return ""
}
