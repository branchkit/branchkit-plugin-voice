package main

import (
	"sync"
	"time"

	"github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/session"
)

// Continuous listening keeps the command_recognition pipeline up, and that
// pipeline can end without anyone asking: a stage fails, or a pipeline reload
// (a plugin enabled or disabled, a pipeline record written) tears it down
// and, the pipeline being manual, does not start it again. The platform says
// so with `_platform.pipeline.stopped` and a reason; voice starts it again
// here. Before, recognition ended while the menu bar still said listening,
// and the people who leave continuous listening on are the ones who cannot
// reach for the keyboard to notice.

// cmdRecRestartLimit restarts after a failure are allowed per
// cmdRecRestartWindow. A stage that fails as it starts would otherwise be
// restarted forever; past the limit continuous listening is turned off and
// the user is told why, which is the truth the menu bar then shows.
const (
	cmdRecRestartLimit  = 3
	cmdRecRestartWindow = 2 * time.Minute
	// cmdRecRestartDelay spaces a restart after a failure from the failure,
	// so a stage that fails at once is not respawned in a tight loop.
	cmdRecRestartDelay = time.Second
)

// cmdRecRecovery is the restart budget: when the restarts after a failure
// happened, within the window.
type cmdRecRecovery struct {
	mu       sync.Mutex
	restarts []time.Time
}

// allow records a restart at now and reports whether it is within budget.
func (r *cmdRecRecovery) allow(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.restarts[:0]
	for _, t := range r.restarts {
		if now.Sub(t) < cmdRecRestartWindow {
			kept = append(kept, t)
		}
	}
	r.restarts = kept
	if len(r.restarts) >= cmdRecRestartLimit {
		return false
	}
	r.restarts = append(r.restarts, now)
	return true
}

// cmdRecWanted reports whether the persistent command_recognition run
// should be up now: continuous listening is on, and neither a dictation nor
// a command hold has set it aside (each brings it back when it ends).
func (h *Host) cmdRecWanted() bool {
	return h.sess.IsContinuousActive() &&
		h.sess.GetCmdRecPipelineState() == session.PipelineActive &&
		!h.sess.IsCmdRecAside()
}

// handleCommandRecognitionStopped acts on command_recognition's `stopped`,
// once it knows which run ended (session.NoteCmdRecStopped):
//
//   - a command hold's ephemeral run: the hold has ended, so its deferred
//     cleanup runs and calibration hears the hold end. The platform emits
//     `stopped` only after its transcript worker has finished every queued
//     `on_transcript` for the run, so the end-of-hold flush has been matched
//     by now and a two-word codeword whose suffix decoded at flush completes
//     before the bridge is cleared. (Notification order alone does not give
//     this: `on_transcript` is a request, handled on its own goroutine.)
//   - continuous listening's persistent run: not a hold end, even when a
//     hold set it aside at its press. Only the restart logic hears it.
//
// The cleanup waits for the hold's own run, the newest one up at its
// release: a hold released before the run it set aside (or the previous
// hold's run) had finished winding down must not be cleaned up on that
// older run's `stopped`, and a run started after the release (a "speak"
// decoded at the flush) is not waited for. A `stopped` for a
// run voice never saw start (one from before this process) is treated as
// everything at once, which is what this did before runs were told apart.
func (h *Host) handleCommandRecognitionStopped(reason, errText string) {
	known, ephemeral, drainDue := h.sess.NoteCmdRecStopped()
	if drainDue {
		// No-op when no drain is pending.
		h.sess.FinishCommandHoldDrain(h.finishCommandHold)
	}
	if !known || ephemeral {
		h.calibHoldEnd()
	}
	if !known || !ephemeral {
		// A stop nobody asked for, while listening: start it again.
		h.handleCmdRecStopped(reason, errText)
	}
}

// handleCmdRecStopped acts on command_recognition's `stopped`. Only a stop
// nobody asked for is acted on: `requested` is voice's own stop (dictation,
// the continuous toggle, a hold's release) or the user's.
func (h *Host) handleCmdRecStopped(reason, errText string) {
	switch reason {
	case "error", "ended", "reload":
	default:
		return
	}
	if !h.cmdRecWanted() {
		return
	}
	branchkit.Logf("voice", "command_recognition stopped while listening (%s): %s", reason, errText)
	if reason == "reload" {
		// Not a failure: the platform rebuilt its pipelines. Start it
		// again at once; this Run reaches the manager after the reload.
		h.runCommandRecognition(false)
		branchkit.Logf("voice", "command_recognition: restarted after a pipeline reload")
		return
	}
	if !h.cmdRecRecovery.allow(time.Now()) {
		h.giveUpContinuous(errText)
		return
	}
	h.cmdRecRestartAfter(cmdRecRestartDelay, func() {
		// Asked again when it fires: the user may have turned listening
		// off, or started a dictation, in the meantime.
		if !h.cmdRecWanted() {
			return
		}
		h.runCommandRecognition(false)
		branchkit.Logf("voice", "command_recognition: restarted after it stopped (%s)", reason)
	})
}

// giveUpContinuous turns continuous listening off after command recognition
// failed past its restart budget, and says so: listening that cannot work
// must not look as if it is on.
func (h *Host) giveUpContinuous(errText string) {
	branchkit.Logf("voice", "command_recognition: failed %d times in %s — turning continuous listening off",
		cmdRecRestartLimit, cmdRecRestartWindow)
	h.sess.SetCmdRecInactive()
	h.plugin.SessionBoundary()
	h.sess.ClearLastMatch()
	if h.sess.DropContinuousBase() {
		// An Option+G hold covers continuous listening: it keeps the
		// screen, and its release now lands on idle rather than restoring
		// listening that nothing runs. The mode record is written again so
		// the base it carries is idle too.
		h.setVoiceMode(h.sess.GetCurrentMode())
	} else {
		h.setVoiceMode("idle")
	}
	h.hud.CloseDiscovery()
	body := "Command recognition kept stopping, so continuous listening is off. " +
		"Turn it on again to retry."
	if errText != "" {
		body += " The last error: " + errText
	}
	if err := h.systemNotify(branchkit.SystemNotifyRequest{
		Title: "Continuous listening stopped",
		Body:  body,
	}); err != nil {
		branchkit.Logf("voice", "continuous-stopped notice failed: %v", err)
	}
}
