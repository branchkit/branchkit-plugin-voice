package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Tie disambiguation — the voice-side consumer of the platform tie signal.
//
// When commands.resolve surfaces 2+ equally-eligible commands (a genuine
// tie), the platform refuses to pick and returns the candidate set instead.
// We stand up a transient choice grammar: each candidate gets a spoken
// codeword (plus the candidate's app name as an alias), backed by the
// `plugin.voice.tie_choices` named_entities collection. A scoped + exclusive mode
// tag (`plugin.voice.disambiguating`) silences off-vocabulary words and
// suppresses the original tied commands so a re-spoken verb ("play spotify")
// can't re-tie. The user speaks a choice; the static `<choice:...>` command
// fires `voice.pick_tie`, which re-resolves the original words with
// `prefer_owner` set to the chosen candidate's owner — letting the matcher
// dispatch it as a normal single winner with its tags applied under platform
// authority.

const (
	collTieChoices = "plugin.voice.tie_choices"
	// The EXCLUSIVE, scoped gate held while a tie is awaiting the user's
	// pick. Writer-held (`held_by_writer`, the tag-preset default), so the
	// platform releases it if this process dies mid-tie — no boot reconcile
	// needed here. It was briefly exported for exactly that reconcile, which
	// the platform reaping made redundant.
	tagDisambiguating = "plugin.voice.disambiguating"
	// tieCard is the card channel (manifest `hud_cards`) the chooser is
	// drawn on: a platform card at the top centre, where every plugin's
	// choices appear, with a pressable row per candidate.
	tieCard = "voice.which"
)

// tieCountdown is how long a tie waits for a pick in continuous mode, where
// no hold's end clears it. A variable so tests can shorten it.
var tieCountdown = 8 * time.Second

// tieCodewordPool supplies spoken codewords for tied candidates. Curated to
// be acoustically distinct and unlikely to collide with real command words.
// A tie almost always has 2-4 candidates; the pool cycles if a rare tie
// exceeds its length.
var tieCodewordPool = []string{
	"harp", "arch", "drum", "bell", "fern", "kite",
	"lake", "moon", "nest", "pearl", "quill", "rune",
}

// TieChoice is one disambiguation option: a spoken codeword (also the
// captured token) mapped to the owning plugin and the display fields the
// chooser HUD renders.
type TieChoice struct {
	Token    string // captured value (== Codeword); maps a pick back to Owner
	Codeword string // spoken badge
	Owner    string // owning plugin — the prefer_owner value on re-resolve
	Label    string // human-readable command description (HUD subtitle)
	Aliases  []string
}

// tieEntryPayload is the named_entities record shape for one choice. `pick`
// is the captured value (== codeword); both the codeword key and the app-name
// aliases resolve to it, so a pick routes back to its owner regardless of
// which spoken form the user used.
type tieEntryPayload struct {
	Codeword string   `json:"codeword"`
	Pick     string   `json:"pick"`
	Aliases  []string `json:"aliases,omitempty"`
}

// IsTieActive reports whether a tie disambiguation is currently in flight.
func (s *State) IsTieActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tieChoices) > 0
}

// beginTie stands up the transient choice grammar for a surfaced tie and
// returns the chooser HUD mutation. Side effects (vocab put, mode tag, vocab
// commit) go through the platform via the typed Platform surface — the same proven path the
// browser plugin uses for hint codewords.
func (s *State) beginTie(tie *tieMatch, mode string) *DiscoveryMutation {
	choices := buildTieChoices(tie.candidates)
	if len(choices) == 0 {
		return nil
	}

	// Install the new tie state, capturing any prior choice ids to drop so a
	// re-entrant tie (rare) doesn't accumulate stale vocab.
	s.mu.Lock()
	priorIDs := tieTokens(s.tieChoices)
	s.tieWords = append([]string{}, tie.words...)
	s.tieChoices = choices
	s.tieGen++
	gen := s.tieGen
	s.mu.Unlock()

	if len(priorIDs) > 0 {
		s.deleteTieEntries(priorIDs)
	}
	s.putTieEntries(choices)
	s.setDisambiguatingTag()
	s.commitVocabulary()

	// The words are counted, not printed: they are what the user said, which
	// the log must not hold. The choice tokens are voice's own codewords.
	branchkit.Logf("voice", "tie: disambiguating %d candidates for %d words: %v",
		len(choices), len(tie.words), tieTokens(choices))
	continuous := mode == "continuous_awake"
	s.showTieCard(choices, continuous)
	if continuous {
		// No hold's end will clear it: the tie lapses after the countdown,
		// unless it was picked or replaced by then.
		time.AfterFunc(tieCountdown, func() { s.clearTieIf(&gen) })
	}
	// The chooser is the card; the discovery panel steps aside for it.
	return &DiscoveryMutation{Open: boolPtr(false)}
}

// ResolveTie resolves a tie and RETURNS the winning action rather than
// dispatching it: the user spoke a codeword (or app-name alias) that captured
// `token`, so tear down the transient grammar and re-resolve the original
// words naming the chosen owner.
//
// Returns nil when there is no active tie, or when the re-resolve does not
// land on a single winner. Both are ordinary outcomes, not errors.
//
// The re-resolve is genuine command routing — the user spoke, the matcher
// decided, and the action below is what it returned, not one voice chose. It
// used to travel back to the platform as a `dispatch` call for exactly that
// reason, echoing the resolve's trace to prove it. Returning it instead means
// there is nothing to prove: it rides the `on_transcript` response the
// platform asked for.
func (s *State) ResolveTie(token string) *branchkit.Action {
	s.mu.Lock()
	words := append([]string{}, s.tieWords...)
	owner := ""
	for _, c := range s.tieChoices {
		if c.Token == token {
			owner = c.Owner
			break
		}
	}
	ids := tieTokens(s.tieChoices)
	s.mu.Unlock()

	if owner == "" || len(words) == 0 {
		branchkit.Logf("voice", "tie: pick token %q has no active tie — ignoring", token)
		s.clearTie()
		return nil
	}

	// Drop the transient vocab + refresh the recognizer, and the
	// disambiguating gate: a spoken pick already cleared it (the choice
	// command's clears_tags), but a press on the chooser card runs
	// voice.pick_tie without the matcher, so the gate would still be up and
	// the re-resolve below, suppressed by it, would match nothing and leave
	// every command gated. Clearing it twice is harmless.
	s.clearDisambiguatingTag()
	s.deleteTieEntries(ids)
	s.commitVocabulary()
	s.mu.Lock()
	s.tieWords = nil
	s.tieChoices = nil
	s.mu.Unlock()
	s.clearTieCard()
	s.hud.CloseDiscovery()

	resp := s.resolvePreferOwner(words, owner)
	if resp == nil || !resp.Matched || resp.Action == nil {
		matched := resp != nil && resp.Matched
		branchkit.Logf("voice", "tie: pick %q (owner=%s) did not resolve to a single winner (matched=%v) — nothing to run", token, owner, matched)
		return nil
	}
	branchkit.Logf("voice", "tie: picked owner=%s for %d words", owner, len(words))
	return resp.Action
}

// ClearTie is the exported form of clearTie, for the end-of-hold path in
// the plugin's own package. No-op when no tie is in flight.
func (s *State) ClearTie() {
	s.clearTie()
}

// clearTie tears down an in-flight tie without picking — the dismiss and
// session-end paths. Drops the transient vocab + mode tag and clears state.
// The caller is responsible for the HUD (its response already closes it).
func (s *State) clearTie() {
	s.clearTieIf(nil)
}

// clearTieIf clears the tie in flight, or only tie `gen` when given: the
// check and the taking of the state happen under one lock, so a countdown
// armed for one tie never clears a newer one begun meanwhile.
func (s *State) clearTieIf(gen *uint64) {
	s.mu.Lock()
	if len(s.tieChoices) == 0 && s.tieWords == nil {
		s.mu.Unlock()
		return
	}
	if gen != nil && s.tieGen != *gen {
		s.mu.Unlock()
		return
	}
	ids := tieTokens(s.tieChoices)
	s.tieWords = nil
	s.tieChoices = nil
	s.mu.Unlock()

	if len(ids) > 0 {
		s.deleteTieEntries(ids)
		s.commitVocabulary()
	}
	s.clearDisambiguatingTag()
	s.clearTieCard()
	branchkit.Logf("voice", "tie: cleared")
}

// resolvePreferOwner re-issues commands.resolve with prefer_owner so the
// matcher breaks the tie and returns the chosen candidate as a single winner.
func (s *State) resolvePreferOwner(words []string, owner string) *resolveResponse {
	if s.Platform == nil {
		return nil
	}
	// ActiveTags nil: the re-resolve runs against the live session tags.
	resp, err := s.resolve(branchkit.CommandsResolveRequest{Words: words, PreferOwner: &owner})
	if err != nil {
		branchkit.Logf("voice", "tie: re-resolve failed: %v", err)
		return nil
	}
	return resp
}

// --- helpers ---

// buildTieChoices assigns a codeword to each tied candidate and derives its
// spoken aliases (the app name) and HUD label.
func buildTieChoices(candidates []tiedCandidate) []TieChoice {
	choices := make([]TieChoice, 0, len(candidates))
	for i, c := range candidates {
		codeword := tieCodewordPool[i%len(tieCodewordPool)]
		label := c.Label
		if label == "" {
			label = c.OwnerPlugin
		}
		var aliases []string
		if alias := spokenOwnerAlias(c.OwnerPlugin); alias != "" && alias != codeword {
			aliases = []string{alias}
		}
		choices = append(choices, TieChoice{
			Token:    codeword,
			Codeword: codeword,
			Owner:    c.OwnerPlugin,
			Label:    label,
			Aliases:  aliases,
		})
	}
	return choices
}

// spokenOwnerAlias turns an owner plugin id into a speakable alias: lowercase,
// non-letter runs collapsed to single spaces. Returns "" if nothing speakable
// remains (the codeword is always the reliable form).
func spokenOwnerAlias(owner string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(owner) {
		if (r >= 'a' && r <= 'z') || r == ' ' {
			if r == ' ' {
				if prevSpace {
					continue
				}
				prevSpace = true
			} else {
				prevSpace = false
			}
			b.WriteRune(r)
		} else if !prevSpace {
			b.WriteRune(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func tieTokens(choices []TieChoice) []string {
	ids := make([]string, len(choices))
	for i, c := range choices {
		ids[i] = c.Token
	}
	return ids
}

func (s *State) putTieEntries(choices []TieChoice) {
	if s.Platform == nil {
		return
	}
	entries := make([]branchkit.CollectionPutEntry, 0, len(choices))
	for _, c := range choices {
		payload, err := json.Marshal(tieEntryPayload{
			Codeword: c.Codeword,
			Pick:     c.Token,
			Aliases:  c.Aliases,
		})
		if err != nil {
			continue
		}
		entries = append(entries, branchkit.CollectionPutEntry{ID: c.Token, Payload: payload})
	}
	if len(entries) == 0 {
		return
	}
	s.Platform.CollectionPut(branchkit.CollectionPutRequest{Name: collTieChoices, Entries: entries})
}

func (s *State) deleteTieEntries(ids []string) {
	if s.Platform == nil || len(ids) == 0 {
		return
	}
	s.Platform.CollectionDeleteRecords(branchkit.CollectionDeleteRecordsRequest{Name: collTieChoices, Ids: ids})
}

func (s *State) setDisambiguatingTag() {
	if s.Platform == nil {
		return
	}
	s.Platform.CollectionPut(branchkit.CollectionPutRequest{
		Name:    tagDisambiguating,
		Entries: []branchkit.CollectionPutEntry{{ID: "singleton", Payload: json.RawMessage("{}")}},
	})
}

func (s *State) commitVocabulary() {
	if s.Platform == nil {
		return
	}
	s.Platform.VocabularyCommit()
}

// showTieCard draws the chooser as a platform card: per candidate, the
// codeword to say as the row's title, the owning app and what the command
// does beneath it, and a press that picks it (voice.pick_tie, this plugin's
// own action, so the platform lets the card run it). In continuous mode a
// bar shows the countdown.
func (s *State) showTieCard(choices []TieChoice, continuous bool) {
	if s.Platform == nil {
		return
	}
	items := make([]branchkit.OutputItem, 0, len(choices))
	for _, c := range choices {
		params, err := json.Marshal(map[string]string{"choice": c.Token})
		if err != nil {
			continue
		}
		dispatch := "voice.pick_tie"
		subtitle := c.Owner
		if c.Label != "" {
			subtitle = c.Owner + " · " + c.Label
		}
		items = append(items, branchkit.OutputItem{
			ID:         c.Token,
			Title:      c.Codeword,
			Phrase:     c.Codeword,
			Subtitle:   &subtitle,
			AltPhrases: c.Aliases,
			Action:     &branchkit.OutputAction{Dispatch: &dispatch, Params: params},
		})
	}
	footer := "Say a codeword or the app name"
	doc := branchkit.OutputState{
		Channel:  tieCard,
		Kind:     branchkit.OutputKindChoices,
		Title:    "Which command?",
		Phrase:   fmt.Sprintf("which command? %d choices", len(choices)),
		Sections: []branchkit.OutputSection{{Items: items}},
		Footer:   &footer,
		Urgency:  branchkit.OutputUrgencyNotable,
		Locale:   "en",
		V:        1,
	}
	if continuous {
		ms := int(tieCountdown / time.Millisecond)
		doc.Progress = &branchkit.OutputProgress{RemainingMs: ms, TotalMs: ms}
	}
	if _, err := s.Platform.OutputState(branchkit.OutputStateRequest{State: doc}); err != nil {
		branchkit.Logf("voice", "tie: chooser card: %v", err)
	}
}

// clearDisambiguatingTag drops the exclusive gate a tie holds.
func (s *State) clearDisambiguatingTag() {
	if s.Platform == nil {
		return
	}
	s.Platform.CollectionDeleteRecords(branchkit.CollectionDeleteRecordsRequest{Name: tagDisambiguating, Ids: []string{"singleton"}})
}

// clearTieCard takes the chooser card down. Clearing a card that is not up
// is harmless.
func (s *State) clearTieCard() {
	if s.Platform == nil {
		return
	}
	if _, err := s.Platform.OutputClear(branchkit.OutputClearRequest{Channel: tieCard}); err != nil {
		branchkit.Logf("voice", "tie: clear chooser card: %v", err)
	}
}

// blockedCard is the card that says why a command you said did nothing.
const blockedCard = "voice.blocked"

// showBlockedCard says why the command the person said is switched off, in
// the platform's words: the card for those who see it, the phrase for a
// renderer that speaks it.
func (s *State) showBlockedCard(b *branchkit.BlockedCommand) {
	if s.Platform == nil || b == nil {
		return
	}
	why := strings.Join(b.Reasons, "; ")
	if why == "" {
		why = "it is switched off"
	}
	doc := branchkit.OutputState{
		Channel: blockedCard,
		// A notice: it goes on its own (6 s, or the person's reading time),
		// so nobody has to close it by hand to get it off the screen.
		Kind:    branchkit.OutputKindNotice,
		Title:   fmt.Sprintf("\u201c%s\u201d didn\u2019t run", b.Command),
		Phrase:  fmt.Sprintf("%s didn't run: %s", b.Command, why),
		Urgency: branchkit.OutputUrgencyNotable,
		Locale:  "en",
		V:       1,
	}
	if _, err := s.Platform.OutputState(branchkit.OutputStateRequest{State: doc}); err != nil {
		branchkit.Logf("voice", "blocked: card: %v", err)
	}
}
