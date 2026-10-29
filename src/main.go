package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	"github.com/branchkit/branchkit-plugin-voice/settings"
	"github.com/branchkit/plugin-sdk-go"
)

// --- Plugin state ---

// editorState holds editor-domain state with its own mutex.
// Includes the rule editor form, test phrase, and schema caches.
type editorState struct {
	mu            sync.Mutex
	editor        EditorState
	testPhrase    string
	lastCommands  []CommandRowData
	actionSchemas map[string]ActionTypeSchema
	listSchemas   map[string]ListSchemaInfo
	tagSchemas    map[string]TagSchemaInfo
	apps          []AppEntry // installed apps for the "Only in app" scope dropdown
}

func (s *editorState) SetTestPhrase(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.testPhrase = v
}

func (s *editorState) GetTestPhrase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.testPhrase
}

// UpdateCaches stores command/schema caches and returns the current editor snapshot.
// Atomically updates caches and reads editor state, fixing the race in buildEditorView.
func (s *editorState) UpdateCaches(
	commands []CommandRowData,
	actionSchemas map[string]ActionTypeSchema,
	listSchemas map[string]ListSchemaInfo,
	tagSchemas map[string]TagSchemaInfo,
) EditorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCommands = commands
	s.actionSchemas = actionSchemas
	s.listSchemas = listSchemas
	s.tagSchemas = tagSchemas
	return s.editor
}

// Type alias left from the calibration package migration. The other four
// (WordResult, Trial, History, HistoricalSession) had no users at all and went
// with the History types themselves.
type CalibrationGame = calibration.Game

// A press shorter than tapMS is a tap rather than a hold. A tap
// alone types like any short hold; two taps within doubleTapWindowMS latch
// dictation on until the next press, so nobody has to hold a key to dictate.
// The window is also how long a lone tap waits before it is transcribed.
const (
	tapMS             = 300
	doubleTapWindowMS = 350
)

// dictationBounceMS is the switch-debounce window for Option+T: a press
// arriving this soon after the quick-tap latch is contact bounce on a key
// still being held, not a deliberate second press.
//
// 60ms sits above observed chatter (a Kinesis Adv360 Pro produced a 6ms
// up→down gap, with HID reports 29-46ms apart) and far below any intentional
// double-press. Debouncing is normally the keyboard's job; this guard exists
// because when it fails the failure is SILENT — a held key latches and
// instantly stops, and the user gets an empty transcript with no error
// anywhere to explain it.
const dictationBounceMS = 60

// sess holds session-domain state (dictation, dedup, phonetics) with its own mutex.

// calib holds calibration-domain state (game, tab visibility) with its own mutex.

// edState holds editor-domain state (form, schema caches) with its own mutex.

func (h *Host) emitDiagnostic(msg string) {
	// Notify, not the generated EventsAppend wrapper: that one is built on
	// Call and would block. The typed payload comes from the generated
	// request struct plus the method constant, which needs no new SDK
	// surface — same shape as plugins/browser's emitEvent.
	data, _ := json.Marshal(map[string]any{"message": msg})
	h.plugin.Notify(branchkit.MethodEventsAppend, &branchkit.EventsAppendRequest{
		EventType: "diagnostic.message",
		Data:      data,
	})
}

type ListSchemaInfo = branchkit.SettingsListSchemaInfo
type TagSchemaInfo = branchkit.SettingsTagSchemaInfo

// clearOrphanedSpeakingTag is GONE. plugin.voice.speaking is a writer-held tag
// (`held_by_writer`, the tag-preset default), so the platform releases it in
// cleanup_terminated_plugin the moment this plugin's previous process
// terminates — a fresh process never inherits it. The hand-rolled boot clear
// that lived here was the original instance of the pattern the platform now
// owns; see the platform's held-record reaping.

// pauseKeybinds / resumeKeybinds hold and release the `suppress_keybinds`
// effect around the command editor's key capture.
//
// This replaced raw `ControlSignal("keybind:pause")` — the ownerless global
// lease of remediation item 1.3, where a crash mid-capture left every hotkey
// dead with nothing that knew to resume. As an effect the platform owns the
// lifetime: the pause fires only on the empty→held transition, resume only
// when the LAST holder releases (so an overlapping keyboard-plugin capture
// isn't stomped), and `cleanup_terminated_plugin` releases the hold if this
// process dies mid-capture, on every platform. The boot-time unconditional
// resume that lived here — and its accepted cross-plugin race — is gone for
// exactly that reason. The keyboard plugin carries the twin of these helpers.
//
// The capture bounds its own pause (captureTimeout): nothing tells this
// plugin when the Settings tab closes mid-recording, so an abandoned
// recording held every hotkey paused — on macOS until the shell's 30 s
// auto-resume, on Linux and Windows until this plugin restarted. The bound
// lives with the holder, on every OS (2026-09-25).
func (h *Host) pauseKeybinds() {
	h.armCaptureTimeout()
	h.holdPause()
}

func (h *Host) resumeKeybinds() {
	h.stopCaptureTimeout()
	h.releasePause()
}

// captureTimeout is how long a recording may hold the pause. A var so tests
// can shorten it.
var captureTimeout = 30 * time.Second

func (h *Host) armCaptureTimeout() {
	h.captureMu.Lock()
	defer h.captureMu.Unlock()
	if h.captureTimer != nil {
		h.captureTimer.Stop()
	}
	h.captureTimer = time.AfterFunc(captureTimeout, h.expireCapture)
}

func (h *Host) stopCaptureTimeout() {
	h.captureMu.Lock()
	defer h.captureMu.Unlock()
	if h.captureTimer != nil {
		h.captureTimer.Stop()
		h.captureTimer = nil
	}
}

// expireCapture ends the recording as Escape would.
func (h *Host) expireCapture() {
	h.edState.mu.Lock()
	h.edState.editor.Recording = false
	h.edState.mu.Unlock()
	branchkit.Logf("voice", "shortcut recording idle for %s — cancelled, hotkeys resumed", captureTimeout)
	h.resumeKeybinds()
}

func (h *Host) holdPauseDefault() {
	out, err := h.plugin.AssertEffect("suppress_keybinds")
	if err != nil {
		branchkit.Logf("voice", "suppress_keybinds assert failed: %v", err)
		return
	}
	if !out.Enforced {
		branchkit.Logf("voice", "suppress_keybinds not enforced — captured keys may also trigger commands")
	}
}

func (h *Host) releasePauseDefault() {
	if _, _, err := h.plugin.RetractEffect("suppress_keybinds"); err != nil {
		branchkit.Logf("voice", "suppress_keybinds retract failed: %v", err)
	}
}

func main() {
	// Phase 2 first: the host owns the platform handle and every domain
	// state, built once here.
	h := newHost(branchkit.NewPlugin())

	// Phase 3: Wire plugin instances and load from collections
	// settings.Init wires the typed settings mirror (preset: settings) —
	// the platform materializes defaults + user changes; no plugin-side load.
	h.speech.LoadFromCollection()
	h.speech.LoadVocabulary()
	// Per-app dictation profiles: user overrides only. Nothing else is stored —
	// the standing defaults are derived from the apps plugin's app traits, so
	// a fact corrected in a later release reaches existing users rather than
	// being frozen at first run.
	h.loadAppProfiles()
	h.loadAppStyles()
	// The FACT half, published by the apps plugin. Read once here and again
	// on every collection.updated, because system's app scan can be slow at
	// boot and the traits may not exist yet when voice starts.
	h.loadAppTraits()
	h.subscribeAppTraits()
	if !h.appTraitsLoadedOK() {
		go h.retryAppTraits()
	}

	branchkit.Logf("voice", "Corrections loaded: %d entries", len(h.speech.Get()))
	h.speech.PushBias()
	vc := h.config.Get()
	branchkit.Logf("voice", "Config loaded: model=%s lang=%s cmdLang=%s fixAllCaps=%v",
		vc.Model, vc.Language, vc.CommandLanguage, vc.FixAllCaps)

	// Wire calibration callbacks
	h.calib.OnGameFinished = func() { h.sess.SetCalibrationConsumed(true) }
	h.calib.Platform = h.plugin
	h.calib.GetPhonetics = func() []struct {
		Word   string
		Letter *byte
	} {
		phonetics := h.sess.GetPhonetics()
		out := make([]struct {
			Word   string
			Letter *byte
		}, len(phonetics))
		for i, p := range phonetics {
			out[i] = struct {
				Word   string
				Letter *byte
			}{Word: p.Word, Letter: p.Letter}
		}
		return out
	}
	h.calib.SettingsRedirect = func(tab string) error {
		return h.plugin.SettingsRedirect(branchkit.SettingsRedirectRequest{Tab: tab})
	}
	h.calib.Append = func(collection string, payload any) error {
		_, err := h.plugin.Append(collection, payload)
		return err
	}
	h.calib.TrialBegin = func() (string, error) {
		return beginTrialOrAsk(
			func() (string, error) {
				resp, err := h.plugin.TrialBegin()
				if err != nil {
					return "", err
				}
				return resp.TrialID, nil
			},
			func() (string, error) {
				reason := calibrationPrivilegeReason
				resp, err := h.plugin.PrivilegesRequest(branchkit.PrivilegesRequestRequest{
					Privilege: calibrationPrivilege,
					Reason:    &reason,
				})
				if err != nil {
					return "", err
				}
				return resp.Status, nil
			},
			&h.calibPrivilegeAsked,
		)
	}
	h.calib.TrialEnd = func(trialID string) error {
		_, err := h.plugin.TrialEnd(branchkit.TrialEndRequest{TrialID: trialID})
		return err
	}
	h.calib.ApplyContext = func(trialID, commandID string) (calibration.AppliedContext, error) {
		// trial.enter_context enters the context for any command tier and returns
		// it flat: kind ("static"|"dynamic"), the mode-gated tags the platform
		// platform-wrote, and (dynamic only) the fixture handle.
		resp, err := h.plugin.TrialEnterContext(branchkit.TrialEnterContextRequest{CommandID: commandID, TrialID: trialID})
		if err != nil {
			return calibration.AppliedContext{}, err
		}
		return calibration.AppliedContext{Kind: resp.Kind}, nil
	}
	// ResolvePreview is the verify-don't-execute resolve: preview=true so the
	// matcher computes the full decision but commits nothing (no tag writes, no
	// bridge, no telemetry). Captures matched/owner/winning_pattern so the
	// Resolves signal can confirm the heard phrase resolves to the intended
	// command in its (now-live) context without firing it.
	h.calib.ResolvePreview = func(heard string) *calibration.MatcherDecision {
		preview := true
		resp, err := h.plugin.CommandsResolve(branchkit.CommandsResolveRequest{
			Preview: &preview,
			Words:   strings.Fields(heard),
		})
		if err != nil {
			branchkit.Logf("voice", "calibration: practice_check(%d words) failed: %v", len(strings.Fields(heard)), err)
			return nil
		}
		// owner_plugin and winning_pattern are optional on the wire; the
		// local mirror declared both as plain strings, so an absent value
		// arrived as "" and read as "no owner" rather than "not reported".
		owner := ""
		if resp.OwnerPlugin != nil {
			owner = *resp.OwnerPlugin
		}
		pattern := ""
		if resp.Telemetry.WinningPattern != nil {
			pattern = *resp.Telemetry.WinningPattern
		}
		return &calibration.MatcherDecision{
			Winner:         string(resp.Telemetry.Winner),
			ActiveGates:    resp.ScopedTags,
			HasCompletions: resp.HasCompletions,
			NextWords:      resp.NextWords,
			Matched:        resp.Matched,
			Owner:          owner,
			WinningPattern: pattern,
		}
	}
	h.calib.EnumerateCommands = h.enumerateCommandsForPractice
	// Owner-supplied calibration prompts: the platform forwards to the owning
	// plugin's `trial_samples` hook. Lets a third-party plugin's dynamic
	// command supply its realistic vocabulary (instead of the codeword stand-in).
	h.calib.CommandSamples = func(commandID string) []string {
		prompts, err := h.plugin.TrialResolveSamples(branchkit.TrialResolveSamplesRequest{CommandID: commandID})
		if err != nil {
			return nil
		}
		return prompts
	}
	h.calib.LoadAttempts = h.loadVerificationAttempts
	// Spoken-form keys of a named_entities list (for binding a slotted target to
	// a real entry so the word is retunable).
	h.calib.CollectionKeys = func(collection string) []string {
		resp, err := h.plugin.CollectionGet(branchkit.CollectionGetRequest{Name: collection})
		if err != nil {
			return nil
		}
		keys := make([]string, 0, len(resp.Entries))
		for k := range resp.Entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	// In-walk retune: rename a vocabulary entry (overrides.apply rename) and
	// re-arm the grammar. Generic over the collection (alphabet codewords, the
	// number/direction/apps lists, …); the dynamic/hint path additionally
	// refreshes its fixture in place.
	h.calib.RenameVocab = h.renameVocabEntry
	h.calib.RevertVocab = h.revertVocabEntry
	h.calib.EngineName = func() string { return "sherpa-offline-nemo" }
	h.calib.CommandModel = h.commandModelName
	// The boot warm-up and the restore of continuous listening start before
	// the settings arrive (the mirror's first fetch lands at on_ready), so they
	// use the default command model. When the settings land, or change from
	// anywhere, the stage moves to the chosen one.
	h.config.OnChange(func(settings.VoiceConfig) { go h.syncCommandModel() })
	// Trial-audio capture (opt-in): the game points the command stage's
	// dump-segments at a per-game dir while running, then re-decodes the captured
	// audio for the fragility ladder.
	h.calib.CaptureEnabled = func() bool {
		return h.config.Get().CalibrationCapture
	}
	h.calib.SetCaptureDir = h.setCommandCaptureDir
	h.calib.RetentionPolicy = h.config.CaptureRetentionResolved
	h.calib.AutoAdvanceEnabled = func() bool { return h.config.Get().CalibrationAutoAdvance }
	// Command-level chaining: on command-exhaust, walk to the next sidebar row.
	h.calib.NextCommandID = h.nextCatalogCommandID
	// Live per-command health: a walk persists attempts → rebuild the cached
	// sidebar catalog so its accuracy glances update as the sitting progresses.
	h.calib.OnAttemptsChanged = h.invalidateCatalogCache
	h.calib.OnClipsChanged = h.invalidateRecordingsCache
	h.calib.StartRefreshCoalescer()

	// Wire session dependencies
	h.sess.Platform = h.plugin
	h.sess.PluginNotify = func(method string, params any) {
		h.plugin.Notify(method, params)
	}
	h.sess.InitRPC()

	// Mirror layout characters from the keyboard plugin (for speech
	// vocabulary + display annotation)

	// Voice commands are auto-registered by the platform from collection_data
	// (kind: "commands" in the voice_commands collection schema).
	// No manual pushCommands call needed.

	// Push voice-owned flat-map lists (number, direction)
	h.pushDefaultLists()

	// Push alphabet as array-of-objects (field names match entry_schema in plugin.json)
	// then build phonetics from the pushed data.
	h.initAlphabet()

	// Seed the floatable-collections catalog from the entity collections we
	// just pushed. Other plugins' collections (e.g. `apps`) arrive async and
	// converge via the collection.updated hook below.
	h.buildFloatableCatalog()

	// No keybind or tag boot reconciles here anymore: the speaking and
	// disambiguating tags are writer-held (platform-reaped on death), and the
	// key-capture pause is the suppress_keybinds EFFECT now — released by
	// cleanup_terminated_plugin like every other effect.

	// The mode tag is the one piece of session state the platform does NOT
	// reap (held_by_writer: false — continuous mode must survive a
	// hot-reload for restoreContinuousMode), so hold-mode residue is still
	// ours to reset. `_platform.input` needs nothing here anymore: it is
	// per-writer and writer-held, so a crashed hold's lift_keys assertion
	// dropped with the process. Must precede the restoreContinuousMode
	// goroutine below, which it is deliberately careful not to clobber.
	// The return value is the continuous mode a reset overlay was covering, if
	// any — the one piece of durable intent the reset itself destroys. It is
	// handed to restoreContinuousMode below.
	displacedContinuous := h.reconcileOrphanedSessionState()

	// Pre-warm every engine at boot (mic closed) so the first Option+G / Option+T
	// isn't eaten by a cold model load, and show a per-engine readiness panel in
	// the Discovery HUD. Readiness lands via _platform.pipeline.warmed below.
	// (The command grammar record is still seeded authoritatively by plugin.json.)
	// Put the chosen microphone back on the pipeline records before anything
	// runs them: the platform re-seeds them from plugin.json on every start.
	h.applyInputDevice(false)

	h.startVoiceWarmup()
	// Recordings a Say a phrase session left unanswered before a restart.
	go h.sweepSayNow()
	go h.sweepDictationRecordings()

	// Report where voice's Setup steps stand. The platform drops reports
	// when this process restarts, so they are sent again on every start.
	go h.refreshSetup()

	// Subscribe to pipeline transcript events
	h.plugin.OnPipelineTranscript(h.handlePipelineTranscript)

	// Hold-end boundary for an active calibration walk. The command engine
	// (sherpa, continuous VAD) emits the recognized words as non-final segments
	// and signals end-of-hold only by stopping the ephemeral command_recognition
	// pipeline on Option+G up — it never sends an is_final transcript frame. So
	// the key-up pipeline stop IS the hold boundary: forward it to calibration
	// (silent-hold tally + auto-advance on a green trial). No-op when not walking.
	// An engine finished warming (boot prewarm or a later warm) — advance its
	// readiness bar; the dictation case also clears the "Loading model…" status.
	// Recently-focused apps, for the per-app profile picker in Settings only.
	// NOT the source of a transcript's profile — see app_profiles_recent.go.
	h.subscribeFocusedApps()

	h.plugin.OnPermissionChanged(func(payload branchkit.PermissionChangedEventParams) {
		h.handlePermissionChanged(payload.Permission, payload.Granted)
	})

	// What the platform did with the actions voice answered a command
	// transcript with. Addressed to voice alone; joined to the History row
	// on the chain id, so History shows when a matched command
	// was refused or failed.
	h.plugin.OnPipelineExecuted(h.handlePipelineExecuted)

	h.plugin.OnPipelineWarming(func(payload branchkit.PipelineWarmingEventParams) {
		h.handlePipelineWarming(payload.Pipeline)
	})

	h.plugin.OnPipelineWarmed(func(payload branchkit.PipelineWarmedEventParams) {
		h.handlePipelineWarmed(payload.Pipeline)
	})

	// A warm that failed: the readiness panel says which engine and why
	// rather than showing it loading for good.
	h.plugin.OnPipelineError(func(payload branchkit.PipelineErrorEventParams) {
		h.handlePipelineError(payload.Pipeline, payload.Error)
	})

	// Which command_recognition run is which: a hold's ephemeral run or
	// continuous listening's persistent one (cmdrec_recovery.go).
	h.plugin.OnPipelineStarted(func(payload branchkit.PipelineStartedEventParams) {
		if payload.Pipeline == "command_recognition" {
			h.sess.NoteCmdRecStarted(payload.Ephemeral != nil && *payload.Ephemeral)
		}
	})

	h.plugin.OnPipelineStopped(func(payload branchkit.PipelineStoppedEventParams) {
		switch payload.Pipeline {
		case "command_recognition":
			errText := ""
			if payload.Error != nil {
				errText = *payload.Error
			}
			h.handleCommandRecognitionStopped(string(payload.Reason), errText)
		case dictationSamplePipeline:
			// Read a sentence's run stopped. Its final transcript, if any,
			// arrived first (ordered delivery) and claimed the mark; a run
			// that ended without one (cancelled, failed) drops it here.
			h.sess.FinishDictationIfDraining()
			h.closeDictationWarming()
			h.readSentence.clearRecording()
		case "dictation":
			// The dictation pipeline has fully stopped. Ordered delivery
			// guarantees its final transcript (if any) arrived first, so a
			// still-draining session got nothing — finish it now instead of
			// waiting out the drain backstop. No-op while Recording (a new
			// session may already be live) or Inactive (transcript path
			// already finished).
			h.sess.FinishDictationIfDraining()
			// A run that failed while its stage started never says warmed.
			h.closeDictationWarming()

		}
	})

	// Subscribe to store update events via JSON-RPC notifications
	h.plugin.OnCollectionUpdated(func(payload branchkit.CollectionUpdatedEventParams) {
		// Keep the floatable-collections catalog in sync as entity collections
		// come and go. Namespaced/high-churn collections (dots) and the catalog
		// itself are excluded inside buildFloatableCatalog, which also no-ops
		// when the eligible set is unchanged — so this stays cheap.
		if !strings.Contains(payload.Collection, ".") { // the catalog itself is namespaced, so this skips it too
			h.buildFloatableCatalog()
		}
		// A download started or finished: the Speech models step moves.
		if payload.Collection == "_platform.model_download" {
			go h.refreshSetup()
			// A chosen command model that just finished downloading takes over.
			go h.syncCommandModel()
		}
	})

	// Register handlers
	branchkit.HandleTyped(h.plugin, "health", func(_ *struct{}) (any, error) {
		return map[string]bool{"ready": true}, nil
	})
	// Per-action handlers from the single source of truth in actions.go.
	for _, r := range h.actionRegistrations() {
		h.plugin.HandleAction(r.Action, r.Handler)
	}
	h.registerSettingsTabs(h.plugin)

	branchkit.HandleTyped(h.plugin, "calibrate", func(req *calibration.CalibrateRequest) (any, error) {
		// Grammar lock/unlock is owned by the calibration State (see
		// LockGrammar/UnlockGrammar wired below) — the handler no
		// longer post-processes a grammar_reset flag.
		return h.calib.HandleCalibrateRPC(req)
	})
	branchkit.HandleTyped(h.plugin, "on_commands_changed", h.handleOnCommandsChanged)
	// The platform asks what a command_recognition transcript means, and
	// executes the actions we answer with. Voice keeps every decision — the
	// transcript walk, bridging, tie-breaks, scope drops — and only the
	// delivery of the answer changed: a return value instead of a fresh
	// authenticated `dispatch` call. See ApplyOrchestrateResults.
	branchkit.HandleTyped(h.plugin, branchkit.HookOnTranscript, h.handleOnTranscript)
	branchkit.HandleCommand(h.plugin, "set_main_mode", h.handleSetMainMode)
	branchkit.HandleCommand(h.plugin, "mark_wrong", h.handleMarkWrong)
	branchkit.HandleCommand(h.plugin, "open_float", h.handleOpenFloatMethod)
	branchkit.HandleCommand(h.plugin, "close_float", h.handleCloseFloat)
	branchkit.HandleCommand(h.plugin, "set_font_size", h.handleSetFontSize)
	branchkit.HandleCommand(h.plugin, "toggle_help_hud", h.handleToggleHelpHud)
	branchkit.HandleCommand(h.plugin, "set_show_badges", h.handleSetShowBadges)
	branchkit.HandleCommand(h.plugin, "set_compact_mode", h.handleSetCompactMode)
	branchkit.HandleCommand(h.plugin, "set_hud_opacity", h.handleSetHudOpacity)
	branchkit.HandleCommand(h.plugin, "set_hud_blur", h.handleSetHudBlur)
	branchkit.HandleCommand(h.plugin, "set_hud_corner_radius", h.handleSetHudCornerRadius)
	branchkit.HandleCommand(h.plugin, "set_hud_channel", h.handleSetHudChannel)
	branchkit.HandleCommand(h.plugin, "set_calibration_capture", h.handleSetCalibrationCapture)
	branchkit.HandleCommand(h.plugin, "set_auto_advance", h.handleSetAutoAdvance)
	branchkit.HandleCommand(h.plugin, "set_capture_retention", h.handleSetCaptureRetention)
	branchkit.HandleCommand(h.plugin, "clear_captures", h.handleClearCaptures)
	branchkit.HandleCommand(h.plugin, "set_clip_disposition", h.handleSetClipDisposition)
	branchkit.HandleCommand(h.plugin, "reprobe_fragility", h.handleReprobeFragility)
	branchkit.HandleCommand(h.plugin, "score_bias", h.handleScoreBias)
	branchkit.HandleCommand(h.plugin, "apply_bias", h.handleApplyBias)
	branchkit.HandleCommand(h.plugin, "set_bias", h.handleSetBias)
	branchkit.HandleCommand(h.plugin, "delete_clip", h.handleDeleteClip)
	branchkit.HandleCommand(h.plugin, "delete_target", h.handleDeleteTarget)
	branchkit.HandleCommand(h.plugin, "set_recordings_view", h.handleSetRecordingsView)
	branchkit.HandleCommand(h.plugin, "export_clips", h.handleExportClips)
	branchkit.HandleCommand(h.plugin, "config_update", h.handleConfigUpdate)
	branchkit.HandleCommand(h.plugin, "model_set", h.handleModelSet)
	branchkit.HandleCommand(h.plugin, "command_model_set", h.handleCommandModelSet)
	branchkit.HandleCommand(h.plugin, "model_pick", h.handleModelPick)
	branchkit.HandleCommand(h.plugin, "command_model_pick", h.handleCommandModelPick)
	branchkit.HandleCommand(h.plugin, "command_models_compare", h.handleCommandModelsCompare)
	branchkit.HandleCommand(h.plugin, "say_now_start", h.handleSayNowStart)
	branchkit.HandleCommand(h.plugin, "say_now_stop", h.handleSayNowStop)
	branchkit.HandleCommand(h.plugin, "say_now_confirm", h.handleSayNowConfirm)
	branchkit.HandleCommand(h.plugin, "say_now_dismiss", h.handleSayNowDismiss)
	branchkit.HandleCommand(h.plugin, "dictation_models_compare", h.handleDictationModelsCompare)
	branchkit.HandleCommand(h.plugin, "read_sentence_start", h.handleReadSentenceStart)
	branchkit.HandleCommand(h.plugin, "read_sentence_stop", h.handleReadSentenceStop)
	branchkit.HandleCommand(h.plugin, "read_sentence_keep", h.handleReadSentenceKeep)
	branchkit.HandleCommand(h.plugin, "read_sentence_discard", h.handleReadSentenceDiscard)
	branchkit.HandleCommand(h.plugin, "model_download", h.handleModelDownload)
	branchkit.HandleCommand(h.plugin, "model_download_recommended", h.handleModelDownloadRecommended)
	branchkit.HandleCommand(h.plugin, "model_delete", h.handleModelDelete)
	branchkit.HandleCommand(h.plugin, "setup_check", h.handleSetupCheck)
	branchkit.HandleCommand(h.plugin, "commands_delete", h.handleCommandsDelete)
	branchkit.HandleCommand(h.plugin, "commands_reset", h.handleCommandsReset)
	branchkit.HandleCommand(h.plugin, "settings_command_filter", h.handleSettingsCommandFilter)
	branchkit.HandleCommand(h.plugin, "dictation_correct", h.speech.HandleDictationCorrect)
	branchkit.HandleCommand(h.plugin, "app_profiles_set", h.HandleAppProfileSet)
	branchkit.HandleCommand(h.plugin, "app_styles_set", h.HandleAppStyleSet)
	branchkit.HandleCommand(h.plugin, "app_styles_default", h.HandleAppStyleDefault)
	branchkit.HandleCommand(h.plugin, "corrections_add", h.speech.HandleCorrectionsAdd)
	branchkit.HandleCommand(h.plugin, "corrections_update", h.speech.HandleCorrectionUpdate)
	branchkit.HandleCommand(h.plugin, "corrections_delete", h.speech.HandleCorrectionDelete)
	branchkit.HandleCommand(h.plugin, "corrections_reset", h.speech.HandleCorrectionReset)
	branchkit.HandleCommand(h.plugin, "corrections_test", h.speech.HandleCorrectionTest)
	branchkit.HandleCommand(h.plugin, "vocabulary_add", h.speech.HandleVocabularyAdd)
	branchkit.HandleCommand(h.plugin, "vocabulary_remove", h.speech.HandleVocabularyRemove)
	branchkit.HandleCommand(h.plugin, "microphone_set", h.handleMicrophoneSet)

	// Phonetic rename/reset: typed {letter, word} payloads, like the rest of
	// the settings UI. (Was Datastar-signal-based, which silently failed.)
	branchkit.HandleCommand(h.plugin, "settings_phonetics_override", h.handlePhoneticOverride)
	branchkit.HandleCommand(h.plugin, "settings_phonetics_reset", h.handlePhoneticReset)
	// Codeword-lab candidate decisions: adopt (rewires the codeword via the
	// same path as the edit box) / reject-with-reason / clear. Ledger:
	// plugin.voice.vocab_candidates.
	branchkit.HandleCommand(h.plugin, "candidates_decide", h.handleCandidateDecision)
	// In-walk codeword retune: edit the failing codeword and retest it on the
	// spot under the same grammar. Typed {letter, word} payload (letter derived
	// from the current target when omitted).
	branchkit.HandleCommand(h.plugin, "calibration_retune", h.handleCalibrationRetune)
	// In-walk revert: reset the current target's entry to its plugin default.
	h.plugin.Handle("calibration_revert", func(_ json.RawMessage) (any, error) {
		return h.handleCalibrationRevert()
	})
	branchkit.HandleCommand(h.plugin, "settings_rules_test", h.handleTestPhrase)
	branchkit.HandleCommand(h.plugin, "settings_rules_select", h.handleRuleSelect)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_open", h.handleEditorOpen)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_update", h.handleEditorUpdate)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_close", h.handleEditorClose)
	h.plugin.Handle("settings_rules_editor_save", func(params json.RawMessage) (any, error) {
		return h.handleEditorSave(params)
	})
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_tag_add", h.handleEditorTagAdd)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_tag_remove", h.handleEditorTagRemove)
	branchkit.HandleCommand(h.plugin, "settings_rules_phrase_slot_update", h.handlePhraseSlotUpdate)
	branchkit.HandleCommand(h.plugin, "settings_rules_phrase_slot_add", h.handlePhraseSlotAdd)
	branchkit.HandleCommand(h.plugin, "settings_rules_phrase_slot_remove", h.handlePhraseSlotRemove)
	branchkit.HandleCommand(h.plugin, "settings_rules_slot_new_list", h.handleSlotNewList)
	branchkit.HandleCommand(h.plugin, "settings_rules_slot_new_list_cancel", h.handleSlotNewListCancel)
	branchkit.HandleCommand(h.plugin, "settings_rules_phrase_raw_toggle", h.handlePhraseRawToggle)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_reveal", h.handleEditorReveal)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_start", h.handleEditorStart)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_record_start", h.handleEditorRecordStart)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_record_stop", h.handleEditorRecordStop)
	branchkit.HandleCommand(h.plugin, "settings_rules_editor_record_keydown", h.handleEditorRecordKeydown)

	// Push initial state to the shell after startup
	go func() {
		// Brief delay to let the platform finish creating channels from manifests
		time.Sleep(500 * time.Millisecond)
		h.hud.PushMainHUD()
		// Restore continuous mode if it was active before a hot-reload —
		// including when the reload landed mid-overlay, where the persisted
		// record no longer says continuous and only the reconcile knows.
		h.restoreContinuousMode(displacedContinuous)
		// A live dictation pipeline can outlive a plugin restart
		// (hot-reload mid-session): our session state is gone, so any
		// transcript would be dropped anyway — but the mic stays hot
		// and the session keeps accumulating audio (observed: a 9-minute
		// orphan inflating the next speak session's final transcription
		// past every drain window). Stop it defensively; no-op when
		// nothing is running.
		h.plugin.PipelinesStop(branchkit.PipelinesStopRequest{Name: "dictation"})
		h.plugin.PipelinesStop(branchkit.PipelinesStopRequest{Name: dictationSamplePipeline})
	}()

	h.plugin.Run()
}
