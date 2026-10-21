package session

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/hud"
	"github.com/branchkit/branchkit-plugin-voice/speech"
)

// LastMatch caches the most recent non-repeat action for the "again" / "N times"
// repeat primitive. Stored as raw JSON so replay is a direct re-dispatch.
type LastMatch struct {
	Action *branchkit.Action
}

// PipelineState tracks the lifecycle of a single pipeline.
type PipelineState int

const (
	PipelineInactive   PipelineState = iota
	PipelineActive                   // command_recognition only
	PipelineSuppressed               // command_recognition only
	PipelineRecording                // dictation only
	PipelineDraining                 // dictation only
)

func (ps PipelineState) String() string {
	switch ps {
	case PipelineInactive:
		return "inactive"
	case PipelineActive:
		return "active"
	case PipelineSuppressed:
		return "suppressed"
	case PipelineRecording:
		return "recording"
	case PipelineDraining:
		return "draining"
	default:
		return "unknown"
	}
}

// State holds the speech session state machine: mode flags,
// and phonetics. Protected by its own mutex — independent of calibration and
// editor state. Mid-codeword continuation state (the captured prefix of a
// two-word codeword) lives server-side in the platform's PendingPartial, not
// here.
type State struct {
	// hud is the overlay this session drives; speech the correction set
	// post-processing runs through.
	hud    *hud.HUD
	speech *speech.Corrections
	// onDictationFinished runs when a transcript lands or the drain backstop
	// gives up — the caller's hook (voice clears its transcribing icon); this
	// package must not know about the shell.
	onDictationFinished func()
	// onDictationLost runs when a dictation leaves DRAINING with no
	// transcript: the pipeline stopped without one, or the backstop gave up.
	// The recognizer always answers a stop with a transcript, empty for
	// silence, so this means the speech is gone (the pipeline's deadline cut
	// it off, or the stage failed). Set with SetOnDictationLost.
	onDictationLost func()

	mu                 sync.Mutex
	dictationStartTime *time.Time
	// When the toggle latched, so a press that arrives within
	// `bounceMS` of it can be recognized as switch chatter rather than a
	// deliberate second press. See DictationStart.
	dictationLatchTime    *time.Time
	dictationToggleActive bool
	dictationActive       bool
	// A short tap was released and the recording is waiting, briefly, to see
	// whether a second tap latches it. tapReleasedAt dates the
	// release for the bounce guard; tapGen invalidates a stale resolve timer.
	dictationTapPending    bool
	tapReleasedAt          *time.Time
	tapGen                 uint64
	speakDictating         bool
	speakTranscriptPending bool
	speakTranscriptDiscard bool
	currentMode            string
	priorMode              string
	// overlay is the dictation-class overlay (EnterOverlay) that is live, or
	// "". A command hold can cover it while Option+G is down; the hold's end
	// returns to it rather than to the base, which would close a dictation
	// that is still recording.
	overlay                    string
	calibrationConsumedSession bool
	phonetics                  []speech.PhoneticEntry
	lastMatch                  *LastMatch
	// lastOutcome is the words of the last command this hold executed while
	// discovery STAYED open (a sticky mode): it led the choices' phrase, and
	// when the hold ends it becomes the channel's final state so a renderer
	// that speaks at the boundary says "snap right", not nothing. Reset at
	// hold start; taken once at hold end.
	lastOutcome string
	// lastProblem is the sentence for a command that failed during this
	// hold. The release clears the discovery channel, which
	// would take an announced failure with it before a renderer that speaks
	// at the boundary said it; so the hold's end makes it the channel's final
	// state instead. Reset at hold start; taken once at hold end.
	lastProblem    string
	cmdRecState    PipelineState
	dictationState PipelineState
	drainTimer     *time.Timer

	// Command-hold deferred teardown: after Option+G up, the session boundary +
	// return-to-idle are deferred until the engine's end-of-hold flush has
	// drained through the matcher, so a two-word codeword whose suffix only
	// decodes at flush can still complete. cmdHoldGen scopes a pending drain to
	// its hold, so a fast re-press cannot let a stale cleanup clobber the new
	// hold. See the jar/day bridge bug.
	cmdHoldGen        int
	cmdHoldDraining   bool
	cmdHoldDrainTimer *time.Timer
	// cmdHoldActive is Option+G being down, tracked apart from currentMode:
	// the mode says what the user SEES (a dictation started under the hold
	// covers it), while the key's release must end the hold whatever covers
	// it. Reading the mode instead is what let G up be ignored under a
	// dictation and strand the hold's pipeline.
	cmdHoldActive bool
	// cmdHoldOwnsPipeline: the hold's release stops command_recognition.
	// False only when the hold's run could not be started (DisownCommandHold).
	// A hold that shares a speak session's run still owns it: if the speak
	// session ends while the key is down, nothing else would stop it.
	cmdHoldOwnsPipeline bool
	// cmdHoldDrainAwait is how many command_recognition runs must say
	// `stopped` before a released hold's cleanup may run: the runs up at
	// the release, the newest of which is the hold's own. A run started
	// after the release (a "speak" decoded at the flush) is not waited for.
	// 0 when voice saw no run start (it then ends on the next `stopped`).
	cmdHoldDrainAwait int
	// cmdRecAside: continuous listening's persistent command_recognition run
	// is set aside for a command hold, which listens on its own ephemeral run
	// instead (the platform treats only an ephemeral run as a held key: the
	// echo filter and spoken answers key on it). Set at the
	// press, cleared when the hold's cleanup restarts the persistent run.
	// Apart from cmdRecState: a dictation can set the run aside too, and the
	// persistent run comes back only when neither holds it.
	cmdRecAside bool
	// cmdRecRuns is the command_recognition runs the platform has said
	// `started` and not yet `stopped`, oldest first, each true when
	// ephemeral. Runs of one pipeline end in the order they started (a new
	// run waits for the previous one to finish), so the oldest entry is the
	// run a `stopped` is about. Lets voice tell a hold's run ending from the
	// persistent run it set aside ending.
	cmdRecRuns []bool

	// Tie disambiguation state (set while the user is choosing among tied
	// commands). tieWords are the words that produced the tie, replayed with
	// prefer_owner on pick; tieChoices maps each spoken choice token to its
	// owner + display fields for the chooser HUD. nil when not disambiguating.
	tieWords   []string
	tieChoices []TieChoice
	// tieGen counts ties begun, so the continuous-mode countdown armed for
	// one tie never clears a later one.
	tieGen uint64

	// External dependencies — set once at startup via main.go, before any handler calls.
	// Platform is the typed SDK surface (generated wrappers); PluginNotify stays
	// a raw notify because events.append's generated wrapper is a request, and
	// these diagnostics are fire-and-forget. nil leaves either unwired.
	Platform     Platform
	PluginNotify func(method string, params any)

	// Swappable RPC function — set to its default by InitRPC(), replaced in
	// tests. SetVoiceMode, SetShellIcon and Calib used to sit beside it: all
	// three were assigned by main.go at startup and never read here, because
	// the mode and icon setters are called directly from the actions package
	// that owns them. listGetFn was the same, one level down — its default was
	// invoked only by the tests that checked it had been wired.
	callResolveFn func(words []string, requireTag *string, activeTags []string) *resolveResponse
	// callResolvePhraseFn resolves a whole utterance into the commands it
	// names (commands.resolve_phrase, committing each step's tag writes).
	callResolvePhraseFn func(words []string) *phraseResponse
}

// InitRPC sets the swappable RPC functions to their default implementations
// that route through Platform. Must be called after Platform is set.
func (s *State) InitRPC() {
	s.callResolveFn = s.callResolveDefault
	s.callResolvePhraseFn = s.callResolvePhraseDefault
}

// --- Dictation ---

func (s *State) GetDictationStartTime() *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dictationStartTime
}

func (s *State) SetDictationStartTime(t *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dictationStartTime = t
}

func (s *State) GetDictationToggleActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dictationToggleActive
}

func (s *State) SetDictationToggleActive(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dictationToggleActive = v
}

func (s *State) GetDictationActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dictationActive
}

func (s *State) SetDictationActive(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dictationActive = v
}

// --- Speak ---

func (s *State) GetSpeakDictating() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.speakDictating
}

func (s *State) SetSpeakDictating(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speakDictating = v
	if v {
		s.speakTranscriptPending = true
	}
}

func (s *State) GetSpeakTranscriptPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.speakTranscriptPending
}

func (s *State) ClearSpeakTranscriptPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speakTranscriptPending = false
}

func (s *State) DiscardSpeakTranscript() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speakTranscriptPending = false
	s.speakTranscriptDiscard = true
}

func (s *State) ConsumeTranscriptDiscard() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.speakTranscriptDiscard {
		s.speakTranscriptDiscard = false
		return true
	}
	return false
}

// --- Mode ---

func (s *State) GetCurrentMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentMode == "" {
		return "idle"
	}
	return s.currentMode
}

func (s *State) SetCurrentMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentMode = mode
}

// ContinuousMode is the always-listening mode underneath any overlay
// (continuous_awake or continuous_sleeping), or "" when always-listening is
// off.
func (s *State) ContinuousMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	mode := s.currentMode
	if s.priorMode != "" {
		mode = s.priorMode
	}
	if mode == "continuous_sleeping" || mode == "continuous_awake" {
		return mode
	}
	return ""
}

func (s *State) IsContinuousActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.continuousBaseLocked()
}

// EnterOverlay transitions to overlayMode, saving the mode underneath.
// Returns the saved mode.
//
// Overlays stack (a command hold and a dictation can both be live), but
// priorMode keeps only the BASE mode, the one no overlay covers: an overlay
// entered over another keeps the base that one saved. Saving the covering
// overlay instead is how releasing Option+T used to restore a command hold
// whose key was already up.
func (s *State) EnterOverlay(overlayMode string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveBaseLocked()
	s.currentMode = overlayMode
	s.overlay = overlayMode
	return s.priorMode
}

// LeaveOverlay leaves the dictation overlay. Returns the mode it lands in:
// the command hold when Option+G is still down under it, else the base mode.
func (s *State) LeaveOverlay() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmdHoldActive {
		s.overlay = ""
		s.currentMode = "command_hold"
		return s.currentMode
	}
	return s.restoreBaseLocked()
}

// saveBaseLocked records the current mode as the base an overlay covers,
// unless an overlay already saved one. Callers hold s.mu.
func (s *State) saveBaseLocked() {
	if s.priorMode != "" {
		return
	}
	s.priorMode = s.currentMode
	if s.priorMode == "" {
		s.priorMode = "idle"
	}
}

// restoreBaseLocked closes every overlay, returning to the base mode.
// Callers hold s.mu.
func (s *State) restoreBaseLocked() string {
	restored := s.priorMode
	if restored == "" {
		restored = "idle"
	}
	s.currentMode = restored
	s.priorMode = ""
	s.overlay = ""
	return restored
}

// DropContinuousBase turns continuous listening off underneath an open
// overlay: the base it will restore becomes idle. It reports whether it did,
// false when no overlay covers continuous listening (the caller then sets the
// mode itself). The overlay keeps the screen until its own release.
func (s *State) DropContinuousBase() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priorMode != "continuous_sleeping" && s.priorMode != "continuous_awake" {
		return false
	}
	s.priorMode = "idle"
	return true
}

// IsInOverlay returns true if a temporary overlay (e.g. dictation during continuous) is active.
func (s *State) IsInOverlay() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.priorMode != ""
}

// PriorMode returns the mode an active overlay is covering, or "" when no
// overlay is open. In-memory only and dies with the process — which is exactly
// why the persisted mode record carries a copy (see setVoiceMode).
func (s *State) PriorMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.priorMode
}

// --- Postprocess ---

// PostprocessDictation shapes a finalized dictation transcript for output.
// `isSpeak` marks a speak-session transcript, which gets the wake/stop phrase
// strip (StripSpeakPhrases) as a text-domain backstop to the audio cutoff.
// This is an explicit caller decision, NOT s.GetSpeakDictating(): a
// phrase-detected stop clears the speaking flag before whisper's final
// transcript lands, so a flag check here would skip the strip on exactly the
// speak path that needs it.
func (s *State) PostprocessDictation(text string, isSpeak bool) string {
	return s.speech.Repair(text, isSpeak)
}

// New is a session bound to the HUD it drives and the correction set its
// post-processing uses.
func New(h *hud.HUD, corr *speech.Corrections, onDictationFinished func()) *State {
	return &State{hud: h, speech: corr, onDictationFinished: onDictationFinished}
}

// --- Calibration consumed ---

func (s *State) GetCalibrationConsumed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calibrationConsumedSession
}

func (s *State) SetCalibrationConsumed(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calibrationConsumedSession = v
}

// --- Phonetics ---

func (s *State) GetPhonetics() []speech.PhoneticEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phonetics
}

func (s *State) SetPhonetics(p []speech.PhoneticEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phonetics = p
}

// StartDecision is what an Option+T press (phase "start") means given the
// current state. Typed rather than a control string so the handler cannot
// confuse "begin recording" with "this press ends a latched toggle" — that
// confusion is what broke quick-tap toggle: "" meant both "latched, keep
// recording" and "state mismatch", so the handler force-stopped every latch
// (2026-08-07).
type StartDecision int

const (
	// StartBegin: no session in flight — begin a new recording.
	StartBegin StartDecision = iota
	// StartToggleStop: a quick-tap latched the previous recording into
	// toggle mode; this press finishes it (drain + transcribe).
	StartToggleStop
	// StartStopSpeak: a speak-dictation session is live; this press finishes
	// it (Option+T escape, no-discard semantics).
	StartStopSpeak
	// StartBounceIgnored: a press arrived within `bounceMS` of the latch or
	// of a tap's release — switch chatter on a key the user is still
	// holding, not a second press. The recording continues; the caller does
	// nothing.
	StartBounceIgnored
	// StartLatch: the second tap of a double-tap. The recording that the
	// first tap started keeps going, latched: releasing does not stop it,
	// the next press does. The caller does nothing else.
	StartLatch
)

// StopDecision is what an Option+T release (phase "stop") means.
type StopDecision int

const (
	// StopIgnore: no recording in flight — nothing to stop. The caller
	// treats this as a genuine state mismatch iff the mode still says
	// "dictation" (e.g. hot-reload mid-session).
	StopIgnore StopDecision = iota
	// StopFinish: hold release — finish the recording and transcribe.
	StopFinish
	// StopTapPending: the press was a short tap. The recording continues
	// for the double-tap window; the caller schedules ResolveTap with the
	// returned generation, which finishes it unless a second tap latched it.
	StopTapPending
	// StopLatchHeld: the release of the tap that latched. The recording
	// continues; the caller does nothing.
	StopLatchHeld
)

// DictationStart handles the state transition for an Option+T press.
// `bounceMS` is the switch-debounce window: a press arriving that soon after
// a release is contact chatter on a key the user is still holding, not a
// second press. Passed in rather than a constant here because the timing
// policy belongs with the plugin's config, and tests need to drive it
// directly. Pass 0 to disable.
func (s *State) DictationStart(bounceMS int64) StartDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.speakDictating:
		s.speakDictating = false
		return StartStopSpeak
	case s.dictationTapPending:
		s.dictationTapPending = false
		s.tapGen++ // the pending resolve is moot either way
		// Chatter: the "release" was the switch bouncing on a key still held
		// down. Carry on as one hold; its real release decides.
		if bounceMS > 0 && s.tapReleasedAt != nil &&
			time.Since(*s.tapReleasedAt).Milliseconds() < bounceMS {
			s.tapReleasedAt = nil
			return StartBounceIgnored
		}
		// The second tap: latch the recording the first tap started.
		s.tapReleasedAt = nil
		s.dictationToggleActive = true
		now := time.Now()
		s.dictationLatchTime = &now
		return StartLatch
	case s.dictationToggleActive:
		// Switch chatter, not a press. A bouncing key sends up→down within
		// tens of milliseconds on a SINGLE physical press; observed on a
		// Kinesis Adv360 Pro at a 6ms gap, which once turned a 60-second
		// hold into a 58-millisecond one. No human releases and re-presses
		// a key inside `bounceMS`, so a press that fast is ignored and the
		// latch stands.
		if bounceMS > 0 && s.dictationLatchTime != nil &&
			time.Since(*s.dictationLatchTime).Milliseconds() < bounceMS {
			return StartBounceIgnored
		}
		s.dictationToggleActive = false
		s.dictationActive = false
		s.dictationStartTime = nil
		s.dictationLatchTime = nil
		return StartToggleStop
	default:
		now := time.Now()
		s.dictationActive = true
		s.dictationStartTime = &now
		s.dictationToggleActive = false
		return StartBegin
	}
}

// DictationStop handles the state transition for an Option+T release.
//
// A hold of `tapMS` or more is push-to-talk: release finishes it. A shorter
// press is a tap, and a tap alone must still type ("Yes."): it used to latch
// the recording on, so a quick one-word reply kept recording instead.
// Now a tap leaves the recording running for the caller's
// double-tap window; a second tap inside it latches (DictationStart), and
// otherwise ResolveTap finishes it as an ordinary short hold. The returned
// generation identifies this tap for ResolveTap.
func (s *State) DictationStop(tapMS int64) (StopDecision, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dictationActive {
		return StopIgnore, 0
	}
	if s.dictationToggleActive {
		return StopLatchHeld, 0
	}
	if s.dictationStartTime != nil &&
		time.Since(*s.dictationStartTime).Milliseconds() < tapMS {
		s.dictationTapPending = true
		now := time.Now()
		s.tapReleasedAt = &now
		s.tapGen++
		return StopTapPending, s.tapGen
	}
	s.dictationActive = false
	s.dictationStartTime = nil
	return StopFinish, 0
}

// CancelDictationPress forgets the press that started the current dictation
// (a hold, a pending tap or a latch), so the key's release, or the next
// press, is not read as ending a recording that was cancelled (Esc).
func (s *State) CancelDictationPress() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dictationActive = false
	s.dictationToggleActive = false
	s.dictationStartTime = nil
	s.dictationLatchTime = nil
	s.dictationTapPending = false
	s.tapReleasedAt = nil
	s.tapGen++
}

// ResolveTap ends a tap's double-tap window. It reports whether the tap is
// still pending (no second tap came): the caller then finishes the recording
// as an ordinary hold. A tap since superseded (latched, bounced, or stopped)
// reports false.
func (s *State) ResolveTap(gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dictationTapPending || gen != s.tapGen {
		return false
	}
	s.dictationTapPending = false
	s.tapReleasedAt = nil
	s.dictationActive = false
	s.dictationStartTime = nil
	return true
}

// --- Last Match (repeat primitive) ---

func (s *State) SetLastMatch(m *LastMatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMatch = m
}

func (s *State) GetLastMatch() *LastMatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastMatch == nil {
		return nil
	}
	// Hand back a copy so a caller cannot mutate the cached match through
	// the returned pointer while the lock is not held. The action's own
	// `Params` / `Actions` still share backing arrays — read-only
	// downstream, where this used to copy the whole raw JSON buffer.
	if s.lastMatch.Action == nil {
		return &LastMatch{}
	}
	actionCopy := *s.lastMatch.Action
	return &LastMatch{Action: &actionCopy}
}

func (s *State) ClearLastMatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMatch = nil
}

// SetLastOutcome records the words of a command whose outcome could not be
// its own state because discovery stayed open (see orchestrate, Phase 4).
func (s *State) SetLastOutcome(phrase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOutcome = phrase
}

// TakeLastOutcome returns and clears the pending outcome of this hold.
func (s *State) TakeLastOutcome() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.lastOutcome
	s.lastOutcome = ""
	return o
}

// SetLastProblem records a failed command's sentence for the hold's end.
func (s *State) SetLastProblem(phrase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastProblem = phrase
}

// TakeLastProblem returns and clears the failure this hold has to say.
func (s *State) TakeLastProblem() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.lastProblem
	s.lastProblem = ""
	return p
}

// ClearCalibrationConsumed resets the calibration-consumed flag.
func (s *State) ClearCalibrationConsumed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calibrationConsumedSession = false
}

// --- Pipeline State ---

func (s *State) emitPipelineEvent(msg string) {
	s.notifyEvent("diagnostic.message", map[string]any{"message": msg})
}

func (s *State) SetCmdRecActive() {
	s.mu.Lock()
	prev := s.cmdRecState
	s.cmdRecState = PipelineActive
	s.mu.Unlock()
	s.emitPipelineEvent("pstate: command_recognition " + prev.String() + " → active")
}

func (s *State) SetCmdRecInactive() {
	s.mu.Lock()
	prev := s.cmdRecState
	s.cmdRecState = PipelineInactive
	s.mu.Unlock()
	s.emitPipelineEvent("pstate: command_recognition " + prev.String() + " → inactive")
}

func (s *State) SuppressCommandRecognition() {
	s.mu.Lock()
	prev := s.cmdRecState
	if s.cmdRecState == PipelineActive {
		s.cmdRecState = PipelineSuppressed
	}
	cur := s.cmdRecState
	s.mu.Unlock()
	if cur != prev {
		s.emitPipelineEvent("pstate: command_recognition " + prev.String() + " → suppressed")
	}
}

func (s *State) UnsuppressCommandRecognition() {
	s.mu.Lock()
	prev := s.cmdRecState
	if s.cmdRecState == PipelineSuppressed {
		s.cmdRecState = PipelineActive
	}
	cur := s.cmdRecState
	s.mu.Unlock()
	if cur != prev {
		s.emitPipelineEvent("pstate: command_recognition " + prev.String() + " → active")
	}
}

func (s *State) IsCmdRecSuppressed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdRecState == PipelineSuppressed
}

// DropsCommandTranscripts reports whether a command_recognition transcript
// must be dropped: a dictation has suppressed continuous listening's
// persistent run and no command hold is listening on a run of its own. A
// hold, down or draining its end-of-hold flush, hears commands under a
// dictation with continuous listening on as it does with it off; reading the
// suppression alone dropped every command spoken in that hold.
func (s *State) DropsCommandTranscripts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdRecState == PipelineSuppressed && !s.cmdHoldActive && !s.cmdHoldDraining
}

func (s *State) StartDictationRecording() {
	s.mu.Lock()
	prev := s.dictationState
	s.dictationState = PipelineRecording
	if s.drainTimer != nil {
		s.drainTimer.Stop()
		s.drainTimer = nil
	}
	s.mu.Unlock()
	s.emitPipelineEvent("pstate: dictation " + prev.String() + " → recording")
}

// drainBackstop bounds how long a draining dictation waits for its transcript.
// The primary finish signal is the dictation pipeline's `pipeline.stopped`
// event (ordered delivery guarantees the transcript, if any, arrived first);
// this timer is the safety net for a stop that never reports back. It MUST
// exceed the pipeline's cancel_delay_ms (60s in plugin.json) — a 10s value
// here once dropped every transcript whose transcription took 10–15s, inside
// the very window the platform holds WhisperKit open for.
//
// 65s against the dictation pipeline's 60s: decoding takes about 1 s plus
// 64 ms per second of audio (811 recorded dictations, 2026-10-04) with spikes
// of several seconds, so a ten-minute dictation — the stage's cap — needs
// about 40 s. The earlier 15s/20s pair dropped every dictation past roughly
// two minutes.
const drainBackstop = 65 * time.Second

func (s *State) DrainDictation() {
	s.mu.Lock()
	prev := s.dictationState
	if s.dictationState == PipelineRecording {
		s.dictationState = PipelineDraining
		if s.drainTimer != nil {
			s.drainTimer.Stop()
		}
		s.drainTimer = time.AfterFunc(drainBackstop, func() {
			s.emitPipelineEvent("pstate: dictation drain timeout — forcing finish")
			s.FinishDictationIfDraining()
		})
	}
	s.mu.Unlock()
	if prev == PipelineRecording {
		s.emitPipelineEvent("pstate: dictation recording → draining")
	}
}

func (s *State) FinishDictation() {
	s.mu.Lock()
	prev := s.dictationState
	s.finishDictationLocked()
	s.mu.Unlock()
	if prev != PipelineInactive {
		s.emitPipelineEvent("pstate: dictation " + prev.String() + " → inactive")
	}
	if prev == PipelineDraining {
		s.onDictationFinished()
	}
}

// FinishDictationIfDraining finishes the dictation session only if it is
// draining. The pipeline.stopped event and the drain backstop timer both land
// here: they refer to the pipeline that was stopped, so they must not clobber
// a recording that started after it (fast re-press while the old pipeline
// wound down).
// onDictationFinished (the hook New was given) fires when a dictation session leaves DRAINING — the
// transcript landed, or the backstop gave up. Injected rather than imported so
// this package stays free of the shell: main.go wires it to clear the
// "transcribing" menu-bar icon.
//

func (s *State) FinishDictationIfDraining() {
	s.mu.Lock()
	if s.dictationState != PipelineDraining {
		s.mu.Unlock()
		return
	}
	s.finishDictationLocked()
	lost := s.onDictationLost
	s.mu.Unlock()
	s.emitPipelineEvent("pstate: dictation draining → inactive (no transcript)")
	s.onDictationFinished()
	if lost != nil {
		lost()
	}
}

// SetOnDictationLost installs the hook run when a dictation ends with no
// transcript (see onDictationLost).
func (s *State) SetOnDictationLost(f func()) {
	s.mu.Lock()
	s.onDictationLost = f
	s.mu.Unlock()
}

// finishDictationLocked resets the dictation pipeline state AND the speak
// transcript flags. Clearing the flags here matters: a transcript that never
// arrives (drain timeout) must not leave a pending/discard flag behind to
// misroute the NEXT session's transcript through the speak path. Callers hold
// s.mu.
func (s *State) finishDictationLocked() {
	s.dictationState = PipelineInactive
	s.speakTranscriptPending = false
	s.speakTranscriptDiscard = false
	if s.drainTimer != nil {
		s.drainTimer.Stop()
		s.drainTimer = nil
	}
}

func (s *State) IsDictationAccepting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dictationState == PipelineRecording ||
		s.dictationState == PipelineDraining
}

func (s *State) GetDictationPipelineState() PipelineState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dictationState
}

func (s *State) GetCmdRecPipelineState() PipelineState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdRecState
}

// --- Command-hold deferred teardown ---

// StartCommandHold marks a new command hold (Option+G down) and layers
// command_hold over the current mode as an overlay. It bumps the generation —
// invalidating any still-pending drain from a previous hold (a fast
// re-press) — and reports whether such a drain was pending, so the caller can
// close the previous hold's boundary before this one begins.
//
// ownsPipeline reports whether the hold's release must stop its
// command_recognition run: always, until DisownCommandHold. Over continuous
// listening that run is the hold's own; the persistent run is set aside for
// the hold (SetCmdRecAsideForHold).
func (s *State) StartCommandHold() (prevDrainPending, ownsPipeline bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevDrainPending = s.cmdHoldDraining
	s.cmdHoldGen++
	s.cmdHoldDraining = false
	s.lastOutcome = ""
	s.lastProblem = ""
	if s.cmdHoldDrainTimer != nil {
		s.cmdHoldDrainTimer.Stop()
		s.cmdHoldDrainTimer = nil
	}
	s.saveBaseLocked()
	ownsPipeline = true
	s.cmdHoldDrainAwait = 0
	s.cmdHoldActive = true
	s.cmdHoldOwnsPipeline = ownsPipeline
	s.currentMode = "command_hold"
	return prevDrainPending, ownsPipeline
}

// EndCommandHold marks Option+G released (or the hold aborted). wasActive is
// false when no hold was in progress — a release with no press, which the
// caller ignores. ownsPipeline is StartCommandHold's answer for this hold.
// The mode is left alone: LeaveCommandHold restores it once the hold's
// pipeline has drained.
func (s *State) EndCommandHold() (wasActive, ownsPipeline bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wasActive, ownsPipeline = s.cmdHoldActive, s.cmdHoldOwnsPipeline
	s.cmdHoldActive = false
	s.cmdHoldOwnsPipeline = false
	return wasActive, ownsPipeline
}

// DisownCommandHold marks the current hold as having no run of its own (its
// run could not be started), so its release stops nothing.
func (s *State) DisownCommandHold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmdHoldOwnsPipeline = false
}

// CommandHoldDraining reports whether a released hold's cleanup is still
// waiting for its run's end-of-hold flush.
func (s *State) CommandHoldDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdHoldDraining
}

// SetCmdRecAsideForHold sets continuous listening's persistent
// command_recognition run aside for a command hold. stopPersistent reports
// whether the caller must stop it: continuous listening is on and the run is
// up (not already set aside by an earlier hold still draining, nor stopped
// by a dictation). Outside continuous listening there is nothing to set
// aside and nothing is recorded.
func (s *State) SetCmdRecAsideForHold() (stopPersistent bool) {
	s.mu.Lock()
	if !s.continuousBaseLocked() || s.cmdRecAside {
		s.mu.Unlock()
		return false
	}
	s.cmdRecAside = true
	stopPersistent = s.cmdRecState == PipelineActive
	state := s.cmdRecState
	s.mu.Unlock()
	s.emitPipelineEvent("pstate: command_recognition " + state.String() + ", set aside for a command hold")
	return stopPersistent
}

// ReleaseCmdRecAside ends a hold's claim on the persistent run. restart
// reports whether the caller must start it again: it was set aside, and
// continuous listening still wants it (on, and no dictation holds it).
//
// A hold's cleanup runs on the notification goroutine and a press on its
// own, so a new hold can begin while the last one's cleanup is still
// running. The claim then passes to the new hold, which found the run
// already aside and stopped nothing: restarting it here would start the
// persistent run beside, or instead of, the new hold's own, and the new
// hold's cleanup would find nothing left to restart. So while a hold is
// down nothing is released, unless the hold itself gives up its run
// (underHold, when its own run could not be started).
func (s *State) ReleaseCmdRecAside() (restart bool) {
	return s.releaseCmdRecAside(false)
}

// ReleaseCmdRecAsideUnderHold is ReleaseCmdRecAside for the hold that is
// down: it gives its claim up itself.
func (s *State) ReleaseCmdRecAsideUnderHold() (restart bool) {
	return s.releaseCmdRecAside(true)
}

func (s *State) releaseCmdRecAside(underHold bool) (restart bool) {
	s.mu.Lock()
	if !s.cmdRecAside || (s.cmdHoldActive && !underHold) {
		s.mu.Unlock()
		return false
	}
	s.cmdRecAside = false
	restart = s.continuousBaseLocked() && s.cmdRecState == PipelineActive
	state := s.cmdRecState
	s.mu.Unlock()
	s.emitPipelineEvent("pstate: command_recognition " + state.String() + ", no longer set aside for a command hold")
	return restart
}

// IsCmdRecAside reports whether a command hold has set the persistent run
// aside: command_recognition is the hold's own run until it is released.
func (s *State) IsCmdRecAside() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdRecAside
}

// continuousBaseLocked is IsContinuousActive for callers holding s.mu.
func (s *State) continuousBaseLocked() bool {
	mode := s.currentMode
	if s.priorMode != "" {
		mode = s.priorMode
	}
	return mode == "continuous_sleeping" || mode == "continuous_awake"
}

// cmdRecRunsCap bounds cmdRecRuns: one `started` heard with no `stopped`
// to follow it must not grow the list forever. A run of each kind at once is
// the most there ever is.
const cmdRecRunsCap = 8

// NoteCmdRecStarted records the platform's `started` for a
// command_recognition run.
func (s *State) NoteCmdRecStarted(ephemeral bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmdRecRuns = append(s.cmdRecRuns, ephemeral)
	if len(s.cmdRecRuns) > cmdRecRunsCap {
		s.cmdRecRuns = s.cmdRecRuns[len(s.cmdRecRuns)-cmdRecRunsCap:]
	}
}

// NoteCmdRecUnseenRun records a command_recognition run that is up but
// whose `started` this process never heard (it started before a restart).
// It is the oldest run there is, so it goes first.
func (s *State) NoteCmdRecUnseenRun(ephemeral bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmdRecRuns = append([]bool{ephemeral}, s.cmdRecRuns...)
	if len(s.cmdRecRuns) > cmdRecRunsCap {
		s.cmdRecRuns = s.cmdRecRuns[len(s.cmdRecRuns)-cmdRecRunsCap:]
	}
}

// NoteCmdRecStopped matches the platform's `stopped` for command_recognition
// to the run it ends: the oldest one started. known is false when voice saw
// no `started` for it (a run from before this process). drainDue reports
// whether a released hold's cleanup may run now: every run that was up at
// its release has stopped (see cmdHoldDrainAwait).
func (s *State) NoteCmdRecStopped() (known, ephemeral, drainDue bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	drainDue = true
	if s.cmdHoldDrainAwait > 0 {
		s.cmdHoldDrainAwait--
		drainDue = s.cmdHoldDrainAwait == 0
	}
	if len(s.cmdRecRuns) == 0 {
		return false, false, drainDue
	}
	ephemeral = s.cmdRecRuns[0]
	s.cmdRecRuns = s.cmdRecRuns[1:]
	return true, ephemeral, drainDue
}

// CommandHoldActive reports whether Option+G is down.
func (s *State) CommandHoldActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdHoldActive
}

// LeaveCommandHold returns from an ended hold to what it covered, and reports
// the mode to write: a dictation still live under the hold (whose own
// release later restores the base), else the base mode. changed is false
// when command_hold is not what the user sees (a dictation started over the
// hold already covers it) or when a newer hold has begun.
func (s *State) LeaveCommandHold() (mode string, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmdHoldActive || s.currentMode != "command_hold" {
		return s.currentMode, false
	}
	if s.overlay != "" {
		s.currentMode = s.overlay
		return s.currentMode, true
	}
	return s.restoreBaseLocked(), true
}

// BeginCommandHoldDrain records that the current hold is draining after key
// release: the pipeline is stopping but its end-of-hold flush is still in
// flight. fire runs at most once — when FinishCommandHoldDrain is called for
// this hold (the pipeline.stopped event), or timeout elapses as a backstop —
// and only while no newer hold has superseded it.
func (s *State) BeginCommandHoldDrain(timeout time.Duration, fire func()) {
	s.mu.Lock()
	gen := s.cmdHoldGen
	s.cmdHoldDraining = true
	s.cmdHoldDrainAwait = len(s.cmdRecRuns)
	if s.cmdHoldDrainTimer != nil {
		s.cmdHoldDrainTimer.Stop()
	}
	s.cmdHoldDrainTimer = time.AfterFunc(timeout, func() {
		if s.consumeCommandHoldDrain(gen) {
			fire()
		}
	})
	s.mu.Unlock()
}

// FinishCommandHoldDrain fires the pending drain cleanup iff one is pending for
// the current hold. Idempotent — safe to call on every command_recognition
// pipeline.stopped, including ones with no drain pending (no-op then).
func (s *State) FinishCommandHoldDrain(fire func()) {
	s.mu.Lock()
	gen := s.cmdHoldGen
	s.mu.Unlock()
	if s.consumeCommandHoldDrain(gen) {
		fire()
	}
}

// CancelCommandHoldDrain discards a pending drain without firing — used on
// abort, which tears the session down immediately.
func (s *State) CancelCommandHoldDrain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmdHoldDraining = false
	if s.cmdHoldDrainTimer != nil {
		s.cmdHoldDrainTimer.Stop()
		s.cmdHoldDrainTimer = nil
	}
}

// consumeCommandHoldDrain clears the pending drain for gen iff it is still the
// active, un-superseded drain. Returns true exactly once. fire is invoked by the
// caller outside the lock.
func (s *State) consumeCommandHoldDrain(gen int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cmdHoldDraining || gen != s.cmdHoldGen {
		return false
	}
	s.cmdHoldDraining = false
	if s.cmdHoldDrainTimer != nil {
		s.cmdHoldDrainTimer.Stop()
		s.cmdHoldDrainTimer = nil
	}
	return true
}

// Platform is the slice of the generated SDK surface the session calls.
// *branchkit.Plugin satisfies it; tests supply a recording fake.
type Platform interface {
	CommandsResolve(req branchkit.CommandsResolveRequest) (*branchkit.CommandsResolveResponse, error)
	CommandsResolvePhrase(req branchkit.CommandsResolvePhraseRequest) (*branchkit.CommandsResolvePhraseResponse, error)
	CollectionPut(req branchkit.CollectionPutRequest) (*branchkit.CollectionPutResponse, error)
	CollectionDeleteRecords(req branchkit.CollectionDeleteRecordsRequest) (*branchkit.CollectionDeleteRecordsResponse, error)
	VocabularyCommit() error
	OutputState(req branchkit.OutputStateRequest) (*branchkit.OutputStateResponse, error)
	OutputClear(req branchkit.OutputClearRequest) (*branchkit.OutputClearResponse, error)
}

// notifyEvent appends one event to the structured log as a NOTIFICATION — no
// reply awaited, errors unobserved, as these always were. The payload is the
// generated request type so the wire shape tracks the spec; only the method
// dispatch is by name, because the generated EventsAppend wrapper is a request.
func (s *State) notifyEvent(eventType string, data any) {
	if s.PluginNotify == nil {
		return
	}
	req := branchkit.EventsAppendRequest{EventType: eventType}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			branchkit.Logf("voice", "events.append %s: marshal failed: %v", eventType, err)
			return
		}
		req.Data = raw
	}
	s.PluginNotify(branchkit.MethodEventsAppend, req)
}
