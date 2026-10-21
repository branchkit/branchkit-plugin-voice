package main

import (
	"encoding/json"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// warmTarget is a pipeline pre-warmed at boot, with a friendly label for the
// readiness HUD. Generic by design: add a pipeline here (or a future third
// engine) and it gets its own bar with no other changes.
type warmTarget struct {
	pipeline string
	label    string
}

var warmTargets = []warmTarget{
	// User-facing labels: engine names (Sherpa/WhisperKit) are internal and
	// stay out of general UI — see the Models tab for the download exception.
	{pipeline: "command_recognition", label: "Command voice engine"},
	{pipeline: "dictation", label: "Dictation voice engine"},
}

// dictationReady reports whether the WhisperKit STT model is loaded and
// dictation can transcribe immediately. Flipped by the dictation pipeline's
// warmed event (boot prewarm) or the first live transcript.

// startVoiceWarmup pre-loads every engine at boot (mic left closed) so the
// first Option+G / Option+T isn't eaten by a cold model load, and shows a
// per-engine progress panel in the Discovery HUD that self-dismisses once all
// engines are ready. Readiness lands via _platform.pipeline.warmed →
// handlePipelineWarmed.
func (h *Host) startVoiceWarmup() {
	h.warmMu.Lock()
	for _, t := range warmTargets {
		h.warmReady[t.pipeline] = false
	}
	h.warmMu.Unlock()

	// Open the panel first so warmGen is set before any warmed event fires
	// (command_recognition warms in ~1s; the RPC round-trip below is slower).
	h.warmGen = h.hud.OpenOverlay(branchkit.OutputKindProgress, "Warming up voice…", "warming up voice", h.readinessSections())

	for _, t := range warmTargets {
		t := t
		go func() {
			// Dictation warms the user's configured model, not just the pipeline
			// default (else a model-changer gets a silently-wrong prewarm).
			var overrides map[string]json.RawMessage
			if t.pipeline == "command_recognition" {
				model := h.commandModelName()
				h.noteCommandModel(model)
				overrides = map[string]json.RawMessage{"model": mustJSON(model)}
			}
			if t.pipeline == "dictation" {
				vc := h.config.Get()
				overrides = map[string]json.RawMessage{
					"model": mustJSON(resolveDictationModel(vc.Model)),
				}
				if vc.Language != "" {
					overrides["language"] = mustJSON(vc.Language)
				}
			}
			if _, err := h.plugin.PipelinesWarm(branchkit.PipelinesWarmRequest{Name: t.pipeline, ParamOverrides: overrides}); err != nil {
				branchkit.Logf("voice", "warmup: %s warm request failed: %v", t.pipeline, err)
			}
		}()
	}
}

// rewarmPipeline re-warms a single pipeline's consumer stages off the hold path
// (fire-and-forget, no readiness HUD) — used after a runtime config change
// rebuilds its record, so the next hold reuses a ready recognizer.
func (h *Host) rewarmPipeline(name string) {
	// The command stage warms on the chosen model, as its runs start on it.
	var overrides map[string]json.RawMessage
	if name == "command_recognition" {
		overrides = map[string]json.RawMessage{"model": mustJSON(h.commandModelName())}
	}
	go func() {
		if _, err := h.plugin.PipelinesWarm(branchkit.PipelinesWarmRequest{Name: name, ParamOverrides: overrides}); err != nil {
			branchkit.Logf("voice", "rewarm: %s failed: %v", name, err)
		}
	}()
}

// handlePipelineWarmed records a pipeline as ready, refreshes the panel, and
// once every tracked engine is ready flips the title and self-dismisses.
func (h *Host) handlePipelineWarmed(pipeline string) {
	// A dictation stage is up, whichever dictation pipeline started it (a
	// reading that cold-starts the stage opens the same overlay): ready,
	// and the overlay closes. Before the boot-warmup bookkeeping below,
	// which tracks only the pipelines it warmed.
	if isDictationPipeline(pipeline) {
		h.markDictationReady()
		h.closeDictationWarming()
	}
	h.warmMu.Lock()
	if _, tracked := h.warmReady[pipeline]; !tracked {
		h.warmMu.Unlock()
		return
	}
	h.warmReady[pipeline] = true
	allReady := true
	for _, t := range warmTargets {
		if !h.warmReady[t.pipeline] {
			allReady = false
			break
		}
	}
	h.warmMu.Unlock()

	branchkit.Logf("voice", "warmup: %s ready (all=%v)", pipeline, allReady)

	if allReady {
		gen := h.warmGen
		h.hud.UpdateOverlay(gen, branchkit.OutputKindProgress, h.readyTitle(), h.readyTitle(), h.readinessSections())
		// A blocked keyboard is the one thing worth reading here; give it
		// time. A clean panel goes as quickly as before.
		hold := 1800 * time.Millisecond
		if h.typingBlockReason() != "" {
			hold = 8 * time.Second
		}
		go func() {
			time.Sleep(hold)
			h.hud.CloseOverlay(gen)
		}()
		return
	}
	h.hud.UpdateOverlay(h.warmGen, branchkit.OutputKindProgress, "Warming up voice…", "warming up voice", h.readinessSections())
}

// handlePipelineWarming is a run that has to start its stage before it can
// transcribe: the stage died since boot (a crash, a rebuilt binary, memory
// pressure) and its model loads again, seconds warm and up to a minute when
// the compiled-model cache is cold. The hold is not lost — the platform keeps
// the audio until the stage is up — but nothing on screen said so, and a
// minute-long wait read as broken (2026-10-06).
func (h *Host) handlePipelineWarming(pipeline string) {
	if !isDictationPipeline(pipeline) {
		return
	}
	branchkit.Logf("voice", "dictation: stage starting; the hold is kept until it is ready")
	h.dictationReady.Store(false)
	h.showDictationWarmingFor(pipeline)
}

// showDictationWarming opens the "dictation is starting" overlay, once.
func (h *Host) showDictationWarming() {
	h.showDictationWarmingFor("dictation")
}

// showDictationWarmingFor opens the overlay for pipeline's hold: a reading is
// recorded, never typed, and the overlay says so.
func (h *Host) showDictationWarmingFor(pipeline string) {
	h.dictationWarmMu.Lock()
	defer h.dictationWarmMu.Unlock()
	if h.dictationWarmGen != 0 {
		return
	}
	st := "Loading… keep talking: what you say is kept and typed once it is ready"
	if pipeline == dictationSamplePipeline {
		st = "Loading… keep reading: your sentence is kept and recorded once it is ready"
	}
	h.dictationWarmGen = h.hud.OpenOverlay(
		branchkit.OutputKindProgress,
		"Starting dictation…",
		"starting dictation",
		[]branchkit.OutputSection{{Items: []branchkit.OutputItem{{
			ID:       "dictation",
			Title:    "Dictation voice engine",
			Phrase:   "dictation voice engine loading, keep talking",
			Subtitle: &st,
		}}}},
	)
}

// closeDictationWarming closes that overlay if it is still the one showing.
func (h *Host) closeDictationWarming() {
	h.dictationWarmMu.Lock()
	gen := h.dictationWarmGen
	h.dictationWarmGen = 0
	h.dictationWarmMu.Unlock()
	if gen != 0 {
		h.hud.CloseOverlay(gen)
	}
}

// markDictationReady records that the STT model is loaded and clears any
// "Loading model…" status. Idempotent — called from the warmed event and, as a
// fallback, the first dictation transcript (if the warmed event was missed).
func (h *Host) markDictationReady() {
	if h.dictationReady.CompareAndSwap(false, true) {
		h.hud.SetPreparing(false)
	}
}

// readinessSections is the readiness panel as a `progress` document: one
// row per engine with its status, then the typing row. What each row means
// is all voice states; how a bar or a check looks is the renderer's.
func (h *Host) readinessSections() []branchkit.OutputSection {
	h.warmMu.Lock()
	defer h.warmMu.Unlock()
	var items []branchkit.OutputItem
	for _, t := range warmTargets {
		status := "Loading…"
		if h.warmReady[t.pipeline] {
			status = "Ready ✓"
		}
		st := status
		items = append(items, branchkit.OutputItem{
			ID:       t.pipeline,
			Title:    t.label,
			Phrase:   t.label + " " + status,
			Subtitle: &st,
		})
	}
	if row, ok := h.typingItem(); ok {
		items = append(items, row)
	}
	return []branchkit.OutputSection{{Items: items}}
}

// --- Keystroke delivery (2026-09-15) ---
//
// A warm model that types nothing: after a re-signed development build the
// WindowServer can refuse this process's synthesized keystrokes while the
// Accessibility checkbox stays on, and a password field anywhere holds the
// keyboard outright. Neither shows up in the model warm-up, and both were
// silent — the transcript was produced, the type call returned ok, the text
// went nowhere. The shell now reports both states through
// _platform.permission.changed (every state once at boot, then on change);
// the readiness panel renders them as a "Typing" row, and a refused
// `input.type_text` shows its reason instead of vanishing.

// handlePermissionChanged records the keystroke-delivery bits the shell
// reports and re-renders the readiness panel while it is showing. Other
// permissions are not this file's business.
func (h *Host) handlePermissionChanged(permission string, granted bool) {
	switch permission {
	case "post_event", "secure_input":
	default:
		return
	}
	h.typingMu.Lock()
	h.typingKnown[permission] = granted
	h.typingMu.Unlock()
	if gen := h.warmGen; gen != 0 {
		h.warmMu.Lock()
		all := true
		for _, t := range warmTargets {
			if !h.warmReady[t.pipeline] {
				all = false
				break
			}
		}
		h.warmMu.Unlock()
		title := "Warming up voice…"
		phrase := "warming up voice"
		if all {
			title = h.readyTitle()
			phrase = title
		}
		h.hud.UpdateOverlay(gen, branchkit.OutputKindProgress, title, phrase, h.readinessSections())
	}
}

// typingBlockReason is why typed text would land nowhere right now, or ""
// when it would land (or nothing has been reported yet).
func (h *Host) typingBlockReason() string {
	h.typingMu.Lock()
	defer h.typingMu.Unlock()
	if granted, known := h.typingKnown["post_event"]; known && !granted {
		return "Blocked — toggle BranchKit off and on under Privacy & Security › Accessibility"
	}
	if granted, known := h.typingKnown["secure_input"]; known && !granted {
		return "Blocked — a password field has the keyboard"
	}
	return ""
}

// readyTitle is the panel title once every engine is warm: plain "Voice
// ready" when typing will land, and the honest version when it will not.
func (h *Host) readyTitle() string {
	if h.typingBlockReason() != "" {
		return "Voice ready — typing blocked"
	}
	return "Voice ready"
}

// typingItem is the readiness panel's last row: whether a transcript will
// actually reach the app. Present only once the shell has reported (an older
// shell never will, and the row then simply does not appear).
func (h *Host) typingItem() (branchkit.OutputItem, bool) {
	h.typingMu.Lock()
	known := len(h.typingKnown) > 0
	h.typingMu.Unlock()
	if !known {
		return branchkit.OutputItem{}, false
	}
	status := h.typingBlockReason()
	if status == "" {
		status = "Ready ✓"
	}
	st := status
	return branchkit.OutputItem{
		ID:       "typing",
		Title:    "Typing",
		Phrase:   "typing " + status,
		Subtitle: &st,
	}, true
}

// reportTypingFailure is what a refused `input.type_text` looks like to the
// user: the platform's reason, in the overlay, for a few seconds. Before
// this the call's error was dropped and dictation "didn't work".
func (h *Host) reportTypingFailure(err error) {
	msg := err.Error()
	branchkit.Logf("voice", "type_text refused: %s", msg)
	h.reportProblem("Couldn't type that", msg)
}

// reportProblem puts a problem document in the overlay for a few seconds:
// drawn on screen, and spoken by a speech renderer, which is how a person
// who cannot see the screen learns something did not work.
func (h *Host) reportProblem(title, phrase string) {
	gen := h.hud.OpenOverlay(branchkit.OutputKindProblem, title, phrase, nil)
	go func() {
		time.Sleep(6 * time.Second)
		h.hud.CloseOverlay(gen)
	}()
}

// mustJSON marshals a scalar override value. param_overrides is
// map[string]json.RawMessage on the generated wrapper — the values are
// per-stage and genuinely open, so the SDK hands them through as raw JSON
// rather than pretending to a shape. Marshalling a string cannot fail.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
