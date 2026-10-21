package session

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// recordedCall captures one Platform invocation for assertions.
type recordedCall struct {
	method string
	params any
}

// tieRecorder is a Platform fake that records every call and, for
// commands.resolve, answers with a canned response.
type tieRecorder struct {
	// mu: the continuous-mode countdown clears a tie from a timer goroutine.
	mu          sync.Mutex
	calls       []recordedCall
	resolveResp *resolveResponse
}

func (r *tieRecorder) record(method string, params any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCall{method: method, params: params})
}

func (r *tieRecorder) CommandsResolve(req branchkit.CommandsResolveRequest) (*branchkit.CommandsResolveResponse, error) {
	r.record(branchkit.MethodCommandsResolve, req)
	var out branchkit.CommandsResolveResponse
	if r.resolveResp != nil {
		// Round-trip the canned session view through the wire so the fake
		// exercises the same typed response the real wrapper returns.
		b, _ := json.Marshal(r.resolveResp)
		_ = json.Unmarshal(b, &out)
	}
	return &out, nil
}

// CommandsResolvePhrase answers every step with the canned resolve, strung
// together the way fakePhrase does, round-tripped through the wire types.
func (r *tieRecorder) CommandsResolvePhrase(req branchkit.CommandsResolvePhraseRequest) (*branchkit.CommandsResolvePhraseResponse, error) {
	r.record(branchkit.MethodCommandsResolvePhrase, req)
	canned := func([]string, *string, []string) *resolveResponse {
		if r.resolveResp == nil {
			return &resolveResponse{}
		}
		c := *r.resolveResp
		return &c
	}
	wire := func(resp *resolveResponse) branchkit.ResolveResult {
		var out branchkit.ResolveResult
		b, _ := json.Marshal(resp)
		_ = json.Unmarshal(b, &out)
		return out
	}
	p := fakePhrase(req.Words, canned)
	out := &branchkit.CommandsResolvePhraseResponse{Dropped: p.Dropped}
	for _, step := range p.Steps {
		out.Steps = append(out.Steps, branchkit.PhraseStep{Start: step.Start, Words: step.Words, Resolved: wire(step.Resolved)})
	}
	if p.Stopped != nil {
		out.Stopped = &branchkit.PhraseStop{At: p.StoppedAt, Resolved: wire(p.Stopped)}
	}
	return out, nil
}

func (r *tieRecorder) CollectionPut(req branchkit.CollectionPutRequest) (*branchkit.CollectionPutResponse, error) {
	r.record(branchkit.MethodCollectionPut, req)
	return &branchkit.CollectionPutResponse{}, nil
}

func (r *tieRecorder) CollectionDeleteRecords(req branchkit.CollectionDeleteRecordsRequest) (*branchkit.CollectionDeleteRecordsResponse, error) {
	r.record(branchkit.MethodCollectionDeleteRecords, req)
	return &branchkit.CollectionDeleteRecordsResponse{}, nil
}

func (r *tieRecorder) OutputState(req branchkit.OutputStateRequest) (*branchkit.OutputStateResponse, error) {
	r.record(branchkit.MethodOutputState, req)
	return &branchkit.OutputStateResponse{}, nil
}

func (r *tieRecorder) OutputClear(req branchkit.OutputClearRequest) (*branchkit.OutputClearResponse, error) {
	r.record(branchkit.MethodOutputClear, req)
	return &branchkit.OutputClearResponse{}, nil
}

func (r *tieRecorder) VocabularyCommit() error {
	r.record(branchkit.MethodVocabularyCommit, nil)
	return nil
}

func (r *tieRecorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.method
	}
	return out
}

func (r *tieRecorder) count(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func (r *tieRecorder) firstParams(method string) any {
	for _, c := range r.calls {
		if c.method == method {
			return c.params
		}
	}
	return nil
}

func tieResolveResp(owners ...string) *resolveResponse {
	cands := make([]tiedCandidate, len(owners))
	for i, o := range owners {
		cands[i] = tiedCandidate{
			OwnerPlugin: o,
			Action:      pluginAction(o+".play", ""),
			Label:       "Play on " + o,
		}
	}
	return &resolveResponse{Matched: false, TiedCandidates: cands}
}

// orchestrate on a surfaced tie should stand up the transient choice grammar
// (vocab put + mode tag + one vocab commit), open the chooser HUD, dispatch
// nothing, and leave the tie active.
func TestOrchestrateTieStandsUpChoiceGrammar(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec
	s.callResolveFn = func(words []string, requireTag *string, activeTags []string) *resolveResponse {
		return tieResolveResp("spotify", "youtube")
	}

	resp := s.orchestrate(&OrchestrateRequest{
		Words: []string{"play"},
		Mode:  "command_hold",
	})

	if len(resp.Actions) != 0 {
		t.Fatalf("a tie must not dispatch any action, got %d", len(resp.Actions))
	}
	// The chooser is a platform card; the discovery panel steps aside.
	if resp.Discovery == nil || resp.Discovery.Open == nil || *resp.Discovery.Open {
		t.Fatalf("expected the discovery panel to close for the chooser card, got %+v", resp.Discovery)
	}
	card, ok := rec.firstParams(branchkit.MethodOutputState).(branchkit.OutputStateRequest)
	if !ok {
		t.Fatalf("expected the chooser card to be published, got %v", rec.methods())
	}
	if card.State.Channel != tieCard || card.State.Kind != branchkit.OutputKindChoices {
		t.Errorf("chooser card = %+v", card.State)
	}
	if n := len(card.State.Sections[0].Items); n != 2 {
		t.Fatalf("expected 2 rows, got %d", n)
	}
	row := card.State.Sections[0].Items[0]
	if row.Action == nil || row.Action.Dispatch == nil || *row.Action.Dispatch != "voice.pick_tie" ||
		!strings.Contains(string(row.Action.Params), row.ID) {
		t.Errorf("a row must press voice.pick_tie with its token, got %+v", row.Action)
	}
	if card.State.Progress != nil {
		t.Error("a held tie has no countdown")
	}
	if !s.IsTieActive() {
		t.Fatal("tie should be active after orchestrate surfaced a tie")
	}
	// plugin.voice.tie_choices put + disambiguating tag put = 2 puts, then 1 commit.
	if got := rec.count("collection.put"); got != 2 {
		t.Errorf("expected 2 collection.put calls (choices + mode tag), got %d (%v)", got, rec.methods())
	}
	if got := rec.count("vocabulary.commit"); got != 1 {
		t.Errorf("expected exactly 1 vocabulary.commit, got %d (%v)", got, rec.methods())
	}

	// The choices map codeword tokens back to owners for the pick re-resolve.
	s.mu.Lock()
	choices := s.tieChoices
	s.mu.Unlock()
	if len(choices) != 2 {
		t.Fatalf("expected 2 stored choices, got %d", len(choices))
	}
	owners := map[string]bool{}
	for _, c := range choices {
		owners[c.Owner] = true
		if c.Codeword == "" || c.Token != c.Codeword {
			t.Errorf("choice has bad codeword/token: %+v", c)
		}
	}
	if !owners["spotify"] || !owners["youtube"] {
		t.Errorf("both tied owners should be stored, got %v", owners)
	}
}

// ResolveTie tears down the transient grammar, re-resolves with prefer_owner
// set to the chosen owner, and RETURNS the winner for the platform to
// execute.
func TestResolveTieReResolvesWithPreferOwnerAndReturnsTheWinner(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{
		resolveResp: &resolveResponse{Matched: true, Action: pluginAction("spotify.play", "")},
	}
	s.Platform = rec

	// Install tie state directly: words "play", two codeword choices.
	choices := buildTieChoices([]tiedCandidate{
		{OwnerPlugin: "spotify", Label: "Play on spotify"},
		{OwnerPlugin: "youtube", Label: "Play on youtube"},
	})
	s.mu.Lock()
	s.tieWords = []string{"play"}
	s.tieChoices = choices
	s.mu.Unlock()

	spotifyToken := choices[0].Token // codeword for spotify
	winner := s.ResolveTie(spotifyToken)

	if s.IsTieActive() {
		t.Fatal("tie should be cleared after a pick")
	}
	// Teardown: delete the transient vocab + a commit.
	if rec.count("collection.delete_records") < 1 {
		t.Errorf("expected the transient choice vocab to be deleted, calls=%v", rec.methods())
	}
	if rec.count("vocabulary.commit") < 1 {
		t.Errorf("expected a vocabulary.commit on teardown, calls=%v", rec.methods())
	}
	// Re-resolve carried prefer_owner = the chosen owner.
	rr, ok := rec.firstParams("commands.resolve").(branchkit.CommandsResolveRequest)
	if !ok {
		t.Fatalf("expected a commands.resolve call, calls=%v", rec.methods())
	}
	if rr.PreferOwner == nil || *rr.PreferOwner != "spotify" {
		t.Errorf("expected prefer_owner=spotify, got %v", rr.PreferOwner)
	}
	if len(rr.Words) != 1 || rr.Words[0] != "play" {
		t.Errorf("re-resolve should replay the original tie words, got %v", rr.Words)
	}
	// The winner comes back to the caller. It is NOT dispatched: the pick
	// arrived as a spoken utterance, so the winner rides the same
	// `on_transcript` response as any other matched command, and the
	// platform executes it.
	if resolvedActionType(winner) != "spotify.play" {
		t.Errorf("expected the re-resolved winner to be returned, got %s", ActionLabel(winner))
	}
	if rec.count("dispatch") != 0 {
		t.Errorf("a tie pick must not dispatch — the platform executes it, calls=%v", rec.methods())
	}
}

// A tie pick needs no trace, because it no longer presents a claim that
// something has to verify.
//
// It used to: the pick was a `dispatch` call from voice to the winning
// plugin, indistinguishable at the interaction gate from voice reaching into
// that plugin on its own initiative, so it echoed the re-resolve's trace to
// prove otherwise. Two tests here asserted the echo was present and that an
// absent trace stayed absent.
//
// Now the winner is returned and the platform runs it, so there is nothing to
// prove and nothing to carry. What replaces those tests is this: no dispatch
// happens at all, whether or not the re-resolve produced a trace.
func TestResolveTieCarriesNoCredentialBecauseItMakesNoClaim(t *testing.T) {
	for _, trace := range []string{"tr-from-the-re-resolve", ""} {
		s := newTestState()
		rec := &tieRecorder{
			resolveResp: &resolveResponse{
				Matched: true,
				Action:  pluginAction("spotify.play", ""),
				TraceID: trace,
			},
		}
		s.Platform = rec

		choices := buildTieChoices([]tiedCandidate{{OwnerPlugin: "spotify"}})
		s.mu.Lock()
		s.tieWords = []string{"play"}
		s.tieChoices = choices
		s.mu.Unlock()

		if winner := s.ResolveTie(choices[0].Token); winner == nil {
			t.Fatalf("trace=%q: expected a winner", trace)
		}
		if rec.count("dispatch") != 0 {
			t.Errorf("trace=%q: nothing should be dispatched, calls=%v", trace, rec.methods())
		}
	}
}

// An unknown pick token (no matching choice) is a safe no-op: nothing to
// run, tie torn down.
func TestResolveTieUnknownTokenReturnsNothing(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{
		resolveResp: &resolveResponse{Matched: true, Action: pluginAction("x", "")},
	}
	s.Platform = rec
	choices := buildTieChoices([]tiedCandidate{{OwnerPlugin: "spotify"}})
	s.mu.Lock()
	s.tieWords = []string{"play"}
	s.tieChoices = choices
	s.mu.Unlock()

	if winner := s.ResolveTie("not-a-real-codeword"); winner != nil {
		t.Errorf("an unknown token must resolve to nothing, got %s", ActionLabel(winner))
	}

	if rec.count("commands.resolve") != 0 {
		t.Errorf("unknown token must not re-resolve, calls=%v", rec.methods())
	}
	if rec.count("dispatch") != 0 {
		t.Errorf("unknown token must not dispatch, calls=%v", rec.methods())
	}
	if s.IsTieActive() {
		t.Error("tie should be cleared even on an unknown pick")
	}
}

// "dismiss" mid-disambiguation backs out: clears the tie and closes the HUD,
// even though the disambiguating gate is a plugin.voice.* tag (excluded from
// the non-voice scoped-tag cancel trigger).
func TestOrchestrateDismissDuringTieClears(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec
	s.callResolveFn = func(words []string, requireTag *string, activeTags []string) *resolveResponse {
		return &resolveResponse{}
	}
	choices := buildTieChoices([]tiedCandidate{{OwnerPlugin: "spotify"}, {OwnerPlugin: "youtube"}})
	s.mu.Lock()
	s.tieWords = []string{"play"}
	s.tieChoices = choices
	s.mu.Unlock()

	resp := s.orchestrate(&OrchestrateRequest{
		Transcript: "dismiss",
		Words:      []string{"dismiss"},
		Mode:       "command_hold",
	})

	if s.IsTieActive() {
		t.Error("dismiss should clear the active tie")
	}
	if resp.Discovery == nil || resp.Discovery.Open == nil || *resp.Discovery.Open {
		t.Errorf("dismiss should close the chooser HUD, got %+v", resp.Discovery)
	}
}

func TestBuildTieChoicesAssignsCodewordsAndAliases(t *testing.T) {
	choices := buildTieChoices([]tiedCandidate{
		{OwnerPlugin: "spotify", Label: "Play music"},
		{OwnerPlugin: "you.tube", Label: ""},
	})
	if len(choices) != 2 {
		t.Fatalf("expected 2 choices, got %d", len(choices))
	}
	if choices[0].Codeword != tieCodewordPool[0] || choices[1].Codeword != tieCodewordPool[1] {
		t.Errorf("codewords should come from the pool in order: %+v", choices)
	}
	if len(choices[0].Aliases) != 1 || choices[0].Aliases[0] != "spotify" {
		t.Errorf("owner name should be a spoken alias, got %v", choices[0].Aliases)
	}
	if choices[1].Label != "you.tube" {
		t.Errorf("empty label should fall back to owner, got %q", choices[1].Label)
	}
	// Non-letter chars in an owner id collapse to spaces in the alias.
	if len(choices[1].Aliases) != 1 || choices[1].Aliases[0] != "you tube" {
		t.Errorf("expected sanitized alias 'you tube', got %v", choices[1].Aliases)
	}
}

func TestSpokenOwnerAlias(t *testing.T) {
	cases := map[string]string{
		"spotify":         "spotify",
		"com.foo.Spotify": "com foo spotify",
		"YouTube":         "youtube",
		"a1b2":            "a b",
		"":                "",
		"123":             "",
	}
	for in, want := range cases {
		if got := spokenOwnerAlias(in); got != want {
			t.Errorf("spokenOwnerAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// A hold released mid-disambiguation must tear the tie down, and the
// end-of-hold path is what calls this.
//
// SessionBoundary clears the disambiguating GATE via the manifest lifecycle,
// but the transient choice vocabulary and the in-memory tie state are voice's
// own. This used to be handled in an orchestrate session-end block that never
// executed — nothing ever set the flag guarding it — so a hold released while
// the chooser was up stranded both until the next tie or an explicit dismiss.
func TestClearTieTearsDownVocabAndState(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec

	choices := buildTieChoices([]tiedCandidate{
		{OwnerPlugin: "spotify", Label: "Play on spotify"},
		{OwnerPlugin: "youtube", Label: "Play on youtube"},
	})
	s.mu.Lock()
	s.tieWords = []string{"play"}
	s.tieChoices = choices
	s.mu.Unlock()

	if !s.IsTieActive() {
		t.Fatal("fixture should have installed an active tie")
	}

	s.ClearTie()

	if s.IsTieActive() {
		t.Error("tie state should be cleared")
	}
	if rec.count("collection.delete_records") < 1 {
		t.Errorf("the transient choice vocab should be deleted, calls=%v", rec.methods())
	}
	if rec.count("vocabulary.commit") < 1 {
		t.Errorf("the recognizer should be refreshed after the vocab drop, calls=%v", rec.methods())
	}
	// Nothing runs: releasing the button is not picking a winner.
	if rec.count("commands.resolve") != 0 || rec.count("dispatch") != 0 {
		t.Errorf("tearing down a tie must not resolve or dispatch, calls=%v", rec.methods())
	}
}

// Idempotent: the end-of-hold path calls this on every release, and the
// overwhelming majority have no tie in flight.
func TestClearTieWithNoTieIsAQuietNoOp(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec

	s.ClearTie()

	if len(rec.methods()) != 0 {
		t.Errorf("no tie means no RPC traffic at all, calls=%v", rec.methods())
	}
}

// commands.resolve reads a nil active_tags as "use the live session tags" and
// a non-nil one — even empty — as an explicit set. The default resolver must
// hand the caller's slice through unchanged so that distinction survives.
func TestCallResolveDefaultPreservesActiveTagsNilVsEmpty(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		wantNil bool
	}{
		{"nil means live tags", nil, true},
		{"empty means no tags", []string{}, false},
		{"explicit set", []string{"plugin.placement.snap_mode"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestState()
			rec := &tieRecorder{}
			s.Platform = rec
			s.callResolveDefault([]string{"x"}, nil, tc.tags)
			rr, ok := rec.firstParams("commands.resolve").(branchkit.CommandsResolveRequest)
			if !ok {
				t.Fatalf("expected a commands.resolve call, calls=%v", rec.methods())
			}
			if (rr.ActiveTags == nil) != tc.wantNil || len(rr.ActiveTags) != len(tc.tags) {
				t.Errorf("active_tags = %#v, want %#v", rr.ActiveTags, tc.tags)
			}
		})
	}
}

// Every way a tie ends takes its card down: dismissing it here.
func TestDismissingATieClearsItsCard(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec
	s.callResolveFn = func(words []string, requireTag *string, activeTags []string) *resolveResponse {
		return tieResolveResp("spotify", "youtube")
	}
	s.orchestrate(&OrchestrateRequest{Words: []string{"play"}, Mode: "command_hold"})
	s.orchestrate(&OrchestrateRequest{Transcript: "dismiss", Words: []string{"dismiss"}, Mode: "command_hold"})
	if rec.count(branchkit.MethodOutputClear) == 0 {
		t.Errorf("dismissing must clear the chooser card, calls=%v", rec.methods())
	}
}

// In continuous mode no hold's end clears a tie: it lapses after the
// countdown, card and all, and the card shows the countdown.
func TestAContinuousTieLapses(t *testing.T) {
	old := tieCountdown
	tieCountdown = 30 * time.Millisecond
	defer func() { tieCountdown = old }()
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec
	s.callResolveFn = func(words []string, requireTag *string, activeTags []string) *resolveResponse {
		return tieResolveResp("spotify", "youtube")
	}
	s.orchestrate(&OrchestrateRequest{Words: []string{"play"}, Mode: "continuous_awake"})
	card, _ := rec.firstParams(branchkit.MethodOutputState).(branchkit.OutputStateRequest)
	if card.State.Progress == nil || card.State.Progress.TotalMs != 30 {
		t.Errorf("a continuous tie shows its countdown, got %+v", card.State.Progress)
	}
	// Wait for the card to come down, the lapse's LAST step: the tie state
	// clears under the lock before the platform calls go out, so waiting on
	// IsTieActive and then counting output.clear raced the clear (1 run in
	// ~250 failed).
	deadline := time.Now().Add(2 * time.Second)
	for rec.count(branchkit.MethodOutputClear) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rec.count(branchkit.MethodOutputClear) == 0 {
		t.Fatalf("a lapsed tie takes its card down, calls=%v", rec.methods())
	}
	if s.IsTieActive() {
		t.Error("the tie should lapse after its countdown")
	}
}

// A press on the chooser card runs voice.pick_tie without the matcher, so
// the disambiguating gate is still up: picking must clear it BEFORE the
// re-resolve, or the re-resolve is suppressed and every command stays gated.
func TestPickingATieClearsTheGateBeforeReResolving(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{
		resolveResp: &resolveResponse{Matched: true, Action: pluginAction("spotify.play", "")},
	}
	s.Platform = rec
	choices := buildTieChoices([]tiedCandidate{{OwnerPlugin: "spotify"}, {OwnerPlugin: "youtube"}})
	s.mu.Lock()
	s.tieWords = []string{"play"}
	s.tieChoices = choices
	s.mu.Unlock()

	if s.ResolveTie(choices[0].Token) == nil {
		t.Fatal("the pick should resolve")
	}
	gateCleared, resolved := -1, -1
	rec.mu.Lock()
	for i, c := range rec.calls {
		if req, ok := c.params.(branchkit.CollectionDeleteRecordsRequest); ok && req.Name == tagDisambiguating && gateCleared < 0 {
			gateCleared = i
		}
		if c.method == branchkit.MethodCommandsResolve && resolved < 0 {
			resolved = i
		}
	}
	rec.mu.Unlock()
	if gateCleared < 0 || resolved < 0 || gateCleared > resolved {
		t.Errorf("the gate must be cleared before the re-resolve (gate at %d, resolve at %d): %v", gateCleared, resolved, rec.methods())
	}
}

// A countdown armed for one tie never clears a newer one.
func TestAnOldCountdownLeavesANewTieAlone(t *testing.T) {
	s := newTestState()
	s.Platform = &tieRecorder{}
	s.mu.Lock()
	s.tieWords = []string{"play"}
	s.tieChoices = buildTieChoices([]tiedCandidate{{OwnerPlugin: "spotify"}})
	s.tieGen = 2
	s.mu.Unlock()
	old := uint64(1)
	s.clearTieIf(&old)
	if !s.IsTieActive() {
		t.Fatal("tie 1's countdown must not clear tie 2")
	}
	current := uint64(2)
	s.clearTieIf(&current)
	if s.IsTieActive() {
		t.Fatal("its own countdown clears it")
	}
}

// Saying a command the platform switched off runs nothing and publishes a
// card saying why, in the platform's words.
func TestOrchestrateBlockedCommandSaysWhy(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{resolveResp: &resolveResponse{Blocked: &branchkit.BlockedCommand{
		Command: "move it",
		Reasons: []string{"Services' 'Update an issue' changed; review the command in Settings"},
		Gates:   []string{"_platform.action_changed:services.update"},
	}}}
	s.Platform = rec
	// Through the real phrase call and its wire mapping, not fakePhrase:
	// the mapping is where `blocked` was once dropped.
	s.callResolvePhraseFn = s.callResolvePhraseDefault

	resp := s.orchestrate(&OrchestrateRequest{Words: []string{"move", "it"}, Mode: "command_hold"})

	if len(resp.Actions) != 0 {
		t.Fatalf("a blocked command must not dispatch, got %d", len(resp.Actions))
	}
	if resp.Stop != "blocked" {
		t.Errorf("stop = %q, want blocked", resp.Stop)
	}
	card, ok := rec.firstParams(branchkit.MethodOutputState).(branchkit.OutputStateRequest)
	if !ok {
		t.Fatalf("expected the reason card, got %v", rec.methods())
	}
	if card.State.Channel != blockedCard || card.State.Kind != branchkit.OutputKindNotice {
		t.Errorf("card = %+v", card.State)
	}
	if !strings.Contains(card.State.Phrase, "move it") || !strings.Contains(card.State.Phrase, "changed") {
		t.Errorf("the spoken phrase must name the command and the reason: %q", card.State.Phrase)
	}
}
