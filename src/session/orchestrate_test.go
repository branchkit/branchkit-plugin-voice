package session

import (
	"encoding/json"
	"github.com/branchkit/branchkit-plugin-voice/hud"
	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/plugin-sdk-go"
	"sync"
	"testing"
)

// These tests cover the pure-function paths in orchestrate() that do NOT
// require HTTP calls to the platform (cancel handling, session-end cleanup).
// Matching-dependent paths are tested via integration tests with a running platform.

func orchReq() *OrchestrateRequest {
	return &OrchestrateRequest{
		Words:      nil,
		ActiveTags: []string{},
	}
}

// --- Test helpers for mocking RPC calls ---

// newTestState is a session on detached instances: a HUD whose renderer
// answers nothing and a correction set with the shipped defaults, both on a
// plugin with no platform behind it.
func newTestState() *State {
	p := branchkit.NewDetachedPlugin()
	h := hud.New(p, func(*hud.RenderHUDRequest) (hud.HudResponse, error) { return hud.HudResponse{}, nil })
	s := New(h, speech.New(p, func() bool { return true }), func() {})
	s.callResolvePhraseFn = func(words []string) *phraseResponse { return fakePhrase(words, s.callResolveFn) }
	return s
}

// fakePhrase stands in for commands.resolve_phrase in these tests by stringing
// the stubbed single resolves together: a tie or a non-match outside a scoped
// mode stops, a non-match inside one is dropped, a match becomes a step. The
// real loop is the platform's phrase resolver and is tested
// there, against the real matcher; what these tests check is what voice does
// with the steps it is handed.
func fakePhrase(words []string, resolve func([]string, *string, []string) *resolveResponse) *phraseResponse {
	out := &phraseResponse{}
	if resolve == nil {
		return out
	}
	for at := 0; at < len(words); {
		resp := resolve(words[at:], nil, nil)
		switch {
		case len(resp.TiedCandidates) > 0:
			out.StoppedAt, out.Stopped = at, resp
			return out
		case resp.Matched:
			n := min(max(resp.ConsumedCount, 1), len(words)-at)
			out.Steps = append(out.Steps, phraseStep{Start: at, Words: words[at : at+n], Resolved: resp})
			at += n
		case len(resp.ScopedTags) > 0 && !resp.HasCompletions:
			out.Dropped = append(out.Dropped, at)
			at++
		default:
			out.StoppedAt, out.Stopped = at, resp
			return out
		}
	}
	return out
}

// cannedPhrase answers every utterance with the same platform result.
func cannedPhrase(s *State, p *phraseResponse) {
	s.callResolvePhraseFn = func([]string) *phraseResponse { return p }
}

// stubRPC replaces the swappable RPC function fields with test stubs. The
// `match` and `completions` fields are convenience shims kept for the
// existing call sites — the platform no longer has separate
// commands.match / commands.completions endpoints. Tests that need the
// unified shape can set `resolve` directly.
func (s *State) stubRPC(stubs rpcStubs) {
	switch {
	case stubs.resolve != nil:
		s.callResolveFn = stubs.resolve
	case stubs.match != nil || stubs.completions != nil:
		matchFn := stubs.match
		compFn := stubs.completions
		s.callResolveFn = func(words []string, requireTag *string, activeTags []string) *resolveResponse {
			resp := &resolveResponse{}
			if matchFn != nil && requireTag == nil {
				if m := matchFn(words, activeTags); m != nil {
					*resp = *m
				}
			}
			if compFn != nil {
				if c := compFn(words, requireTag, activeTags); c != nil {
					resp.HasCompletions = c.HasCompletions
					resp.NextWords = c.NextWords
					resp.Title = c.Title
					resp.Items = c.Items
				}
			}
			return resp
		}
	default:
		s.callResolveFn = func(words []string, requireTag *string, activeTags []string) *resolveResponse {
			return &resolveResponse{}
		}
	}
}

// matchResponse and completionsResponse are test-only aliases of the
// unified resolveResponse: existing call sites use them to construct
// "match-only" or "completions-only" stubs without rewriting every test
// to spell out the unified envelope. Tests that exercise the unified
// surface directly should use `resolveResponse` and `rpcStubs.resolve`.
type matchResponse = resolveResponse
type completionsResponse = resolveResponse

type rpcStubs struct {
	match       func(words, activeTags []string) *matchResponse
	completions func(words []string, requireTag *string, activeTags []string) *completionsResponse
	resolve     func(words []string, requireTag *string, activeTags []string) *resolveResponse
}

// --- Pure function tests ---

func TestIsScopedTag(t *testing.T) {
	tests := []struct {
		tag      string
		prefixes []string
		want     bool
	}{
		{"plugin.browser.hints.active", []string{"plugin.browser.hints."}, true},
		{"plugin.browser.hints.active", []string{"plugin.wm."}, false},
		{"plugin.wm.resize_mode", []string{"plugin.browser.hints.", "plugin.wm."}, true},
		{"app.chrome", []string{"plugin.browser.hints."}, false},
		{"plugin.browser.hints.active", nil, false},
		{"plugin.browser.hints.active", []string{}, false},
		{"", []string{"plugin."}, false},
	}
	for _, tt := range tests {
		got := isScopedTag(tt.tag, tt.prefixes)
		if got != tt.want {
			t.Errorf("isScopedTag(%q, %v) = %v, want %v", tt.tag, tt.prefixes, got, tt.want)
		}
	}
}

func TestFindPluginModeTag(t *testing.T) {
	scopedPrefixes := []string{"plugin.wm.", "plugin.browser.hints.pair.", "plugin.voice.mode."}
	tests := []struct {
		name       string
		scopedTags []string
		setTags    []string
		want       *string
	}{
		{
			name:    "sets scoped plugin.wm tag",
			setTags: []string{"plugin.wm.resize_mode"},
			want:    strPtr("plugin.wm.resize_mode"),
		},
		{
			name:    "ignores non-scoped plugin tag (ambient status flag)",
			setTags: []string{"plugin.browser.hints"},
			want:    nil,
		},
		{
			name:       "scoped tag triggers mode (already in mode)",
			scopedTags: []string{"plugin.wm.resize_mode"},
			want:       strPtr("plugin.wm.resize_mode"),
		},
		{
			name:       "ignores plugin.voice scoped tags",
			scopedTags: []string{"plugin.voice.mode.alphabet"},
			want:       nil,
		},
		{
			name:       "ignores plain plugin.voice scoped tag",
			scopedTags: []string{"plugin.voice.group.something"},
			want:       nil,
		},
		{
			name:       "no plugin tags",
			scopedTags: []string{"app.chrome"},
			setTags:    []string{"app.safari"},
			want:       nil,
		},
		{
			name: "empty tags",
			want: nil,
		},
		{
			name:       "setTags wins over scopedTags (new mode entry takes priority)",
			scopedTags: []string{"plugin.wm.resize_mode"},
			setTags:    []string{"plugin.browser.hints.pair.bake"},
			want:       strPtr("plugin.browser.hints.pair.bake"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findPluginModeTag(tt.scopedTags, tt.setTags, scopedPrefixes)
			if tt.want == nil && got != nil {
				t.Errorf("want nil, got %q", *got)
			} else if tt.want != nil && got == nil {
				t.Errorf("want %q, got nil", *tt.want)
			} else if tt.want != nil && *got != *tt.want {
				t.Errorf("want %q, got %q", *tt.want, *got)
			}
		})
	}
}

func TestTagsToSlice(t *testing.T) {
	tags := map[string]bool{"a": true, "b": true, "c": true}
	got := tagsToSlice(tags)
	if len(got) != 3 {
		t.Fatalf("expected 3 tags, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, tag := range got {
		seen[tag] = true
	}
	for _, expected := range []string{"a", "b", "c"} {
		if !seen[expected] {
			t.Errorf("missing tag %q", expected)
		}
	}
}

func TestTagsToSliceEmpty(t *testing.T) {
	got := tagsToSlice(map[string]bool{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestBuildEmptyDiscoveryMutation(t *testing.T) {
	t.Run("with words", func(t *testing.T) {
		d := buildEmptyDiscoveryMutation([]string{"snap"}, nil, true)
		if d.Title == nil || *d.Title != "SNAP" {
			t.Errorf("expected title SNAP, got %v", d.Title)
		}
		if d.CountdownSeconds == nil {
			t.Fatal("expected countdown with countdown=true")
		}
		// The "nothing matched" state is the document's phrase now — the
		// body of a `choices` with no sections — not a footer line.
		if d.Phrase == nil || *d.Phrase != "No matching commands" {
			t.Error("expected 'No matching commands' phrase")
		}
	})

	t.Run("with require tag", func(t *testing.T) {
		d := buildEmptyDiscoveryMutation(nil, strPtr("plugin.wm.resize_mode"), false)
		// Identifier separators become spaces — the panel must never show a
		// raw variable-style name like "RESIZE_MODE".
		if d.Title == nil || *d.Title != "RESIZE MODE" {
			t.Errorf("expected humanized title from tag suffix, got %v", d.Title)
		}
		if d.CountdownSeconds != nil {
			t.Error("should not have countdown with countdown=false")
		}
	})

	t.Run("no words no tag", func(t *testing.T) {
		d := buildEmptyDiscoveryMutation(nil, nil, false)
		if d.Title == nil || *d.Title != "COMMANDS" {
			t.Errorf("expected default title COMMANDS, got %v", d.Title)
		}
	})
}

// --- matchWords tests (with mocked RPC) ---

func TestMatchWordsNoWords(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := s.matchWords(nil)
	if result.matched {
		t.Fatal("should not match with no words")
	}
	if result.partial {
		t.Fatal("should not be partial with no words")
	}
}

// pluginAction builds a RESOLVED plugin action the way `commands.resolve`
// returns one. Since 2026-09-19 the response is typed (`branchkit.Action`),
// so a test can no longer hand the predicates the flat AUTHORING shape
// (`{"type":"voice.repeat"}`) that they never see in production — the trap
// TestIsRepeatAction below was written to catch, now closed by the type.
func pluginAction(actionType string, params string) *branchkit.Action {
	a := &branchkit.Action{Type: branchkit.ActionKindPlugin, ActionType: &actionType}
	if params != "" {
		a.Params = json.RawMessage(params)
	}
	return a
}

// opaqueAction is a stand-in for "some action the matcher returned" in tests
// that only care THAT one came back, not what it was.
func opaqueAction(name string) *branchkit.Action {
	return pluginAction("test."+name, "")
}

func TestMatchWordsSingleMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "undo" {
				return &matchResponse{
					Matched:       true,
					Action:        opaqueAction("shortcut"),
					ConsumedCount: 1,
				}
			}
			return &matchResponse{}
		},
	})

	result := s.matchWords([]string{"undo"})
	if !result.matched {
		t.Fatal("expected match")
	}
	if len(result.actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.actions))
	}
	if len(result.events) != 1 || result.events[0].Type != "match" {
		t.Fatal("expected match event")
	}
}

func TestMatchWordsSetsTags(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "resize" {
				return &matchResponse{
					Matched:       true,
					Action:        opaqueAction("resize_start"),
					ConsumedCount: 1,
					SetsTags:      []string{"plugin.wm.resize_mode"},
				}
			}
			return &matchResponse{}
		},
	})

	result := s.matchWords([]string{"resize"})
	if !result.matched {
		t.Fatal("expected match")
	}
	if len(result.setTags) != 1 || result.setTags[0] != "plugin.wm.resize_mode" {
		t.Errorf("expected setTags=[plugin.wm.resize_mode], got %v", result.setTags)
	}
}

func TestMatchWordsClearsTags(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "done" {
				return &matchResponse{
					Matched:       true,
					Action:        opaqueAction("exit_mode"),
					ConsumedCount: 1,
					ClearsTags:    []string{"plugin.wm.resize_mode"},
				}
			}
			return &matchResponse{}
		},
	})

	result := s.matchWords([]string{"done"})
	if !result.matched {
		t.Fatal("expected match")
	}
	if len(result.clearTags) != 1 || result.clearTags[0] != "plugin.wm.resize_mode" {
		t.Errorf("expected clearTags=[plugin.wm.resize_mode], got %v", result.clearTags)
	}
}

// Sub-sprint 6.4 — the inline auto-clear branch in matchWords moved to the
// platform's commands.match resolution. The platform-side coverage lives in
// `services::matching_service::auto_clear_tests`. Voice now trusts what the
// matchResponse carries, so the previous TestMatchWordsAutoCleansScopedTagsOnNonScopedMatch
// is no longer testing voice's behavior and was deleted.

func TestMatchWordsPreservesScopedTagOnScopedMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "go" {
				return &matchResponse{
					Matched:        true,
					Action:         opaqueAction("go_hint"),
					ConsumedCount:  1,
					SetsTags:       []string{"plugin.browser.hints.go"},
					ScopedPrefixes: []string{"plugin.browser.hints."},
				}
			}
			return &matchResponse{ScopedPrefixes: []string{"plugin.browser.hints."}}
		},
	})

	result := s.matchWords([]string{"go"})

	// Match sets a scoped tag, so existing scoped tags should NOT be auto-cleared
	for _, ct := range result.clearTags {
		if ct == "plugin.browser.hints.active" {
			t.Error("should not clear existing scoped tag when match sets a new scoped tag")
		}
	}
	if result.newScopedTag == nil || *result.newScopedTag != "plugin.browser.hints.go" {
		t.Errorf("expected newScopedTag=plugin.browser.hints.go, got %v", result.newScopedTag)
	}
}

// Words the platform dropped as silence inside a scoped mode count toward
// scopeDropped, which keeps them from closing discovery.
func TestMatchWordsCountsWordsThePlatformDropped(t *testing.T) {
	s := newTestState()
	cannedPhrase(s, &phraseResponse{Dropped: []int{0}})

	result := s.matchWords([]string{"undo"})

	if result.matched {
		t.Fatal("unrelated word in scoped mode should be dropped, not matched")
	}
	if result.scopeDropped != 1 {
		t.Fatalf("expected 1 scope-dropped word, got %d", result.scopeDropped)
	}
}

// Every step the platform resolved is acted on, in order: "desk five" is the
// mode entry and then the command it gates, and both reach the response.
func TestMatchWordsActsOnEveryStep(t *testing.T) {
	s := newTestState()
	prefixes := []string{"plugin.placement.desk_mode"}
	cannedPhrase(s, &phraseResponse{Steps: []phraseStep{
		{Start: 0, Words: []string{"desk"}, Resolved: &resolveResponse{
			Matched: true, Action: opaqueAction("desk_mode"), ConsumedCount: 1,
			SetsTags: prefixes, ScopedPrefixes: prefixes,
		}},
		{Start: 1, Words: []string{"five"}, Resolved: &resolveResponse{
			Matched: true, Action: opaqueAction("desk_switch"), ConsumedCount: 1,
			SetsTags: prefixes, RequiresTags: prefixes, ScopedPrefixes: prefixes,
		}},
	}})

	result := s.matchWords([]string{"desk", "five"})

	if got := ActionLabels(result.actions); len(result.actions) != 2 {
		t.Fatalf("expected desk_mode then desk_switch, got %s", got)
	}
	if len(result.events) != 2 || result.events[1].Data.(map[string]interface{})["phrase"] != "five" {
		t.Fatalf("expected one match event per step, the second for \"five\": %+v", result.events)
	}
	if want := []string{"desk", "five"}; len(result.outcomes) != 2 || result.outcomes[1] != want[1] {
		t.Fatalf("outcomes = %v, want %v", result.outcomes, want)
	}
}

// A tie after a step stops there: the tie carries the words it was over.
func TestMatchWordsTieCarriesTheWordsItWasOver(t *testing.T) {
	s := newTestState()
	cannedPhrase(s, &phraseResponse{
		Steps: []phraseStep{{Start: 0, Words: []string{"undo"}, Resolved: &resolveResponse{
			Matched: true, Action: opaqueAction("undo"), ConsumedCount: 1,
		}}},
		StoppedAt: 1,
		Stopped:   tieResolveResp("browser", "keyboard"),
	})

	result := s.matchWords([]string{"undo", "play"})

	if result.tie == nil || len(result.tie.words) != 1 || result.tie.words[0] != "play" {
		t.Fatalf("expected a tie over [play], got %+v", result.tie)
	}
	if len(result.tie.candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(result.tie.candidates))
	}
}

func TestMatchWordsPartialMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		completions: func(words []string, requireTag *string, activeTags []string) *completionsResponse {
			if len(words) > 0 && words[0] == "snap" {
				return &completionsResponse{
					HasCompletions: true,
					NextWords:      []string{"left", "right"},
					Title:          "SNAP",
					Items:          []discoverItem{{ID: "left", Title: "left", Tag: "Next"}},
				}
			}
			return &completionsResponse{}
		},
	})

	result := s.matchWords([]string{"snap"})
	if result.matched {
		t.Fatal("should not be a full match")
	}
	if !result.partial {
		t.Fatal("expected partial match")
	}
	if result.completions == nil || !result.completions.HasCompletions {
		t.Error("expected completions with has_completions=true")
	}
}

// A doubled-word codeword ("bat bat") captures the prefix on the first word,
// leaving a server-side bridge awaiting the suffix. The suffix repeats the
// prefix word verbatim, and must still reach orchestrate — otherwise the
// bridge never completes. Regression for the "say the second bat" miss.
func TestOrchestrateRepeatsDoubledWordCodeword(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		completions: func(words []string, requireTag *string, activeTags []string) *completionsResponse {
			if len(words) > 0 && words[0] == "bat" {
				return &completionsResponse{
					HasCompletions: true,
					NextWords:      []string{"bat"},
					Title:          "BAT",
					Items:          []discoverItem{{ID: "bat", Title: "bat", Tag: "Next"}},
				}
			}
			return &completionsResponse{}
		},
	})

	mkReq := func() *OrchestrateRequest {
		return &OrchestrateRequest{
			Transcript: "bat",
			Words:      []string{"bat"},
			ActiveTags: []string{},
		}
	}

	resp1 := s.OrchestrateInternal(mkReq())
	if resp1.Completions == nil {
		t.Fatal("first \"bat\" should produce a partial (completions set)")
	}

	resp2 := s.OrchestrateInternal(mkReq())
	if resp2.Result == "pass" {
		t.Fatal("second \"bat\" was skipped; the doubled-word codeword can never complete")
	}
	if resp2.Completions == nil {
		t.Error("second \"bat\" should re-enter orchestrate")
	}
}

// A retry of a missed command must be processed again, not skipped as a
// duplicate: the first attempt can miss because its context (app focus, a
// loaded list) had not settled yet.
func TestOrchestrateProcessesARepeatedMiss(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	mkReq := func() *OrchestrateRequest {
		return &OrchestrateRequest{
			Transcript: "xyzzy",
			Words:      []string{"xyzzy"},
			ActiveTags: []string{},
		}
	}

	_ = s.OrchestrateInternal(mkReq())
	resp2 := s.OrchestrateInternal(mkReq())
	if resp2.Result == "pass" {
		t.Fatalf("a repeated miss was skipped; got Result=%q", resp2.Result)
	}
}

func TestMatchWordsDetectsPluginModeTag(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "resize" {
				return &matchResponse{
					Matched:        true,
					ConsumedCount:  1,
					SetsTags:       []string{"plugin.wm.resize_mode"},
					ScopedPrefixes: []string{"plugin.wm."},
				}
			}
			return &matchResponse{ScopedPrefixes: []string{"plugin.wm."}}
		},
	})

	result := s.matchWords([]string{"resize"})
	if result.pluginModeTag == nil || *result.pluginModeTag != "plugin.wm.resize_mode" {
		t.Errorf("expected pluginModeTag=plugin.wm.resize_mode, got %v", result.pluginModeTag)
	}
}

// --- decideDiscovery tests ---

func TestDecideDiscoveryMatchedPlainClosesDiscovery(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{matched: true}
	d := s.decideDiscovery(result, true, true, "command_hold")

	if d.mutation == nil {
		t.Fatal("expected mutation to close discovery")
	}
	if d.mutation.Open == nil || *d.mutation.Open != false {
		t.Fatal("expected discovery close")
	}
}

func TestDecideDiscoveryMatchedPlainNoopWhenClosed(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{matched: true}
	d := s.decideDiscovery(result, false, true, "command_hold")

	if d.mutation != nil {
		t.Error("expected no mutation when discovery already closed")
	}
}

func TestDecideDiscoveryMatchedWithScopedTagOpensDiscovery(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	scopedTag := "plugin.browser.hints.go"
	result := matchResult{matched: true, newScopedTag: &scopedTag}
	d := s.decideDiscovery(result, false, true, "command_hold")

	if d.mutation == nil {
		t.Fatal("expected discovery mutation")
	}
	if d.mutation.Open == nil || *d.mutation.Open != true {
		t.Fatal("expected discovery to open")
	}
}

// A select list is drawn by the platform as a palette card: its mode entry
// and a partial inside it (a drill's category word) leave Discovery closed,
// and close it if it was open.
func TestDecideDiscoveryLeavesSelectListsToThePlatform(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})
	s.callResolveFn = func([]string, *string, []string) *resolveResponse {
		return &resolveResponse{HasCompletions: true, DrawnByPlatform: true, Items: []discoverItem{{ID: "ape", Title: "ape"}}}
	}
	tag := "_discovery.snippets.snippet"
	d := s.decideDiscovery(matchResult{matched: true, newScopedTag: &tag}, false, true, "command_hold")
	if d.isOpen || d.mutation != nil {
		t.Fatalf("mode entry: Discovery should stay closed, got %+v", d)
	}
	partial := matchResult{
		partial:     true,
		completions: &resolveResponse{HasCompletions: true, DrawnByPlatform: true, Items: []discoverItem{{ID: "bay", Title: "bay"}}},
	}
	d = s.decideDiscovery(partial, true, true, "command_hold")
	if d.isOpen || d.mutation == nil || d.mutation.Open == nil || *d.mutation.Open {
		t.Fatalf("partial in a select list: Discovery should close, got %+v", d)
	}
}

func TestDecideDiscoveryMatchedWithPluginModeTag(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	modeTag := "plugin.wm.resize_mode"
	result := matchResult{matched: true, pluginModeTag: &modeTag}
	// Hold-to-talk terminates on key release; no countdown safety net needed.
	d := s.decideDiscovery(result, false, true, "command_hold")

	if d.mutation == nil {
		t.Fatal("expected discovery mutation")
	}
	if d.mutation.Open == nil || *d.mutation.Open != true {
		t.Fatal("expected discovery to open for mode tag")
	}
	if d.mutation.Countdown != nil && *d.mutation.Countdown {
		t.Error("expected no countdown in command_hold mode (key release terminates session)")
	}
}

func TestDecideDiscoveryMatchedWithPluginModeTagContinuous(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	modeTag := "plugin.wm.resize_mode"
	result := matchResult{matched: true, pluginModeTag: &modeTag}
	// Continuous awake has no natural terminator; countdown is the safety net.
	d := s.decideDiscovery(result, false, true, "continuous_awake")

	if d.mutation == nil {
		t.Fatal("expected discovery mutation")
	}
	if d.mutation.Countdown == nil || !*d.mutation.Countdown {
		t.Error("expected countdown for continuous_awake mode")
	}
}

func TestDecideDiscoveryPartialOpensDiscovery(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{partial: true, partialWords: []string{"snap"}}
	d := s.decideDiscovery(result, false, true, "command_hold")

	if d.mutation == nil {
		t.Fatal("expected discovery mutation for partial")
	}
	if d.mutation.Open == nil || *d.mutation.Open != true {
		t.Fatal("expected discovery to open")
	}
}

func TestDecideDiscoveryPartialKeepsOpenWhenAlreadyOpen(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{partial: true, partialWords: []string{"snap"}}
	d := s.decideDiscovery(result, true, true, "command_hold")

	// Discovery already open — should update mutation but keep isOpen
	if !d.isOpen {
		t.Error("discovery should remain open")
	}
}

func TestDecideDiscoveryPartialWithCompletions(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{
		partial:      true,
		partialWords: []string{"snap"},
		completions: &completionsResponse{
			HasCompletions: true,
			Title:          "SNAP",
			Items:          []discoverItem{{ID: "left", Title: "left", Tag: "Next"}, {ID: "right", Title: "right", Tag: "Next"}},
		},
	}
	d := s.decideDiscovery(result, false, true, "command_hold")

	if d.mutation == nil {
		t.Fatal("expected discovery mutation for partial with completions")
	}
	if d.mutation.Open == nil || *d.mutation.Open != true {
		t.Fatal("expected discovery to open")
	}
	// Hold-to-talk: no countdown — session ends on key release.
	if d.mutation.CountdownSeconds != nil {
		t.Errorf("expected no countdown seconds in command_hold mode, got %v", *d.mutation.CountdownSeconds)
	}
	if d.mutation.Countdown != nil && *d.mutation.Countdown {
		t.Error("expected countdown flag false in command_hold mode")
	}
	if d.mutation.Footer == nil || *d.mutation.Footer != "" {
		t.Errorf("expected empty footer, got %v", d.mutation.Footer)
	}
}

func TestDecideDiscoveryPartialWithCompletionsContinuous(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{
		partial:      true,
		partialWords: []string{"snap"},
		completions: &completionsResponse{
			HasCompletions: true,
			Title:          "SNAP",
			Items:          []discoverItem{{ID: "left", Title: "left", Tag: "Next"}, {ID: "right", Title: "right", Tag: "Next"}},
		},
	}
	d := s.decideDiscovery(result, false, true, "continuous_awake")

	if d.mutation == nil {
		t.Fatal("expected discovery mutation for partial with completions")
	}
	// Continuous awake: countdown is the only termination mechanism.
	if d.mutation.CountdownSeconds == nil || *d.mutation.CountdownSeconds != 8 {
		t.Errorf("expected 8s countdown in continuous_awake mode, got %v", d.mutation.CountdownSeconds)
	}
}

func TestDecideDiscoveryNoMatchClosesWhenWordsSpoken(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{}
	d := s.decideDiscovery(result, true, true, "command_hold")

	if d.mutation == nil {
		t.Fatal("expected mutation to close discovery")
	}
	if d.mutation.Open == nil || *d.mutation.Open != false {
		t.Fatal("expected discovery close on no match with words")
	}
}

func TestDecideDiscoveryNoMatchKeepsOpenWithoutWords(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{}
	d := s.decideDiscovery(result, true, false, "command_hold")

	// Empty words (session end with no speech) should not close discovery
	if d.mutation != nil {
		t.Error("should not close discovery without spoken words")
	}
	if !d.isOpen {
		t.Error("discovery should remain open")
	}
}

func TestDecideDiscoveryNoMatchAlreadyClosed(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	result := matchResult{}
	d := s.decideDiscovery(result, false, true, "command_hold")

	if d.mutation != nil {
		t.Error("expected no mutation when already closed")
	}
}

// --- orchestrate integration tests (with mocked RPC) ---

// "dismiss" inside a plugin mode is the matcher's business: the platform's
// escape clears the mode's tag inside the resolve, and voice reports the
// clear without treating the escape as a command.
func TestOrchestrateDismissInAModeClearsItThroughTheMatcher(t *testing.T) {
	s := newTestState()
	var resolved [][]string
	s.stubRPC(rpcStubs{
		resolve: func(words []string, requireTag *string, activeTags []string) *resolveResponse {
			resolved = append(resolved, words)
			return &resolveResponse{
				Matched:       true,
				Action:        pluginAction("_escape", `{}`),
				ConsumedCount: 1,
				ClearsTags:    []string{"plugin.tiling.snap_mode"},
				ScopedTags:    []string{"plugin.tiling.snap_mode"},
			}
		},
	})

	req := orchReq()
	req.Transcript = "dismiss"
	req.Words = []string{"dismiss"}
	req.ActiveTags = []string{"plugin.tiling.snap_mode"}

	resp := s.orchestrate(req)
	if len(resolved) != 1 {
		t.Fatalf("\"dismiss\" must reach the matcher, resolves=%v", resolved)
	}
	if resp.TagChanges == nil || len(resp.TagChanges.Clear) != 1 {
		t.Fatalf("the mode clears through the resolve, got %+v", resp.TagChanges)
	}
	if len(resp.Actions) != 0 || resp.Outcome != nil {
		t.Errorf("the escape is neither dispatched nor spoken, got actions=%s outcome=%v", ActionLabels(resp.Actions), resp.Outcome)
	}
}

// With no tie open there is no shortcut at all: "dismiss" is a word for the
// matcher like any other, whether or not a mode is active.
func TestOrchestrateDismissWithoutATieReachesTheMatcher(t *testing.T) {
	s := newTestState()
	var resolved [][]string
	s.stubRPC(rpcStubs{
		resolve: func(words []string, requireTag *string, activeTags []string) *resolveResponse {
			resolved = append(resolved, words)
			return &resolveResponse{}
		},
	})

	req := orchReq()
	req.Transcript = "dismiss"
	req.Words = []string{"dismiss"}
	req.ActiveTags = []string{}

	resp := s.orchestrate(req)
	if len(resolved) != 1 || resolved[0][0] != "dismiss" {
		t.Fatalf("\"dismiss\" must reach the matcher, resolves=%v", resolved)
	}
	if resp.ResetEngine {
		t.Error("an unmatched word does not reset the engine")
	}
}

func TestOrchestrateNoWordsNoMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	req := orchReq()
	resp := s.orchestrate(req)
	if resp.Result != "handled" {
		t.Fatalf("expected handled, got %s", resp.Result)
	}
	if resp.ResetEngine {
		t.Fatal("should not reset engine with no words")
	}
}

func TestOrchestrateMatchExecutesActions(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "undo" {
				return &matchResponse{
					Matched:       true,
					Action:        opaqueAction("shortcut"),
					ConsumedCount: 1,
				}
			}
			return &matchResponse{}
		},
	})

	req := orchReq()
	req.Words = []string{"undo"}
	resp := s.orchestrate(req)

	if len(resp.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(resp.Actions))
	}
	if !resp.ResetEngine {
		t.Fatal("expected reset engine on match")
	}
}

func TestOrchestrateMatchWithTagChanges(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "resize" {
				return &matchResponse{
					Matched:       true,
					ConsumedCount: 1,
					SetsTags:      []string{"plugin.wm.resize_mode"},
				}
			}
			return &matchResponse{}
		},
	})

	req := orchReq()
	req.Words = []string{"resize"}
	resp := s.orchestrate(req)

	if resp.TagChanges == nil {
		t.Fatal("expected tag changes")
	}
	if len(resp.TagChanges.Set) != 1 || resp.TagChanges.Set[0] != "plugin.wm.resize_mode" {
		t.Errorf("expected set=[plugin.wm.resize_mode], got %v", resp.TagChanges.Set)
	}
}

func TestOrchestrateMissEventOnNoMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	req := orchReq()
	req.Words = []string{"gibberish", "nonsense"}
	resp := s.orchestrate(req)

	if len(resp.Events) != 1 || resp.Events[0].Type != "miss" {
		t.Errorf("expected miss event, got %v", resp.Events)
	}
}

// Stop says why resolving ended early, for voice's History record:
// independent of whether earlier steps produced actions.
func TestOrchestrateReportsWhyItStopped(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})
	req := orchReq()
	req.Words = []string{"gibberish", "nonsense"}
	if resp := s.orchestrate(req); resp.Stop != "miss" || len(resp.Unresolved) != 2 {
		t.Errorf("no match: Stop=%q Unresolved=%v", resp.Stop, resp.Unresolved)
	}

	s = newTestState()
	cannedPhrase(s, &phraseResponse{
		Steps: []phraseStep{{Start: 0, Words: []string{"undo"}, Resolved: &resolveResponse{
			Matched: true, Action: opaqueAction("undo"), ConsumedCount: 1,
		}}},
		StoppedAt: 1,
		Stopped:   &resolveResponse{},
	})
	req = orchReq()
	req.Words = []string{"undo", "blah"}
	resp := s.orchestrate(req)
	if len(resp.Actions) != 1 || resp.Stop != "miss" || len(resp.Unresolved) != 1 || resp.Unresolved[0] != "blah" {
		t.Errorf("ran undo then missed blah: actions=%d Stop=%q Unresolved=%v", len(resp.Actions), resp.Stop, resp.Unresolved)
	}

	s = newTestState()
	cannedPhrase(s, &phraseResponse{StoppedAt: 0, Stopped: tieResolveResp("browser", "keyboard")})
	req = orchReq()
	req.Words = []string{"play"}
	if resp := s.orchestrate(req); resp.Stop != "tie" {
		t.Errorf("tie: Stop=%q", resp.Stop)
	}

	s = newTestState()
	cannedPhrase(s, &phraseResponse{Steps: []phraseStep{{Start: 0, Words: []string{"undo"}, Resolved: &resolveResponse{
		Matched: true, Action: opaqueAction("undo"), ConsumedCount: 1,
	}}}})
	req = orchReq()
	req.Words = []string{"undo"}
	if resp := s.orchestrate(req); resp.Stop != "" || len(resp.Unresolved) != 0 {
		t.Errorf("every word used: Stop=%q Unresolved=%v", resp.Stop, resp.Unresolved)
	}
}

// --- repeated utterances ---

// The same command twice in a row fires twice ("right" "right" rapid-fire):
// each segment is independent audio.
func TestOrchestrateRepeatedMatchFiresAgain(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) == 1 && words[0] == "right" {
				return &matchResponse{
					Matched:       true,
					Action:        opaqueAction("snap_right"),
					ConsumedCount: 1,
				}
			}
			return &matchResponse{}
		},
	})

	for i := 0; i < 2; i++ {
		req := orchReq()
		req.Transcript = "right"
		req.Words = []string{"right"}
		if resp := s.OrchestrateInternal(req); len(resp.Actions) == 0 {
			t.Fatalf("'right' #%d did not fire", i+1)
		}
	}
}

// --- Concurrency safety test ---

func TestOrchestrateConcurrent(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			req := orchReq()
			req.Transcript = "test"
			if n%2 == 0 {
				req.Mode = "command_hold"
			}
			s.OrchestrateInternal(req)
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Discovery HUD visibility invariant tests
//
// All discovery open/close goes through openDiscovery()/closeDiscovery() →
// signalShell() → hud.show/hud.hide RPC. The orchestrate response must NEVER
// contain discovery-related ControlMessage strings — that path was dead code
// (Swift shell reads "shell_action", not "control_message" from speech
// responses) and caused double-fire when pipeline_full applied the mutation
// locally AND returned the signal in the response.
// ---------------------------------------------------------------------------

// The one cancel shortcut left is the tie: it closes the chooser through the
// discovery mutation, never through a control message.
func TestOrchestrateNoDiscoveryControlMessage_Cancel(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec
	s.stubRPC(rpcStubs{})
	s.mu.Lock()
	s.tieWords = []string{"play"}
	s.tieChoices = buildTieChoices([]tiedCandidate{{OwnerPlugin: "spotify"}, {OwnerPlugin: "youtube"}})
	s.mu.Unlock()

	req := orchReq()
	req.Transcript = "dismiss"
	req.Words = []string{"dismiss"}
	resp := s.orchestrate(req)
	if resp.Discovery == nil || resp.Discovery.Open == nil || *resp.Discovery.Open {
		t.Fatal("dismiss: expected Discovery.Open=false")
	}
}

func TestOrchestrateNoDiscoveryControlMessage_PlainMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "undo" {
				return &matchResponse{Matched: true, ConsumedCount: 1}
			}
			return &matchResponse{}
		},
	})

	// Discovery open → plain match should close via mutation, not ControlMessage
	req := orchReq()
	req.Words = []string{"undo"}
	req.Transcript = "undo"
	_ = s.orchestrate(req)
}

func TestOrchestrateNoDiscoveryControlMessage_ScopedTag(t *testing.T) {
	s := newTestState()
	scopedTag := "plugin.browser.hints.go"
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			return &matchResponse{
				Matched:       true,
				ConsumedCount: 1,
				SetsTags:      []string{scopedTag},
			}
		},
	})

	req := orchReq()
	req.Words = []string{"go"}
	req.Transcript = "go"
	_ = s.orchestrate(req)
}

func TestOrchestrateNoDiscoveryControlMessage_PartialMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		completions: func(words []string, requireTag *string, activeTags []string) *completionsResponse {
			return &completionsResponse{HasCompletions: true, Title: "SNAP", Items: []discoverItem{{ID: "test", Title: "test", Tag: "Next"}}}
		},
	})

	req := orchReq()
	req.Words = []string{"snap"}
	req.Transcript = "snap"
	_ = s.orchestrate(req)
}

func TestDecideDiscoveryNeverProducesControlMessages(t *testing.T) {
	// decideDiscovery is a pure function — it should only produce mutations,
	// never control message strings. Verify across all code paths.
	s := newTestState()
	s.stubRPC(rpcStubs{})

	cases := []struct {
		name            string
		result          matchResult
		isDiscoveryOpen bool
		hasWords        bool
	}{
		{"plain match closes discovery", matchResult{matched: true}, true, true},
		{"plain match noop when closed", matchResult{matched: true}, false, true},
		{"scoped tag opens discovery", matchResult{matched: true, newScopedTag: strPtr("plugin.browser.hints.go")}, false, true},
		{"plugin mode opens discovery", matchResult{matched: true, pluginModeTag: strPtr("plugin.wm.resize_mode")}, false, true},
		{"partial opens discovery", matchResult{partial: true, partialWords: []string{"snap"}}, false, true},
		{"partial updates discovery", matchResult{partial: true, partialWords: []string{"snap"}}, true, true},
		{"no match closes discovery", matchResult{}, true, true},
		{"no match noop when closed", matchResult{}, false, true},
		{"no words keeps discovery", matchResult{}, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := s.decideDiscovery(tc.result, tc.isDiscoveryOpen, tc.hasWords, "command_hold")
			// Verify decideDiscovery only produces mutations + state flags,
			// never control messages (those go through hud.show/hud.hide RPC).
			if d.mutation != nil && d.mutation.Open != nil {
				// Mutation should only set Open true/false — no side-channel signals
				if d.mutation.Open == nil {
					t.Error("mutation.Open should not be nil when mutation is set")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Dependency injection and initialization tests
// ---------------------------------------------------------------------------

func TestInitRPCSetsDefaults(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{}
	s.Platform = rec
	s.InitRPC()

	if s.callResolveFn == nil {
		t.Fatal("callResolveFn not set")
	}
	// Verify defaults actually route through Platform
	s.callResolveFn([]string{"test"}, nil, nil)
	if n := rec.count("commands.resolve"); n != 1 {
		t.Fatalf("expected 1 commands.resolve invocation, got %d", n)
	}
}

func TestDefaultRPCFunctionsSafeWithNilPlatform(t *testing.T) {
	s := newTestState()
	s.InitRPC()

	// Must not panic even though Platform is nil.
	resp := s.callResolveFn([]string{"test"}, nil, nil)
	if resp == nil || resp.Matched {
		t.Error("expected empty non-nil resolveResponse from callResolveDefault with nil Platform")
	}
}

// --- LastMatch / repeat primitive tests ---

func TestLastMatchCachedAfterSuccessfulMatch(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && words[0] == "undo" {
				return &matchResponse{
					Matched:       true,
					Action:        pluginAction("input.shortcut", `{"name":"z","modifiers":["cmd"]}`),
					ConsumedCount: 1,
				}
			}
			return &matchResponse{}
		},
	})

	s.matchWords([]string{"undo"})

	lm := s.GetLastMatch()
	if lm == nil {
		t.Fatal("expected lastMatch to be cached after successful match")
	}
	if resolvedActionType(lm.Action) != "input.shortcut" {
		t.Fatalf("unexpected cached action: %s", ActionLabel(lm.Action))
	}
	if string(lm.Action.Params) != `{"name":"z","modifiers":["cmd"]}` {
		t.Fatalf("unexpected cached params: %s", string(lm.Action.Params))
	}
}

// The resolved shape is load-bearing here — `{"type":"plugin",
// "action_type":"voice.repeat"}`, not the flat authoring shape. Stubbing
// the flat one made this test pass against a predicate that never fired.
func repeatAction(params string) *branchkit.Action {
	return pluginAction("voice.repeat", params)
}

func stubRepeatMatch(s *State, params string) {
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			if len(words) > 0 && (words[0] == "again" || words[0] == "repeat") {
				return &matchResponse{
					Matched:       true,
					Action:        repeatAction(params),
					ConsumedCount: 1,
				}
			}
			return &matchResponse{}
		},
	})
}

func TestLastMatchNotUpdatedForRepeatAction(t *testing.T) {
	s := newTestState()
	originalAction := pluginAction("input.scroll", `{"direction":"down"}`)
	s.SetLastMatch(&LastMatch{Action: originalAction})
	stubRepeatMatch(s, `{}`)

	s.matchWords([]string{"again"})

	lm := s.GetLastMatch()
	if lm == nil {
		t.Fatal("lastMatch should not have been cleared")
	}
	// "again again" must repeat the original command, not itself.
	if ActionLabel(lm.Action) != ActionLabel(originalAction) {
		t.Fatalf("lastMatch should be unchanged, got: %s", ActionLabel(lm.Action))
	}
}

// Repeat now resolves to copies of the remembered action, in the same pass
// that matched it. Nothing is returned for the platform to route back to
// voice, which is the point: this was the last user-triggered path that left
// the platform and came back as a cross-plugin `dispatch`.
func TestRepeatExpandsIntoCopiesOfTheLastAction(t *testing.T) {
	original := pluginAction("input.scroll", `{"direction":"down"}`)

	cases := []struct {
		name   string
		params string
		want   int
	}{
		{"bare again repeats once", `{}`, 1},
		{"count repeats N more", `{"count":"3"}`, 3},
		{"total counts the original as the first", `{"count":"3","mode":"total"}`, 2},
		{"total of one adds nothing", `{"count":"1","mode":"total"}`, 0},
		{"unparseable count does nothing", `{"count":"lots"}`, 0},
		{"out-of-range count does nothing", `{"count":"500"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestState()
			s.SetLastMatch(&LastMatch{Action: original})
			stubRepeatMatch(s, tc.params)

			result := s.matchWords([]string{"again"})

			if len(result.actions) != tc.want {
				t.Fatalf("expected %d action(s), got %d", tc.want, len(result.actions))
			}
			for i := range result.actions {
				a := result.actions[i]
				if ActionLabel(&a) != ActionLabel(original) {
					t.Errorf("action %d is not the remembered command: %s", i, ActionLabel(&a))
				}
				// Each entry is its own struct. The `Params` bytes are
				// shared with the remembered action — read-only downstream,
				// where this used to check the raw buffers did not alias.
				if tc.want > 1 && i > 0 && &result.actions[i] == &result.actions[0] {
					t.Errorf("action %d aliases action 0", i)
				}
			}
		})
	}
}

// With nothing remembered there is nothing to repeat, and that is a quiet
// no-op rather than an error or an empty action the platform would try to
// run.
func TestRepeatWithNoPreviousCommandProducesNothing(t *testing.T) {
	s := newTestState()
	s.ClearLastMatch()
	stubRepeatMatch(s, `{"count":"5"}`)

	result := s.matchWords([]string{"again"})

	if len(result.actions) != 0 {
		t.Fatalf("expected no actions, got %d", len(result.actions))
	}
}

func TestLastMatchUpdatedToLastActionInChain(t *testing.T) {
	s := newTestState()
	callIdx := 0
	s.stubRPC(rpcStubs{
		match: func(words, activeTags []string) *matchResponse {
			callIdx++
			if len(words) > 0 && words[0] == "snap" {
				return &matchResponse{
					Matched:       true,
					Action:        opaqueAction("noop"),
					ConsumedCount: 1,
					SetsTags:      []string{"plugin.wm.snap_mode"},
				}
			}
			if len(words) > 0 && words[0] == "left" {
				return &matchResponse{
					Matched:       true,
					Action:        pluginAction("placement.snap", `{"direction":"left"}`),
					ConsumedCount: 1,
				}
			}
			return &matchResponse{}
		},
	})

	s.matchWords([]string{"snap", "left"})

	lm := s.GetLastMatch()
	if lm == nil {
		t.Fatal("expected lastMatch to be cached")
	}
	if resolvedActionType(lm.Action) != "placement.snap" {
		t.Fatalf("expected last action in chain, got: %s", ActionLabel(lm.Action))
	}
}

func TestLastMatchClearResetsSlot(t *testing.T) {
	s := newTestState()
	s.SetLastMatch(&LastMatch{Action: opaqueAction("remembered")})

	if s.GetLastMatch() == nil {
		t.Fatal("expected non-nil before clear")
	}

	s.ClearLastMatch()

	if s.GetLastMatch() != nil {
		t.Fatal("expected nil after clear")
	}
}

func TestLastMatchGetReturnsCopy(t *testing.T) {
	s := newTestState()
	s.SetLastMatch(&LastMatch{Action: opaqueAction("remembered")})

	lm1 := s.GetLastMatch()
	lm2 := s.GetLastMatch()

	// Independent structs: mutating one caller's copy must not reach
	// another's, nor the cached match itself. (The `Params` bytes are
	// shared and read-only; this used to poke byte 0 of the raw JSON.)
	replaced := "test.replaced"
	lm1.Action.ActionType = &replaced
	if resolvedActionType(lm2.Action) == replaced {
		t.Fatal("GetLastMatch should return independent copies")
	}
	if resolvedActionType(s.GetLastMatch().Action) == replaced {
		t.Fatal("GetLastMatch's copy must not alias the cached match")
	}
}

// The shapes here are the RESOLVED ones the platform actually returns on a
// `commands.resolve` response. This test was written because it used to
// assert only the AUTHORING shape from commands.json
// (`{"type":"voice.repeat"}`), so it passed while the predicate returned
// false for every action production ever handed it — the same false-green as
// an earlier `args.session_id` gate,
// where unit tests injected a value the live path never populated.
//
// Since 2026-09-19 `commands.resolve` is typed, so that trap is closed by
// construction: a `branchkit.Action` has no field the authoring shape could
// go in. What is left to check is the variant and absence handling.
func TestIsRepeatAction(t *testing.T) {
	tests := []struct {
		name   string
		action *branchkit.Action
		want   bool
	}{
		{"resolved bare again", pluginAction("voice.repeat", `{}`), true},
		{"resolved repeat N", pluginAction("voice.repeat", `{"count":"3","mode":"more"}`), true},
		{"resolved other plugin action", pluginAction("input.scroll", `{"direction":"down"}`), false},
		{"resolved windows action", pluginAction("placement.snap", `{}`), false},
		// A sequence is a different variant and carries no action_type; a
		// repeat can never be one, so this must not be mistaken for one.
		{"sequence", &branchkit.Action{Type: branchkit.ActionKindSequence}, false},
		// A plugin variant with no action_type at all, and no action.
		// `{}` and malformed JSON used to be the cases here; neither is
		// expressible now that the field is typed.
		{"no action type", &branchkit.Action{Type: branchkit.ActionKindPlugin}, false},
		{"absent", nil, false},
	}
	for _, tt := range tests {
		if got := isRepeatAction(tt.action); got != tt.want {
			t.Errorf("%s: isRepeatAction(%s) = %v, want %v", tt.name, ActionLabel(tt.action), got, tt.want)
		}
	}
}

// "over" inside an exclusive plugin mode reaches the matcher. The platform's
// mode-global escape clears the mode's tag inside the resolve; voice must
// neither swallow the word before the matcher sees it (the 2026-09-19 bug:
// snap mode survived "over" in continuous mode) nor treat the escape as a
// command — no action to dispatch, no outcome to say, nothing to repeat.
func TestOrchestrateOverInAPluginModeReachesTheMatcherAndIsNotACommand(t *testing.T) {
	s := newTestState()
	var resolved [][]string
	s.stubRPC(rpcStubs{
		resolve: func(words []string, requireTag *string, activeTags []string) *resolveResponse {
			resolved = append(resolved, words)
			if len(words) == 1 && words[0] == "over" {
				return &resolveResponse{
					Matched:       true,
					Action:        pluginAction("_escape", `{}`),
					ConsumedCount: 1,
					ClearsTags:    []string{"plugin.placement.snap_mode"},
					ScopedTags:    []string{"plugin.placement.snap_mode"},
				}
			}
			return &resolveResponse{}
		},
	})
	s.SetLastMatch(&LastMatch{Action: pluginAction("placement.snap", `{}`)})

	resp := s.orchestrate(&OrchestrateRequest{
		Transcript: "over",
		Words:      []string{"over"},
		ActiveTags: []string{"plugin.placement.snap_mode", "plugin.voice.mode.idle"},
		Mode:       "continuous_awake",
	})

	if len(resolved) != 1 || len(resolved[0]) != 1 || resolved[0][0] != "over" {
		t.Fatalf("\"over\" must reach the matcher exactly once, resolves=%v", resolved)
	}
	if !resp.ResetEngine {
		t.Error("the escape resets the engine like any match")
	}
	if len(resp.Actions) != 0 {
		t.Errorf("_escape is not dispatched, got %s", ActionLabels(resp.Actions))
	}
	if resp.Outcome != nil {
		t.Errorf("the escape has no spoken outcome of its own, got %q", *resp.Outcome)
	}
	if resp.TagChanges == nil || len(resp.TagChanges.Clear) != 1 || resp.TagChanges.Clear[0] != "plugin.placement.snap_mode" {
		t.Errorf("the mode's tag clears through the resolve, got %+v", resp.TagChanges)
	}
	if lm := s.GetLastMatch(); lm == nil || resolvedActionType(lm.Action) != "placement.snap" {
		t.Errorf("\"again\" must not repeat an escape, lastMatch=%+v", lm)
	}
	// The resolve's scoped_tags still name the mode it just cleared; that
	// must not read as "still in a mode" and reopen the choices (seen live:
	// "over" re-pushed the six commands).
	if resp.Discovery != nil && resp.Discovery.Open != nil && *resp.Discovery.Open {
		t.Errorf("the escape must not reopen discovery, got %+v", resp.Discovery)
	}
}

// decideDiscovery on an escape: closes an open panel, reopens nothing.
func TestDecideDiscoveryEscapeClosesAndNeverReopens(t *testing.T) {
	s := newTestState()
	mode := "plugin.placement.snap_mode"
	result := matchResult{matched: true, escaped: true, pluginModeTag: &mode}
	d := s.decideDiscovery(result, true, true, "continuous_awake")
	if d.isOpen || d.mutation == nil || d.mutation.Open == nil || *d.mutation.Open {
		t.Errorf("open panel must close on escape, got %+v", d)
	}
	d = s.decideDiscovery(result, false, true, "command_hold")
	if d.isOpen || d.mutation != nil {
		t.Errorf("closed panel stays closed on escape, got %+v", d)
	}
}

// A command executed while its mode keeps discovery open (a sticky mode) is
// remembered as the hold's outcome, so the hold's end can make it the
// channel's final state instead of a bare clear; a command that closes
// discovery pushes its outcome itself and leaves nothing pending.
func TestStickyModeOutcomeIsRememberedForTheHoldsEnd(t *testing.T) {
	s := newTestState()
	s.stubRPC(rpcStubs{
		resolve: func(words []string, requireTag *string, activeTags []string) *resolveResponse {
			if requireTag != nil {
				return &resolveResponse{}
			}
			return &resolveResponse{
				Matched:        true,
				Action:         pluginAction("placement.snap", `{}`),
				ConsumedCount:  2,
				SetsTags:       []string{"plugin.placement.snap_mode"},
				ScopedTags:     []string{"plugin.placement.snap_mode"},
				ScopedPrefixes: []string{"plugin.placement.snap_mode"},
			}
		},
	})
	resp := s.orchestrate(&OrchestrateRequest{
		Transcript: "snap right", Words: []string{"snap", "right"}, Mode: "command_hold",
	})
	if resp.Outcome != nil {
		t.Fatalf("with discovery open the outcome leads the phrase, not a standalone push; got %q", *resp.Outcome)
	}
	if got := s.TakeLastOutcome(); got != "snap right" {
		t.Fatalf("the hold's outcome must be remembered, got %q", got)
	}
	if got := s.TakeLastOutcome(); got != "" {
		t.Fatalf("taken once, got %q", got)
	}
	s.SetLastOutcome("stale")
	s.StartCommandHold()
	if got := s.TakeLastOutcome(); got != "" {
		t.Fatalf("a new hold starts with no outcome, got %q", got)
	}
}
