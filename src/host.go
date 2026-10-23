package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	"github.com/branchkit/branchkit-plugin-voice/hud"
	"github.com/branchkit/branchkit-plugin-voice/session"
	"github.com/branchkit/branchkit-plugin-voice/settings"
	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/plugin-sdk-go"
)

// Host is what every handler in the voice plugin needs: the platform
// handle, the three domain states (each with its own mutex), the grammar
// manager, and the smaller caches and rings that used to be package
// variables. Handlers are methods on it, so a handler's dependencies are
// visible in its signature and each mutex sits beside the data it guards.
type Host struct {
	plugin *branchkit.Plugin

	// cmdModelMu guards cmdModelApplied, the command model the stage was last
	// put on, so a settings change or a finished download can tell whether the
	// stage should move (claimCommandModel).
	cmdModelMu      sync.Mutex
	cmdModelApplied string
	// compareMu lets one command-model comparison run at a time.
	compareMu sync.Mutex
	// compareStateMu guards compareState, the comparison as the Models tab
	// shows it (model_compare.go).
	compareStateMu sync.Mutex
	compareState   compareRun
	// sayNow is the Models tab's Say a phrase (saynow.go).
	sayNow sayNow
	// readSentence is the Models tab's Read a sentence (readsentence.go).
	readSentence readSentence
	// dictPipelineMu guards dictPipeline, the pipeline the current
	// dictation hold started ("dictation" or Read a sentence's
	// "dictation_sample").
	dictPipelineMu sync.Mutex
	dictPipeline   string
	// dictCompareMu guards dictCompare, the dictation comparison as the
	// Models tab shows it (dictation_compare.go).
	dictCompareMu sync.Mutex
	dictCompare   dictationRun
	// stageCaptureMu guards captureOwner, who holds the command stage's
	// capture folder (claimCapture), and serializes changing it.
	stageCaptureMu sync.Mutex
	captureOwner   string

	// sess holds session-domain state (dictation, dedup, phonetics).
	sess *session.State
	// calib holds calibration-domain state (game, tab visibility).
	calib *calibration.State
	// calibPrivilegeAsked: the optional `calibration` privilege has been
	// requested this run (beginTrialOrAsk), so a refused trial asks once.
	calibPrivilegeAsked atomic.Bool
	// edState holds editor-domain state (form, schema caches).
	edState *editorState

	// Platform calls a test swaps for a stub. Each starts as its Default
	// method (the real call); a test on a detached plugin installs what the
	// platform would have answered.
	statusIndicatorAvailable func() (bool, error)
	persistAppProfiles       func(overrides map[string]string) error
	// putAppStyles stores the styles record (default and exceptions); a seam
	// so a test can watch what is written.
	putAppStyles            func(record map[string]any) error
	recognitionLogRecording func() (bool, error)
	// pipelinesRun / pipelinesStop start and stop a pipeline (the hold and
	// dictation handlers); seams so a test can watch which pipeline a key
	// edge starts or stops.
	pipelinesRun  func(req branchkit.PipelinesRunRequest) (*branchkit.PipelinesRunResponse, error)
	pipelinesStop func(req branchkit.PipelinesStopRequest) (*branchkit.PipelinesStopResponse, error)
	// pipelinesStatus lists the running pipelines (a seam a test answers).
	pipelinesStatus func() (*branchkit.PipelinesStatusResponse, error)
	// systemNotify posts a notice; cmdRecRestartAfter schedules a restart
	// of command recognition (time.AfterFunc). Seams a test replaces.
	systemNotify       func(req branchkit.SystemNotifyRequest) error
	cmdRecRestartAfter func(d time.Duration, f func())
	// calibHoldEnd tells calibration a hold ended (a seam a test watches).
	calibHoldEnd func()
	// The restart budget for command recognition while listening
	// (cmdrec_recovery.go).
	cmdRecRecovery cmdRecRecovery

	// config is the typed voice-config mirror (settings.New).
	config *settings.Config
	// speech is the dictation-correction set (rules, persistence, bias push).
	speech *speech.Corrections
	// hud is the main HUD channel and the discovery overlay.
	hud *hud.HUD

	lastContinuousToggle time.Time
	dictationReady       atomic.Bool

	// speakCarry is the speak session's previous command transcript's last
	// word, so a stop phrase split across two transcripts still pairs
	// (speak_stop.go).
	speakCarry speakStopCarry

	// Voice's Setup steps (setup.go): what was last reported, and the
	// "Check it works" health check (setup_health.go).
	setup  *setupReporter
	health setupHealth

	// Per-app dictation profiles and the app facts they derive from.
	recentAppsMu        sync.Mutex
	recentApps          []string
	appNamesMu          sync.RWMutex
	appNamesByID        map[string]string
	appProfilesMu       sync.RWMutex
	appProfileOverrides map[string]string
	// appProfilesLoadErr is set when reading the saved overrides FAILED
	// (not when absent or unparseable). Every edit saves the whole map, so
	// saving on top of an empty map would erase the user's other rows;
	// setAppProfileOverride reads again first. Guarded by appProfilesMu.
	appProfilesLoadErr error
	appTraitsMu        sync.RWMutex
	appTraitsByID      map[string][]string
	appTraitsLoaded    bool

	// Mode writes (setVoiceMode). modeWriteMu orders them, so a retry of an
	// older mode can never land after a newer mode's writes; writeMode is the
	// write itself and modeRetryAfter schedules a retry (time.AfterFunc),
	// seams tests replace.
	modeWriteMu     sync.Mutex
	writeMode       func(mode string) error
	modeRetryDelays []time.Duration
	modeRetryAfter  func(d time.Duration, f func())

	// Floating-panel bookkeeping.
	floatableMu    sync.Mutex
	floatableNames string // sorted, comma-joined snapshot of the last CONFIRMED push
	floatMu        sync.Mutex
	floatPanels    map[string]bool

	// Rings shown on the Dictation / History surfaces (oldest-first).
	// Per-app dictation styles (styles.go).
	styles appStyles
	// When the refine stage last answered (refine.go warmRefine).
	refineMu         sync.Mutex
	refineLastUsed   time.Time
	dictationMu      sync.Mutex
	dictationEntries []dictationEntry
	historyMu        sync.Mutex
	historyEntries   []historyEntry
	// pendingExecuted holds platform reports that arrived before their
	// History row: a turn dropped while voice was still deciding it.
	pendingExecuted []pendingReport

	// The recognition_log recording flag as last read, and when. See
	// recognitionLogOn in history.go.
	recLogMu        sync.Mutex
	recLogOn        bool
	recLogCheckedAt time.Time

	// Readiness panel: which stages are warm, and which typing paths are
	// known granted.
	warmMu    sync.Mutex
	warmReady map[string]bool
	// Why a tracked engine's warm failed, until a later warm succeeds.
	warmFailed  map[string]string
	warmGen     int // discovery-overlay generation owning the readiness panel
	typingMu    sync.Mutex
	typingKnown map[string]bool // "post_event" / "secure_input" → granted (typing lands)

	// The overlay saying dictation is still starting, while one is up
	// (0 otherwise). Its own lock: opening an overlay renders the HUD, and
	// the render reads the readiness panel under warmMu.
	dictationWarmMu  sync.Mutex
	dictationWarmGen int

	// Settings-tab view state that is server-held on purpose (shared across
	// windows): the command filter and the selected rule.
	commandFilterMu  sync.Mutex
	commandFilter    string
	selectedRuleMu   sync.Mutex
	selectedRule     string
	lastExportStatus string

	// Render caches for the calibration sidebar and recordings list.
	catalogMu             sync.Mutex
	catalogCached         *catalogSnapshot
	recordingsMu          sync.Mutex
	recordingsCached      *recordingsSnapshot
	recordingsViewMode    string
	lastBiasApplyStatus   string
	biasApplyNeedsConfirm bool

	// The hotkey pause the command editor's key capture holds, as seams so a
	// test can watch it, and the timer that bounds it (captureTimeout).
	holdPause    func()
	releasePause func()
	captureMu    sync.Mutex
	captureTimer *time.Timer
}

func newHost(p *branchkit.Plugin) *Host {
	h := &Host{
		plugin:              p,
		calib:               calibration.NewState(),
		edState:             &editorState{},
		config:              settings.New(p),
		appProfileOverrides: map[string]string{},
		floatPanels:         map[string]bool{},
		warmReady:           map[string]bool{},
		warmFailed:          map[string]string{},
		typingKnown:         map[string]bool{},
		recordingsViewMode:  "grouped",
	}
	h.setup = &setupReporter{send: h.sendSetupReport}
	h.speech = speech.New(p, func() bool { return h.config.Get().FixAllCaps })
	h.writeMode = h.writeModeState
	h.modeRetryDelays = defaultModeRetryDelays
	h.modeRetryAfter = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	h.hud = hud.New(p, h.renderHUD)
	// Clear the "transcribing" icon once the transcript lands (or the drain
	// backstop gives up), re-deriving from the CURRENT mode: a dictation taken
	// during continuous mode returns to the continuous icon, not to idle.
	h.sess = session.New(h.hud, h.speech, func() { h.applyModeIcon(h.sess.GetCurrentMode()) })
	// A dictation that ends without a transcript used to vanish: the pill
	// went away and nothing was typed, with nothing to say why.
	h.sess.SetOnDictationLost(func() {
		branchkit.Logf("voice", "dictation: ended with no transcript")
		go func() {
			if err := h.plugin.SystemNotify(branchkit.SystemNotifyRequest{
				Title: "Dictation didn't come back",
				Body:  "It didn't finish transcribing, so nothing was typed.",
			}); err != nil {
				branchkit.Logf("voice", "dictation: lost-notice failed: %v", err)
			}
		}()
	})
	h.statusIndicatorAvailable = h.statusIndicatorAvailableDefault
	h.persistAppProfiles = h.persistAppProfilesDefault
	h.putAppStyles = func(record map[string]any) error {
		return h.plugin.Put(appStylesCollection, "singleton", record)
	}
	h.recognitionLogRecording = h.recognitionLogRecordingDefault
	h.pipelinesRun = p.PipelinesRun
	h.pipelinesStop = p.PipelinesStop
	h.pipelinesStatus = p.PipelinesStatus
	h.systemNotify = p.SystemNotify
	h.cmdRecRestartAfter = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	h.calibHoldEnd = func() { h.calib.HandleHoldEnd() }
	h.holdPause = h.holdPauseDefault
	h.releasePause = h.releasePauseDefault
	return h
}

func (h *Host) statusIndicatorAvailableDefault() (bool, error) {
	ind, err := h.plugin.NativeStatusIndicator()
	if err != nil {
		return false, err
	}
	return ind.Available, nil
}

func (h *Host) persistAppProfilesDefault(overrides map[string]string) error {
	return h.plugin.Put(appProfilesCollection, "singleton", map[string]any{
		"overrides": overrides,
	})
}

// renderHUD answers the HUD's pull for one mode's content — the voice
// plugin's own handler, handed to hud.New.
func (h *Host) renderHUD(req *hud.RenderHUDRequest) (hud.HudResponse, error) {
	respAny, err := h.handleRenderHud(req)
	if err != nil {
		return hud.HudResponse{}, err
	}
	resp, ok := respAny.(hud.HudResponse)
	if !ok {
		return hud.HudResponse{}, nil
	}
	return resp, nil
}
