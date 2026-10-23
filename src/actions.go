package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/branchkit/branchkit-plugin-voice/hud"
	"github.com/branchkit/branchkit-plugin-voice/session"
	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/plugin-sdk-go"
)

// --- Request/Response types ---

type RenderHUDRequest = hud.RenderHUDRequest

type RenderSettingsRequest = branchkit.RenderSettingsRequest

type OnCommandsChangedRequest struct {
	CommandsByPlugin map[string][]CommandSnapshot `json:"commands_by_plugin"`
	UserCommands     []CommandSnapshot            `json:"user_commands"`
	DisabledPlugins  []string                     `json:"disabled_plugins"`
}

type OnCommandsChangedResponse struct {
	PhoneticsCount int `json:"processed_count"`
}

// --- Handlers ---

// handled is the ok reply every per-action handler returns through, to keep
// call sites terse. A reply carries no host message: a handler that opens or
// closes a window calls hud.show / hud.hide itself.
func (h *Host) handled() branchkit.OnActionResponse {
	return branchkit.OnActionResponse{Status: branchkit.OnActionStatusOk}
}

// actionRegistrations is the single source of truth for which dotted action
// types this plugin handles. main.go iterates and registers each via
// plugin.HandleAction; tests iterate to dispatch handlers directly without
// going through the full RPC plumbing.
type actionRegistration struct {
	Action  string
	Handler branchkit.ActionHandlerFunc
}

func (h *Host) actionRegistrations() []actionRegistration {
	return []actionRegistration{
		{"hud.show", h.handleHudShow},
		{"voice.open_float", h.handleOpenFloatAction},
		{"voice.close_floats", h.handleCloseFloatsAction},
		{"voice.enter_picker", h.handleEnterPicker},
		{"voice.dictation", h.handleVoiceDictation},
		{"voice.command", h.handleVoiceCommand},
		{"voice.speak", h.handleVoiceSpeak},
		{"voice.continuous_toggle", h.handleVoiceContinuousToggle},
		{"voice.wake", h.handleVoiceWake},
		{"voice.retype_last", h.handleRetypeLast},
		{"voice.rewrite_selection", h.handleRewriteSelection},
		{"voice.dictation_cancel", h.handleDictationCancel},
		{"voice.sleep", h.handleVoiceSleep},
		{"voice.help_toggle", h.handleVoiceHelpToggle},
		{"voice.repeat", h.handleVoiceRepeat},
		{"voice.pick_tie", h.handleVoicePickTie},
	}
}

// hud.show — voice plugin owns the main HUD; platform dispatches here when
// the user runs `show <target>`. setMainMode picks the right mode/channel.
func (h *Host) handleHudShow(req *branchkit.OnActionRequest) (any, error) {
	var p struct {
		Target string `json:"target"`
	}
	if err := req.UnmarshalParams(&p); err != nil {
		return nil, err
	}
	if !h.hud.Config().IsChannelEnabled(hud.ChannelMain) {
		return h.handled(), nil
	}
	h.hud.SetMainMode(p.Target)
	h.hud.SetHudOpen(true)
	if err := h.plugin.HUDShow(branchkit.HUDShowRequest{Channel: "main"}); err != nil {
		return nil, err
	}
	return h.handled(), nil
}

// handleEnterPicker backs the "open app" / "open collection" mode-entry
// commands. The real work is the command's sets_tags (the exclusive picker
// tag), which narrows the matcher and auto-opens the Discovery HUD to the
// app / collection list. This handler exists only so the commands carry a
// real, distinct action: a command with no action compiles to `_noop`, which
// the matcher hides from discovery completions — so bare "open" would show
// neither "app" nor "collection". Distinct `mode` values also keep the two
// from collapsing under the completion de-dup.
func (h *Host) handleEnterPicker(_ *branchkit.OnActionRequest) (any, error) {
	return h.handled(), nil
}

func (h *Host) handleVoiceDictation(req *branchkit.OnActionRequest) (any, error) {
	// Feature switch, checked at the entry point rather than by disabling the
	// plugin: not running the pipeline is this plugin's decision, stages spawn
	// on demand, and an unspawned stage holds no model and no hardware grant.
	// Checked on every phase so a toggle flipped mid-hold cannot strand a
	// session — the stop phases below still run.
	if !h.config.Get().DictationEnabled && !isStopPhase(req) && !isAbortPhase(req) {
		branchkit.Logf("voice", "dictation: disabled in settings, ignoring")
		return h.handled(), nil
	}
	if isAbortPhase(req) {
		branchkit.Logf("voice", "dictation: abort")
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: h.dictationPipeline()})
		h.sess.FinishDictation()
		h.restartCmdRecIfContinuous()
		h.leaveDictationOverlay()
		return h.handled(), nil
	}
	if isStopPhase(req) {
		decision, tap := h.sess.DictationStop(tapMS)
		switch decision {
		case session.StopFinish:
			branchkit.Logf("voice", "dictation: stop")
			h.finishDictationSession()
		case session.StopTapPending:
			// A tap: keep recording through the double-tap window, then
			// finish unless a second tap latched it.
			time.AfterFunc(doubleTapWindowMS*time.Millisecond, func() {
				if h.sess.ResolveTap(tap) {
					branchkit.Logf("voice", "dictation: single tap — stop")
					h.finishDictationSession()
				}
			})
		case session.StopLatchHeld:
			// The release of the second tap: the latch holds.
		case session.StopIgnore:
			if h.sess.GetCurrentMode() == "dictation" {
				// Genuine mismatch: the mode says a session is live but the
				// state machine has none (e.g. hot-reload mid-session).
				branchkit.Logf("voice", "dictation: stop (forced — state mismatch)")
				h.sess.SetDictationActive(false)
				h.finishDictationSession()
			} else {
				branchkit.Logf("voice", "dictation: stop not active, ignoring")
			}
		}
		return h.handled(), nil
	}

	switch h.sess.DictationStart(dictationBounceMS) {
	case session.StartStopSpeak:
		// Deliberately consumes the press and starts nothing. This is a
		// recovery gesture (the spoken stop phrase can fail to decode), it
		// matches handleVoiceContinuousToggle's escape, and starting a fresh
		// dictation here would share the `dictation` pipeline while whisper is
		// still transcribing the speak audio (cancel_delay_ms 60000), landing
		// the old transcript mid-hold. If the silent consumed press bites,
		// surface it (HUD/notice); do not change what the press means.
		branchkit.Logf("voice", "dictation: stopping speak dictation via Option+T")
		h.stopSpeakDictation(nil)
	case session.StartBounceIgnored:
		// Contact bounce on a key still being held — see session.bounceMS.
		// Logged because a chattering keyboard is worth knowing about: if
		// this line is frequent, the switch is failing.
		branchkit.Logf("voice", "dictation: ignoring key bounce (press within the debounce window of the latch)")
	case session.StartLatch:
		branchkit.Logf("voice", "dictation: double tap — latched, recording until the next press")
	case session.StartToggleStop:
		branchkit.Logf("voice", "dictation: toggle stop")
		h.finishDictationSession()
	case session.StartBegin:
		branchkit.Logf("voice", "dictation: starting pipeline")
		h.dictationStartSound()
		h.sess.StartDictationRecording()
		h.warmRefine()
		if h.sess.IsContinuousActive() {
			h.stopCmdRecForDictation()
		}
		h.enterDictationOverlay()
		// Read a sentence: this hold runs dictation_sample, recorded and
		// never typed; every other hold runs dictation.
		args := h.dictationRunParams()
		if dir := h.readSentence.recordDirForRun(time.Now()); dir != "" {
			args.Name, args.RecordTo = dictationSamplePipeline, &dir
		}
		h.setDictationPipeline(args.Name)
		// Model not warmed yet (first press after launch, before boot prewarm
		// finished) — say so, so the hold doesn't read as broken. A stage
		// that has to start cold later on says so itself
		// (_platform.pipeline.warming).
		if !h.dictationReady.Load() {
			h.showDictationWarmingFor(args.Name)
		}
		h.holdBegan()
		if err := h.runPipeline(args); err != nil {
			branchkit.Logf("voice", "dictation: pipelines.run error: %v", err)
		}
	}
	return h.handled(), nil
}

// finishDictationSession ends the recording: drain (the transcript is still in
// flight), stop the pipeline, resume continuous command recognition, restore
// the prior mode. Shared by hold release, toggle stop, and the forced
// state-mismatch recovery.
// handleDictationCancel is Esc during a dictation: bound with
// `while: plugin.voice.mode.dictation`, so the platform grabs Esc only while
// a dictation is recording and every other Esc stays with the focused app.
// Nothing is typed: the pipeline stops and its transcript, when it lands,
// finds the session already finished and is dropped.
func (h *Host) handleDictationCancel(_ *branchkit.OnActionRequest) (any, error) {
	if h.sess.GetDictationPipelineState() != session.PipelineRecording {
		return h.handled(), nil
	}
	branchkit.Logf("voice", "dictation: cancelled (Esc)")
	h.sess.CancelDictationPress()
	h.pipelinesStop(branchkit.PipelinesStopRequest{Name: h.dictationPipeline()})
	h.sess.FinishDictation()
	h.restartCmdRecIfContinuous()
	h.leaveDictationOverlay()
	return h.handled(), nil
}

func (h *Host) finishDictationSession() {
	h.dictationStopSound()
	// The caret is read now, while the transcript is computed, not after it.
	h.prefetchCaret()
	h.sess.DrainDictation()
	h.holdEnded()
	// Audio is done; the transcript is not. That gap is measured at
	// 0.68 s + 40 ms per second of speech, and the menu bar used to sit on
	// `recording` throughout it — so a user could not tell "still listening"
	// from "working on it". the session's dictation-finished hook (see newHost) clears this when the
	// transcript lands or the drain backstop gives up.
	//
	// Set here rather than derived from the mode: `pipelines.stop` below BLOCKS
	// until the pipeline winds down, so by the time the mode changes the
	// session has already finished and a mode-derived check never fires.
	h.setIndicator(branchkit.IndicatorStateProcessing)
	h.pipelinesStop(branchkit.PipelinesStopRequest{Name: h.dictationPipeline()})
	h.restartCmdRecIfContinuous()
	h.leaveDictationOverlay()
}

func (h *Host) stopCmdRecForDictation() {
	h.sess.SuppressCommandRecognition()
	if h.sess.IsCmdRecAside() {
		// A command hold already set the persistent run aside, and
		// command_recognition is now the hold's own run: its release stops it.
		branchkit.Logf("voice", "dictation: command_recognition is a command hold's run; left to the hold")
		return
	}
	h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "command_recognition"})
	branchkit.Logf("voice", "dictation: stopped command_recognition pipeline")
}

func (h *Host) restartCmdRecIfContinuous() {
	if !h.sess.IsContinuousActive() {
		h.sess.UnsuppressCommandRecognition()
		return
	}
	h.sess.UnsuppressCommandRecognition()
	if h.sess.IsCmdRecAside() {
		// A command hold still has it aside; the hold's cleanup restarts it.
		branchkit.Logf("voice", "dictation: command_recognition left to the command hold to restart")
		return
	}
	h.runCommandRecognition(false)
	branchkit.Logf("voice", "dictation: restarted command_recognition pipeline")
}

// setCmdRecAsideForHold stops continuous listening's persistent
// command_recognition run so a command hold can listen on an ephemeral run
// of its own. The platform treats a transcript as the person holding the key
// only when its run is ephemeral: over the persistent run, BranchKit's own
// speech was filtered out of the hold's first words as echo, and an app's
// question could not be answered by voice. The platform starts
// the new run's microphone at once and buffers it while the old run winds
// down, so the stop loses no speech. Dictation's stopCmdRecForDictation is
// the same move.
func (h *Host) setCmdRecAsideForHold() {
	if h.sess.SetCmdRecAsideForHold() {
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "command_recognition"})
		branchkit.Logf("voice", "command: stopped continuous command_recognition for the hold")
	}
}

// returnCmdRecAtRelease starts continuous listening's persistent run again
// at a hold's release, so the platform captures speech from that moment: a
// new run's microphone starts at once and is buffered while the hold's run
// winds down. Waiting for the hold's cleanup instead lost whatever was said
// in between (tens of milliseconds usually, up to the end-of-hold flush's
// decode or the backstop), and losing a leading word turns "snap left" into
// "left". The run's transcripts are held back until the cleanup has
// restored continuous listening's mode (handleOnTranscript), so nothing said
// after the release is resolved as the hold's command. When a dictation
// holds the microphone or listening is off, the claim stays for the cleanup.
func (h *Host) returnCmdRecAtRelease() {
	if h.sess.ReturnCmdRecAtRelease() {
		h.runCommandRecognition(false)
		branchkit.Logf("voice", "command: restarted continuous command_recognition at the release")
	}
}

// heldBackLimit bounds how long a transcript of the persistent run waits for
// a released hold's cleanup. The cleanup runs on the hold's run's `stopped`
// or the drain backstop, so this is reached only if the cleanup never runs;
// the transcript is then resolved in whatever mode voice is in.
const heldBackLimit = commandHoldDrainBackstop + time.Second

// awaitHoldCleanup holds a persistent-run transcript that arrived after a
// released hold's run ended but before the hold's cleanup restored the base
// mode, so it is resolved in that mode. The platform answers one transcript
// of a run at a time, so held transcripts are resolved in the order spoken.
func (h *Host) awaitHoldCleanup() {
	gate := h.sess.CmdRecHeldBack()
	if gate == nil {
		return
	}
	branchkit.Logf("voice", "on_transcript: waiting for the released hold's cleanup tr=%s", h.traceID())
	select {
	case <-gate:
	case <-time.After(heldBackLimit):
		branchkit.Logf("voice", "on_transcript: the released hold's cleanup did not run within %s; resolving now tr=%s",
			heldBackLimit, h.traceID())
	}
}

// restartCmdRecAfterHold brings continuous listening's persistent run back
// once a hold that set it aside has ended, unless a dictation still holds
// the microphone (its own end restarts it) or listening was turned off.
// A release that could start it already did (returnCmdRecAtRelease); this
// covers the rest: an abort, and a dictation or a re-press that kept the
// claim at the release.
func (h *Host) restartCmdRecAfterHold() {
	if h.sess.ReleaseCmdRecAside() {
		h.runCommandRecognition(false)
		branchkit.Logf("voice", "command: restarted continuous command_recognition after the hold")
	}
}

func (h *Host) enterDictationOverlay() {
	h.sess.EnterOverlay("dictation")
	h.setVoiceMode("dictation")
	h.hud.CloseDiscovery()
}

func (h *Host) leaveDictationOverlay() {
	restored := h.sess.LeaveOverlay()
	h.setVoiceMode(restored)
}

// runCommandRecognition starts the command_recognition pipeline.
//
// It used to also re-seed the recognizer with the union grammar through
// `pipelines.inject("vocabulary_update", …)`, because a pooled sherpa stage
// respawned without a seed can only emit words it holds. That seed is the
// platform's job now: the pipeline manager pushes a vocabulary_update into
// every recognition stage it starts, and vocabulary.commit refreshes it. The
// plugin-side inject was deleted 2026-09-23 after pipelines.inject began
// refusing any event type outside ext.<vendor>.<name> (app e2c4c745), which
// had made it fail on every call with the error discarded.
func (h *Host) runCommandRecognition(ephemeral bool) {
	if err := h.runPipeline(commandRecognitionRunParams(h.commandModelName(), ephemeral)); err != nil {
		branchkit.Logf("voice", "command_recognition: pipelines.run error: %v", err)
	}
}

// runPipeline starts a pipeline and reports a refusal as an error.
// pipelines.run answers "not started" (no pipeline manager running, …) with a
// successful {started:false, reason} rather than an RPC error, so a caller that
// only checks err believes a pipeline is running when nothing is listening.
func (h *Host) runPipeline(a runArgs) error {
	resp, err := h.pipelinesRun(branchkit.PipelinesRunRequest{Name: a.Name, Ephemeral: a.Ephemeral, ParamOverrides: a.Overrides, RecordTo: a.RecordTo})
	if a.RecordTo != nil && (err != nil || !resp.Started) {
		h.readSentence.clearRecording() // the recorded run never started
	}
	if err != nil {
		return err
	}
	if !resp.Started {
		reason := "no reason given"
		if resp.Reason != nil && *resp.Reason != "" {
			reason = *resp.Reason
		}
		return fmt.Errorf("%s not started: %s", a.Name, reason)
	}
	return nil
}

// runArgs is what PipelinesRun takes, named. The generated wrapper is
// positional (ephemeral, name, param_overrides) and these two builders are
// unit-tested, so a struct keeps both the call sites and the tests readable.
// It replaced a map[string]any whose keys had to match the wire by hand.
type runArgs struct {
	Name      string
	Ephemeral *bool
	Overrides map[string]json.RawMessage
	// RecordTo asks the platform to record the run's audio into this folder
	// of the plugin's data dir (Read a sentence), or is nil.
	RecordTo *string
}

// commandRecognitionRunParams starts command_recognition on model, the chosen
// command model. The record (seeded by plugin.json) stays authoritative for
// everything else; the model rides on each run the way dictation's does, so a
// choice survives the manifest re-seeding the record on every reload, and the
// pool respawns the stage when --model differs from the warm one.
func commandRecognitionRunParams(model string, ephemeral bool) runArgs {
	a := runArgs{Name: "command_recognition", Overrides: map[string]json.RawMessage{
		"model": mustJSON(model),
	}}
	if ephemeral {
		t := true
		a.Ephemeral = &t
	}
	return a
}

// commandModelName is the command model the stage should load now.
func (h *Host) commandModelName() string {
	return commandModelOnDisk(h.chosenCommandModel(), modelOnDisk)
}

// chosenCommandModel is the command model the settings name.
func (h *Host) chosenCommandModel() string {
	return resolveCommandModel(h.config.Get().CommandModel)
}

// dictationRunParams starts dictation on the chosen model.
func (h *Host) dictationRunParams() runArgs {
	vc := h.config.Get()
	overrides := map[string]json.RawMessage{
		"model": mustJSON(resolveDictationModel(vc.Model)),
	}
	if vc.Language != "" {
		overrides["language"] = mustJSON(vc.Language)
	}
	ephemeral := true
	return runArgs{Name: "dictation", Ephemeral: &ephemeral, Overrides: overrides}
}

// dictationSamplePipeline is Read a sentence's copy of dictation: the same
// stages, run only to record a sentence read aloud. Its own name keeps its
// transcripts and its stop apart from every ordinary dictation's, so a
// reading is never typed and a dictation is never taken for a reading.
const dictationSamplePipeline = "dictation_sample"

// isDictationPipeline reports whether name is one of voice's dictation
// pipelines: dictation itself, or Read a sentence's copy of it. Everything
// that holds for a dictation run as a run (the chosen microphone, the
// warming overlay, an empty transcript finishing the session) holds for
// both; only what happens to the text differs.
func isDictationPipeline(name string) bool {
	return name == "dictation" || name == dictationSamplePipeline
}

// setDictationPipeline records which pipeline the current hold started, so
// its release, abort or cancel stops that one.
func (h *Host) setDictationPipeline(name string) {
	h.dictPipelineMu.Lock()
	h.dictPipeline = name
	h.dictPipelineMu.Unlock()
}

// dictationPipeline is the pipeline the current hold started ("dictation"
// unless it is reading a sentence).
func (h *Host) dictationPipeline() string {
	h.dictPipelineMu.Lock()
	defer h.dictPipelineMu.Unlock()
	if h.dictPipeline == "" {
		return "dictation"
	}
	return h.dictPipeline
}

// commandHoldDrainBackstop bounds how long the deferred end-of-hold cleanup
// waits for the pipeline.stopped event before running anyway. The event
// normally fires within the pipeline's cancel_delay (sub-second); this is the
// safety net for a stop that never reports back.
const commandHoldDrainBackstop = 2 * time.Second

// finishCommandHold runs the deferred end-of-hold cleanup: emit the session
// boundary (clears the matcher bridge), reset dedup + last-match, return to
// idle. Deferred from Option+G up until the engine's end-of-hold flush has
// drained through the matcher, so a two-word codeword whose suffix only decodes
// at flush can still complete. Ordered notification delivery guarantees the
// flush transcript is matched before pipeline.stopped fires this cleanup.
func (h *Host) finishCommandHold() {
	h.plugin.SessionBoundary()
	// Tear down an in-flight tie. SessionBoundary above clears the
	// disambiguating GATE (manifest lifecycle, ClearOnEvent), but the
	// transient choice vocabulary and the in-memory tie state are voice's
	// own and need saying so explicitly — otherwise a hold released
	// mid-disambiguation strands both until the next tie or a dismiss.
	//
	// This used to live in an orchestrate session-end block that never ran,
	// because nothing ever set the flag guarding it. It belongs on the path
	// that actually fires at end of hold, which is this one.
	h.sess.ClearTie()
	h.sess.ClearLastMatch()
	h.leaveCommandHold()
	// After the mode is back, so the persistent run's first words are heard
	// in continuous listening's mode, not the hold's: a run started here
	// hears them after, and one started at the release has had them held
	// back until now.
	h.restartCmdRecAfterHold()
	h.sess.OpenCmdRecHeldBack()
}

// leaveCommandHold returns an ended hold to the mode it covered (idle, or
// continuous listening). A dictation that started under the hold still covers
// that mode, and its own release restores it, so the mode is left to it.
func (h *Host) leaveCommandHold() {
	if mode, changed := h.sess.LeaveCommandHold(); changed {
		h.setVoiceMode(mode)
	}
}

func (h *Host) handleVoiceCommand(req *branchkit.OnActionRequest) (any, error) {
	pipeline := "command_recognition"

	// Same shape as dictation: a start with the switch off does nothing, while
	// stop and abort always run so a mid-hold flip cannot leave the recognizer
	// pipeline up.
	if !h.config.Get().VoiceCommandsEnabled && !isStopPhase(req) && !isAbortPhase(req) {
		branchkit.Logf("voice", "command: disabled in settings, ignoring")
		return h.handled(), nil
	}

	if isAbortPhase(req) {
		branchkit.Logf("voice", "command: abort")
		h.hud.CloseDiscovery()
		// An aborted hold still ran what it matched; a failure among those
		// is said here, as the stop phase says it.
		if p := h.sess.TakeLastProblem(); p != "" {
			h.reportProblem("Didn't work", p)
		}
		// Abort tears down now — drop any pending deferred cleanup so it can't
		// fire a second boundary later.
		h.sess.CancelCommandHoldDrain()
		h.plugin.SessionBoundary()
		// Only the hold's own run is stopped. Outside continuous listening a
		// live speak session listens for its stop phrase on it, so it keeps
		// it; over continuous listening the persistent run comes back below.
		active, owns := h.sess.EndCommandHold()
		speakKeeps := h.sess.GetSpeakDictating() && !h.sess.IsContinuousActive()
		stray := !active && !h.sess.IsContinuousActive()
		if (owns || stray) && !speakKeeps {
			h.pipelinesStop(branchkit.PipelinesStopRequest{Name: pipeline})
		}
		h.sess.ClearLastMatch()
		h.leaveCommandHold()
		h.restartCmdRecAfterHold()
		// An abort after the release took the cleanup's place (the drain
		// is cancelled above): let any transcript held for it go on, now
		// that the mode is back.
		h.sess.OpenCmdRecHeldBack()
		return h.handled(), nil
	}
	if isStopPhase(req) {
		// The hold flag, not the mode: a dictation started under the hold
		// covers the mode, and its key's release must still end the hold.
		active, owns := h.sess.EndCommandHold()
		if !active {
			branchkit.Logf("voice", "command: stop not active, ignoring")
			return h.handled(), nil
		}
		branchkit.Logf("voice", "command: stop")
		// The hold's last command, if its mode kept discovery open, is the
		// channel's final state — spoken at this boundary — else a clear. A
		// command that failed during the hold outranks both: its sentence
		// names the command, and clearing the channel would take it away
		// before it was said.
		o := h.sess.TakeLastOutcome()
		if p := h.sess.TakeLastProblem(); p != "" {
			h.hud.CloseDiscovery()
			h.reportProblem("Didn't work", p)
		} else if o != "" && h.hud.IsDiscoveryOpen() {
			h.hud.EndDiscoveryWithOutcome(o)
		} else {
			h.hud.CloseDiscovery()
		}
		h.holdEnded()
		if !owns || (h.sess.GetSpeakDictating() && !h.sess.IsContinuousActive()) {
			// The run outlives the hold: it is how a live speak session hears
			// its stop phrase (ownership passes to it; stopSpeakDictation
			// stops the run). No end-of-hold flush to wait for and no
			// pipeline.stopped to wait on, so clean up now. Over continuous
			// listening a speak session listens on the persistent run, which
			// the hold's cleanup restarts, so the hold's run is stopped below.
			h.finishCommandHold()
			return h.handled(), nil
		}
		// Stop the pipeline (triggers the engine's end-of-hold flush), but
		// DEFER the session boundary + idle until that flush has drained
		// through the matcher — so a codeword whose suffix only decodes at
		// flush can still complete. pipeline.stopped (main.go) fires the
		// cleanup; the backstop timer is the safety net. The run's
		// microphone stops here, so words spoken after the release never
		// reach it and cannot run as the hold's commands. Over continuous
		// listening the persistent run starts again now, so words said
		// between the release and the cleanup are heard; its transcripts
		// wait for the cleanup to restore the mode (awaitHoldCleanup).
		// After BeginCommandHoldDrain, so the drain does not wait on the
		// run it starts.
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: pipeline})
		h.sess.BeginCommandHoldDrain(commandHoldDrainBackstop, h.finishCommandHold)
		h.returnCmdRecAtRelease()
		return h.handled(), nil
	}

	// New hold: if a previous hold is still draining (fast re-press), close its
	// boundary now so its bridge can't leak into this hold. StartCommandHold
	// also bumps the generation, invalidating that stale drain's cleanup.
	// Over continuous listening the hold listens on its own ephemeral run, as
	// push-to-talk does: the persistent run is set aside until the hold's
	// release (setCmdRecAsideForHold, returnCmdRecAtRelease). Commands count while the key is down,
	// a sleeping session included.
	prevDraining, _ := h.sess.StartCommandHold()
	if prevDraining {
		h.plugin.SessionBoundary()
	}
	// A calibration game finishing suppresses the remainder of ITS session only.
	// The lazy clear (session-ending orchestrate) is not guaranteed to run — a
	// game that ends with no trailing transcript, or the liveness-timeout close
	// with no session at all, leaves the flag set and every later hold gets
	// silently suppressed. A fresh hold can never legitimately start consumed.
	h.sess.ClearCalibrationConsumed()
	h.setVoiceMode("command_hold")
	h.holdBegan()
	if h.sess.GetSpeakDictating() && !h.sess.IsContinuousActive() {
		// A speak session outside continuous listening already runs an
		// ephemeral command_recognition run to hear its stop phrase: the
		// hold shares it. Its release stops it unless the speak session is
		// still live then.
		branchkit.Logf("voice", "command: hold shares the speak session's run")
		return h.handled(), nil
	}
	h.setCmdRecAsideForHold()
	branchkit.Logf("voice", "command: start pipeline (%s)", pipeline)
	if err := h.runPipeline(commandRecognitionRunParams(h.commandModelName(), true)); err != nil {
		// No run of the hold's own: give continuous listening its run back
		// rather than leave nothing listening, and let the release stop
		// nothing.
		branchkit.Logf("voice", "command_recognition: pipelines.run error: %v", err)
		h.sess.DisownCommandHold()
		if h.sess.ReleaseCmdRecAsideUnderHold() {
			h.runCommandRecognition(false)
			branchkit.Logf("voice", "command: restarted continuous command_recognition (the hold has no run)")
		}
	}
	return h.handled(), nil
}

// The stop/cancel phrase tables live in the speech package
// (speech.StopPhrases / speech.CancelPhrases) — single source shared by the
// sherpa-side detector (speakPhraseIndex) and the text-domain backstop
// (speech.StripSpeakPhrases). Every phrase must also exist as a gated
// command pattern in commands.json (requires_tags: plugin.voice.speaking) —
// that is what puts the phrase words into the platform's union vocabulary,
// and the recognizer can only emit words in its grammar.
// TestSpeakPhraseTablesMatchCommandPatterns pins the sources together; a
// desync silently broke voice-stop for 25 days once.

// stripWordPunctuation removes trailing/leading punctuation from a single
// word for fuzzy matching. "stop." → "stop", "\"speak" → "speak".
func stripWordPunctuation(w string) string {
	return strings.TrimFunc(w, func(r rune) bool {
		switch r {
		case ',', '.', '!', '?', ';', ':', '-', '—', '"', '\'', '\u201c', '\u201d':
			return true
		}
		return false
	})
}

// speakPhraseIndex returns the index of the first word of the earliest
// adjacent pair matching any phrase in table, or -1. The index doubles as the
// lookup key into the transcript's word_onsets_ms — the phrase's audio onset.
func speakPhraseIndex(words []string, table [][]string) int {
	for i := 0; i+1 < len(words); i++ {
		w0 := stripWordPunctuation(words[i])
		w1 := stripWordPunctuation(words[i+1])
		for _, phrase := range table {
			if w0 == phrase[0] && w1 == phrase[1] {
				return i
			}
		}
	}
	return -1
}

// speakStopCutoffPadMs backs the audio cutoff off the detected phrase onset:
// CTC onsets run a touch late, and whisper tolerates a clipped trailing
// consonant far better than a leaked phrase word.
const speakStopCutoffPadMs = 80

// speakStopCutoffMs converts a detected stop phrase (word index) plus the
// transcript's shared-clock word onsets into the dictation pipeline's audio
// cutoff. nil when the engine supplied no usable onsets — the stop still
// happens, whisper just transcribes everything (conservative).
func speakStopCutoffMs(onsets []uint64, wordIdx int) *uint64 {
	if wordIdx < 0 || wordIdx >= len(onsets) {
		return nil
	}
	c := onsets[wordIdx]
	if c > speakStopCutoffPadMs {
		c -= speakStopCutoffPadMs
	} else {
		c = 0
	}
	return &c
}

// fmtWordScores renders per-word margins for a log line: "[+5.2 -1.8]".
// Signed one-decimal matches the stage's own [PERF] margin readout, so the
// two log streams are directly comparable.
func fmtWordScores(scores []float64) string {
	parts := make([]string, len(scores))
	for i, s := range scores {
		parts[i] = fmt.Sprintf("%+.1f", s)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// Dictation profiles — what an utterance IS, declared by whatever mode is
// active and stamped on the transcript by the platform. The platform
// carries the name; this is
// the only place that knows what one MEANS, because the engine and its
// postprocessing live here.
const (
	// profileProse is writing: punctuation is intended and kept. The default
	// for an unstamped transcript, so behaviour is unchanged wherever no mode
	// declares otherwise.
	profileProse = "prose"
	// profileQuery is a search term or a name — a palette box, a find bar, a
	// form field. WhisperKit punctuates prosody reliably, including after a
	// single word ("Gmail."), and none of it was spoken.
	profileQuery = "query"
	// profileChat is a message to a person: a chat box, a comment field.
	//
	// Only the TRAILING sentence period goes. People do not end a chat message
	// with a full stop and Whisper adds one to every utterance, but internal
	// punctuation is still wanted — a message is prose, just prose that stops
	// without ceremony.
	//
	// Deliberately does not touch capitalisation, and no profile should. That
	// is the insert step's (insertion.go), which sees the text around the caret
	// and whether the previous sentence ended. Two steps shaping the same
	// property, unable to see each other, is how you get a capital that
	// flickers back. Profiles shape punctuation for the destination; the
	// insert step shapes the seam.
	profileChat = "chat"
)

// profileOrProse names the profile for a log line — an unstamped transcript
// is prose, and saying so beats an empty field.
func profileOrProse(profile string) string {
	if profile == "" {
		return profileProse
	}
	return profile
}

// applyDictationProfile shapes a transcript for the profile in force. Form
// only — punctuation and whitespace. Meaning is never touched: spelled numbers,
// spoken symbols and filler words are the consumer's problem (its matching is
// already tolerant) or the user's, via the corrections list, which applies in
// both profiles. An unknown profile — the `custom:` valve, or a name from a
// newer manifest — falls back to prose rather than guessing.
func applyDictationProfile(text, profile string) string {
	switch profile {
	case profileQuery:
		return stripPunctuation(text)
	case profileChat:
		return strings.TrimRight(text, ".")
	default:
		return text
	}
}

// stripPunctuation removes prosody punctuation and collapses whitespace.
// Dots and hyphens flanked by non-space characters survive: WhisperKit only
// writes those when they're structural ("wikipedia.com", "t-mobile" — it
// collapses spoken "dot com" into ".com"), and stripping them turned a
// dictated address into a search ("Wikipedia com"). Terminal or
// space-adjacent ones are prosody as before ("Gmail." → "Gmail").
func stripPunctuation(s string) string {
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	for i, r := range runes {
		switch r {
		case ',', '!', '?', ';', ':', '—':
			out = append(out, ' ')
		case '.', '-':
			if i > 0 && i < len(runes)-1 &&
				!unicode.IsSpace(runes[i-1]) && !unicode.IsSpace(runes[i+1]) {
				out = append(out, r)
			} else {
				out = append(out, ' ')
			}
		default:
			out = append(out, r)
		}
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

func (h *Host) handleVoiceWake(_ *branchkit.OnActionRequest) (any, error) {
	if h.sess.GetCurrentMode() != "continuous_sleeping" {
		return h.handled(), nil
	}
	h.setVoiceMode("continuous_awake")
	branchkit.Logf("voice", "continuous: wake")
	return h.handled(), nil
}

func (h *Host) handleVoiceSleep(_ *branchkit.OnActionRequest) (any, error) {
	if h.sess.GetCurrentMode() != "continuous_awake" {
		return h.handled(), nil
	}
	h.plugin.SessionBoundary()
	h.setVoiceMode("continuous_sleeping")
	h.hud.CloseDiscovery()
	branchkit.Logf("voice", "continuous: sleep")
	return h.handled(), nil
}

// handleVoiceRepeat handles a `voice.repeat` action that did NOT come from
// the matcher.
//
// The spoken path no longer reaches here. Orchestrate expands a resolved
// `voice.repeat` into N copies of the last action in place (see
// session.expandRepeat), so the platform executes the repeat directly and
// voice never dispatches for it. That closed the last routing path that left
// the platform and came back as a cross-plugin call carrying no trace.
//
// What is left is everything else that can carry an action: a keybind, a
// user-authored command bound outside the recognition pipeline, a manual
// dispatch. Those arrive as an ordinary `on_action` with no orchestration
// around them, so this still has to dispatch, and still has to say so when
// it is refused.
func (h *Host) handleVoiceRepeat(req *branchkit.OnActionRequest) (any, error) {
	lastMatch := h.sess.GetLastMatch()
	if lastMatch == nil {
		branchkit.Logf("voice", "repeat: no previous command")
		return h.handled(), nil
	}

	// Rebuild the resolved action shape expandRepeat reads, so one function
	// owns the count-and-mode rule for both entry points. `req.Params` is
	// already the action's params block. This used to marshal a
	// hand-written map; `branchkit.Action` is generated from the platform's
	// enum as of 2026-09-19.
	actionType := "voice.repeat"
	count := session.RepeatCount(&branchkit.Action{
		Type:       branchkit.ActionKindPlugin,
		ActionType: &actionType,
		Params:     req.Params,
	})
	if count == 0 {
		return h.handled(), nil
	}
	if lastMatch.Action == nil {
		return h.handled(), nil
	}

	branchkit.Logf("voice", "repeat: dispatching %d time(s) (non-orchestrate path)", count)

	// Replay the cached ACTION, never re-match the words (a tag-setting
	// command would match differently on a second pass) and never replay its
	// tag writes. Dispatch sequentially so ordered side effects (tab next x3)
	// don't race.

	for i := 0; i < count; i++ {
		// A refused dispatch is a SUCCESSFUL RPC carrying
		// `{"status":"denied"}` — not an error — so passing nil for the
		// result discarded the refusal twice over and repeat failed in
		// total silence.
		resp, err := h.plugin.Dispatch(branchkit.DispatchRequest{Action: *lastMatch.Action})
		if err != nil {
			branchkit.Logf("voice", "repeat: dispatch %d/%d failed: %v", i+1, count, err)
			break
		}
		if resp.Status != "ok" {
			msg := ""
			if resp.Message != nil {
				msg = *resp.Message
			}
			branchkit.Logf("voice", "repeat: dispatch %d/%d refused (status=%s) %s",
				i+1, count, resp.Status, msg)
			break
		}
		data, _ := json.Marshal(map[string]any{
			"repeat_index": i + 1,
			"repeat_total": count,
			"action":       session.ActionLabel(lastMatch.Action),
		})
		h.plugin.Notify(branchkit.MethodEventsAppend, &branchkit.EventsAppendRequest{
			EventType: "command_repeat",
			Data:      data,
		})
	}

	return h.handled(), nil
}

// handleVoicePickTie handles a `voice.pick_tie` action that did NOT come from
// the matcher.
//
// The spoken path no longer reaches here: orchestrate re-resolves the tie in
// place and returns the winner for the platform to execute (see
// isPickTieAction in orchestrate.go), which was the last routing path that
// left the platform and came back as a `dispatch` call. This remains for a
// keybind or a manual dispatch naming the action directly.
func (h *Host) handleVoicePickTie(req *branchkit.OnActionRequest) (any, error) {
	var p struct {
		Choice string `json:"choice"`
	}
	if err := req.UnmarshalParams(&p); err != nil {
		return nil, err
	}
	branchkit.Logf("voice", "pick_tie: choice=%q (non-orchestrate path)", p.Choice)
	winner := h.sess.ResolveTie(p.Choice)
	if winner == nil {
		return h.handled(), nil
	}
	// A refusal comes back as a successful RPC carrying
	// `{"status":"denied"}`, so discarding the result would hide it
	// entirely: the user picks a codeword and nothing happens, with nothing
	// logged anywhere.
	out, err := h.plugin.Dispatch(branchkit.DispatchRequest{Action: *winner})
	if err != nil {
		branchkit.Logf("voice", "pick_tie: dispatch failed: %v", err)
		return h.handled(), nil
	}
	if out.Status != "" && out.Status != "ok" {
		msg := ""
		if out.Message != nil {
			msg = *out.Message
		}
		branchkit.Logf("voice", "pick_tie: dispatch refused (status=%s) %s", out.Status, msg)
	}
	return h.handled(), nil
}

// handlePipelineTranscript is the `_platform.pipeline.transcript`
// subscription: the dictation pipeline's transcripts, typed at the cursor.
// A method rather than a closure inside main so a test can drive it.
func (h *Host) handlePipelineTranscript(payload branchkit.PipelineTranscriptEventParams) {
	// FocusedApp: which app had focus when the utterance ended, reported by
	// the platform, interpreted here; the standing default a gate overrides
	// FocusedAppNow: which app has focus as the
	// transcript is emitted; differs when focus moved while it transcribed.
	// DictationProfile: what the active modes say this utterance IS; empty
	// is prose. WordScores: per-word acoustic margin (sherpa CTC path),
	// log-only for now.
	focusedApp, focusedAppNow := derefString(payload.FocusedApp), derefString(payload.FocusedAppNow)
	dictationProfile := derefString(payload.DictationProfile)
	// An empty final dictation transcript is an ANSWER: the recognizer
	// heard nothing (a tap, silence). It must reach the dictation case and
	// finish the session there; dropped here, the session sat draining until
	// the pipeline stopped and voice reported the dictation lost — "Dictation
	// didn't come back" after every short tap.
	if payload.Text == "" && !isDictationPipeline(payload.Pipeline) {
		return
	}

	// ONE case, deliberately. `command_recognition` is not handled here:
	// it declares `dispatch: "delegated"`, so the platform asks voice
	// directly via `on_transcript` and executes the actions voice
	// answers with. Handling it here as well would orchestrate every
	// utterance twice. The empty-text branch above still runs for it —
	// that end-of-stream marker carries nothing for the platform to ask
	// about, and clearing the dedup state on it is voice's own
	// bookkeeping.
	switch payload.Pipeline {
	case dictationSamplePipeline:
		// Read a sentence: the reading, never typed. The recording is offered
		// to keep (readsentence.go).
		h.markDictationReady()
		if !payload.IsFinal {
			return
		}
		h.takeCaretPrefetch()
		if session, sentence, ok := h.readSentence.takeRecordedTranscript(time.Now()); ok {
			branchkit.Logf("voice", "pipeline transcript: %d chars → a Read a sentence recording, not typed tr=%s", len(payload.Text), h.traceID())
			go h.collectReadRecording(session, sentence, payload.Text)
		}
		h.sess.FinishDictation()
	case "dictation":
		// A dictation transcript proves the model is loaded — mark ready in
		// case the boot warm event was missed (e.g. plugin restart after warm).
		h.markDictationReady()
		if !payload.IsFinal {
			return
		}
		// The caret read the release started belongs to this transcript,
		// whichever way it ends: taken here, once, so a transcript that is
		// dropped, kept or empty cannot leave it for a later one (a speak
		// session never starts its own and reads the caret live).
		caret := h.takeCaretPrefetch()
		if !h.sess.IsDictationAccepting() {
			branchkit.Logf("voice", "pipeline transcript: %d chars → dropped (dictation %s) tr=%s",
				len(payload.Text), h.sess.GetDictationPipelineState(), h.traceID())
			h.emitDiagnostic("pstate: dictation transcript dropped (state=" + h.sess.GetDictationPipelineState().String() + ")")
			return
		}
		if h.sess.GetSpeakTranscriptPending() {
			// The audio cutoff (the samples from the stop phrase's onset
			// are dropped before whisper sees them) is the primary
			// stop-phrase remover — whisper usually never transcribes the
			// phrase, so the speak repair below is a no-op. It runs as a
			// text-domain backstop for the paths the cut can't cover: a stop
			// mis-heard by sherpa, a phrase split across VAD segments, an
			// escape close with no onset (nil cutoff), or the Linux
			// whisper-stt stage, which ignores cutoff_ms.
			h.sess.ClearSpeakTranscriptPending()
			rec := dictationEntry{Heard: payload.Text, App: focusedApp, Speak: true,
				Confidence: payload.Confidence, WordScores: payload.WordScores}
			rec.Repaired = h.sess.PostprocessDictation(rec.Heard, true)
			rec.Shaped = rec.Repaired
			rec.Refined = rec.Shaped
			if rec.Shaped != "" && h.refineEnabled() {
				rec.Refined, rec.RefineNote = h.refineDictation(rec.Shaped, profileProse)
			}
			if rec.Refined != "" {
				rec.Text, rec.Caret = h.fitDictationToCaret(rec.Refined)
				branchkit.Logf("voice", "pipeline transcript: %d chars → type_text (speak session, caret=%s) tr=%s", len(rec.Text), rec.Caret, h.traceID())
				if err := h.plugin.InputTypeText(branchkit.InputTypeTextRequest{Text: rec.Text}); err != nil {
					rec.TypeError = err.Error()
					h.reportTypingFailure(err)
				}
			}
			h.recordDictation(rec)
			if h.sess.GetSpeakDictating() {
				h.stopSpeakDictation(nil)
			}
			h.sess.FinishDictation()
			return
		}
		if h.sess.ConsumeTranscriptDiscard() {
			branchkit.Logf("voice", "pipeline transcript: %d chars → discarded (speak cancelled) tr=%s", len(payload.Text), h.traceID())
			h.sess.FinishDictation()
			return
		}
		// The typed-at-the-cursor path — the palette box, a form field, a
		// document — through the text pipeline's steps:
		// repair what was said, shape it for the destination's profile, then
		// fit it to the caret and type it. Each step's output is recorded.
		rec := dictationEntry{Heard: payload.Text, App: focusedApp,
			Confidence: payload.Confidence, WordScores: payload.WordScores}
		rec.Profile, rec.ProfileFrom = h.dictationProfileWithSource(dictationProfile, focusedApp)
		rec.Repaired = h.sess.PostprocessDictation(rec.Heard, false)
		rec.Shaped = applyDictationProfile(rec.Repaired, rec.Profile)
		if rec.Shaped == "" {
			h.recordDictation(rec)
			h.sess.FinishDictation()
			return
		}
		rec.Refined = rec.Shaped
		// A style chosen for this app rewrites; otherwise the clean-up step
		// may delete. Never both: a style already removes what clean-up would.
		if style := h.effectiveStyle(focusedApp); style != "" && styleModelReady() {
			rec.Refined, rec.RefineNote = h.styleDictation(rec.Shaped, style, rec.Profile)
		} else if h.refineEnabled() {
			rec.Refined, rec.RefineNote = h.refineDictation(rec.Shaped, rec.Profile)
		}
		if dictationFocusMoved(focusedApp, focusedAppNow) {
			rec.Kept, rec.Text = true, rec.Refined
			h.keepDictation(rec.Refined, focusedApp, focusedAppNow)
			h.recordDictation(rec)
			h.sess.FinishDictation()
			return
		}
		rec.Text, rec.Caret = h.fitReleasedDictationToCaret(rec.Refined, caret)
		branchkit.Logf("voice", "pipeline transcript: %d chars → type_text (profile=%s app=%s caret=%s) tr=%s",
			len(rec.Text), profileOrProse(rec.Profile), appOrUnknown(focusedApp), rec.Caret, h.traceID())
		if err := h.plugin.InputTypeText(branchkit.InputTypeTextRequest{Text: rec.Text}); err != nil {
			rec.TypeError = err.Error()
			h.reportTypingFailure(err)
		} else {
			// The first dictation that types finishes Setup's "Check it works".
			h.markTried()
		}
		h.recordDictation(rec)
		h.sess.FinishDictation()
	}
}

// handleOnTranscript is the platform's question: "your pipeline produced
// this — what should I do?"
//
// It is the ONLY entry point for command recognition now. The
// `_platform.pipeline.transcript` subscription (handlePipelineTranscript)
// deliberately does not handle this pipeline, because orchestrating an
// utterance twice would fire every command twice.
//
// Answering with an empty action list is normal and covers most utterances:
// a partial, a scope-dropped word, a miss, a suppressed pipeline, a
// calibration frame. None of those are errors and none of them should look
// like one.
func (h *Host) handleOnTranscript(req *branchkit.OnTranscriptRequest) (any, error) {
	// Defensive: voice owns two pipelines and only one is delegated.
	// `dictation` types text and never produces actions, so a transcript
	// arriving here from it would mean a manifest edit went wrong.
	if req.Pipeline != "command_recognition" {
		branchkit.Logf("voice", "on_transcript: ignoring transcript from unexpected pipeline %q", req.Pipeline)
		return &branchkit.OnTranscriptResponse{}, nil
	}

	// Speech right after an Option+G release, heard by the persistent run
	// started at the release: resolved once the base mode is back.
	h.awaitHoldCleanup()

	if h.sess.DropsCommandTranscripts() {
		branchkit.Logf("voice", "on_transcript: %d words → dropped (command_recognition suppressed) tr=%s",
			len(strings.Fields(req.Text)), h.traceID())
		h.emitDiagnostic("pstate: command_recognition transcript dropped (suppressed)")
		return &branchkit.OnTranscriptResponse{}, nil
	}

	isFinal := req.IsFinal != nil && *req.IsFinal
	// The wire carries onsets as plain integers; the speak-stop cutoff math
	// works in the shared chunk timebase as u64.
	onsets := make([]uint64, 0, len(req.WordOnsetsMs))
	for _, ms := range req.WordOnsetsMs {
		if ms < 0 {
			continue
		}
		onsets = append(onsets, uint64(ms))
	}

	actions := h.handleCommandTranscript(req.Text, isFinal, req.Confidence, onsets, req.WordScores, req.ActiveTags)
	return &branchkit.OnTranscriptResponse{Actions: actions}, nil
}

// handleCommandTranscript decides what one command-recognition transcript
// means and returns the actions the PLATFORM should execute.
//
// It returns them rather than dispatching them. `command_recognition`
// declares `dispatch: "delegated"`, so the platform calls this (via
// `on_transcript`) and executes the answer itself — which is what makes
// command routing structurally distinguishable from voice reaching into
// another plugin on its own initiative. Every early return below is an
// utterance that produced nothing to execute, and answering with an empty
// list is the ordinary way to say so.
func (h *Host) handleCommandTranscript(text string, isFinal bool, confidence *float64, wordOnsets []uint64, wordScores []float64, activeTags []string) []branchkit.Action {
	// Field-soak margin line (LOG ONLY, nothing gated on it): every continuous
	// decode — sleeping-mode wake candidates, speak-session stop-phrase
	// candidates, awake commands — records its per-word margins tagged with the
	// mode, so the genuine/coerced distributions accumulate from normal use.
	// Thresholds land per-consumer only after the distributions visibly
	// separate. Emitted BEFORE the calibration
	// intercept below: calibration-game utterances are the only labeled
	// ground-truth samples, so they are the highest-value input to the genuine
	// distribution and must not be excluded from the soak.
	//
	// The scores stand without the words: they align 1:1 with the platform's
	// own recognized-segment line for the same tr_, which shows the words
	// under the platform's recording rule.
	if len(wordScores) > 0 {
		branchkit.Logf("voice", "margin: mode=%s words=%d scores=%s tr=%s",
			h.sess.GetCurrentMode(), len(strings.Fields(text)), fmtWordScores(wordScores), h.traceID())
	}

	// Intercept for calibration game — feed frames before orchestration.
	// Calibration runs against the full union grammar (the
	// LockGrammar/UnlockGrammar callbacks wired in main.go are no-ops).
	if h.calib.ProcessSpeechFrame(text, isFinal) {
		return nil
	}

	if h.sess.GetCurrentMode() == "dictation" {
		if !h.sess.GetSpeakDictating() {
			return nil
		}
		// No confidence gate YET: the sherpa stage now emits per-word CTC
		// argmax margins (Transcript.word_scores, threaded here as
		// wordScores) but they are log-only until field distributions are
		// measured — the old per-word gate (55/word, tuned on the retired
		// streaming engine) ran as a silent no-op for weeks, so the new gate
		// lands against measured numbers, not inherited ones. Until then
		// robustness rests on the phrase being an exact adjacent pair
		// against the full union grammar.
		lowerWords := strings.Fields(strings.ToLower(text))
		// The pair may straddle a VAD split ("SPEAK" | "OVER"): the previous
		// transcript's last word pairs with this one's first, within the
		// split's time bound (speakPhraseSpan).
		if isFinal && len(lowerWords) == 0 {
			h.speakCarry.reset() // end of the recognition audio
			return nil
		}
		prev := h.speakCarry.swap(lowerWords, wordOnsets)
		stopIdx, found := speakPhraseSpan(prev, lowerWords, wordOnsets, speech.StopPhrases)
		if !found {
			if _, cancel := speakPhraseSpan(prev, lowerWords, wordOnsets, speech.CancelPhrases); cancel {
				h.speakCarry.reset()
				h.sess.DiscardSpeakTranscript()
				h.stopSpeakDictation(nil)
				branchkit.Logf("voice", "continuous pipeline: speak cancelled (%d words) tr=%s", len(lowerWords), h.traceID())
			}
			return nil
		}
		h.speakCarry.reset()
		if stopIdx < 0 {
			// A split pair always has the carried word's onset, so the cut
			// lands where the phrase began, in the previous segment.
			cutoff := speakPhraseCutoffMs(prev, wordOnsets, stopIdx)
			h.stopSpeakDictation(cutoff)
			if cutoff != nil {
				branchkit.Logf("voice", "continuous pipeline: speak done detected across a segment split (cutoff %dms) tr=%s",
					*cutoff, h.traceID())
			}
			return nil
		}
		// Stop the dictation pipeline AT the phrase: the cutoff (phrase
		// onset minus a small pad, in the shared chunk timebase) rides
		// pipelines.stop to whisper's buffer, so the phrase — and anything
		// spoken after it — never reaches the model. When onsets are absent
		// (nil cutoff) whisper transcribes everything and the text-domain
		// backstop (speech.StripSpeakPhrases) removes the trailing phrase.
		cutoff := speakStopCutoffMs(wordOnsets, stopIdx)
		h.stopSpeakDictation(cutoff)
		if cutoff != nil {
			branchkit.Logf("voice", "continuous pipeline: speak done detected at word %d of %d (cutoff %dms) tr=%s",
				stopIdx+1, len(lowerWords), *cutoff, h.traceID())
		} else {
			branchkit.Logf("voice", "continuous pipeline: speak done detected at word %d of %d (no onsets — transcribing everything) tr=%s",
				stopIdx+1, len(lowerWords), h.traceID())
		}
		return nil
	}

	// The Models tab's Say a phrase: while it listens, a command is recorded
	// to compare the models on and never run (saynow.go).
	if strings.TrimSpace(text) != "" && h.sayNow.intercepts(time.Now()) {
		return nil
	}

	// Setup's command check: while it listens, a command is scored in
	// preview and never run (setup_health.go).
	if strings.TrimSpace(text) != "" && h.health.intercepts(time.Now()) {
		go h.checkCommandTranscript(text)
		return nil
	}

	mode := h.sess.GetCurrentMode()

	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return nil
	}

	cfg := h.config.Get()
	worst := worstMargin(wordScores)
	switch commandGate(mode, worst, cfg.CommandConfidenceGate, cfg.CommandConfidenceFloor) {
	case gateDrop:
		branchkit.Logf("voice", "confidence gate: dropped %q worst=%+.1f floor=%+.1f mode=%s tr=%s",
			strings.ToLower(text), *worst, cfg.CommandConfidenceFloor, mode, h.traceID())
		h.recordSkipped(strings.ToLower(text), mode, worst)
		return nil
	case gateFlag:
		branchkit.Logf("voice", "confidence gate (shadow): would drop %q worst=%+.1f floor=%+.1f mode=%s tr=%s",
			strings.ToLower(text), *worst, cfg.CommandConfidenceFloor, mode, h.traceID())
	}

	orchReq := &session.OrchestrateRequest{
		Transcript: strings.ToLower(text),
		IsFinal:    isFinal,
		Mode:       mode,
		Words:      words,
		// The tags the platform says are active. This used to be left
		// empty on every call, which made `req_tags=[]` in the orchestrate
		// log a lie rather than a fact and left the dismiss/over bypass
		// below unreachable. The platform sends them on `on_transcript`;
		// dropping them here was half-wiring the field into the contract.
		ActiveTags: activeTags,
	}
	orchResp := h.sess.OrchestrateInternal(orchReq)
	// Everything except the actions — discovery, the event log, the HUD —
	// is voice's own business and is applied here. The actions go back on
	// the response.
	h.sess.ApplyOrchestrateResults(orchResp)

	matched := len(orchResp.Actions) > 0

	confStr := "none"
	if confidence != nil {
		confStr = fmt.Sprintf("%.2f", *confidence)
	}
	branchkit.Logf("voice", "continuous pipeline: orchestrated %d words conf=%s matched=%v tr=%s",
		len(words), confStr, matched, h.traceID())

	// Feed the recent-command history (the Option+B "History" tab): what
	// fired and how sure the engine was — the misfire surface — and also
	// what was heard and resolved to nothing. The platform's
	// report of what each action did joins the entry later, on its trace
	// (handlePipelineExecuted). Confidence is display-only here; the eventual
	// floor gates on the same worst-word margin (see history.go).
	rec := commandRecord{
		Phrase: orchReq.Transcript, Mode: mode, Conf: confidence, Worst: worst,
		Trace: h.plugin.CurrentCorrelation(), Stop: orchResp.Stop, Unresolved: orchResp.Unresolved,
	}
	switch {
	case matched:
		for _, a := range orchResp.Actions {
			rec.Actions = append(rec.Actions, actionTypeLabel(a))
		}
		h.recordMatch(rec)
	case recordsUnresolved(mode, orchResp.Stop):
		h.recordUnresolved(rec)
	}

	return orchResp.Actions
}

// recordsUnresolved says whether an utterance that resolved to nothing gets
// a History row. A miss or a tie is a command that did not happen. A partial
// is not one yet (its continuation follows), and asleep, every word that is
// not the wake phrase misses by design.
func recordsUnresolved(mode, stop string) bool {
	if mode == "continuous_sleeping" {
		return false
	}
	return stop == "miss" || stop == "tie" || stop == "blocked"
}

// worstMargin returns the minimum (least-confident) per-word margin, or nil
// when the engine carried no per-word signal. The min is the misfire predictor
// — the mean hides a single deeply-coerced word.
func worstMargin(scores []float64) *float64 {
	if len(scores) == 0 {
		return nil
	}
	w := scores[0]
	for _, s := range scores[1:] {
		if s < w {
			w = s
		}
	}
	return &w
}

// stopSpeakDictation closes a live speak session. cutoffMs (shared chunk
// timebase) rides pipelines.stop to whisper's buffer so audio at/after the
// detected stop phrase is never transcribed; nil (the escape/cancel/toggle
// paths, where no phrase position is known) transcribes everything buffered.
func (h *Host) stopSpeakDictation(cutoffMs *uint64) {
	if !h.sess.GetSpeakDictating() {
		return
	}
	h.sess.SetSpeakDictating(false)
	h.sess.DrainDictation()
	h.plugin.CollectionDeleteRecords(branchkit.CollectionDeleteRecordsRequest{Name: "plugin.voice.speaking", Ids: []string{"singleton"}})
	// The local cutoff is *uint64; the wrapper takes *int. Converting here
	// rather than widening the wrapper — the untyped map hid this mismatch
	// by round-tripping through JSON.
	var cutoff *int
	if cutoffMs != nil {
		c := int(*cutoffMs)
		cutoff = &c
	}
	h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "dictation", AudioCutoffMs: cutoff})
	// The command_recognition run is this session's to stop only when nothing
	// else holds it: continuous listening keeps its persistent run, and a
	// command hold still down keeps its own (its release stops it).
	if !h.sess.IsContinuousActive() && !h.sess.CommandHoldActive() {
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "command_recognition"})
	}
	h.leaveDictationOverlay()
}

func (h *Host) handleVoiceSpeak(req *branchkit.OnActionRequest) (any, error) {
	if isAbortPhase(req) || isStopPhase(req) {
		if !h.sess.GetSpeakDictating() {
			return h.handled(), nil
		}
		branchkit.Logf("voice", "speak: stop background dictation")
		h.sess.DiscardSpeakTranscript()
		h.stopSpeakDictation(nil)
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "dictation"})
		return h.handled(), nil
	}

	branchkit.Logf("voice", "speak: start background dictation")
	h.startSpeakSession()
	return h.handled(), nil
}

// startSpeakSession opens a background speak-dictation session: the dictation
// engine transcribes free text that the transcript sink types at the cursor,
// while command_recognition runs in parallel to catch the spoken stop-phrase.
// Shared by `speak start` (handleVoiceSpeak) and `find text` (handleVoiceFindDictate).
func (h *Host) startSpeakSession() {
	h.speakCarry.reset() // nothing pairs with the previous session's words
	h.sess.StartDictationRecording()
	h.warmRefine()
	h.enterDictationOverlay()
	h.sess.SetSpeakDictating(true)
	h.plugin.CollectionPut(branchkit.CollectionPutRequest{
		Name:    "plugin.voice.speaking",
		Entries: []branchkit.CollectionPutEntry{{ID: "singleton", Payload: json.RawMessage("{}")}},
	})
	if err := h.runPipeline(h.dictationRunParams()); err != nil {
		branchkit.Logf("voice", "speak: pipelines.run error: %v", err)
	}
	if !h.sess.IsContinuousActive() {
		h.runCommandRecognition(true)
	}
	h.setIndicator(branchkit.IndicatorStateSpeaking)
}

// isStopPhase returns true if phase is "stop" (from keybind release,
// toggle deactivate, or voice command).
func isStopPhase(req *branchkit.OnActionRequest) bool {
	return req.Phase != nil && *req.Phase == "stop"
}

// isAbortPhase returns true if phase is "abort" — abort without
// finalizing (e.g., discard a recording instead of transcribing it).
func isAbortPhase(req *branchkit.OnActionRequest) bool {
	return req.Phase != nil && *req.Phase == "abort"
}

func (h *Host) handleVoiceContinuousToggle(_ *branchkit.OnActionRequest) (any, error) {
	now := time.Now()
	if now.Sub(h.lastContinuousToggle) < 200*time.Millisecond {
		branchkit.Logf("voice", "continuous-toggle: debounce (%.0fms since last)", now.Sub(h.lastContinuousToggle).Seconds()*1000)
		return h.handled(), nil
	}
	h.lastContinuousToggle = now
	branchkit.Logf("voice", "continuous-toggle")

	// A toggle during speak-dictation must close the speak session first
	// — IsContinuousActive() is true during the speak overlay (priorMode),
	// so the off-path below would tear down continuous and leave the
	// dictation pipeline running with a hot mic (observed 2026-06-12: a
	// 9-minute orphaned session). Same no-discard semantics as the
	// Option+T escape path.
	if h.sess.GetSpeakDictating() {
		branchkit.Logf("voice", "continuous-toggle: closing live speak dictation first")
		h.stopSpeakDictation(nil)
	}

	// While Option+G is down the toggle waits for its release, in both
	// directions: turning continuous off under a hold would leave the hold to
	// restore a mode that is gone (its overlay saved continuous as the base).
	if h.sess.CommandHoldActive() {
		branchkit.Logf("voice", "continuous-toggle: blocked — command_hold active")
		return h.handled(), nil
	}
	// Released but still draining its run's last words: end the hold now,
	// so the toggle below finds the mode the hold covered rather than
	// command_hold. Its claim on the persistent run is dropped first, so
	// turning listening off does not restart that run on the way.
	if h.sess.CommandHoldDraining() {
		branchkit.Logf("voice", "continuous-toggle: ending the released hold first")
		h.sess.ReleaseCmdRecAside()
		h.sess.FinishCommandHoldDrain(h.finishCommandHold)
	}

	if h.sess.IsContinuousActive() {
		h.sess.SetCmdRecInactive()
		h.plugin.SessionBoundary()
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "command_recognition"})
		h.sess.ClearLastMatch()
		h.setVoiceMode("idle")
		h.hud.CloseDiscovery()
	} else {
		mode := h.sess.GetCurrentMode()
		if mode == "command_hold" || mode == "dictation" {
			branchkit.Logf("voice", "continuous-toggle: blocked — %s active", mode)
			return h.handled(), nil
		}
		// 3D.2 privacy gate: continuous = mic hot with no physically-held
		// key, so it requires a visible recording indicator. On Linux with
		// no tray host (stock GNOME without the AppIndicator extension) the
		// platform reports no surface — refuse rather than record silently.
		// Fail-closed: an errored probe also refuses; push-to-talk (where
		// the held key IS the indicator) is never gated.
		if available, err := h.statusIndicatorAvailable(); err != nil || !available {
			if err != nil {
				branchkit.Logf("voice", "continuous-toggle: refused — indicator probe failed: %v", err)
			} else {
				branchkit.Logf("voice", "continuous-toggle: refused — no visible recording indicator")
			}
			body := "Continuous listening needs a visible recording indicator. " +
				"Install your desktop's AppIndicator/tray extension, or use push-to-talk."
			if _, nerr := h.plugin.NativeNotify(branchkit.NativeNotifyRequest{Title: "BranchKit", Body: &body}); nerr != nil {
				branchkit.Logf("voice", "continuous-toggle: refusal notification failed: %v", nerr)
			}
			return h.handled(), nil
		}
		h.sess.SetCmdRecActive()
		h.setVoiceMode("continuous_sleeping")
		h.runCommandRecognition(false)
	}
	return h.handled(), nil
}

// toggleHelpHud flips the main HUD between the commands view and closed, and
// reports what still has to be effected on the channel. Two callers share it
// and differ only in how they effect that: an action returns a control
// message, while a settings method has no control-message channel and issues
// the RPC itself.
//
// enabled is false when the channel is switched off, in which case neither
// caller should do anything at all.
func (h *Host) toggleHelpHud() (open bool, enabled bool) {
	if !h.hud.Config().IsChannelEnabled("main") {
		return false, false
	}
	if h.hud.IsHudOpen() {
		h.hud.SetHudOpen(false)
		return false, true
	}
	h.hud.SetMainMode("commands")
	h.hud.SetHudOpen(true)
	return true, true
}

func (h *Host) handleVoiceHelpToggle(_ *branchkit.OnActionRequest) (any, error) {
	open, enabled := h.toggleHelpHud()
	if !enabled {
		return h.handled(), nil
	}
	if open {
		if err := h.plugin.HUDShow(branchkit.HUDShowRequest{Channel: "main"}); err != nil {
			return nil, err
		}
		return h.handled(), nil
	}
	if err := h.plugin.HUDHide(branchkit.HUDHideRequest{Channel: "main"}); err != nil {
		return nil, err
	}
	return h.handled(), nil
}

// handleToggleHelpHud is the pointer-reachable route to the same toggle.
//
// It exists because the "Show HUD" menu bar item that used to open this HUD
// hardcoded "voice.help_toggle" in the Swift shell -- the shell's only
// plugin-specific action string -- and has been removed. opt+b still
// dispatches the action above, but a two-key chord is not a mouse path, so
// the HUD settings tab carries one.
func (h *Host) handleToggleHelpHud(_ *struct{}) error {
	open, enabled := h.toggleHelpHud()
	if !enabled {
		return nil
	}
	if open {
		return h.plugin.HUDShow(branchkit.HUDShowRequest{Channel: "main"})
	}
	return h.plugin.HUDHide(branchkit.HUDHideRequest{Channel: "main"})
}

func (h *Host) handleSetMainMode(req *struct {
	Mode string `json:"mode"`
}) error {
	h.hud.SetMainMode(req.Mode)
	return nil
}

func (h *Host) handleRenderHud(req *RenderHUDRequest) (any, error) {
	switch req.HudMode {
	case "commands", "selection":
		// Fetch command data from the platform API — plugin owns rendering
		var sections []hud.HudSection
		title := req.HudMode
		footer := ""

		if listResp, err := h.plugin.CommandsList(); err == nil {
			sections = hud.SectionsFromCommandList(listResp.Sections)
			title, footer = listResp.Title, listResp.Footer
		}

		conf := h.hud.Config()
		html := hud.RenderSectionsHTML(sections, conf, "commands")

		return hud.HudResponse{
			Title:       title,
			Footer:      footer,
			ContentHTML: &html,
			Sections:    sections,
		}, nil

	case "entities":
		// Fetch entity collections from the platform
		entity := "entity"
		sections, err := h.plugin.CollectionsList(branchkit.CollectionsListRequest{Kind: &entity})
		if err != nil {
			branchkit.Logf("voice", "hud: collections.list failed: %v", err)
			return hud.HudResponse{Title: "Entities", Footer: "Failed to load", Sections: []hud.HudSection{}}, nil
		}

		conf := h.hud.Config()
		entitiesHTML := hud.RenderEntitySectionsHTML(sections, conf)

		return hud.HudResponse{
			Title:       "Entities",
			Footer:      fmt.Sprintf("%d collections", len(sections)),
			ContentHTML: &entitiesHTML,
		}, nil

	case "history":
		historyHTML := h.renderHistoryHTML()
		return hud.HudResponse{
			Title:       "History",
			Footer:      "Recent commands and how confident the engine was",
			ContentHTML: &historyHTML,
		}, nil

	case "dictation":
		dictationHTML := h.renderDictationHistoryHTML()
		return hud.HudResponse{
			Title:       "Dictation",
			Footer:      "Recent dictations — Copy puts one back on the clipboard",
			ContentHTML: &dictationHTML,
		}, nil

	default:
		return hud.HudResponse{
			Title:    "Unknown",
			Sections: []hud.HudSection{},
		}, nil
	}
}

// settingsSnap is the state every voice tab reads at the top of a render.
type settingsSnap struct {
	search      string
	phonetics   []PhoneticEntry
	calibActive bool
	calibGame   CalibrationGame
	testPhrase  string
}

// voiceTab wraps one tab's draw function with the work every voice tab
// shares. The SDK's render hook has already refreshed both settings
// mirrors, so the snapshot is at least as fresh as whatever woke the render.
func (h *Host) voiceTab(key string, draw func(req *RenderSettingsRequest, snap settingsSnap) string) branchkit.PageFunc {
	return func(req *RenderSettingsRequest) (string, error) {
		// Track which settings tab is visible for calibration gating.
		// Liveness is NOT touched here — a server-side render doesn't prove
		// the browser page is still open (refreshes are server-driven). Only
		// the page's own heartbeat ping (client-originated) bumps lastSeen.
		if key == "calibration" {
			h.calib.SetTabVisible(true)
		} else {
			h.calib.SetTabVisibleIfNotActive(false)
		}
		calibActive, calibGame := h.calib.SnapshotForRender()
		snap := settingsSnap{
			search:      strings.ToLower(req.Search),
			phonetics:   h.sess.GetPhonetics(),
			calibActive: calibActive,
			calibGame:   calibGame,
			testPhrase:  h.edState.GetTestPhrase(),
		}
		return draw(req, snap), nil
	}
}

// registerSettingsTabs installs one renderer per tab declared in plugin.json.
func (h *Host) registerSettingsTabs(p *branchkit.Plugin) {
	p.SettingsCSS(voiceCSS)
	// Get started is also the body of voice's Setup step (setup.go).
	p.Page("start", h.voiceTab("start", func(req *RenderSettingsRequest, _ settingsSnap) string {
		return h.renderGetStarted(modelDownloadFromCollection(req.CollectionData), req.CollectionData["_platform.bindings.active"])
	}))
	p.Page("commands", h.voiceTab("commands", func(req *RenderSettingsRequest, snap settingsSnap) string {
		editorSnap := h.edState.UpdateCaches(req.Commands, req.ActionTypeSchemas, req.ListSchemas, req.TagSchemas)
		return h.renderRules(snap.search, req.Commands, h.getCommandFilter(), snap.testPhrase, &editorSnap, req.ListSchemas)
	}))
	p.Page("phonetics", h.voiceTab("phonetics", func(req *RenderSettingsRequest, snap settingsSnap) string {
		return h.renderPhonetics(snap.search, snap.phonetics, req.Commands)
	}))
	p.Page("calibration", h.voiceTab("calibration", func(_ *RenderSettingsRequest, snap settingsSnap) string {
		return h.renderCalibration(snap.calibActive, len(snap.phonetics) > 0, snap.calibGame, h.calib.ActiveCommandID())
	}))
	p.Page("recordings", h.voiceTab("recordings", func(_ *RenderSettingsRequest, _ settingsSnap) string {
		return h.renderRecordings()
	}))
	p.Page("dictation", h.voiceTab("dictation", func(req *RenderSettingsRequest, snap settingsSnap) string {
		lang := h.config.Get().Language
		pluginCorr := h.speech.Get()
		corrRows := make([]CorrectionRowData, len(pluginCorr))
		for i, c := range pluginCorr {
			corrRows[i] = CorrectionRowData{
				ID: c.ID, MatchPattern: c.MatchPattern,
				Replacement: c.Replacement, IsRegex: c.IsRegex, Enabled: c.Enabled,
				Source: c.Source,
			}
		}
		return h.renderDictation(snap.search, lang, corrRows, req.CollectionData["_platform.bindings.active"], modelDownloadFromCollection(req.CollectionData))
	}))
	p.Page("models", h.voiceTab("models", func(req *RenderSettingsRequest, _ settingsSnap) string {
		return h.renderModels(modelDownloadFromCollection(req.CollectionData), req.CollectionData["_platform.bindings.active"])
	}))
	p.Page("hud", h.voiceTab("hud", func(_ *RenderSettingsRequest, _ settingsSnap) string {
		return renderHudSettings(h.hud.Config())
	}))
}

func (h *Host) handleOnCommandsChanged(req *OnCommandsChangedRequest) (any, error) {
	newPhonetics := h.buildPhonetics()
	count := len(newPhonetics)
	branchkit.Logf("voice", "Command registry rebuilt: %d phonetics", count)

	// Refresh the in-memory cache only. Do NOT re-push the alphabet here:
	// buildPhonetics reads the override-applied view, and re-pushing it via
	// collection.replace would bake user codeword overrides into the base
	// collection (defeating the override layer + leaving un-resettable residue).
	// The base is owned by initAlphabet (boot defaults); overrides compose on top.
	h.sess.SetPhonetics(newPhonetics)

	// The command set changed — drop the cached calibration sidebar catalog.
	h.invalidateCatalogCache()

	return OnCommandsChangedResponse{PhoneticsCount: count}, nil
}

// derefString is the value of an optional string field, or "".
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
