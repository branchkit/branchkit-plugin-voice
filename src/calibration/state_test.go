package calibration

import (
	"fmt"
	"testing"
	"time"
)

// Acoustic-only walk (bare codewords): heard == prompt → pass; no Resolves.
func TestWalk_Acoustic_HeardEqualsTarget_Passes(t *testing.T) {
	s := NewState()
	s.StartWalk(targetsForWords("arch", "bake"), nil, "alphabet")

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("arch", true) })
	s.HandleCalibrate("advance", "")

	results := s.GetResults()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Passed {
		t.Errorf("expected acoustic pass on heard==target, got %+v", results[0])
	}
	if results[0].Trial.Resolves != nil {
		t.Errorf("codeword trial must have no Resolves signal, got %v", *results[0].Trial.Resolves)
	}
	if results[0].Trial.Verdict != PracticePass {
		t.Errorf("verdict = %q, want pass", results[0].Trial.Verdict)
	}
}

// Acoustic mismatch → fail.
func TestWalk_Acoustic_Mismatch_Fails(t *testing.T) {
	s := NewState()
	s.StartWalk(targetsForWords("arch"), nil, "alphabet")

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("art", true) })
	s.HandleCalibrate("advance", "")

	results := s.GetResults()
	if len(results) != 1 || results[0].Passed {
		t.Errorf("expected acoustic fail on mismatch, got %+v", results)
	}
}

func browserScrollTarget() Target {
	return Target{CommandID: "browser:scroll down", Owner: "browser", Pattern: "scroll down", Tier: TierBase, Prompt: "scroll down"}
}

// Command target, both signals: heard cleanly AND resolves to the intended
// command → pass. The preview-resolve path is consulted.
func TestWalk_Command_ResolvesRight_Passes(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	applied := 0
	s.ApplyContext = func(_, cmdID string) (AppliedContext, error) {
		applied++
		if cmdID != "browser:scroll down" {
			t.Errorf("ApplyContext command_id = %q", cmdID)
		}
		return AppliedContext{Kind: "static"}, nil
	}
	resolveCalls := 0
	s.ResolvePreview = func(string) *MatcherDecision {
		resolveCalls++
		return &MatcherDecision{Matched: true, Owner: "browser", WinningPattern: "scroll down"}
	}

	s.StartWalk([]Target{browserScrollTarget()}, nil, "browser:scroll down")
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll down", true) })
	s.HandleCalibrate("advance", "")

	results := s.GetResults()
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected command trial to pass on heard+resolves, got %+v", results)
	}
	if results[0].Trial.Verdict != PracticePass {
		t.Errorf("verdict = %q, want pass", results[0].Trial.Verdict)
	}
	if results[0].Trial.Resolves == nil || !*results[0].Trial.Resolves {
		t.Errorf("expected Resolves=true, got %v", results[0].Trial.Resolves)
	}
	if applied != 1 {
		t.Errorf("ApplyContext should be called once per target, got %d", applied)
	}
	if resolveCalls != 1 {
		t.Errorf("ResolvePreview should be called once, got %d", resolveCalls)
	}
}

// Command target: heard cleanly but resolves to a different command → warn,
// not a pass.
func TestWalk_Command_ResolvesElsewhere_Warns(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(_, _ string) (AppliedContext, error) { return AppliedContext{Kind: "static"}, nil }
	s.ResolvePreview = func(string) *MatcherDecision {
		return &MatcherDecision{Matched: true, Owner: "system", WinningPattern: "scroll up"}
	}

	s.StartWalk([]Target{browserScrollTarget()}, nil, "browser:scroll down")
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll down", true) })
	s.HandleCalibrate("advance", "")

	results := s.GetResults()
	if len(results) != 1 || results[0].Passed {
		t.Fatalf("expected command trial to NOT pass when resolving elsewhere, got %+v", results)
	}
	if results[0].Trial.Verdict != PracticeWarn {
		t.Errorf("verdict = %q, want warn", results[0].Trial.Verdict)
	}
}

// Each target enters its own context (per-target trial scope): ApplyContext is
// called once per target in order, and the trial scope is begun/ended per
// target (advancing auto-closes the prior, finish closes the last).
func TestWalk_PerTargetContextLifecycle(t *testing.T) {
	s := NewState()
	begins, ends := 0, 0
	s.TrialBegin = func() (string, error) { begins++; return fmt.Sprintf("t%d", begins), nil }
	s.TrialEnd = func(string) error { ends++; return nil }
	var applied []string
	s.ApplyContext = func(_, cmdID string) (AppliedContext, error) {
		applied = append(applied, cmdID)
		return AppliedContext{Kind: "static"}, nil
	}

	t1 := Target{CommandID: "a:one", Owner: "a", Pattern: "one", Tier: TierBase, Prompt: "one"}
	t2 := Target{CommandID: "b:two", Owner: "b", Pattern: "two", Tier: TierBase, Prompt: "two"}
	s.StartWalk([]Target{t1, t2}, nil, "selection")

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("one", true) })
	s.HandleCalibrate("advance", "") // → target 1, auto-closes target 0's scope
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("two", true) })
	s.HandleCalibrate("advance", "") // → finish, closes target 1's scope

	if len(applied) != 2 || applied[0] != "a:one" || applied[1] != "b:two" {
		t.Errorf("ApplyContext per target = %v, want [a:one b:two]", applied)
	}
	if begins != 2 {
		t.Errorf("trial_begin count = %d, want 2 (one per target)", begins)
	}
	if ends != 2 {
		t.Errorf("trial_end count = %d, want 2 (auto-close prior + finish)", ends)
	}
}

// Consecutive targets of the SAME command (the spread of pairs for one dynamic
// hint command) reuse one trial — the fixture applies once, not per pair.
func TestWalk_SameContextReusesTrial(t *testing.T) {
	s := NewState()
	begins, ends := 0, 0
	s.TrialBegin = func() (string, error) { begins++; return fmt.Sprintf("t%d", begins), nil }
	s.TrialEnd = func(string) error { ends++; return nil }
	var applied []string
	s.ApplyContext = func(_, cmdID string) (AppliedContext, error) {
		applied = append(applied, cmdID)
		return AppliedContext{Kind: "dynamic"}, nil
	}
	mk := func(prompt string) Target {
		return Target{CommandID: "browser:hints", Owner: "browser", Pattern: "<p> <s>", Tier: TierDynamic, Prompt: prompt}
	}
	s.StartWalk([]Target{mk("air bat"), mk("bat air")}, nil, "browser:hints")

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("air bat", true) })
	s.HandleCalibrate("advance", "") // → second pair, same context → reuse trial
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("bat air", true) })
	s.HandleCalibrate("advance", "") // → finish

	if begins != 1 {
		t.Errorf("same-context pairs should reuse one trial, begins = %d, want 1", begins)
	}
	if len(applied) != 1 {
		t.Errorf("fixture should apply once for same-context pairs, applied = %v", applied)
	}
	if ends != 1 {
		t.Errorf("ends = %d, want 1 (one trial closed at finish)", ends)
	}
}

func dynamicHintTarget(codeword string) Target {
	return Target{
		CommandID: "browser:hints", Owner: "browser", Pattern: "<p> <s>",
		Tier: TierDynamic, Prompt: codeword,
		Binding: &VocabBinding{Collection: "alphabet", Key: codeword},
	}
}

// Retuning a dynamic (hint) target renames the bound alphabet entry, refreshes
// the live fixture in place on the SAME trial (no new trial scope), and repoints
// the current target at the new codeword for an immediate retest.
func TestRetune_Dynamic_RefreshesFixtureInPlace(t *testing.T) {
	s := NewState()
	begins, ends, applied := 0, 0, 0
	s.TrialBegin = func() (string, error) { begins++; return "trial", nil }
	s.TrialEnd = func(string) error { ends++; return nil }
	s.ApplyContext = func(trialID, cmdID string) (AppliedContext, error) {
		applied++
		if trialID != "trial" {
			t.Errorf("refresh must reuse the open trial, got trial_id=%q", trialID)
		}
		return AppliedContext{Kind: "dynamic"}, nil
	}
	var gotColl, gotOld, gotNew string
	s.RenameVocab = func(coll, oldKey, newKey string) error {
		gotColl, gotOld, gotNew = coll, oldKey, newKey
		return nil
	}

	s.StartWalk([]Target{dynamicHintTarget("sun")}, nil, "browser:hints")
	if applied != 1 || begins != 1 {
		t.Fatalf("after StartWalk applied=%d begins=%d, want 1/1", applied, begins)
	}

	if err := s.RetuneTarget("sol"); err != nil {
		t.Fatalf("RetuneTarget: %v", err)
	}

	if gotColl != "alphabet" || gotOld != "sun" || gotNew != "sol" {
		t.Errorf("RenameVocab(%q,%q,%q), want (alphabet,sun,sol)", gotColl, gotOld, gotNew)
	}
	if applied != 2 {
		t.Errorf("dynamic retune should re-apply the fixture once, applied=%d want 2", applied)
	}
	if begins != 1 || ends != 0 {
		t.Errorf("refresh must reuse the trial (no begin/end), begins=%d ends=%d", begins, ends)
	}
	prompt, _ := s.GetCurrentTarget()
	if prompt != "sol" {
		t.Errorf("current prompt after retune = %q, want sol", prompt)
	}
}

// Retuning a bare codeword (base alphabet walk) renames the bound entry, with no
// fixture to re-apply, and repoints the target.
func TestRetune_BaseCodeword(t *testing.T) {
	s := NewState()
	applied := 0
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(string, string) (AppliedContext, error) { applied++; return AppliedContext{Kind: "static"}, nil }
	var gotColl, gotOld, gotNew string
	s.RenameVocab = func(coll, oldKey, newKey string) error {
		gotColl, gotOld, gotNew = coll, oldKey, newKey
		return nil
	}

	s.StartWalk(targetsForWords("sun"), []string{"s"}, "alphabet")

	if err := s.RetuneTarget("sol"); err != nil {
		t.Fatalf("RetuneTarget: %v", err)
	}

	if gotColl != "alphabet" || gotOld != "sun" || gotNew != "sol" {
		t.Errorf("RenameVocab(%q,%q,%q), want (alphabet,sun,sol)", gotColl, gotOld, gotNew)
	}
	if applied != 0 {
		t.Errorf("a bare codeword has no command context to apply, got %d", applied)
	}
	prompt, _ := s.GetCurrentTarget()
	if prompt != "sol" {
		t.Errorf("current prompt after retune = %q, want sol", prompt)
	}
}

// A list-slot target renames its bound list entry and substitutes the new word
// into the prompt phrase, leaving the rest of the command intact.
func TestRetune_ListSlot_SubstitutesInPrompt(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(string, string) (AppliedContext, error) { return AppliedContext{Kind: "static"}, nil }
	var gotColl, gotOld, gotNew string
	s.RenameVocab = func(coll, oldKey, newKey string) error {
		gotColl, gotOld, gotNew = coll, oldKey, newKey
		return nil
	}

	tgt := Target{
		CommandID: "browser:scroll down <number>", Owner: "browser",
		Pattern: "scroll down <number>", Tier: TierSlotted, Prompt: "scroll down three",
		Binding: &VocabBinding{Collection: "number", Key: "three"},
	}
	s.StartWalk([]Target{tgt}, nil, "browser:scroll down <number>")

	if err := s.RetuneTarget("trio"); err != nil {
		t.Fatalf("RetuneTarget: %v", err)
	}
	if gotColl != "number" || gotOld != "three" || gotNew != "trio" {
		t.Errorf("RenameVocab(%q,%q,%q), want (number,three,trio)", gotColl, gotOld, gotNew)
	}
	prompt, _ := s.GetCurrentTarget()
	if prompt != "scroll down trio" {
		t.Errorf("prompt after retune = %q, want 'scroll down trio'", prompt)
	}
}

// A target with no binding (a hint pair, a command-pattern word) isn't retunable.
func TestRetune_UnboundTarget_Errors(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(string, string) (AppliedContext, error) { return AppliedContext{Kind: "dynamic"}, nil }
	s.RenameVocab = func(string, string, string) error { t.Fatal("must not rename an unbound target"); return nil }

	unbound := Target{CommandID: "browser:hints", Owner: "browser", Pattern: "<p> <s>", Tier: TierDynamic, Prompt: "air bat"}
	s.StartWalk([]Target{unbound}, nil, "browser:hints")
	if err := s.RetuneTarget("sol"); err == nil {
		t.Error("expected error retuning a target with no binding")
	}
}

// The lexicon pre-screen rejects unrepresentable candidates before the rename,
// so the user never spends a retest on a word the engine can't hear.
func TestRetune_RejectsUnrepresentableCandidate(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(string, string) (AppliedContext, error) { return AppliedContext{Kind: "dynamic"}, nil }
	renames := 0
	s.RenameVocab = func(string, string, string) error { renames++; return nil }
	s.StartWalk([]Target{dynamicHintTarget("sun")}, nil, "browser:hints")

	for _, bad := range []string{"", "  ", "s0l", "café", "S@L"} {
		if err := s.RetuneTarget(bad); err == nil {
			t.Errorf("expected %q to be rejected by the lexicon pre-screen", bad)
		}
	}
	if renames != 0 {
		t.Errorf("a rejected candidate must not persist a rename, got %d calls", renames)
	}
}

func TestRepresentableSurfaceForm(t *testing.T) {
	// Single + multi-word lowercase forms are fine (e.g. app names).
	for _, ok := range []string{"sol", "arch", "zebra", "google chrome"} {
		if err := representableSurfaceForm(ok); err != nil {
			t.Errorf("representableSurfaceForm(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "s0l", "Über", "x!"} {
		if err := representableSurfaceForm(bad); err == nil {
			t.Errorf("representableSurfaceForm(%q) = nil, want error", bad)
		}
	}
}

// RevertTarget resets the current target's entry to its plugin default and
// re-points the prompt at the default key the platform reports.
func TestRevertTarget(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(string, string) (AppliedContext, error) { return AppliedContext{Kind: "static"}, nil }
	var gotColl, gotKey string
	s.RevertVocab = func(coll, key string) (string, error) {
		gotColl, gotKey = coll, key
		return "air", nil // the platform reports the default key
	}

	s.StartWalk([]Target{CodewordTarget("ace")}, []string{"a"}, "alphabet")
	if err := s.RevertTarget(); err != nil {
		t.Fatalf("RevertTarget: %v", err)
	}
	if gotColl != "alphabet" || gotKey != "ace" {
		t.Errorf("RevertVocab(%q,%q), want (alphabet,ace)", gotColl, gotKey)
	}
	prompt, _ := s.GetCurrentTarget()
	if prompt != "air" {
		t.Errorf("prompt after revert = %q, want air", prompt)
	}
}

// Revert leaves the prompt unchanged when the entry is already the default
// (platform returns the same key).
func TestRevertTarget_AlreadyDefault(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(string, string) (AppliedContext, error) { return AppliedContext{Kind: "static"}, nil }
	s.RevertVocab = func(_, key string) (string, error) { return key, nil }
	s.StartWalk([]Target{CodewordTarget("air")}, []string{"a"}, "alphabet")
	if err := s.RevertTarget(); err != nil {
		t.Fatalf("RevertTarget: %v", err)
	}
	if prompt, _ := s.GetCurrentTarget(); prompt != "air" {
		t.Errorf("prompt unchanged = %q, want air", prompt)
	}
}

// Retune is a no-op error when no walk is running.
func TestRetune_NotRunning_Errors(t *testing.T) {
	s := NewState()
	s.RenameVocab = func(string, string, string) error { t.Fatal("rename must not run when idle"); return nil }
	if err := s.RetuneTarget("sol"); err == nil {
		t.Error("expected error retuning with no active walk")
	}
}

// SkipGroup dismisses every remaining target in the current group at once,
// leaving targets outside the group untouched; an ungrouped target skips singly.
func TestSkipGroup(t *testing.T) {
	s := NewState()
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { return nil }
	s.ApplyContext = func(string, string) (AppliedContext, error) { return AppliedContext{Kind: "static"}, nil }

	grp := func(prompt, group string) Target {
		return Target{CommandID: "c:" + group, Owner: "c", Pattern: group, Tier: TierSlotted, Prompt: prompt, Group: group}
	}
	s.StartWalk([]Target{
		grp("focus a", "focus <apps>"),
		grp("focus b", "focus <apps>"),
		grp("focus c", "focus <apps>"),
		CodewordTarget("solo"), // ungrouped target after the group
	}, nil, "all")

	// Skip the whole 3-entry apps group → land on the ungrouped "solo".
	s.HandleCalibrate("skip_group", "")
	prompt, idx := s.GetCurrentTarget()
	if idx != 3 || prompt != "solo" {
		t.Fatalf("after skip_group, at index %d prompt %q, want index 3 'solo'", idx, prompt)
	}

	// skip_group on an ungrouped target is a single skip → finishes the walk.
	s.HandleCalibrate("skip_group", "")
	if s.IsActive() {
		t.Error("walk should be finished after skipping the last (ungrouped) target")
	}
	results := s.GetResults()
	if len(results) != 4 {
		t.Fatalf("expected 4 results (3 group-skipped + 1), got %d", len(results))
	}
	for i, r := range results {
		if r.Trial.Outcome != "skipped" {
			t.Errorf("result %d outcome = %q, want skipped", i, r.Trial.Outcome)
		}
	}
}

func TestSingleListSlot(t *testing.T) {
	cases := []struct{ pattern, coll, slot string }{
		{"scroll down <number>", "number", "<number>"},
		{"scroll <direction>", "direction", "<direction>"},
		{"<choice:plugin.voice.tie_choices>", "plugin.voice.tie_choices", "<choice:plugin.voice.tie_choices>"},
		{"scroll down", "", ""},           // no slot
		{"send <a> <b>", "", ""},          // multiple slots
		{"press <keys|alphabet>", "", ""}, // union slot
	}
	for _, c := range cases {
		coll, slot := singleListSlot(c.pattern)
		if coll != c.coll || slot != c.slot {
			t.Errorf("singleListSlot(%q) = (%q,%q), want (%q,%q)", c.pattern, coll, slot, c.coll, c.slot)
		}
	}
}

// A slotted command backed by a real list expands to one bound target per
// entry; free-text and non-retunable (hint/tie) slots pass through unchanged.
func TestExpandListSlotTargets(t *testing.T) {
	s := NewState()
	s.CollectionKeys = func(coll string) []string {
		if coll == "number" {
			return []string{"one", "two"}
		}
		return nil // unknown / free-text collections have no keys
	}
	in := []Target{
		{Pattern: "scroll down <number>", Tier: TierSlotted, Prompt: "scroll down sample"},
		{Pattern: "find <text>", Tier: TierSlotted, Prompt: "find sample"},
		{Pattern: "press <prefix:browser_hints_prefix>", Tier: TierSlotted, Prompt: "press sample"},
	}
	out := s.expandListSlotTargets(in)

	// 2 number entries + the two pass-through targets = 4.
	if len(out) != 4 {
		t.Fatalf("expanded to %d targets, want 4: %+v", len(out), out)
	}
	// First two are the expanded number entries, in key order.
	for i, want := range []struct{ prompt, key string }{
		{"scroll down one", "one"}, {"scroll down two", "two"},
	} {
		if out[i].Prompt != want.prompt {
			t.Errorf("out[%d].Prompt = %q, want %q", i, out[i].Prompt, want.prompt)
		}
		if b := out[i].Binding; b == nil || b.Collection != "number" || b.Key != want.key {
			t.Errorf("out[%d].Binding = %+v, want {number, %s}", i, b, want.key)
		}
	}
	// Free-text + hint slots pass through unbound.
	if out[2].Binding != nil || out[2].Prompt != "find sample" {
		t.Errorf("free-text slot should pass through unbound, got %+v", out[2])
	}
	if out[3].Binding != nil {
		t.Error("hint-collection slot must stay unbound")
	}
}

// Cancelling an active walk closes its open trial scope.
func TestWalk_Cancel_ClosesTrial(t *testing.T) {
	s := NewState()
	ends := 0
	s.TrialBegin = func() (string, error) { return "trial", nil }
	s.TrialEnd = func(string) error { ends++; return nil }
	s.ApplyContext = func(_, _ string) (AppliedContext, error) { return AppliedContext{Kind: "static"}, nil }

	s.StartWalk([]Target{browserScrollTarget()}, nil, "browser:scroll down")
	s.HandleCalibrate("cancel", "")

	if ends != 1 {
		t.Errorf("cancel should close the open trial once, got %d", ends)
	}
	if s.IsActive() {
		t.Error("calibration should be inactive after cancel")
	}
}

// A stale liveness heartbeat (the calibration page was left) closes the sitting:
// under the live-dashboard model leaving ends the walk (per-command attempts were
// already persisted incrementally), which also stops the speech interceptor from
// swallowing Option+G. A fresh heartbeat leaves a running walk alone.
func TestSuspendIfStale_ClosesSittingWhenPageLeaves(t *testing.T) {
	s := NewState()
	s.StartWalk(targetsForWords("arm"), nil, "alphabet")

	// Fresh heartbeat (StartWalk stamped lastSeenAt) → keeps running.
	s.suspendIfStale()
	s.WithLock(func(g *Game, _ bool) {
		if !g.IsRunning() {
			t.Fatal("walk should keep running while the page heartbeat is fresh")
		}
	})

	// Page goes away: heartbeat goes stale.
	s.mu.Lock()
	s.lastSeenAt = time.Now().Add(-2 * livenessTimeout)
	s.mu.Unlock()

	s.suspendIfStale()
	s.WithLock(func(g *Game, _ bool) {
		if g.Phase != PhaseFinished {
			t.Fatalf("stale heartbeat should close the sitting (finish); phase=%v", g.Phase)
		}
		if g.IsActive() {
			t.Fatal("a closed sitting should no longer be active (interceptor released)")
		}
	})
}

// With a rail filter active, Next steps through only the matching targets,
// marking the filtered-out ones skipped (so the Next button respects the
// visible rail, not the full queue).
func TestAdvance_RespectsRailFilter(t *testing.T) {
	s := NewState()
	s.StartWalk(targetsForWords("arm", "boot", "card", "deck", "art"), nil, "custom")

	// current = "arm" (index 0, matches "ar"). Advance with the filter: score
	// arm, skip "boot" (no "ar"), land on "card" (index 2).
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("arm", true) })
	s.HandleCalibrate("advance", "ar")
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 2 {
			t.Fatalf("advance with filter should land on index 2 (card), got %d (%q)", g.CurrentIndex, g.Words[g.CurrentIndex])
		}
	})
	res := s.GetResults()
	if len(res) != 2 || res[1].Word != "boot" || res[1].Trial.Outcome != "skipped" {
		t.Fatalf("filtered-out 'boot' should be recorded skipped, got %+v", res)
	}

	// Advance again: score card, skip "deck", land on "art" (index 4, last match).
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("card", true) })
	s.HandleCalibrate("advance", "ar")
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 4 {
			t.Fatalf("second filtered advance should land on index 4 (art), got %d", g.CurrentIndex)
		}
	})
}

// Auto-advance on: a hold that lands the trial green (a recognized segment +
// the end-of-hold boundary) moves to the next target with no advance action,
// scoring the trial it leaves behind. A second green hold finishes the walk.
func TestAutoAdvance_GreenHoldAdvances(t *testing.T) {
	s := NewState()
	s.AutoAdvanceEnabled = func() bool { return true }
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("arch", "bake"), nil, "alphabet")

	s.ProcessSpeechFrame("arch", false) // recognized segment (engine sends non-final)
	s.HandleHoldEnd()                   // Option+G up → hold boundary
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 1 {
			t.Fatalf("green hold should auto-advance to index 1, got %d", g.CurrentIndex)
		}
	})
	if res := s.GetResults(); len(res) != 1 || !res[0].Passed {
		t.Fatalf("auto-advanced trial should be recorded as a pass, got %+v", res)
	}

	s.ProcessSpeechFrame("bake", false)
	s.HandleHoldEnd()
	s.WithLock(func(g *Game, _ bool) {
		if g.IsActive() {
			t.Fatalf("second green hold should finish the walk")
		}
	})
	if res := s.GetResults(); len(res) != 2 {
		t.Fatalf("expected 2 results after auto-advancing the whole walk, got %d", len(res))
	}
}

// Auto-advance off (the default): a green hold stays put — the user clicks Next.
func TestAutoAdvance_Off_StaysPut(t *testing.T) {
	s := NewState()
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("arch", "bake"), nil, "alphabet")

	s.ProcessSpeechFrame("arch", false)
	s.HandleHoldEnd()
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 0 {
			t.Fatalf("auto-advance off: a green hold must not advance, got index %d", g.CurrentIndex)
		}
		if !g.MatchStatus() {
			t.Fatalf("trial should still be green, waiting for a Next click")
		}
	})
	if res := s.GetResults(); len(res) != 0 {
		t.Fatalf("nothing should be scored until Next, got %+v", res)
	}
}

// Auto-advance on but the hold mismatches: the walk holds on the current target
// so the user can retry.
func TestAutoAdvance_MissDoesNotAdvance(t *testing.T) {
	s := NewState()
	s.AutoAdvanceEnabled = func() bool { return true }
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("arch", "bake"), nil, "alphabet")

	s.ProcessSpeechFrame("art", false)
	s.HandleHoldEnd()
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 0 {
			t.Fatalf("a mismatched hold must not auto-advance, got index %d", g.CurrentIndex)
		}
	})
}

// Auto-advance on: a recognized segment with no hold boundary yet (mid-hold)
// must not jump — only the end-of-hold boundary advances.
func TestAutoAdvance_SegmentWithoutBoundaryDoesNotAdvance(t *testing.T) {
	s := NewState()
	s.AutoAdvanceEnabled = func() bool { return true }
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("arch", "bake"), nil, "alphabet")

	s.ProcessSpeechFrame("arch", false)
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 0 {
			t.Fatalf("a recognized segment without a hold boundary must not auto-advance, got index %d", g.CurrentIndex)
		}
	})
}

// A multi-word target the VAD splits into two segments within ONE hold
// ("snap" | "left") is heard as the whole phrase, so it scores as one.
func TestHoldSegmentsJoinBeforeScoring(t *testing.T) {
	s := NewState()
	s.AutoAdvanceEnabled = func() bool { return true }
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("snap left", "bake"), nil, "alphabet")

	s.ProcessSpeechFrame("snap", false)
	s.ProcessSpeechFrame("left", false)
	s.WithLock(func(g *Game, _ bool) {
		if heard := g.CurrentTrial.FinalHeard(); heard != "snap left" {
			t.Fatalf("FinalHeard = %q, want the hold's segments joined (%q)", heard, "snap left")
		}
	})
	s.HandleHoldEnd()
	res := s.GetResults()
	if len(res) != 1 || !res[0].Passed {
		t.Fatalf("a split multi-word target must score as a pass, got %+v", res)
	}
}

// Segments from DIFFERENT holds are separate attempts: they never join, and
// the trial is scored on the last hold that heard something.
func TestHoldSegmentsDoNotJoinAcrossHolds(t *testing.T) {
	s := NewState()
	s.AutoAdvanceEnabled = func() bool { return true }
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("snap left", "bake"), nil, "alphabet")

	s.ProcessSpeechFrame("snap", false)
	s.HandleHoldEnd()
	s.ProcessSpeechFrame("left", false)
	s.HandleHoldEnd()
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 0 {
			t.Fatalf("two holds each with half the target must not pass, got index %d", g.CurrentIndex)
		}
		if heard := g.CurrentTrial.FinalHeard(); heard != "left" {
			t.Fatalf("FinalHeard = %q, want the last hold's words (%q)", heard, "left")
		}
	})

	// A silent hold after a heard one does not erase what was heard.
	s.HandleHoldEnd()
	s.WithLock(func(g *Game, _ bool) {
		if heard := g.CurrentTrial.FinalHeard(); heard != "left" {
			t.Fatalf("after a silent hold FinalHeard = %q, want %q", heard, "left")
		}
	})
}

// A hold that repeats or corrects itself is scored on its last try: only as
// many trailing segments join as the target has words.
func TestHoldSegmentsJoinOnlyTheLastTry(t *testing.T) {
	cases := []struct {
		target string
		frames []string
		want   string
	}{
		{"bake", []string{"bake", "bake"}, "bake"},
		{"bake", []string{"bike", "bake"}, "bake"},
		{"snap left", []string{"snap left", "snap left"}, "snap left"},
		{"snap left", []string{"snap", "left", "snap", "left"}, "snap left"},
		{"snap left", []string{"slap", "snap", "left"}, "snap left"},
		// Fewer words than the target: everything the hold heard.
		{"air bat cap", []string{"air", "bat"}, "air bat"},
		// A segment longer than the target is never cut.
		{"bake", []string{"bake", "the cake"}, "the cake"},
	}
	for _, tc := range cases {
		trial := &Trial{Target: tc.target}
		for _, f := range tc.frames {
			trial.Frames = append(trial.Frames, Frame{Text: f})
		}
		if got := trial.FinalHeard(); got != tc.want {
			t.Errorf("target %q, hold %q: FinalHeard = %q, want %q", tc.target, tc.frames, got, tc.want)
		}
	}
}

// A streaming-style hold (revising partials, then a final) is scored on its
// final frame, not on the partials joined.
func TestHoldWithFinalFrameScoresTheFinal(t *testing.T) {
	trial := &Trial{Target: "snap left", Frames: []Frame{
		{Text: "snap", Hold: 0},
		{Text: "snap left", IsFinal: true, Hold: 0},
	}}
	if heard := trial.FinalHeard(); heard != "snap left" {
		t.Fatalf("FinalHeard = %q, want the final frame (%q)", heard, "snap left")
	}
}
