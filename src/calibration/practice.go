package calibration

import (
	"sort"
	"strings"
)

// Unified calibration: one engine, a catalog of every command, two signals.
// A walk is a queue of Targets; per target the engine derives the context to
// enter (base / mode-gated / dynamic / slotted) and computes up to two
// independent signals — Heard (acoustic) and Resolves (functional, via
// `commands.resolve --preview`). Nothing the user picks is a "mode"; both
// context and scoring are derived from the command itself.
//
// The scoring spine is the platform's preview-resolve (`commands.resolve` with
// `preview: true`): it returns the full matcher decision but commits no tag
// writes, seeds no bridge, and emits no production telemetry.

// Three-way verdict labels, derived from the two signals and persisted on the
// attempt record (and rendered today). pass = both signals held; warn =
// exactly one; fail = neither. For acoustic-only targets (bare codewords,
// slotted fillers) there's a single signal, so only pass/fail apply.
const (
	PracticePass = "pass"
	PracticeWarn = "warn"
	PracticeFail = "fail"
)

// Selection keywords for a walk. Anything else is treated as either a
// "category:<owner>" filter or an explicit target-id / codeword list.
const (
	SelectionAll      = "all"      // every enumerated command, all tiers
	SelectionAlphabet = "alphabet" // the voice codeword set (built from phonetics)
	SelectionDrill    = "drill"    // never-tested first, then weak/stale
	categoryPrefix    = "category:"
)

// EnumeratedCommand mirrors one row of the `commands.enumerate` response — the
// identity + classification metadata the catalog and scoring need. Also the
// decode target for the EnumerateCommands callback in main.go.
type EnumeratedCommand struct {
	ID           string   `json:"id"`
	Owner        string   `json:"owner_plugin"`
	Pattern      string   `json:"pattern"`
	Action       string   `json:"action"`
	RequiresTags []string `json:"requires_tags"`
	// SetsTags marks a MODE ENTRY: saying this command sets these tags, which
	// gate other commands. The catalog nests those gated commands under the
	// entry (the combo tree).
	SetsTags    []string `json:"sets_tags"`
	Dynamic     bool     `json:"dynamic"`
	Category    string   `json:"category,omitempty"`
	Description string   `json:"description,omitempty"`
}

// VocabBinding ties a target to the vocabulary entry it tests — the
// named_entities collection and the entry's current key (its matchable surface
// form: a codeword, a number word, a direction, an app name). It's what makes a
// target *retunable*: when the engine mishears, the user edits this surface form
// and the platform renames the entry (overrides.apply rename). nil when the
// target isn't backed by a single editable list entry (a command-pattern word, a
// hint pair, free text).
type VocabBinding struct {
	Collection string
	Key        string
}

// Target is one row of a calibration walk. A complete command carries its
// CommandID (`<owner>:<pattern>`); a bare voice codeword (alphabet) leaves it
// empty, which is what suppresses the Resolves signal — a codeword isn't a
// command. Pattern is the canonical identity (compared against the resolve
// winner); Prompt is what the user is shown/asked to say (== Pattern except
// for slotted targets, where it's a generated concrete filler).
type Target struct {
	CommandID string
	Owner     string
	Pattern   string
	Tier      Tier
	Prompt    string
	// Binding is the vocabulary entry this target tests, when retunable
	// (nil otherwise). See VocabBinding.
	Binding *VocabBinding
	// Group bundles the targets that came from expanding one command over a
	// vocabulary (a list slot's entries, or the hint codeword cross product) so
	// the walk can present + skip them as a contained unit. Empty for a
	// standalone target. The value is the command pattern, shared by every
	// expansion of that command.
	Group string
}

// normPhrase folds a phrase for comparison: lowercase, single-spaced, trimmed.
func normPhrase(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Verdict is the result of scoring one trial: up to two independent signals.
//
//   - Heard (acoustic): the engine transcribed the prompt (heard ≈ prompt).
//     Always computable.
//   - Resolves (functional): the heard words resolve to the intended command,
//     in the command's own context. nil when not applicable (a bare codeword,
//     which isn't a command; a slotted filler, scored acoustic-only; or a
//     resolve RPC that failed, leaving the signal genuinely unknown).
type Verdict struct {
	Heard    bool
	Resolves *bool
}

// Passed reports whether the trial counts as a success. A complete-command
// target (Resolves computed) passes only when both signals hold; an
// acoustic-only target passes on Heard alone.
func (v Verdict) Passed() bool {
	if v.Resolves != nil {
		return v.Heard && *v.Resolves
	}
	return v.Heard
}

// Label collapses the two signals into the persisted/rendered three-way
// string. With both signals present: pass = both, warn = exactly one, fail =
// neither. Acoustic-only (Resolves nil): pass = Heard, fail = not.
func (v Verdict) Label() string {
	if v.Resolves == nil {
		if v.Heard {
			return PracticePass
		}
		return PracticeFail
	}
	switch {
	case v.Heard && *v.Resolves:
		return PracticePass
	case v.Heard || *v.Resolves:
		return PracticeWarn
	default:
		return PracticeFail
	}
}

// ScoreTrial computes the two signals for one trial. `heard` is the engine
// transcript, `target` the row being tested, and `dec` the preview-resolve
// decision (nil when the resolve RPC wasn't run or failed).
//
// Heard is the acoustic match against the prompt. Resolves is computed only
// for a complete-command target (has a CommandID and isn't slotted) and is
// left unknown (nil) when the resolve RPC produced no decision — an infra
// failure shouldn't masquerade as "won't fire."
func ScoreTrial(heard string, target Target, dec *MatcherDecision) Verdict {
	prompt := normPhrase(target.Prompt)
	v := Verdict{Heard: prompt != "" && normPhrase(heard) == prompt}

	if target.resolvable() && dec != nil {
		r := dec.Matched &&
			strings.EqualFold(dec.Owner, target.Owner) &&
			normPhrase(dec.WinningPattern) == normPhrase(target.Pattern)
		v.Resolves = &r
	}
	return v
}

// resolvable reports whether a target gets a functional (Resolves) signal: a
// complete command that is NOT a bare codeword, a slotted filler, or a dynamic
// (hint) command. Dynamic commands are acoustic-only — the activate command
// resolves against the live focused-tab hint projection (only on-screen hints),
// which calibration can't reproduce, so there's no meaningful Resolves signal
// to compute (only whether the engine HEARD the codewords under the heavy
// grammar).
func (t Target) resolvable() bool {
	return t.CommandID != "" && t.Tier != TierSlotted && t.Tier != TierDynamic
}

// Tier classifies a command by the setup it needs before a trial and how it
// can be scored — a pure function of its enumerate fields.
type Tier string

const (
	// TierBase: static, no requires_tags, no <slot>. No setup; both signals.
	TierBase Tier = "base"
	// TierModeGated: static with requires_tags. The platform platform-writes
	// the tags on context entry; both signals.
	TierModeGated Tier = "mode_gated"
	// TierDynamic: hints (DependentCapture / undeclared collection). The owning
	// plugin applies its fixture so the codewords become recognizable under the
	// heavy union grammar; scored acoustic-only. The activate command resolves
	// against the live focused-tab hint projection (visible on-screen hints),
	// which calibration can't fake — so there's no functional (Resolves) signal.
	TierDynamic Tier = "dynamic"
	// TierSlotted: pattern carries a <slot>. A concrete filler is substituted
	// for the prompt; acoustic-only until a filler that resolves is generated.
	TierSlotted Tier = "slotted"
)

// Tier reports the command's calibration tier — the exported accessor the
// catalog view uses to badge each row.
func (c EnumeratedCommand) Tier() Tier { return tierFor(c) }

// tierFor derives a command's calibration tier from its enumerate fields.
// Precedence: dynamic (fixture) > slotted (filler + acoustic-only) >
// mode_gated (tags) > base. A command that is both slotted and mode-gated is
// reported slotted because scoring is acoustic-only; the platform still writes
// its requires_tags on context entry, since it classifies the command as
// static independently of the slot.
func tierFor(c EnumeratedCommand) Tier {
	switch {
	case c.Dynamic:
		return TierDynamic
	case strings.Contains(c.Pattern, "<"):
		return TierSlotted
	case len(c.RequiresTags) > 0:
		return TierModeGated
	default:
		return TierBase
	}
}

// promptFor returns the phrase the user is asked to say for a command. For a
// slotted pattern it substitutes a concrete filler per slot so the prompt is
// speakable (acoustic-only); otherwise the pattern is the prompt.
func promptFor(c EnumeratedCommand) string {
	if !strings.Contains(c.Pattern, "<") {
		return c.Pattern
	}
	var b strings.Builder
	for i, tok := range strings.Fields(c.Pattern) {
		if i > 0 {
			b.WriteByte(' ')
		}
		if strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">") {
			b.WriteString(slotFiller(tok))
		} else {
			b.WriteString(tok)
		}
	}
	return b.String()
}

// slotFiller picks a concrete word for a `<slot>` token, by a coarse read of
// the slot name. One filler is enough for an acoustic read; richer per-slot
// variation is an open training-quality question (see the design note).
func slotFiller(tok string) string {
	name := strings.ToLower(strings.Trim(tok, "<>"))
	switch {
	case strings.Contains(name, "num") || strings.Contains(name, "count") || name == "n":
		return "three"
	default:
		return "sample"
	}
}

// targetFromCommand builds a walk Target from an enumerated command.
func targetFromCommand(c EnumeratedCommand, reason string) Target {
	return Target{
		CommandID: c.ID,
		Owner:     c.Owner,
		Pattern:   c.Pattern,
		Tier:      tierFor(c),
		Prompt:    promptFor(c),
	}
}

// dynamicSlotCombos enumerates every concrete prompt for a dynamic (all-slot)
// command — the full cartesian product of the codeword set across the pattern's
// <slot>s, with literal words kept in place. For the two-slot hint command over
// 26 codewords that's 26×26 = 676 pairs, ordered prefix-outer / suffix-inner
// (air air, air bat, …, air yank, bat air, …). Returns the pattern unchanged
// when it has no slots or there are no codewords.
func dynamicSlotCombos(pattern string, codewords []string) []string {
	toks := strings.Fields(pattern)
	var slots []int
	for i, tok := range toks {
		if strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">") {
			slots = append(slots, i)
		}
	}
	if len(slots) == 0 || len(codewords) == 0 {
		return []string{strings.Join(toks, " ")}
	}
	n := len(codewords)
	total := 1
	for range slots {
		total *= n
	}
	out := make([]string, 0, total)
	idx := make([]int, len(slots)) // mixed-radix counter, last slot fastest
	for {
		row := append([]string(nil), toks...)
		for s, pos := range slots {
			row[pos] = codewords[idx[s]]
		}
		out = append(out, strings.Join(row, " "))
		k := len(idx) - 1
		for ; k >= 0; k-- {
			idx[k]++
			if idx[k] < n {
				break
			}
			idx[k] = 0
		}
		if k < 0 {
			break
		}
	}
	return out
}

// ExpandDynamicTargets replaces each dynamic (all-slot) target with concrete
// codeword prompts under its hint context. With `single` false it's the full
// cross product across the slots (26×26 = 676 for the hint command) — the whole
// hint vocabulary. With `single` true it's each codeword on its own (26) — to
// test individual codeword acoustics under the heavy grammar. Non-dynamic
// targets pass through unchanged; the input is returned as-is with no codewords.
func ExpandDynamicTargets(targets []Target, codewords []string, single bool) []Target {
	if len(codewords) == 0 {
		return targets
	}
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if t.Tier != TierDynamic {
			out = append(out, t)
			continue
		}
		prompts := dynamicSlotCombos(t.Pattern, codewords)
		if single {
			prompts = codewords // each codeword alone
		}
		for _, prompt := range prompts {
			nt := t
			nt.Prompt = prompt
			nt.Group = t.Pattern // skip the whole 26/676 expansion as a unit
			// A single-codeword prompt is a retunable alphabet entry; a pair
			// ("air bat") names two codewords, so it isn't bound to one.
			if single {
				nt.Binding = &VocabBinding{Collection: "alphabet", Key: prompt}
			}
			out = append(out, nt)
		}
	}
	return out
}

// expandDynamicTargets expands the dynamic targets in `targets`, preferring
// owner-supplied samples over the codeword stand-in. For each dynamic target it
// asks the owning plugin (via CommandSamples → the `trial_samples` hook)
// for concrete prompts; if the owner supplies any they become the walk group
// (one target per prompt, grouped under the pattern). Otherwise it falls back to
// the codeword cross-product / single set (ExpandDynamicTargets). This is how a
// third-party plugin's dynamic command — whose real vocabulary isn't the voice
// codewords — gets the same grouped-walk treatment as hints. Non-dynamic targets
// pass through untouched.
func (s *State) expandDynamicTargets(targets []Target, single bool) []Target {
	codewords := s.sortedCodewords()
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if t.Tier != TierDynamic {
			out = append(out, t)
			continue
		}
		if prompts := s.commandSamples(t.CommandID); len(prompts) > 0 {
			for _, p := range prompts {
				nt := t
				nt.Prompt = p
				nt.Group = t.Pattern // walk + skip the owner-supplied set as a unit
				out = append(out, nt)
			}
			continue
		}
		out = append(out, ExpandDynamicTargets([]Target{t}, codewords, single)...)
	}
	return out
}

// commandSamples asks the owning plugin for a command's calibration prompts
// (nil when the accessor is unwired or the owner supplies none).
func (s *State) commandSamples(commandID string) []string {
	if s.CommandSamples == nil || commandID == "" {
		return nil
	}
	return s.CommandSamples(commandID)
}

// singleListSlot returns the collection a pattern's sole `<slot>` binds to, plus
// the slot token, when the pattern has exactly one plain slot backed by a single
// list. Returns ("", "") for patterns with no slot, multiple slots, a union
// (`<a|b>`), or a slot carrying a modifier (`<x>+`) — none of which name one
// editable list entry. `<name:collection>` resolves to `collection`; a bare
// `<number>` to `number`.
func singleListSlot(pattern string) (collection, slot string) {
	found := ""
	for _, tok := range strings.Fields(pattern) {
		if strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">") {
			if found != "" {
				return "", "" // more than one slot — ambiguous
			}
			found = tok
		}
	}
	if found == "" {
		return "", ""
	}
	inner := strings.Trim(found, "<>")
	if strings.Contains(inner, "|") {
		return "", "" // union slot — not a single list
	}
	if i := strings.Index(inner, ":"); i >= 0 {
		return inner[i+1:], found
	}
	return inner, found
}

// retunableCollection reports whether a slot's collection is a user-editable
// vocabulary list. Excludes the dynamic hint collections and the transient
// disambiguation list; non-existent collections (e.g. a free-text `<text>`
// slot) are filtered separately by their empty key set.
func retunableCollection(coll string) bool {
	if coll == "" || strings.Contains(coll, "hints") || coll == "plugin.voice.tie_choices" {
		return false
	}
	return true
}

// expandListSlotTargets replaces each slotted target backed by a single
// retunable list with one bound target per list entry — so the walk covers (and
// lets you retune) every number/direction/app word, the way the alphabet walk
// covers every codeword and ExpandDynamicTargets covers every hint codeword.
// Targets whose slot is a union, free text, or a non-retunable collection pass
// through unchanged with their synthetic filler. Returns a new slice.
func (s *State) expandListSlotTargets(targets []Target) []Target {
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if t.Tier != TierSlotted || t.Binding != nil {
			out = append(out, t)
			continue
		}
		coll, slot := singleListSlot(t.Pattern)
		// List-backed slot: expand over its entries (each retunable).
		if retunableCollection(coll) && s.CollectionKeys != nil {
			if keys := s.CollectionKeys(coll); len(keys) > 0 {
				for _, key := range keys {
					nt := t
					nt.Prompt = strings.Replace(t.Pattern, slot, key, 1)
					nt.Binding = &VocabBinding{Collection: coll, Key: key}
					nt.Group = t.Pattern
					out = append(out, nt)
				}
				continue
			}
		}
		// Free-text / no-list slot (e.g. `<text>`): today acoustic-only with a
		// single generic filler. If the owning plugin supplies calibration
		// samples, walk those (grouped) instead — the same `trial_samples`
		// hook that backs dynamic commands, so a third-party free-text command
		// can offer realistic examples.
		if prompts := s.commandSamples(t.CommandID); len(prompts) > 0 {
			for _, p := range prompts {
				nt := t
				nt.Prompt = p
				nt.Group = t.Pattern
				out = append(out, nt)
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

// CodewordTarget builds an acoustic-only Target for a bare voice codeword
// (alphabet, or an arbitrary user-supplied word). No CommandID → no Resolves.
// Bound to the alphabet entry so it's retunable in place.
func CodewordTarget(word string) Target {
	return Target{
		Owner:   "voice",
		Pattern: word,
		Tier:    TierBase,
		Prompt:  word,
		Binding: &VocabBinding{Collection: "alphabet", Key: word},
	}
}

// BuildCommandTargets turns the enumerated command set into a walk queue for a
// command selection — `all`, `drill`, or `category:<owner>`. Spans every tier
// (the unification: mode-gated / dynamic / slotted commands are testable too,
// not just the base set the old Tier-1 practice could reach).
//
//   - all / category: sorted by owner then pattern.
//   - drill: never-tested first, then the RecommendDrill prioritization
//     (low-accuracy / confused-pair / stale / high-friction) over `attempts`.
func BuildCommandTargets(enum []EnumeratedCommand, selection string, attempts []VerificationAttempt) []Target {
	owner := ""
	if rest, ok := strings.CutPrefix(selection, categoryPrefix); ok {
		owner = rest
	}

	pool := make([]EnumeratedCommand, 0, len(enum))
	byPattern := make(map[string]EnumeratedCommand, len(enum))
	for _, c := range enum {
		if owner != "" && c.Owner != owner {
			continue
		}
		pool = append(pool, c)
		byPattern[c.Pattern] = c
	}

	if selection != SelectionDrill {
		out := make([]Target, 0, len(pool))
		for _, c := range pool {
			out = append(out, targetFromCommand(c, ""))
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Owner != out[j].Owner {
				return out[i].Owner < out[j].Owner
			}
			return out[i].Pattern < out[j].Pattern
		})
		return out
	}

	// Drill: never-tested first (RecommendDrill only knows commands that
	// already have attempts), then the recommender's weak/stale picks.
	tested := make(map[string]bool)
	for _, a := range attempts {
		tested[a.Target] = true
	}

	var out []Target
	seen := make(map[string]bool)
	add := func(c EnumeratedCommand, reason string) {
		if seen[c.Pattern] {
			return
		}
		seen[c.Pattern] = true
		out = append(out, targetFromCommand(c, reason))
	}

	var fresh []EnumeratedCommand
	for _, c := range pool {
		if !tested[c.Pattern] {
			fresh = append(fresh, c)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Pattern < fresh[j].Pattern })
	for _, c := range fresh {
		add(c, "never_tested")
	}

	if rec := RecommendDrill(attempts, 0); rec != nil {
		for _, d := range rec.Commands {
			if d.Reason == "never_tested" {
				continue // covered above
			}
			if c, ok := byPattern[d.Command]; ok {
				add(c, d.Reason)
			}
		}
	}
	return out
}

// TargetsFromIDs resolves an explicit list of command ids into walk Targets,
// preserving the caller's order. Unknown ids are dropped.
func TargetsFromIDs(enum []EnumeratedCommand, ids []string) []Target {
	byID := make(map[string]EnumeratedCommand, len(enum))
	for _, c := range enum {
		byID[c.ID] = c
	}
	out := make([]Target, 0, len(ids))
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			out = append(out, targetFromCommand(c, ""))
		}
	}
	return out
}

// Prompts returns each target's prompt in order — the word list handed to the
// Game (each trial's Target is the prompt phrase).
func Prompts(targets []Target) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Prompt
	}
	return out
}
