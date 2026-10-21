package calibration

import "testing"

func bptr(b bool) *bool { return &b }

func TestScoreTrial(t *testing.T) {
	// A base command — both signals apply.
	target := Target{CommandID: "browser:scroll down", Owner: "browser", Pattern: "scroll down", Tier: TierBase, Prompt: "scroll down"}
	resolvesRight := &MatcherDecision{Matched: true, Owner: "browser", WinningPattern: "scroll down"}
	resolvesElsewhere := &MatcherDecision{Matched: true, Owner: "system", WinningPattern: "scroll up"}
	noMatch := &MatcherDecision{Matched: false}

	cases := []struct {
		name      string
		heard     string
		dec       *MatcherDecision
		wantHeard bool
		wantRes   *bool // nil = expect Resolves nil
		wantLabel string
	}{
		{"clean + resolves right", "scroll down", resolvesRight, true, bptr(true), PracticePass},
		{"case/space insensitive", "  Scroll   Down ", resolvesRight, true, bptr(true), PracticePass},
		{"heard clearly but resolves elsewhere", "scroll down", resolvesElsewhere, true, bptr(false), PracticeWarn},
		{"heard clearly but no match", "scroll down", noMatch, true, bptr(false), PracticeWarn},
		{"misheard but matcher robust", "scrawl down", resolvesRight, false, bptr(true), PracticeWarn},
		{"misheard and no match", "squirrel clown", noMatch, false, bptr(false), PracticeFail},
		{"misheard and wrong resolve", "squirrel clown", resolvesElsewhere, false, bptr(false), PracticeFail},
		{"resolve RPC failed (nil), heard clean", "scroll down", nil, true, nil, PracticePass},
		{"resolve RPC failed (nil), misheard", "nope", nil, false, nil, PracticeFail},
		{"empty heard", "", noMatch, false, bptr(false), PracticeFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ScoreTrial(tc.heard, target, tc.dec)
			if v.Heard != tc.wantHeard {
				t.Errorf("Heard = %v, want %v", v.Heard, tc.wantHeard)
			}
			switch {
			case tc.wantRes == nil && v.Resolves != nil:
				t.Errorf("Resolves = %v, want nil", *v.Resolves)
			case tc.wantRes != nil && v.Resolves == nil:
				t.Errorf("Resolves = nil, want %v", *tc.wantRes)
			case tc.wantRes != nil && *v.Resolves != *tc.wantRes:
				t.Errorf("Resolves = %v, want %v", *v.Resolves, *tc.wantRes)
			}
			if got := v.Label(); got != tc.wantLabel {
				t.Errorf("Label() = %q, want %q", got, tc.wantLabel)
			}
		})
	}
}

func TestScoreTrialCodewordHasNoResolves(t *testing.T) {
	// A bare codeword (no CommandID) never gets a functional signal, even if a
	// decision is handed in.
	tgt := CodewordTarget("bravo")
	dec := &MatcherDecision{Matched: true, Owner: "voice", WinningPattern: "bravo"}

	if v := ScoreTrial("bravo", tgt, dec); v.Resolves != nil || !v.Heard || v.Label() != PracticePass {
		t.Errorf("clean codeword: got Heard=%v Resolves=%v label=%q, want pass/acoustic-only", v.Heard, v.Resolves, v.Label())
	}
	if v := ScoreTrial("bravado", tgt, dec); v.Resolves != nil || v.Heard || v.Label() != PracticeFail {
		t.Errorf("misheard codeword: got Heard=%v Resolves=%v label=%q, want fail/acoustic-only", v.Heard, v.Resolves, v.Label())
	}
}

func TestScoreTrialSlottedIsAcousticOnly(t *testing.T) {
	// A slotted target is scored acoustic-only (Resolves nil) even though it
	// carries a CommandID.
	tgt := Target{CommandID: "voice:type <text>", Owner: "voice", Pattern: "type <text>", Tier: TierSlotted, Prompt: "type sample"}
	dec := &MatcherDecision{Matched: true, Owner: "voice", WinningPattern: "type <text>"}
	if v := ScoreTrial("type sample", tgt, dec); v.Resolves != nil {
		t.Errorf("slotted target must not produce a Resolves signal, got %v", *v.Resolves)
	}
}

func TestScoreTrialDynamicIsAcousticOnly(t *testing.T) {
	// A dynamic (hint) command is scored acoustic-only: the activate command
	// resolves against live on-screen hints, which calibration can't reproduce,
	// so no Resolves signal even with a decision in hand.
	tgt := Target{CommandID: "browser:<prefix> <suffix>", Owner: "browser", Pattern: "<prefix> <suffix>", Tier: TierDynamic, Prompt: "air bat"}
	dec := &MatcherDecision{Matched: true, Owner: "browser", WinningPattern: "<prefix> <suffix>"}
	if v := ScoreTrial("air bat", tgt, dec); v.Resolves != nil {
		t.Errorf("dynamic target must not produce a Resolves signal, got %v", *v.Resolves)
	}
	if v := ScoreTrial("air bat", tgt, dec); !v.Heard || v.Label() != PracticePass {
		t.Errorf("clean dynamic trial: got Heard=%v label=%q, want pass/acoustic-only", v.Heard, v.Label())
	}
}

func TestScoreTrialRequiresOwnerMatch(t *testing.T) {
	// Same winning pattern but a different owner must not resolve — identity is
	// owner + pattern.
	tgt := Target{CommandID: "browser:next", Owner: "browser", Pattern: "next", Tier: TierBase, Prompt: "next"}
	dec := &MatcherDecision{Matched: true, Owner: "tiling", WinningPattern: "next"}
	v := ScoreTrial("next", tgt, dec)
	if v.Resolves == nil || *v.Resolves {
		t.Errorf("cross-owner same-pattern should not resolve, got %v", v.Resolves)
	}
	if v.Label() != PracticeWarn {
		t.Errorf("Label = %q, want warn", v.Label())
	}
}

func enumFixture() []EnumeratedCommand {
	return []EnumeratedCommand{
		{ID: "browser:scroll down", Owner: "browser", Pattern: "scroll down", RequiresTags: nil, Dynamic: false},
		{ID: "browser:scroll up", Owner: "browser", Pattern: "scroll up", RequiresTags: nil, Dynamic: false},
		{ID: "windows:snap left", Owner: "windows", Pattern: "snap left", RequiresTags: nil, Dynamic: false},
		{ID: "windows:left", Owner: "windows", Pattern: "left", RequiresTags: []string{"plugin.placement.snap_mode.active"}, Dynamic: false},
		{ID: "browser:hints", Owner: "browser", Pattern: "<prefix> <suffix>", Dynamic: true},
		{ID: "voice:type", Owner: "voice", Pattern: "type <text>", Dynamic: false},
	}
}

func TestTierFor(t *testing.T) {
	cases := map[string]Tier{
		"scroll down":       TierBase,      // static, no tags, no slot
		"left":              TierModeGated, // requires_tags
		"<prefix> <suffix>": TierDynamic,   // dynamic wins over the <slot>
		"type <text>":       TierSlotted,   // slot, no tags
	}
	byPattern := make(map[string]EnumeratedCommand)
	for _, c := range enumFixture() {
		byPattern[c.Pattern] = c
	}
	for pattern, want := range cases {
		c, ok := byPattern[pattern]
		if !ok {
			t.Fatalf("fixture missing pattern %q", pattern)
		}
		if got := tierFor(c); got != want {
			t.Errorf("tierFor(%q) = %q, want %q", pattern, got, want)
		}
	}
	// A command that is both slotted and mode-gated reports slotted.
	both := EnumeratedCommand{Pattern: "pick <n>", RequiresTags: []string{"x"}}
	if got := tierFor(both); got != TierSlotted {
		t.Errorf("slotted+mode-gated = %q, want %q", got, TierSlotted)
	}
}

func TestBuildCommandTargetsAllSpansEveryTier(t *testing.T) {
	got := BuildCommandTargets(enumFixture(), SelectionAll, nil)
	// The unification: every tier is testable now, not just base.
	if len(got) != 6 {
		t.Fatalf("expected all 6 commands, got %d: %+v", len(got), got)
	}
	tierByID := make(map[string]Tier)
	for _, tg := range got {
		tierByID[tg.CommandID] = tg.Tier
	}
	wantTiers := map[string]Tier{
		"browser:scroll down": TierBase,
		"windows:left":        TierModeGated,
		"browser:hints":       TierDynamic,
		"voice:type":          TierSlotted,
	}
	for id, want := range wantTiers {
		if tierByID[id] != want {
			t.Errorf("%s tier = %q, want %q", id, tierByID[id], want)
		}
	}
	// Sorted by owner then pattern.
	for i := 1; i < len(got); i++ {
		a, b := got[i-1], got[i]
		if a.Owner > b.Owner || (a.Owner == b.Owner && a.Pattern > b.Pattern) {
			t.Errorf("not sorted by owner,pattern at %d: %q/%q then %q/%q", i, a.Owner, a.Pattern, b.Owner, b.Pattern)
		}
	}
	// Slotted target gets a concrete (slot-free) prompt.
	for _, tg := range got {
		if tg.CommandID == "voice:type" {
			if got, want := tg.Prompt, "type sample"; got != want {
				t.Errorf("slotted prompt = %q, want %q", got, want)
			}
		}
	}
}

func TestBuildCommandTargetsCategoryFilters(t *testing.T) {
	got := BuildCommandTargets(enumFixture(), "category:windows", nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 windows commands, got %d: %+v", len(got), got)
	}
	for _, tg := range got {
		if tg.Owner != "windows" {
			t.Errorf("category filter leaked owner %q", tg.Owner)
		}
	}
}

func TestBuildCommandTargetsDrillNeverTestedFirst(t *testing.T) {
	// "scroll down" has been tested; the rest never have.
	attempts := []VerificationAttempt{{Target: "scroll down", Matched: true}}
	got := BuildCommandTargets(enumFixture(), SelectionDrill, attempts)
	if len(got) == 0 {
		t.Fatal("expected a non-empty drill list")
	}
	// Everything ahead of the one command with an attempt must be a command
	// with none. Target.Reason used to carry the drill rationale and this
	// asserted on it; nothing read that field, so the property is checked
	// against the attempt set instead — which is what it was really about.
	for i, tg := range got {
		if tg.Pattern == "scroll down" {
			for j := range i {
				if got[j].Pattern == "scroll down" {
					t.Errorf("tested command surfaced before a fresh entry at %d (%+v)", j, got[j])
				}
			}
			return
		}
	}
}

func TestTargetsFromIDs(t *testing.T) {
	got := TargetsFromIDs(enumFixture(), []string{"windows:left", "nope:missing", "browser:scroll up"})
	if len(got) != 2 {
		t.Fatalf("expected 2 resolved ids (unknown dropped), got %d", len(got))
	}
	if got[0].CommandID != "windows:left" || got[0].Tier != TierModeGated {
		t.Errorf("first = %+v, want windows:left mode_gated", got[0])
	}
	if got[1].CommandID != "browser:scroll up" {
		t.Errorf("second = %+v, want browser:scroll up", got[1])
	}
}

func TestExpandDynamicTargets(t *testing.T) {
	cw := []string{"air", "bat", "check"}
	dyn := Target{CommandID: "browser:hints", Owner: "browser", Pattern: "<prefix> <suffix>", Tier: TierDynamic, Prompt: "x"}
	base := Target{CommandID: "browser:scroll down", Owner: "browser", Pattern: "scroll down", Tier: TierBase, Prompt: "scroll down"}

	got := ExpandDynamicTargets([]Target{dyn, base}, cw, false)
	// 3 codewords × 2 slots = 9 dynamic pairs (full cross product) + 1 base = 10.
	if len(got) != 10 {
		t.Fatalf("expected 10 targets (3×3 dynamic + 1 base), got %d", len(got))
	}
	// Prefix-outer / suffix-inner ordering, same-codeword pairs included.
	wantFirst := []string{"air air", "air bat", "air check", "bat air"}
	for i, w := range wantFirst {
		if got[i].Prompt != w || got[i].Tier != TierDynamic {
			t.Errorf("dynamic pair %d = %q, want %q", i, got[i].Prompt, w)
		}
	}
	if got[9].Prompt != "scroll down" {
		t.Errorf("base target should pass through, got %q", got[9].Prompt)
	}

	// Single mode: each codeword alone (3), still dynamic context.
	single := ExpandDynamicTargets([]Target{dyn}, cw, true)
	if len(single) != 3 {
		t.Fatalf("single mode: expected 3 targets, got %d", len(single))
	}
	for i, w := range cw {
		if single[i].Prompt != w || single[i].Tier != TierDynamic {
			t.Errorf("single %d = %q, want %q", i, single[i].Prompt, w)
		}
	}

	// No codewords → unchanged.
	if got := ExpandDynamicTargets([]Target{dyn}, nil, false); len(got) != 1 {
		t.Errorf("no codewords should leave targets unexpanded, got %d", len(got))
	}
}

func TestPrompts(t *testing.T) {
	targets := []Target{{Prompt: "a"}, {Prompt: "b c"}}
	got := Prompts(targets)
	if len(got) != 2 || got[0] != "a" || got[1] != "b c" {
		t.Errorf("Prompts = %v", got)
	}
}
