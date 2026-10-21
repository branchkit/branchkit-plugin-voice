package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/session"
	"github.com/branchkit/plugin-sdk-go"
)

// --- Helpers ---

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// modeRecord describes one voice mode and the platform-input projections it
// drives. The Go map (`modeRecords`) is the lookup table; setVoiceMode reads
// from it and writes the resulting projection payload to `_platform.input`.
// Adding a mode = adding a row. Adding a directive = adding a field here AND
// adding the key to the platform's closed directive vocabulary.
type modeRecord struct {
	active     bool
	exclusive  bool
	directives map[string]any
}

// modeRecords is the source of truth for what each mode projects to
// `_platform.input`. Replaces the prior `liftKeys := mode == "command_hold"`
// derivation with declarative data — new input plugins (foot pedal, eye
// tracker, etc.) follow this same shape rather than re-inventing
// string-equality checks against their own mode vocabularies.
//
// Do not mutate `rec.directives` in callers. Go map values are reference
// types, so writes like `rec.directives["foo"] = bar` would persist on
// the table and leak into every subsequent setVoiceMode call. If a
// dynamic directive ever needs to be added at runtime, build a fresh
// map locally instead of mutating the shared one.
var modeRecords = map[string]modeRecord{
	"idle":                {active: false, directives: map[string]any{}},
	"command_hold":        {active: true, directives: map[string]any{branchkit.InputDirectiveLiftKeysOnDispatch: true}},
	"continuous_awake":    {active: true, directives: map[string]any{}},
	"continuous_sleeping": {active: false, exclusive: true, directives: map[string]any{}},
	"dictation":           {active: true, directives: map[string]any{}},
	"spelling":            {active: true, directives: map[string]any{}},
}

// modePayload builds the `plugin.voice.mode` record. Split out from the I/O
// for the same reason modeNeedsBootReset is: what the record says is the whole
// decision, and it should be checkable without a live plugin.
//
// `prior` is the mode an open overlay is covering. It matters because an
// overlay (dictation, or a speak session, over continuous) REPLACES the
// durable record with its own transient one, while the mode it covers lives
// only in the session's in-memory priorMode — which dies with the process. A
// hot-reload mid-overlay therefore used to lose continuous mode outright: boot
// reconcile reset the `dictation` residue to idle, and restoreContinuousMode
// then read `idle` and had nothing to restore. Carrying the covered mode on
// the record is what lets the durable intent survive the write that displaces
// it.
func modePayload(mode string, rec modeRecord, prior string) map[string]any {
	payload := map[string]any{"name": mode}
	if rec.exclusive {
		payload["exclusive"] = true
	}
	if isContinuousMode(prior) {
		payload["overlay_of"] = prior
	}
	return payload
}

// traceID is the tr_ of the platform request or notification this goroutine
// is handling, or "-" when there is none.
//
// Voice's transcript log lines carry it in place of what was said, so its
// log never holds the user's words. The platform's own lines for the same tr_ show the words, under
// the platform's recording rule: kept in a dev build, reduced to their size
// in a release build unless the user turned recording on. Voice does not
// re-decide that rule; it just never prints the words itself.
func (h *Host) traceID() string {
	if id := h.plugin.CurrentCorrelation(); id != "" {
		return id
	}
	return "-"
}

// setVoiceMode writes voice's mode tag and the platform input projection.
// `plugin.voice.mode` carries the singleton-name-field gate (auto-clears the
// prior mode). `_platform.input` carries `{active, source, directives}` —
// looked up from `modeRecords`, not derived from string equality. No-op when
// plugin is nil (test mode).
//
// The session's mode changes first, and the platform must follow it. A write
// that fails or times out used to be logged and left, so voice believed it
// was in the new mode while the platform held the old tag or input state
// until the next transition. Now a failed write is re-asserted (see
// retryModeWrite). Both writes are silent when they change nothing, so
// writing the whole state again is free.
func (h *Host) setVoiceMode(mode string) {
	h.sess.SetCurrentMode(mode)
	if _, ok := modeRecords[mode]; !ok {
		branchkit.Logf("voice", "setVoiceMode: unknown mode %q — refusing to write", mode)
		return
	}
	h.modeWriteMu.Lock()
	err := h.writeMode(mode)
	h.modeWriteMu.Unlock()
	if err != nil {
		h.retryModeWrite(mode, 0)
	}

	h.applyModeIcon(mode)

	go h.plugin.SettingsRefresh()
}

// defaultModeRetryDelays is how long each re-assert of a failed mode write
// waits; after the last, voice gives up and says so.
var defaultModeRetryDelays = []time.Duration{250 * time.Millisecond, time.Second, 4 * time.Second}

// retryModeWrite re-asserts `mode` after a failed write, unless a newer
// transition has superseded it. The check and the write happen under
// modeWriteMu, the lock setVoiceMode writes under, so a newer mode's writes
// always come after this one's.
func (h *Host) retryModeWrite(mode string, attempt int) {
	if attempt >= len(h.modeRetryDelays) {
		branchkit.Logf("voice", "mode %q: platform state still not written after %d retries; giving up until the next transition", mode, attempt)
		return
	}
	h.modeRetryAfter(h.modeRetryDelays[attempt], func() {
		h.modeWriteMu.Lock()
		if h.sess.GetCurrentMode() != mode {
			h.modeWriteMu.Unlock()
			return // superseded: the newer transition writes its own state
		}
		err := h.writeMode(mode)
		h.modeWriteMu.Unlock()
		if err != nil {
			h.retryModeWrite(mode, attempt+1)
			return
		}
		branchkit.Logf("voice", "mode %q: platform state written on retry %d", mode, attempt+1)
	})
}

// writeModeState makes both of a mode's platform writes, attempting the
// second even when the first fails, and reports either failure.
func (h *Host) writeModeState(mode string) error {
	rec := modeRecords[mode]
	var errs []error
	if err := h.plugin.Put("plugin.voice.mode", mode, modePayload(mode, rec, h.sess.PriorMode())); err != nil {
		branchkit.Logf("voice", "failed to set voice mode: %v", err)
		errs = append(errs, err)
	}
	// Per-writer record: the id IS this plugin's identity (`_platform.input`
	// keys records by `source`), so a foot pedal or eye tracker holding its
	// own session is a sibling record, not a clobber target — and the
	// platform folds the union. Writer-held, so a crash mid-hold drops this
	// assertion instead of leaving lift_keys_on_dispatch stuck for everyone.
	if err := h.plugin.Put("_platform.input", h.plugin.ID(), map[string]any{
		"active":     rec.active,
		"source":     h.plugin.ID(),
		"directives": rec.directives,
	}); err != nil {
		branchkit.Logf("voice", "failed to write _platform.input: %v", err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// applyModeIcon maps a voice mode to a shell icon — unless a dictation is still
// DRAINING, in which case the transcript is in flight and that is the more
// useful thing to report.
//
// The guard is load-bearing and was learned the hard way. `pipelines.stop` does
// NOT block until the pipeline winds down: it returns immediately, so
// `leaveDictationOverlay` restores the prior mode roughly a millisecond after
// the hold ends. Without this, the "transcribing" icon set on the drain path
// was overwritten by "ready" within the same millisecond — the state existed,
// the animation started, and both were gone before a frame could render. The
// live log showed `transcribing` lasting 0.00s, every time.
func (h *Host) applyModeIcon(mode string) {
	if h.sess != nil && h.sess.GetDictationPipelineState() == session.PipelineDraining {
		h.setIndicator(branchkit.IndicatorStateProcessing)
		return
	}
	switch mode {
	case "continuous_sleeping":
		h.setIndicator(branchkit.IndicatorStateStandby)
	case "continuous_awake":
		h.setIndicator(branchkit.IndicatorStateListening)
	case "command_hold":
		h.setIndicator(branchkit.IndicatorStateInputAction)
	case "dictation":
		h.setIndicator(branchkit.IndicatorStateInputText)
	case "idle":
		h.setIndicator(branchkit.IndicatorStateIdle)
	}
}

// setIndicator shows what input is doing on the recording indicator. The
// states are the platform's (what the input becomes), not voice's modes; each
// host draws its own glyph for them.
func (h *Host) setIndicator(state branchkit.IndicatorState) {
	if err := h.plugin.IndicatorSet(branchkit.IndicatorSetRequest{State: state}); err != nil {
		branchkit.Logf("voice", "indicator.set %s: %v", state, err)
	}
}

// reconcileOrphanedSessionState resets hold-mode residue in the mode TAG a
// previous process may have left mid-hold.
//
// This used to also be the fix for `_platform.input` (a crash mid-hold left
// lift_keys_on_dispatch asserted for every plugin's dispatch) — no longer:
// that collection is per-writer and writer-held now, so a dead process's
// assertion drops with it. What remains is the `plugin.voice.mode` gate,
// which opts OUT of platform reaping (held_by_writer: false) because
// continuous mode is durable intent restoreContinuousMode reads back — and
// that opt-out is exactly why its hold-mode residue (a stale `command_hold`
// gate making hold-only commands eligible with no hold in progress) is still
// this plugin's own semantics to reset.
//
// The rule the reset follows is the `modeRecords` table's own classification
// rather than an invented one: the HOLD modes describe a key being physically
// held, which a fresh process cannot be doing, so they are unrepresentable at
// boot and reset to idle. The CONTINUOUS modes are deliberately persistent —
// they survive a hot-reload by design — so they are left for
// `restoreContinuousMode` to pick up. Reading, not assuming, is what keeps this
// from clobbering that restore.
//
// The one shape that classification has no name for is an OVERLAY standing on
// continuous: `dictation` is a hold mode by every test above, so it resets —
// taking the continuous intent underneath it with it, since `priorMode` was
// process-local and just died. `overlay_of` on the record is that missing
// evidence, and this is where it gets read.
//
// Returns the continuous mode the reset displaced, if the residue was an
// overlay covering one — "" otherwise. The reset has to write `idle` over the
// record before restoreContinuousMode ever reads it, so the durable intent
// riding that record can only survive as a return value. Handing it back
// explicitly beats a package-level var: the two functions already have a
// stated ordering relationship, and this makes the payload of that ordering
// visible at the call site instead of implied.
func (h *Host) reconcileOrphanedSessionState() string {
	// Limit(1) because only records[0] is ever read. Reading with nil would
	// pull the whole collection to look at its head, and the platform's
	// default list limit would silently decide how much of it arrived.
	records, err := h.plugin.List("plugin.voice.mode", branchkit.NewListOpts().Limit(1).Build())
	if err != nil || len(records) == 0 {
		return ""
	}
	var mode struct {
		Name      string `json:"name"`
		OverlayOf string `json:"overlay_of"`
	}
	if err := json.Unmarshal(records[0].Payload, &mode); err != nil {
		return ""
	}
	if !modeNeedsBootReset(mode.Name) {
		return ""
	}
	branchkit.Logf("voice", "boot reconcile: previous process left mode %q — resetting to idle", mode.Name)
	h.setVoiceMode("idle")
	if isContinuousMode(mode.OverlayOf) {
		branchkit.Logf("voice", "boot reconcile: %q was an overlay over %q — deferring to continuous restore",
			mode.Name, mode.OverlayOf)
		return mode.OverlayOf
	}
	return ""
}

// modeNeedsBootReset answers whether a persisted mode is one a FRESH process
// contradicts by existing.
//
// Split out from the I/O so the classification is testable without a live
// plugin — and because the classification is the whole decision. It reads
// `modeRecords` rather than listing mode names, so a mode added to that table
// is covered here by construction; a new hold mode that forgot to appear here
// is the failure this shape makes unrepresentable.
//
//   - hold modes (`active`, non-continuous): describe a key being physically
//     held. A fresh process is not holding anything → reset.
//   - continuous modes: deliberately survive a hot-reload → leave for
//     restoreContinuousMode. `continuous_sleeping` is `active: false` and would
//     be skipped anyway; the prefix check makes that intent explicit rather
//     than incidental, so flipping that flag can't silently start clobbering
//     the restore.
//   - idle, or anything not in the table: nothing to undo.
func modeNeedsBootReset(name string) bool {
	if isContinuousMode(name) {
		return false
	}
	rec, ok := modeRecords[name]
	return ok && rec.active
}

// isContinuousMode answers whether a persisted mode name is one of the durable
// continuous modes. Prefix-matched, and shared by every caller that asks the
// question, so the boot reset, the overlay carry, and the restore cannot come
// to different answers about which names are continuous — and a third
// `continuous_*` mode is covered by all three without edits.
func isContinuousMode(name string) bool {
	return strings.HasPrefix(name, "continuous_")
}

// restoreContinuousMode re-enters continuous mode if it was active before a
// hot-reload killed the plugin. `displaced` is reconcileOrphanedSessionState's
// answer: non-empty when the dead process was inside an overlay (dictation, or
// a speak session) covering continuous, in which case the record itself no
// longer says so and this is the only surviving evidence.
func (h *Host) restoreContinuousMode(displaced string) {
	target := displaced
	if target == "" {
		records, err := h.plugin.List("plugin.voice.mode", branchkit.NewListOpts().Limit(1).Build())
		if err != nil || len(records) == 0 {
			return
		}
		var mode struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(records[0].Payload, &mode); err != nil {
			return
		}
		target = mode.Name
	}
	if !isContinuousMode(target) {
		return
	}
	if h.sess.IsContinuousActive() {
		return
	}
	branchkit.Logf("voice", "restoring continuous mode: %s (post hot-reload)", target)
	// setVoiceMode, not SetCurrentMode + setShellIcon: the record needs
	// rewriting in the displaced case (reconcile just wrote `idle` over it, and
	// continuous_sleeping's tag is what makes its gate exclusive), and
	// `_platform.input` needs writing in BOTH — it is per-writer and
	// writer-held, so the dead process's record dropped with it and this
	// process has asserted nothing yet.
	h.setVoiceMode(target)
	// As the toggle does: a command hold reads this to decide whether to set
	// the persistent run aside and whether its cleanup restarts it, and the
	// restart-on-failure logic reads it too (cmdRecWanted). Left at its zero
	// value, the first hold after a restart stopped the run for good.
	h.sess.SetCmdRecActive()
	// A run the dead process started may still be up. This process never
	// heard it start, so it is recorded first: voice matches each `stopped`
	// to the oldest run it knows of (session.NoteCmdRecStopped), and a run
	// it does not know of would have its `stopped` taken for the next run's,
	// ending a later hold's drain early.
	running, ephemeral, err := h.commandRecognitionRunning()
	if err != nil {
		branchkit.Logf("voice", "command_recognition: pipelines.status failed: %v", err)
	}
	if running {
		h.sess.NoteCmdRecUnseenRun(ephemeral)
		if !ephemeral {
			// Continuous listening's run survived the restart: keep it.
			branchkit.Logf("voice", "command_recognition: kept the run from before the restart")
			return
		}
		// A command hold's run the dead process left. The platform stops a
		// terminated plugin's ephemeral runs itself; this covers a stop it
		// could not send. Left up, this Run would be skipped as already
		// running and continuous listening would go on over a run the
		// platform treats as a held key (no echo filter; spoken answers to
		// an app's question). The new run starts at once and buffers while
		// the old one winds down.
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "command_recognition"})
	}
	h.runCommandRecognition(false)
}

// commandRecognitionRunning asks the platform whether a command_recognition
// run is up, and whether it is ephemeral.
func (h *Host) commandRecognitionRunning() (running, ephemeral bool, err error) {
	st, err := h.pipelinesStatus()
	if err != nil || st == nil {
		return false, false, err
	}
	for _, p := range st.Pipelines {
		if p.Name == "command_recognition" {
			return true, p.Ephemeral, nil
		}
	}
	return false, false, nil
}
