package calibration

import "testing"

// catalogCmd is a base command (no slots / tags / dynamic) for the catalog.
func catalogCmd(id, owner, pattern string) EnumeratedCommand {
	return EnumeratedCommand{ID: id, Owner: owner, Pattern: pattern}
}

// liveState wires a State with a fixed command set, a capturing Append, and a
// linear NextCommandID over the given ids (chaining order = the order passed).
func liveState(t *testing.T, ids ...string) (*State, *appendLog) {
	t.Helper()
	s := NewState()
	enum := make([]EnumeratedCommand, len(ids))
	for i, id := range ids {
		enum[i] = catalogCmd(id, "browser", id)
	}
	s.EnumerateCommands = func() []EnumeratedCommand { return enum }
	next := map[string]string{}
	for i := 0; i+1 < len(ids); i++ {
		next[ids[i]] = ids[i+1]
	}
	s.NextCommandID = func(cur, _ string) string { return next[cur] }
	log := &appendLog{}
	s.Append = func(collection string, payload any) error {
		log.calls = append(log.calls, appendCall{collection, payload})
		return nil
	}
	return s, log
}

type appendCall struct {
	collection string
	payload    any
}

type appendLog struct{ calls []appendCall }

func (l *appendLog) count(collection string) int {
	n := 0
	for _, a := range l.calls {
		if a.collection == collection {
			n++
		}
	}
	return n
}

const (
	attemptsColl = "plugin.voice.verification_attempts"
	sessionsColl = "plugin.voice.verification_sessions"
)

// Activating a catalog command starts a single-command walk in the chaining
// posture, with the cursor on that command.
func TestActivateCommand_StartsWalk(t *testing.T) {
	s, _ := liveState(t, "scroll down", "scroll up")

	active, redirect := s.ActivateCommand("scroll down", "", "")
	if !active || !redirect {
		t.Fatalf("ActivateCommand = (active=%v, redirect=%v), want (true, true)", active, redirect)
	}
	s.WithLock(func(g *Game, _ bool) {
		if !g.IsRunning() {
			t.Fatal("walk should be running after activation")
		}
		if got := g.currentCommandID(); got != "scroll down" {
			t.Fatalf("active command = %q, want scroll down", got)
		}
		if len(g.Words) != 1 {
			t.Fatalf("single-command walk should have 1 target, got %d", len(g.Words))
		}
	})
	if s.ActiveCommandID() != "scroll down" {
		t.Errorf("ActiveCommandID = %q, want scroll down", s.ActiveCommandID())
	}
}

// Activating an unknown command records nothing and leaves the walk untouched.
func TestActivateCommand_Unknown_NoOp(t *testing.T) {
	s, _ := liveState(t, "scroll down")
	active, redirect := s.ActivateCommand("nope", "", "")
	if active || redirect {
		t.Errorf("activating an unknown command should be a no-op, got (active=%v, redirect=%v)", active, redirect)
	}
}

// Exhausting one command's targets appends the next filtered catalog command
// to the SAME walk (chaining by append) — the cursor lands on it, the queue
// grew, and the sitting stays active.
func TestChaining_AppendsNextCommand(t *testing.T) {
	s, _ := liveState(t, "scroll down", "scroll up")
	s.ActivateCommand("scroll down", "", "")

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll down", true) })
	s.HandleCalibrate("advance", "")

	s.WithLock(func(g *Game, _ bool) {
		if !g.IsRunning() {
			t.Fatal("chaining should keep the walk running, not finish it")
		}
		if got := g.currentCommandID(); got != "scroll up" {
			t.Fatalf("after exhausting scroll down, cursor should chain to scroll up, got %q", got)
		}
		if len(g.Words) != 2 {
			t.Fatalf("queue should have grown to 2 by appending, got %d", len(g.Words))
		}
	})
}

// Chaining stops (the walk finishes) when no next command remains.
func TestChaining_StopsAtEndOfCatalog(t *testing.T) {
	s, calls := liveState(t, "scroll up") // only command → no next
	s.ActivateCommand("scroll up", "", "")

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll up", true) })
	s.HandleCalibrate("advance", "")

	s.WithLock(func(g *Game, _ bool) {
		if g.Phase != PhaseFinished {
			t.Fatalf("reaching the end of the catalog should finish the walk, phase=%v", g.Phase)
		}
	})
	if calls.count(sessionsColl) != 1 {
		t.Errorf("a finished sitting should write exactly one session record, got %d", calls.count(sessionsColl))
	}
}

// A bulk walk (chaining off) finishes when its pre-built queue runs out — it
// does not chain into the catalog.
func TestBulkWalk_DoesNotChain(t *testing.T) {
	s, _ := liveState(t, "scroll down", "scroll up")
	s.StartWalk(targetsForWords("arch"), nil, "alphabet") // chaining=false

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("arch", true) })
	s.HandleCalibrate("advance", "")

	s.WithLock(func(g *Game, _ bool) {
		if g.Phase != PhaseFinished {
			t.Fatalf("a bulk walk should finish at the end of its queue, phase=%v", g.Phase)
		}
	})
}

// Attempts persist incrementally as the walk advances — each completed trial is
// written immediately (live per-command health), not batched at finish. The
// OnAttemptsChanged hook fires so the sidebar catalog can refresh.
func TestIncrementalPersistence_WritesPerAdvance(t *testing.T) {
	s, calls := liveState(t, "scroll down", "scroll up")
	refreshes := 0
	s.OnAttemptsChanged = func() { refreshes++ }
	s.ActivateCommand("scroll down", "", "")

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll down", true) })
	s.HandleCalibrate("advance", "") // exhausts scroll down → chains to scroll up

	if got := calls.count(attemptsColl); got != 1 {
		t.Fatalf("first attempt should persist live (before finish), got %d attempts", got)
	}
	if calls.count(sessionsColl) != 0 {
		t.Errorf("no session record should be written mid-sitting, got %d", calls.count(sessionsColl))
	}
	if refreshes == 0 {
		t.Error("OnAttemptsChanged should fire when an attempt persists")
	}

	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll up", true) })
	s.HandleCalibrate("advance", "") // exhausts scroll up → end of catalog → finish

	if got := calls.count(attemptsColl); got != 2 {
		t.Fatalf("second attempt should also persist, got %d total", got)
	}
	if got := calls.count(sessionsColl); got != 1 {
		t.Fatalf("closing the sitting should write one session record, got %d", got)
	}
}

// All attempts of a chained sitting share one session id.
func TestChainedSitting_SharesOneSessionID(t *testing.T) {
	s, calls := liveState(t, "scroll down", "scroll up")
	s.ActivateCommand("scroll down", "", "")
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll down", true) })
	s.HandleCalibrate("advance", "")
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll up", true) })
	s.HandleCalibrate("advance", "")

	var ids []string
	var sessionID string
	for _, a := range calls.calls {
		switch p := a.payload.(type) {
		case *VerificationAttempt:
			ids = append(ids, p.SessionID)
		case *VerificationSession:
			sessionID = p.ID
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 attempt records, got %d", len(ids))
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Errorf("chained attempts should share one session id, got %q and %q", ids[0], ids[1])
	}
	if sessionID != ids[0] {
		t.Errorf("session record id %q should match the attempts' session id %q", sessionID, ids[0])
	}
}

// The command engine emits the recognized word as a non-final segment and
// signals end-of-hold only by stopping the pipeline on key-up (HandleHoldEnd),
// never an is_final transcript frame. Auto-advance must fire on that key-up
// boundary when the segment already turned the trial green — this is the bug the
// live walk hit: green showed but nothing advanced.
func TestAutoAdvance_KeyUpBoundary_Advances(t *testing.T) {
	s := NewState()
	s.AutoAdvanceEnabled = func() bool { return true }
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("arch", "bake"), nil, "alphabet")

	s.ProcessSpeechFrame("arch", false) // recognized segment → green, must NOT advance yet
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 0 {
			t.Fatalf("a recognized segment must not advance before the hold ends, index=%d", g.CurrentIndex)
		}
	})
	s.HandleHoldEnd() // Option+G up → pipeline stopped → hold boundary
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 1 {
			t.Fatalf("key-up hold boundary should auto-advance the green trial, index=%d", g.CurrentIndex)
		}
	})
}

// The group rail's click jumps the walk cursor: a forward jump skip-marks the
// passed-over targets; a backward jump truncates results so they re-walk.
func TestGotoTarget_JumpsCursor(t *testing.T) {
	s := NewState()
	s.SetTabVisible(true)
	s.StartWalk(targetsForWords("arch", "bake", "cusp", "dune"), nil, "alphabet")

	s.GotoTarget(2) // 0 -> 2, skip-marks arch + bake
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 2 {
			t.Fatalf("forward goto should land on index 2, got %d", g.CurrentIndex)
		}
		if len(g.Results) != 2 {
			t.Fatalf("forward goto should skip-mark the 2 passed targets, got %d results", len(g.Results))
		}
	})

	s.GotoTarget(1) // 2 -> 1, truncates results to re-walk bake
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentIndex != 1 {
			t.Fatalf("back goto should land on index 1, got %d", g.CurrentIndex)
		}
		if len(g.Results) != 1 {
			t.Fatalf("back goto should truncate results to 1, got %d", len(g.Results))
		}
	})
}

// A dynamic command whose owner supplies calibration samples walks those
// prompts (grouped under the pattern) instead of the codeword stand-in — this
// is how a third-party dynamic command gets the hint-badges treatment. Absent
// owner samples, expansion falls back to the codeword path (here: unchanged,
// since the test wires no codewords).
func TestExpandDynamic_PrefersOwnerSamples(t *testing.T) {
	s := NewState()
	s.CommandSamples = func(id string) []string {
		if id == "peripheral:tap <button>" {
			return []string{"tap red", "tap blue"}
		}
		return nil
	}
	dyn := func(id string) Target {
		return Target{CommandID: id, Owner: "peripheral", Pattern: id, Tier: TierDynamic, Prompt: id}
	}

	out := s.expandDynamicTargets([]Target{dyn("peripheral:tap <button>")}, false)
	if len(out) != 2 || out[0].Prompt != "tap red" || out[1].Prompt != "tap blue" {
		t.Fatalf("owner samples should drive the group, got %+v", out)
	}
	if out[0].Group != "peripheral:tap <button>" {
		t.Errorf("owner-sample targets should be grouped under the pattern, got %q", out[0].Group)
	}

	// No owner samples + no codewords wired → falls back, leaving the dynamic
	// target as-is (rather than erroring or dropping it).
	out = s.expandDynamicTargets([]Target{dyn("browser:other")}, false)
	if len(out) != 1 || out[0].Prompt != "browser:other" {
		t.Fatalf("absent owner samples should fall back to the codeword path, got %+v", out)
	}
}

// A free-text slotted command (no list to expand) whose owner supplies samples
// walks those prompts (grouped) instead of the single generic filler — the same
// hook that backs dynamic commands, now reaching free-text slots.
func TestExpandListSlot_FreeTextUsesOwnerSamples(t *testing.T) {
	s := NewState()
	s.CommandSamples = func(id string) []string {
		if id == "browser:search <text>" {
			return []string{"new york", "berlin"}
		}
		return nil
	}
	ft := Target{CommandID: "browser:search <text>", Owner: "browser", Pattern: "search <text>", Tier: TierSlotted, Prompt: "search sample"}

	out := s.expandListSlotTargets([]Target{ft})
	if len(out) != 2 || out[0].Prompt != "new york" || out[1].Prompt != "berlin" {
		t.Fatalf("free-text owner samples should drive the walk, got %+v", out)
	}
	if out[0].Group != "search <text>" {
		t.Errorf("free-text samples should be grouped under the pattern, got %q", out[0].Group)
	}

	// No owner samples → passthrough with the generic filler (unchanged).
	ft.CommandID = "browser:other <text>"
	out = s.expandListSlotTargets([]Target{ft})
	if len(out) != 1 || out[0].Prompt != "search sample" {
		t.Fatalf("absent owner samples should keep the filler, got %+v", out)
	}
}

// Stop closes the sitting: persists, summarizes, and returns to the catalog
// (Finished phase) rather than opening a pause menu.
func TestStop_ClosesSitting(t *testing.T) {
	s, calls := liveState(t, "scroll down", "scroll up")
	s.ActivateCommand("scroll down", "", "")
	s.WithLock(func(g *Game, _ bool) { g.ProcessFrame("scroll down", true) })

	s.HandleCalibrate("stop", "")

	s.WithLock(func(g *Game, _ bool) {
		if g.Phase != PhaseFinished {
			t.Fatalf("Stop should finish the sitting, phase=%v", g.Phase)
		}
	})
	if calls.count(sessionsColl) != 1 {
		t.Errorf("Stop should write one session record, got %d", calls.count(sessionsColl))
	}
}
