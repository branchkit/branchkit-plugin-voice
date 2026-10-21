package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	"github.com/branchkit/branchkit-plugin-voice/session"
	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/plugin-sdk-go"
)

func resetState(h *Host) {
	h.calib = calibration.NewState()
	h.edState = &editorState{}
	h.sess = session.New(h.hud, h.speech, func() {})
}

func postAction(h *Host, t *testing.T, action string) branchkit.OnActionResponse {
	t.Helper()
	return postActionWith(h, t, action, nil, nil)
}

func postActionWithPhase(h *Host, t *testing.T, action string, phase string) branchkit.OnActionResponse {
	t.Helper()
	return postActionWith(h, t, action, &phase, nil)
}

func postActionWith(h *Host, t *testing.T, action string, phase *string, params *json.RawMessage) branchkit.OnActionResponse {
	t.Helper()
	req := &branchkit.OnActionRequest{Action: action, Phase: phase}
	if params != nil {
		req.Params = *params
	}
	handler, ok := actionHandlersForTest(h)[action]
	if !ok {
		t.Fatalf("postAction: no handler registered for %q", action)
	}
	result, err := handler(req)
	if err != nil {
		t.Fatalf("handler %s error: %v", action, err)
	}
	return result.(branchkit.OnActionResponse)
}

// actionHandlersForTest is derived from actions.go's actionRegistrations so
// adding a new per-action handler in one place picks it up in the test
// dispatcher automatically — no manual sync required.
func actionHandlersForTest(h *Host) map[string]branchkit.ActionHandlerFunc {
	m := make(map[string]branchkit.ActionHandlerFunc, len(h.actionRegistrations()))
	for _, r := range h.actionRegistrations() {
		m[r.Action] = r.Handler
	}
	return m
}

// Speak mode now uses pipelines instead of control messages.
// Start activates the dictation pipeline + command_recognition for stop-phrase detection.
// Stop/abort halts both pipelines. No control messages returned.

func TestSpeakStartViaPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	resp := postActionWithPhase(h, t, "voice.speak", "start")
	if resp.Status != branchkit.OnActionStatusOk {
		t.Fatalf("expected ok, got %s", resp.Status)
	}
	if !h.sess.GetSpeakDictating() {
		t.Fatal("expected speakDictating=true after speak start")
	}
}

func TestSpeakStopViaPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.speak", "start")
	resp := postActionWithPhase(h, t, "voice.speak", "stop")
	if resp.Status != branchkit.OnActionStatusOk {
		t.Fatalf("expected ok, got %s", resp.Status)
	}
	if h.sess.GetSpeakDictating() {
		t.Fatal("expected speakDictating=false after speak stop")
	}
}

func TestSpeakStartAbsentPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	_ = postAction(h, t, "voice.speak")
	if !h.sess.GetSpeakDictating() {
		t.Fatal("expected speakDictating=true after speak start")
	}
}

// --- Abort phase tests ---

func TestSpeakAbortPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.speak", "start")
	_ = postActionWithPhase(h, t, "voice.speak", "abort")
	if h.sess.GetSpeakDictating() {
		t.Fatal("expected speakDictating=false after speak abort")
	}
}

func TestDictationAbortPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	_ = postActionWithPhase(h, t, "voice.dictation", "abort")
	// Pipeline path returns empty control message (pipelines.stop handles cleanup)
}

func TestCommandAbortPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.command", "start")
	_ = postActionWithPhase(h, t, "voice.command", "abort")
}

// --- Dictation phase-aware tests ---

func TestDictationStartPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	_ = postActionWithPhase(h, t, "voice.dictation", "start")
	// Pipeline path returns empty control message (pipelines.run handles start)
}

// waitPast sleeps just beyond a timing window, so a test drives a press a
// human could produce rather than one inside a debounce or tap window.
func waitPast(ms int) { time.Sleep(time.Duration(ms+40) * time.Millisecond) }

// A tap on its own types, like any short hold: it no longer latches the
// recording on. It finishes once the double-tap window passes.
func TestDictationSingleTapFinishesAfterTheWindow(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "stop")
	if h.sess.GetDictationToggleActive() {
		t.Fatal("a single tap must not latch")
	}
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineRecording {
		t.Fatalf("within the double-tap window the recording continues, got %s", got)
	}
	waitPast(doubleTapWindowMS)
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineDraining {
		t.Fatalf("a lone tap must finish after the window, got %s", got)
	}
}

// Two taps latch: the recording continues past both releases and the next
// press stops it.
func TestDictationDoubleTapLatchesUntilTheNextPress(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "stop")
	waitPast(dictationBounceMS) // a real second tap, not chatter
	postActionWithPhase(h, t, "voice.dictation", "start")
	if !h.sess.GetDictationToggleActive() {
		t.Fatal("the second tap must latch")
	}
	postActionWithPhase(h, t, "voice.dictation", "stop") // second tap's release
	waitPast(doubleTapWindowMS)
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineRecording {
		t.Fatalf("a latched dictation keeps recording past both releases, got %s", got)
	}
	if h.sess.GetCurrentMode() != "dictation" {
		t.Fatalf("expected mode dictation while latched, got %s", h.sess.GetCurrentMode())
	}

	waitPast(dictationBounceMS)
	postActionWithPhase(h, t, "voice.dictation", "start")
	if h.sess.GetDictationToggleActive() {
		t.Fatal("the next press must clear the latch")
	}
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineDraining {
		t.Fatalf("expected draining after the stopping press, got %s", got)
	}
	// That press's own release must not disturb the draining session.
	postActionWithPhase(h, t, "voice.dictation", "stop")
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineDraining {
		t.Fatalf("expected drain undisturbed by the stopping press's release, got %s", got)
	}
}

// A chattering key sends up→down within a few milliseconds on ONE physical
// press. Observed live on a Kinesis Adv360 Pro: a 60-second hold produced a
// 58-millisecond session. Chatter right after a tap's release is the key still
// held, not a second tap: it must neither latch nor stop.
func TestDictationIgnoresKeyBounceAfterATap(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "stop")  // bounce release
	postActionWithPhase(h, t, "voice.dictation", "start") // bounce press, at once
	if h.sess.GetDictationToggleActive() {
		t.Fatal("chatter must not latch")
	}
	waitPast(doubleTapWindowMS)
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineRecording {
		t.Fatalf("the key is still held: recording must continue, got %s", got)
	}
}

// Chatter right after the latch is not the stopping press either.
func TestDictationIgnoresKeyBounceAfterLatch(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "stop")
	waitPast(dictationBounceMS)
	postActionWithPhase(h, t, "voice.dictation", "start") // latches
	postActionWithPhase(h, t, "voice.dictation", "stop")
	postActionWithPhase(h, t, "voice.dictation", "start") // chatter, at once
	if !h.sess.GetDictationToggleActive() {
		t.Fatal("chatter after the latch must leave it standing")
	}
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineRecording {
		t.Fatalf("recording must continue through chatter, got %s", got)
	}
}

func TestFinishDictationClearsSpeakFlags(t *testing.T) {
	h := newTestHost()
	resetState(h)
	// A dropped transcript (drain timeout / stopped-event finish) must not
	// leave the speak flags behind to misroute the next session's transcript.
	h.sess.SetSpeakDictating(true) // sets speakTranscriptPending
	h.sess.SetSpeakDictating(false)
	h.sess.StartDictationRecording()
	h.sess.DrainDictation()
	h.sess.FinishDictationIfDraining()
	if h.sess.GetSpeakTranscriptPending() {
		t.Fatal("expected speakTranscriptPending cleared by finish")
	}
	if h.sess.ConsumeTranscriptDiscard() {
		t.Fatal("expected speakTranscriptDiscard cleared by finish")
	}
}

func TestFinishDictationIfDrainingIgnoresRecording(t *testing.T) {
	h := newTestHost()
	resetState(h)
	// A stale pipeline.stopped (or drain backstop) from the PREVIOUS session
	// must not clobber a recording that started after it.
	h.sess.StartDictationRecording()
	h.sess.FinishDictationIfDraining()
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineRecording {
		t.Fatalf("expected recording untouched, got %s", got)
	}
}

func TestDictationStopPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	// A fast stop is a tap: no control message either way.
	postActionWithPhase(h, t, "voice.dictation", "start")
	_ = postActionWithPhase(h, t, "voice.dictation", "stop")
}

// --- Command phase-aware tests ---

func TestCommandStartPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	_ = postActionWithPhase(h, t, "voice.command", "start")
}

func TestCommandStopPhase(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.command", "start")
	_ = postActionWithPhase(h, t, "voice.command", "stop")
}

func TestCommandStopWhenNotActiveIsIgnored(t *testing.T) {
	h := newTestHost()
	resetState(h)
	_ = postActionWithPhase(h, t, "voice.command", "stop")
}

// --- Continuous mode pipeline tests ---

func TestContinuousToggleEntersSleeping(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	if mode := h.sess.GetCurrentMode(); mode != "continuous_sleeping" {
		t.Fatalf("expected continuous_sleeping, got %s", mode)
	}
}

func TestContinuousToggleExitsToIdle(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	// The handler debounces toggles within 200ms. This test is about the
	// second toggle exiting, not about the debounce, so age the first one.
	// (While the clock was a package variable this passed only when an
	// earlier test happened to have run more than 200ms before.)
	h.lastContinuousToggle = time.Time{}
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	if mode := h.sess.GetCurrentMode(); mode != "idle" {
		t.Fatalf("expected idle after second toggle, got %s", mode)
	}
}

func TestContinuousToggleBlockedDuringCommandHold(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	if h.sess.IsContinuousActive() {
		t.Fatal("continuous should not activate while command is held")
	}
	if mode := h.sess.GetCurrentMode(); mode != "command_hold" {
		t.Fatalf("expected command_hold unchanged, got %s", mode)
	}
}

func TestContinuousToggleBlockedDuringDictation(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	if h.sess.IsContinuousActive() {
		t.Fatal("continuous should not activate while dictation is active")
	}
}

func TestWakeActionChangesMode(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.setVoiceMode("continuous_sleeping")
	h.handleVoiceWake(nil)
	if mode := h.sess.GetCurrentMode(); mode != "continuous_awake" {
		t.Fatalf("expected continuous_awake after wake action, got %s", mode)
	}
}

func TestWakeActionNoopWhenNotSleeping(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.setVoiceMode("continuous_awake")
	h.handleVoiceWake(nil)
	if mode := h.sess.GetCurrentMode(); mode != "continuous_awake" {
		t.Fatalf("expected mode unchanged, got %s", mode)
	}
}

func TestSleepActionChangesMode(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.setVoiceMode("continuous_awake")
	h.handleVoiceSleep(nil)
	if mode := h.sess.GetCurrentMode(); mode != "continuous_sleeping" {
		t.Fatalf("expected continuous_sleeping after sleep action, got %s", mode)
	}
}

func TestSleepActionNoopWhenNotAwake(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.setVoiceMode("continuous_sleeping")
	h.handleVoiceSleep(nil)
	if mode := h.sess.GetCurrentMode(); mode != "continuous_sleeping" {
		t.Fatalf("expected mode unchanged, got %s", mode)
	}
}

func TestContinuousTranscriptSuppressedDuringDictationOverlay(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.setVoiceMode("continuous_awake")
	h.sess.EnterOverlay("dictation")
	h.setVoiceMode("dictation")
	// Transcript should be silently dropped — mode is "dictation"
	h.handleCommandTranscript("snap right", true, nil, nil, nil, nil)
	if mode := h.sess.GetCurrentMode(); mode != "dictation" {
		t.Fatalf("expected mode to remain dictation, got %s", mode)
	}
}

func TestContinuousTranscriptResumesAfterOverlayLeave(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.setVoiceMode("continuous_awake")
	h.sess.EnterOverlay("dictation")
	h.setVoiceMode("dictation")
	// Leave overlay — should restore continuous_awake
	restored := h.sess.LeaveOverlay()
	h.setVoiceMode(restored)
	if mode := h.sess.GetCurrentMode(); mode != "continuous_awake" {
		t.Fatalf("expected mode restored to continuous_awake, got %s", mode)
	}
}

// --- speakPhraseIndex / cutoff tests ---

func TestSpeakPhraseIndex(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"speak over", 0},
		{"[unk] speak over", 1},
		{"speak over [unk]", 0},
		{"[unk] speak over [unk]", 1},
		{"[unk] branch over [unk]", 1},
		{"[unk] [unk] speak over", 2},
		// Earliest occurrence wins.
		{"speak over speak over", 0},
		// Single words / no phrase.
		{"speak", -1},
		{"over", -1},
		{"[unk]", -1},
		{"[unk] [unk]", -1},
		{"snap right", -1},
	}
	for _, tc := range cases {
		words := strings.Fields(tc.input)
		if got := speakPhraseIndex(words, speech.StopPhrases); got != tc.want {
			t.Errorf("speakPhraseIndex(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestSpeakStopCutoffMs(t *testing.T) {
	onsets := []uint64{1000, 2000, 3000}
	// Phrase at word 1: cutoff = onset - pad.
	if got := speakStopCutoffMs(onsets, 1); got == nil || *got != 2000-speakStopCutoffPadMs {
		t.Fatalf("expected %d, got %v", 2000-speakStopCutoffPadMs, got)
	}
	// Onset inside the pad clamps to zero rather than underflowing.
	if got := speakStopCutoffMs([]uint64{40}, 0); got == nil || *got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
	// Index outside the onsets (engine sent none, or a misaligned vector):
	// no cutoff — the stop still happens, whisper transcribes everything.
	if got := speakStopCutoffMs(onsets, 3); got != nil {
		t.Fatalf("expected nil for out-of-range index, got %v", got)
	}
	if got := speakStopCutoffMs(nil, 0); got != nil {
		t.Fatalf("expected nil for missing onsets, got %v", got)
	}
}

// --- Speak mode pipeline tests ---

func TestSpeakStartSetsSpeakDictating(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.speak", "start")
	if !h.sess.GetSpeakDictating() {
		t.Fatal("expected speakDictating=true")
	}
}

func TestSpeakStopClearsSpeakDictating(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.speak", "start")
	postActionWithPhase(h, t, "voice.speak", "stop")
	if h.sess.GetSpeakDictating() {
		t.Fatal("expected speakDictating=false")
	}
}

func TestSpeakStopWhenNotActiveIsNoop(t *testing.T) {
	h := newTestHost()
	resetState(h)
	resp := postActionWithPhase(h, t, "voice.speak", "stop")
	if resp.Status != branchkit.OnActionStatusOk {
		t.Fatalf("expected ok, got %s", resp.Status)
	}
}

// --- Confidence gate tests ---

// The transcript's overall confidence gates nothing (confidence scores are
// log-likelihoods, not 0–1; the command gate reads per-word margins). Low,
// high and absent confidence must all reach orchestration: the phrase is
// resolved once and its match comes back as the action.
func assertConfidenceReachesOrchestrate(t *testing.T, conf *float64) {
	t.Helper()
	h := orchestratingHost(t, branchkit.CommandsResolveResponse{
		Matched: true, ConsumedCount: 2, Action: snapAction(),
	})
	actions := h.handleCommandTranscript("snap right", true, conf, nil, nil, nil)
	if n := h.sess.Platform.(*resolvePlatform).resolveCalls(); n != 1 {
		t.Fatalf("the platform was asked to resolve %d times, want 1", n)
	}
	if len(actions) != 1 || actions[0].ActionType == nil || *actions[0].ActionType != "placement.snap" {
		t.Fatalf("actions = %+v, want the resolved placement.snap", actions)
	}
}

func TestContinuousTranscriptPassesLowConfidence(t *testing.T) {
	low := 0.1
	assertConfidenceReachesOrchestrate(t, &low)
}

func TestContinuousTranscriptAcceptsHighConfidence(t *testing.T) {
	high := 0.9
	assertConfidenceReachesOrchestrate(t, &high)
}

func TestContinuousTranscriptAcceptsNilConfidence(t *testing.T) {
	// Nil confidence (e.g. from pipelines without confidence) passes through.
	assertConfidenceReachesOrchestrate(t, nil)
}

// --- Pipeline state machine tests ---

func TestDictationStartSetsPipelineRecording(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	if s := h.sess.GetDictationPipelineState(); s != session.PipelineRecording {
		t.Fatalf("expected dictation pipeline recording, got %s", s)
	}
}

func TestDictationStopSetsPipelineDraining(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	// Force elapsed time past quick-tap threshold so DictationStop actually stops
	h.sess.SetDictationStartTime(nil)
	h.sess.SetDictationActive(true)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	if s := h.sess.GetDictationPipelineState(); s != session.PipelineDraining {
		t.Fatalf("expected dictation pipeline draining, got %s", s)
	}
}

func TestDictationAbortSetsPipelineInactive(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "abort")
	if s := h.sess.GetDictationPipelineState(); s != session.PipelineInactive {
		t.Fatalf("expected dictation pipeline inactive after abort, got %s", s)
	}
}

func TestDictationStartSuppressesCmdRecDuringContinuous(t *testing.T) {
	h := newTestHost()
	resetState(h)
	// Enter continuous mode first
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.setVoiceMode("continuous_awake")
	h.sess.SetCmdRecActive()
	// Start dictation — should suppress command_recognition
	postActionWithPhase(h, t, "voice.dictation", "start")
	if !h.sess.IsCmdRecSuppressed() {
		t.Fatal("expected command_recognition suppressed during dictation in continuous mode")
	}
}

func TestDictationStopUnsuppressesCmdRec(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.setVoiceMode("continuous_awake")
	h.sess.SetCmdRecActive()
	postActionWithPhase(h, t, "voice.dictation", "start")
	// Force past quick-tap
	h.sess.SetDictationStartTime(nil)
	h.sess.SetDictationActive(true)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	if h.sess.IsCmdRecSuppressed() {
		t.Fatal("expected command_recognition unsuppressed after dictation stop")
	}
}

func TestDictationAbortUnsuppressesCmdRec(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.setVoiceMode("continuous_awake")
	h.sess.SetCmdRecActive()
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "abort")
	if h.sess.IsCmdRecSuppressed() {
		t.Fatal("expected command_recognition unsuppressed after dictation abort")
	}
}

func TestDictationStartFromIdleDoesNotSuppressCmdRec(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	if h.sess.IsCmdRecSuppressed() {
		t.Fatal("should not suppress command_recognition when continuous is not active")
	}
}

func TestContinuousToggleSetsCmdRecActive(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.lastContinuousToggle = time.Time{}
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	if s := h.sess.GetCmdRecPipelineState(); s != session.PipelineActive {
		t.Fatalf("expected cmdRec active after continuous toggle on, got %s", s)
	}
}

func TestContinuousToggleOffSetsCmdRecInactive(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.lastContinuousToggle = time.Time{}
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.lastContinuousToggle = time.Time{}
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	if s := h.sess.GetCmdRecPipelineState(); s != session.PipelineInactive {
		t.Fatalf("expected cmdRec inactive after continuous toggle off, got %s", s)
	}
}

func TestSpeakStartSetsPipelineRecording(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.speak", "start")
	if s := h.sess.GetDictationPipelineState(); s != session.PipelineRecording {
		t.Fatalf("expected dictation pipeline recording after speak start, got %s", s)
	}
}

func TestSpeakStopSetsPipelineDraining(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.speak", "start")
	postActionWithPhase(h, t, "voice.speak", "stop")
	if s := h.sess.GetDictationPipelineState(); s != session.PipelineDraining {
		t.Fatalf("expected dictation pipeline draining after speak stop, got %s", s)
	}
}

func TestSpeakDoesNotSuppressCmdRec(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.setVoiceMode("continuous_awake")
	h.sess.SetCmdRecActive()
	postActionWithPhase(h, t, "voice.speak", "start")
	if h.sess.IsCmdRecSuppressed() {
		t.Fatal("speak mode should not suppress command_recognition (grammar lock handles this)")
	}
}

func TestIsDictationAcceptingStates(t *testing.T) {
	h := newTestHost()
	resetState(h)
	if h.sess.IsDictationAccepting() {
		t.Fatal("inactive should not accept")
	}
	h.sess.StartDictationRecording()
	if !h.sess.IsDictationAccepting() {
		t.Fatal("recording should accept")
	}
	h.sess.DrainDictation()
	if !h.sess.IsDictationAccepting() {
		t.Fatal("draining should accept")
	}
	h.sess.FinishDictation()
	if h.sess.IsDictationAccepting() {
		t.Fatal("inactive after finish should not accept")
	}
}

func TestFinishDictationIdempotent(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.sess.FinishDictation()
	h.sess.FinishDictation()
	if s := h.sess.GetDictationPipelineState(); s != session.PipelineInactive {
		t.Fatalf("expected inactive, got %s", s)
	}
}

func TestDrainFromNonRecordingIsNoop(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.sess.DrainDictation()
	if s := h.sess.GetDictationPipelineState(); s != session.PipelineInactive {
		t.Fatalf("drain from inactive should stay inactive, got %s", s)
	}
}

func TestSuppressFromNonActiveIsNoop(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.sess.SuppressCommandRecognition()
	if h.sess.IsCmdRecSuppressed() {
		t.Fatal("suppress from inactive should be noop")
	}
}

func TestUnsuppressFromNonSuppressedIsNoop(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.sess.SetCmdRecActive()
	h.sess.UnsuppressCommandRecognition()
	if s := h.sess.GetCmdRecPipelineState(); s != session.PipelineActive {
		t.Fatalf("unsuppress from active should stay active, got %s", s)
	}
}

func TestDictationStopRestoresCmdRecActiveInContinuous(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.lastContinuousToggle = time.Time{}
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.setVoiceMode("continuous_awake")
	h.sess.SetCmdRecActive()
	postActionWithPhase(h, t, "voice.dictation", "start")
	// Force past quick-tap
	h.sess.SetDictationStartTime(nil)
	h.sess.SetDictationActive(true)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	// After stop, cmdRec should be active again (pipeline restarted)
	if s := h.sess.GetCmdRecPipelineState(); s != session.PipelineActive {
		t.Fatalf("expected cmdRec active after dictation stop in continuous, got %s", s)
	}
}

func TestDictationAbortRestoresCmdRecActiveInContinuous(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.lastContinuousToggle = time.Time{}
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.setVoiceMode("continuous_awake")
	h.sess.SetCmdRecActive()
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "abort")
	if s := h.sess.GetCmdRecPipelineState(); s != session.PipelineActive {
		t.Fatalf("expected cmdRec active after dictation abort in continuous, got %s", s)
	}
}

func TestDictationStopFromIdleDoesNotRestartCmdRec(t *testing.T) {
	h := newTestHost()
	resetState(h)
	// Start dictation from idle (no continuous)
	postActionWithPhase(h, t, "voice.dictation", "start")
	h.sess.SetDictationStartTime(nil)
	h.sess.SetDictationActive(true)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	// cmdRec should remain inactive — no restart since continuous isn't active
	if s := h.sess.GetCmdRecPipelineState(); s != session.PipelineInactive {
		t.Fatalf("expected cmdRec inactive (no continuous), got %s", s)
	}
}

func TestContinuousOffDuringDictationNoRestart(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.lastContinuousToggle = time.Time{}
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	h.setVoiceMode("continuous_awake")
	h.sess.SetCmdRecActive()
	// Start dictation — stops cmdRec
	postActionWithPhase(h, t, "voice.dictation", "start")
	if !h.sess.IsCmdRecSuppressed() {
		t.Fatal("expected cmdRec suppressed during dictation")
	}
	// Simulate continuous toggled off while dictating
	h.sess.SetCmdRecInactive()
	h.sess.SetCurrentMode("dictation")
	// Now abort dictation — should NOT restart cmdRec since continuous is gone
	postActionWithPhase(h, t, "voice.dictation", "abort")
	if s := h.sess.GetCmdRecPipelineState(); s != session.PipelineInactive {
		t.Fatalf("expected cmdRec inactive (continuous was toggled off), got %s", s)
	}
}

// Esc during a hold cancels it; the key's later release does nothing.
func TestDictationCancelDuringHoldIgnoresTheRelease(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postAction(h, t, "voice.dictation_cancel")
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineInactive {
		t.Fatalf("cancel must end the dictation, got %s", got)
	}
	postActionWithPhase(h, t, "voice.dictation", "stop")
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineInactive {
		t.Fatalf("the release after a cancel must not start a drain, got %s", got)
	}
}

// Esc during a latched dictation cancels it, and the next press starts a
// fresh dictation instead of "stopping" the cancelled one.
func TestDictationCancelDuringLatchThenNextPressStarts(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.dictation", "stop")
	waitPast(dictationBounceMS)
	postActionWithPhase(h, t, "voice.dictation", "start") // latches
	postActionWithPhase(h, t, "voice.dictation", "stop")
	postAction(h, t, "voice.dictation_cancel")
	if h.sess.GetDictationToggleActive() {
		t.Fatal("cancel must clear the latch")
	}
	waitPast(doubleTapWindowMS)
	postActionWithPhase(h, t, "voice.dictation", "start")
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineRecording {
		t.Fatalf("the next press after a cancel starts recording, got %s", got)
	}
}

// Esc when nothing is recording is not dictation's to handle.
func TestDictationCancelWhenIdleDoesNothing(t *testing.T) {
	h := newTestHost()
	resetState(h)
	postAction(h, t, "voice.dictation_cancel")
	if got := h.sess.GetDictationPipelineState(); got != session.PipelineInactive {
		t.Fatalf("got %s", got)
	}
}

// A tap or a silent hold comes back as an empty final transcript. That is the
// recognizer's answer, not a lost dictation: the session finishes on it, and
// the pipeline's stop afterwards finds nothing draining, so no "Dictation
// didn't come back" notice is raised.
func TestEmptyDictationTranscriptIsNotALostDictation(t *testing.T) {
	h := newTestHost()
	resetState(h)
	lost := 0
	h.sess.SetOnDictationLost(func() { lost++ })
	h.sess.StartDictationRecording()
	h.sess.DrainDictation()

	h.handlePipelineTranscript(dictationTranscript(t, ""))
	if st := h.sess.GetDictationPipelineState(); st != session.PipelineInactive {
		t.Fatalf("after an empty transcript the dictation is %s, want inactive", st)
	}
	h.sess.FinishDictationIfDraining() // what pipeline.stopped does next
	if lost != 0 {
		t.Fatalf("an empty transcript was reported as a lost dictation %d time(s)", lost)
	}
}
