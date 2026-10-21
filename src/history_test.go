package main

import (
	branchkit "github.com/branchkit/plugin-sdk-go"
	"strings"
	"testing"
	"time"
)

// recognition_log is off by default, so on most installs every
// matched command would be a refused append — and every refusal is a warn in
// platform.log. Voice reads the flag instead, and only appends when it's on.

func hostWithRecognitionLog(on bool) (*Host, *int) {
	h := newTestHost()
	resetState(h)
	calls := 0
	h.recognitionLogRecording = func() (bool, error) {
		calls++
		return on, nil
	}
	return h, &calls
}

func TestRecordMatchSkipsTheLogWhileRecordingIsOff(t *testing.T) {
	h, calls := hostWithRecognitionLog(false)
	worst := -0.4
	logged := captureStderr(t, func() {
		h.recordMatch(commandRecord{Phrase: "marigold zebra", Actions: []string{"placement.snap"}, Mode: "command_hold", Worst: &worst})
		h.recordMatch(commandRecord{Phrase: "quantum", Actions: []string{"placement.snap"}, Mode: "command_hold", Worst: &worst})
	})
	// The detached plugin fails every call, so an attempted append would log
	// "append failed". None means none was attempted.
	if strings.Contains(logged, "append failed") {
		t.Errorf("recording is off, but an append was attempted:\n%s", logged)
	}
	if *calls != 1 {
		t.Errorf("the flag should be read once and reused, read %d times", *calls)
	}
	// The History HUD still gets both, from memory.
	if got := len(h.snapshotHistory()); got != 2 {
		t.Errorf("history ring holds %d entries, want 2", got)
	}

	// Flagging one strikes it in memory and says so — even once recording is
	// on, since the match itself was never written and there is no record to
	// annotate.
	h.recognitionLogRecording = func() (bool, error) { return true, nil }
	h.recLogMu.Lock()
	h.recLogCheckedAt = time.Time{}
	h.recLogMu.Unlock()
	id := h.snapshotHistory()[0].ID
	logged = captureStderr(t, func() {
		h.handleMarkWrong(&struct {
			ID string `json:"id"`
		}{ID: id})
	})
	if !strings.Contains(logged, "memory only (match not recorded)") || strings.Contains(logged, "annotate wrong") {
		t.Errorf("a flag with recording off should stay in memory, logged:\n%s", logged)
	}
}

func TestRecordMatchAppendsWhileRecordingIsOn(t *testing.T) {
	h, _ := hostWithRecognitionLog(true)
	logged := captureStderr(t, func() {
		h.recordMatch(commandRecord{Phrase: "marigold", Actions: []string{"placement.snap"}, Mode: "command_hold"})
	})
	if !strings.Contains(logged, "recognition_log: append failed") {
		t.Errorf("recording is on, so the append should have been attempted:\n%s", logged)
	}
}

func TestRecognitionLogFlagIsReReadOnceStale(t *testing.T) {
	h, calls := hostWithRecognitionLog(false)
	h.recognitionLogOn()
	h.recognitionLogOn()
	if *calls != 1 {
		t.Fatalf("fresh flag re-read: %d reads", *calls)
	}
	h.recLogMu.Lock()
	h.recLogCheckedAt = time.Now().Add(-2 * recognitionLogCheckInterval)
	h.recLogMu.Unlock()
	h.recognitionLogOn()
	if *calls != 2 {
		t.Fatalf("a stale flag should be read again: %d reads", *calls)
	}
}

// The platform runs the actions after voice answers, so what they did reaches
// the History row later, joined on the turn's chain id. A
// refused command used to show as if it ran.
func TestExecutedReportJoinsTheRowByTrace(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	h.recordMatch(commandRecord{Phrase: "snap left", Actions: []string{"placement.snap"}, Mode: "command_hold", Trace: "tr_a"})
	h.recordMatch(commandRecord{Phrase: "copy that paste that", Actions: []string{"keyboard.copy", "keyboard.paste"}, Mode: "command_hold", Trace: "tr_b"})

	if label, _ := h.snapshotHistory()[0].failure(); label != "" {
		t.Fatalf("no report yet, so no failure; got %q", label)
	}
	if h.applyExecuted("tr_zzz", "ran", "", nil) {
		t.Fatal("an unknown trace must not join any row")
	}
	ok := h.applyExecuted("tr_a", "ran", "", []actionResult{
		{ActionType: "placement.snap", Status: "denied", Denial: "no grant for placement"},
	})
	if !ok {
		t.Fatal("report for tr_a did not join")
	}
	h.applyExecuted("tr_b", "ran", "", []actionResult{
		{ActionType: "keyboard.copy", Status: "ok"}, {ActionType: "keyboard.paste", Status: "ok"},
	})

	byPhrase := map[string]historyEntry{}
	for _, e := range h.snapshotHistory() {
		byPhrase[e.Phrase] = e
	}
	label, title := byPhrase["snap left"].failure()
	if label != "refused" || !strings.Contains(title, "no grant for placement") {
		t.Errorf("refused command: label=%q title=%q", label, title)
	}
	if label, _ := byPhrase["copy that paste that"].failure(); label != "" {
		t.Errorf("both actions ran ok, got failure %q", label)
	}
	if html := h.renderHistoryHTML(); !strings.Contains(html, "refused") || !strings.Contains(html, "keyboard.copy, keyboard.paste") {
		t.Errorf("panel should show the refusal and both actions:\n%s", html)
	}
}

// A drop is reported while voice is still deciding the turn, so the report
// arrives before the row it marks; recordMatch claims it (push review,
// 2026-10-08).
func TestDropReportedBeforeTheRowStillMarksIt(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	if h.applyExecuted("tr_late", "dropped", "'voice' did not answer in time", nil) {
		t.Fatal("no row yet, so nothing to join now")
	}
	h.recordMatch(commandRecord{Phrase: "scroll down", Actions: []string{"browser.scroll"}, Mode: "command_hold", Trace: "tr_late"})
	if label, _ := h.snapshotHistory()[0].failure(); label != "didn't run" {
		t.Errorf("the waiting drop report should mark the row, got %q", label)
	}
	h.recordMatch(commandRecord{Phrase: "scroll up", Actions: []string{"browser.scroll"}, Mode: "command_hold", Trace: "tr_late"})
	if label, _ := h.snapshotHistory()[0].failure(); label != "" {
		t.Errorf("a report is claimed once, got %q on a second row", label)
	}
}

// A spoken command that cannot be undone is held for the person to say go
// ahead: first reported "asking", then on the same chain "ran" or
// "cancelled". Neither is a failure to announce.
func TestAHeldCommandIsAskingThenRanOrCancelled(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	h.recordMatch(commandRecord{Phrase: "close window", Actions: []string{"placement.close"}, Mode: "command_hold", Trace: "tr_h"})
	h.applyExecuted("tr_h", "asking", "", []actionResult{{ActionType: "placement.close", Status: "asking"}})
	e := h.snapshotHistory()[0]
	if label, _ := e.failure(); label != "" || e.problemPhrase() != "" {
		t.Fatalf("asking is not a failure, got %q", label)
	}
	if label, _ := e.held(); label != "asking" {
		t.Fatalf("held label %q", label)
	}
	h.applyExecuted("tr_h", "cancelled", "you cancelled it", []actionResult{{ActionType: "placement.close", Status: "cancelled"}})
	e = h.snapshotHistory()[0]
	if label, _ := e.held(); label != "cancelled" {
		t.Fatalf("held label %q", label)
	}
	if !strings.Contains(h.renderHistoryHTML(), ">cancelled<") {
		t.Errorf("the row should say it was cancelled:\n%s", h.renderHistoryHTML())
	}
	h.applyExecuted("tr_h", "ran", "", []actionResult{{ActionType: "placement.close", Status: "ok"}})
	if label, _ := h.snapshotHistory()[0].held(); label != "" {
		t.Errorf("it ran, so nothing is held: %q", label)
	}
}

func TestDroppedTurnMarksTheRow(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	h.recordMatch(commandRecord{Phrase: "scroll down", Actions: []string{"browser.scroll"}, Mode: "command_hold", Trace: "tr_d"})
	h.applyExecuted("tr_d", "dropped", "voice did not answer in time", nil)
	label, title := h.snapshotHistory()[0].failure()
	if label != "didn't run" || !strings.Contains(title, "did not answer") {
		t.Errorf("dropped turn: label=%q title=%q", label, title)
	}
}

// A command heard and resolved to nothing now leaves a row; asleep, misses
// are ambient speech and do not.
func TestUnresolvedCommandsLeaveARow(t *testing.T) {
	for _, c := range []struct {
		mode, stop string
		want       bool
	}{
		{"command_hold", "miss", true},
		{"continuous_awake", "tie", true},
		{"command_hold", "blocked", true},
		{"command_hold", "partial", false},
		{"command_hold", "", false},
		{"continuous_sleeping", "miss", false},
	} {
		if got := recordsUnresolved(c.mode, c.stop); got != c.want {
			t.Errorf("recordsUnresolved(%q, %q) = %v, want %v", c.mode, c.stop, got, c.want)
		}
	}
	h, _ := hostWithRecognitionLog(false)
	h.recordUnresolved(commandRecord{Phrase: "snap lift", Mode: "command_hold", Stop: "miss", Unresolved: []string{"snap", "lift"}, Trace: "tr_m"})
	html := h.renderHistoryHTML()
	if !strings.Contains(html, "snap lift") || !strings.Contains(html, "no match") {
		t.Errorf("a miss should be a row marked no match:\n%s", html)
	}
	if strings.Contains(html, `class="hist-flag"`) {
		t.Error("a miss ran nothing, so it offers no 'that was wrong' flag")
	}
	if h.applyExecuted("tr_m", "ran", "", nil) {
		t.Error("a report never joins a row that handed the platform no actions")
	}
}

// A command that failed is said to have failed, in the person's words, once
// the platform's report arrives; one that worked is not
// announced again.
func TestFailedCommandIsAnnounced(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	h.recordMatch(commandRecord{Phrase: "snap left", Actions: []string{"placement.snap"}, Mode: "command_hold", Trace: "tr_f"})
	h.recordMatch(commandRecord{Phrase: "copy that", Actions: []string{"keyboard.copy"}, Mode: "command_hold", Trace: "tr_ok"})

	logged := captureStderr(t, func() {
		e, _ := h.joinExecuted("tr_ok", "ran", "", []actionResult{{ActionType: "keyboard.copy", Status: "ok"}})
		h.announceFailure(e)
	})
	if strings.Contains(logged, "command failed: announced") {
		t.Errorf("a command that worked must not be announced as failed:\n%s", logged)
	}

	logged = captureStderr(t, func() {
		e, _ := h.joinExecuted("tr_f", "ran", "", []actionResult{
			{ActionType: "placement.snap", Status: "denied", Denial: "not authorized to dispatch to 'placement'"},
		})
		if got := e.problemPhrase(); got != "“snap left” was refused: not authorized to dispatch to 'placement'" {
			t.Errorf("problem phrase: %q", got)
		}
		h.announceFailure(e)
	})
	if !strings.Contains(logged, "command failed: announced") {
		t.Errorf("a refused command should be announced:\n%s", logged)
	}
}

// A drop reported before its row is announced when the row claims it.
func TestDropClaimedByTheRowIsAnnounced(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	h.applyExecuted("tr_x", "dropped", "'voice' did not answer", nil)
	logged := captureStderr(t, func() {
		h.recordMatch(commandRecord{Phrase: "scroll down", Actions: []string{"browser.scroll"}, Mode: "command_hold", Trace: "tr_x"})
	})
	if !strings.Contains(logged, "command failed: announced") {
		t.Errorf("the claimed drop should be announced:\n%s", logged)
	}
	if got := h.snapshotHistory()[0].problemPhrase(); got != "“scroll down” didn't run: 'voice' did not answer" {
		t.Errorf("problem phrase: %q", got)
	}
}

func TestProblemPhraseCoversEveryFailure(t *testing.T) {
	for _, c := range []struct {
		r    actionResult
		want string
	}{
		{actionResult{Status: "not_handled", Handler: "tiling"}, "“go” was not handled by tiling"},
		{actionResult{Status: "error"}, "“go” failed"},
		{actionResult{Status: "denied"}, "“go” was refused"},
		{actionResult{Status: "simulated"}, ""},
	} {
		e := historyEntry{Phrase: "go", Actions: []string{"x.y"}, Results: []actionResult{c.r}}
		if got := e.problemPhrase(); got != c.want {
			t.Errorf("%+v: got %q want %q", c.r, got, c.want)
		}
	}
}

// A command that fails while the key is still down is said at the release:
// the release clears the discovery channel, which used to take the failure
// with it before a boundary renderer said it.
func TestFailureDuringAHoldSurvivesTheRelease(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	phase := func(p string) *branchkit.OnActionRequest {
		return &branchkit.OnActionRequest{Phase: &p}
	}
	if _, err := h.handleVoiceCommand(phase("start")); err != nil {
		t.Fatal(err)
	}
	if !h.sess.CommandHoldActive() {
		t.Fatal("the start phase should begin a command hold")
	}
	h.recordMatch(commandRecord{Phrase: "snap left", Actions: []string{"placement.snap"}, Mode: "command_hold", Trace: "tr_h"})
	e, _ := h.joinExecuted("tr_h", "ran", "", []actionResult{{ActionType: "placement.snap", Status: "not_handled", Handler: "placement"}})
	h.announceFailure(e)

	logged := captureStderr(t, func() {
		if _, err := h.handleVoiceCommand(phase("stop")); err != nil {
			t.Fatal(err)
		}
	})
	if got := h.sess.TakeLastProblem(); got != "" {
		t.Errorf("the release should have taken the hold's failure, %q is left", got)
	}
	if strings.Contains(logged, "panic") {
		t.Errorf("stop: %s", logged)
	}
}

// An aborted hold says its failure too.
func TestFailureDuringAnAbortedHoldIsSaid(t *testing.T) {
	h, _ := hostWithRecognitionLog(false)
	phase := func(p string) *branchkit.OnActionRequest { return &branchkit.OnActionRequest{Phase: &p} }
	if _, err := h.handleVoiceCommand(phase("start")); err != nil {
		t.Fatal(err)
	}
	h.recordMatch(commandRecord{Phrase: "snap left", Actions: []string{"placement.snap"}, Mode: "command_hold", Trace: "tr_a"})
	e, _ := h.joinExecuted("tr_a", "ran", "", []actionResult{{ActionType: "placement.snap", Status: "error"}})
	h.announceFailure(e)
	if _, err := h.handleVoiceCommand(phase("abort")); err != nil {
		t.Fatal(err)
	}
	if got := h.sess.TakeLastProblem(); got != "" {
		t.Errorf("the abort should have said the hold's failure, %q is left", got)
	}
}
