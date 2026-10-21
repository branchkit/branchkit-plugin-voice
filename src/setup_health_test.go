package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMicVerdictReadsSilenceAsAFailure(t *testing.T) {
	db := func(v float64) *float64 { return &v }
	if r, note := micVerdict(db(-32)); r != checkPass || note != "" {
		t.Fatalf("speech: got %q %q", r, note)
	}
	// A quiet room is not silence.
	if r, _ := micVerdict(db(-78)); r != checkPass {
		t.Fatalf("quiet room: got %q", r)
	}
	if r, note := micVerdict(db(-120)); r != checkFail || !strings.Contains(note, "different microphone") {
		t.Fatalf("digital silence: got %q %q", r, note)
	}
	if r, note := micVerdict(nil); r != checkFail || !strings.Contains(note, "No sound reached") {
		t.Fatalf("no audio at all: got %q %q", r, note)
	}
}

func TestTryStepSaysWhatWorksAndWhatIsLeft(t *testing.T) {
	cases := []struct {
		r       healthResults
		state   string
		summary string
		report  bool
	}{
		{healthResults{}, "", "", false},
		{healthResults{Dictation: checkPass}, setupStateInProgress, "Dictation works · microphone and commands not tried yet", true},
		{healthResults{Mic: checkPass, Dictation: checkPass}, setupStateInProgress, "Microphone and dictation work · commands not tried yet", true},
		{healthResults{Mic: checkPass, Dictation: checkPass, Commands: checkPass}, setupStateDone, "Microphone, dictation and commands work", true},
		// A silent hold reopens the step, even after everything worked.
		{healthResults{Mic: checkFail, Dictation: checkPass, Commands: checkPass}, setupStateNeedsYou, "Your microphone is silent", true},
	}
	for _, c := range cases {
		state, summary, report := tryStep(c.r)
		if state != c.state || summary != c.summary || report != c.report {
			t.Errorf("%+v: got (%q, %q, %v), want (%q, %q, %v)", c.r, state, summary, report, c.state, c.summary, c.report)
		}
	}
}

func TestHealthResultsSurviveARestart(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	var sent []string
	h := &Host{setup: &setupReporter{send: func(step, state, summary string) error {
		sent = append(sent, step+"="+state)
		return nil
	}}}
	h.setup.setModels(setupStateDone, "Large · 1.6 GB · commands on")
	h.markTried()
	h.markTried()
	if !reflect.DeepEqual(sent, []string{"start=done"}) {
		t.Fatalf("a dictation reports once: %v", sent)
	}
	if !h.health.recordMic(checkFail, "silent") {
		t.Fatal("a failure is a change")
	}
	if h.health.recordMic(checkFail, "silent") {
		t.Fatal("the same failure again is not")
	}

	h2 := &Host{}
	got := h2.health.snapshot(time.Now()).results
	if got != (healthResults{Mic: checkFail, Dictation: checkPass}) {
		t.Fatalf("results survive a restart: %+v", got)
	}
	// What was said is never saved.
	b, _ := os.ReadFile(healthPath())
	if strings.Contains(string(b), "silent") {
		t.Fatalf("only results are saved: %s", b)
	}
}

func TestCommandCheckScoresWithoutRunning(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	var s setupHealth
	now := time.Unix(1000, 0)
	if s.intercepts(now) {
		t.Fatal("not listening until asked")
	}
	s.startListening(now)
	if !s.intercepts(now) || !s.snapshot(now).listening {
		t.Fatal("listening once asked")
	}

	// A miss keeps listening and says what was heard.
	if s.heardCommand(now, "scroll down", true) {
		t.Fatal("another command is not the check")
	}
	if v := s.snapshot(now); v.heard != "scroll down" || !v.listening || v.results.Commands != "" {
		t.Fatalf("miss: %+v", v)
	}
	// The phrase, heard but not matched, is not a pass either.
	if s.heardCommand(now, "show help", false) {
		t.Fatal("unmatched is not a pass")
	}

	// The phrase, matched: a pass. The rest of that hold is still
	// swallowed, then commands run again.
	if !s.heardCommand(now, "show help", true) {
		t.Fatal("matched phrase passes")
	}
	if v := s.snapshot(now); v.results.Commands != checkPass || v.listening {
		t.Fatalf("pass: %+v", v)
	}
	if !s.intercepts(now.Add(time.Second)) {
		t.Fatal("a trailing word in the same hold is swallowed")
	}
	if s.heardCommand(now.Add(time.Second), "close window", true) {
		t.Fatal("the grace scores nothing")
	}
	if s.intercepts(now.Add(commandCheckGrace + time.Millisecond)) {
		t.Fatal("after the grace, commands run again")
	}

	// Left alone, listening lapses.
	s.startListening(now)
	if s.intercepts(now.Add(commandCheckWindow + time.Millisecond)) {
		t.Fatal("listening lapses")
	}
}

// The command check asks for a phrase only voice's own commands answer,
// live in every context, so the check needs no other plugin.
func TestCommandCheckPhraseIsAnUngatedVoiceCommand(t *testing.T) {
	raw, err := os.ReadFile("../commands.json")
	if err != nil {
		t.Fatalf("read commands.json: %v", err)
	}
	var entries []struct {
		Pattern      []json.RawMessage `json:"pattern"`
		RequiresTags []string          `json:"requires_tags"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse commands.json: %v", err)
	}
	ungated := map[string]bool{}
	for _, e := range entries {
		if len(e.RequiresTags) > 0 {
			continue
		}
		for _, phrase := range expandPattern(t, e.Pattern) {
			ungated[phrase] = true
		}
	}
	if !commandCheckAccepts[commandCheckPhrase] {
		t.Fatalf("the asked-for phrase %q is not accepted", commandCheckPhrase)
	}
	for phrase := range commandCheckAccepts {
		if !ungated[phrase] {
			t.Errorf("%q is not an ungated voice command", phrase)
		}
	}
}

// A silent hold shows the notice once, then not again inside the spacing
// window: a muted mic makes every hold silent.
func TestSilentMicNoticeIsSpaced(t *testing.T) {
	h := newTestHost()
	h.noticeSilentMic("first")
	first := h.health.silentNoticeAt
	if first.IsZero() {
		t.Fatal("the first silent hold must show the notice")
	}
	h.noticeSilentMic("second")
	if !h.health.silentNoticeAt.Equal(first) {
		t.Fatal("a second silent hold inside the window must not show it again")
	}
	h.health.silentNoticeAt = time.Now().Add(-silentMicNoticeEvery - time.Second)
	h.noticeSilentMic("third")
	if h.health.silentNoticeAt.Before(time.Now().Add(-time.Second)) {
		t.Fatal("after the window the notice shows again")
	}
}
