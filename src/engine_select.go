package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	"github.com/branchkit/plugin-sdk-go"
)

// setCommandCaptureDir toggles trial-audio capture on the command stage by
// patching only the `dump-segments` param on the seeded command_recognition
// record (empty disables). A calibration game sets it for the game's duration.
// Wired to the calibration State's SetCaptureDir callback in main.go.
func (h *Host) setCommandCaptureDir(dir string) bool {
	return h.claimCapture(captureOwnerCalibration, dir)
}

// captureOwnerCalibration owns the capture while a calibration game records;
// a Say a phrase session owns it as "saynow:<folder>".
const captureOwnerCalibration = "calibration"

// claimCapture sets the command stage's one capture folder on owner's behalf,
// and reports false when it did not (refused, or the record update failed).
// Calibration and Say a phrase share that single setting, so a release ("")
// clears it only when owner still holds it: a Say a phrase session ending must
// not stop a calibration game's recording, nor an old session's cleanup stop a
// new session's. A calibration game starting takes the capture over and ends a
// Say a phrase session. The read-modify-write of the pipeline record runs
// under stageCaptureMu, so two claims never interleave.
func (h *Host) claimCapture(owner, dir string) bool {
	h.stageCaptureMu.Lock()
	defer h.stageCaptureMu.Unlock()
	next, patch, endSayNow, refused := captureClaim(h.captureOwner, owner, dir)
	if refused {
		return false
	}
	if !patch {
		return true // someone else's capture now: leave it
	}
	if dir == "" {
		// A release ends its owner's claim even if the record update fails:
		// the game or session is over either way, and a stale owner would
		// block every later claim.
		h.captureOwner = ""
		if !h.patchCommandCaptureDir("") {
			branchkit.Logf("voice", "capture: %s released the capture but the stage's folder could not be cleared", owner)
			return false
		}
		return true
	}
	if !h.patchCommandCaptureDir(dir) {
		return false // the stage did not change: the owner stays who it was
	}
	if endSayNow {
		h.sayNow.disarm()
	}
	h.captureOwner = next
	return true
}

// captureClaim is claimCapture's decision: the owner after the claim, whether
// to change the stage's folder, whether a Say a phrase session ends, and
// whether the claim is refused. A calibration game takes the capture from a
// Say a phrase session; a session never takes it from a game. The owner is
// "calibration" only while a game holds the capture: its release ends the
// claim even when clearing the stage fails (claimCapture).
func captureClaim(current, owner, dir string) (next string, patch, endSayNow, refused bool) {
	if dir == "" {
		if current != owner {
			return current, false, false, false
		}
		return "", true, false, false
	}
	if current == captureOwnerCalibration && owner != captureOwnerCalibration {
		return current, false, false, true
	}
	return owner, true, owner == captureOwnerCalibration && strings.HasPrefix(current, "saynow:"), false
}

// handleSetCalibrationCapture persists the trial-audio capture toggle (rendered on
// the Calibration tab) and triggers a settings refresh so the tab re-renders. The
// calibration view needs command data the @post doesn't carry, so we refresh the
// active tab rather than returning HTML to patch (mirrors the calibrate buttons).
func (h *Host) handleSetCalibrationCapture(req *struct {
	Enabled bool `json:"enabled"`
}) error {
	if err := h.config.SetUserField("calibration_capture", req.Enabled); err != nil {
		branchkit.Logf("voice", "set calibration capture save error: %v", err)
	}
	_ = h.plugin.SettingsRefresh()
	return nil
}

// handleSetAutoAdvance persists the calibration auto-advance preference (a
// target heard correctly advances on its own, no "Next" click) and refreshes
// the Calibration tab so the toggle + walk controls re-render.
func (h *Host) handleSetAutoAdvance(req *struct {
	Enabled bool `json:"enabled"`
}) error {
	if err := h.config.SetUserField("calibration_auto_advance", req.Enabled); err != nil {
		branchkit.Logf("voice", "set auto-advance save error: %v", err)
	}
	_ = h.plugin.SettingsRefresh()
	return nil
}

// captureRetentionOrDefault returns the configured retention policy, defaulting to
// keep_bounded when unset (e.g. configs predating the field).
func (h *Host) captureRetentionOrDefault() string {
	if p := h.config.Get().CaptureRetention; p != "" {
		return p
	}
	return calibration.RetentionKeepBounded
}

// handleSetCaptureRetention persists the retention policy (how long captured audio
// is kept) and refreshes the Calibration tab.
func (h *Host) handleSetCaptureRetention(req *struct {
	Policy string `json:"policy"`
}) error {
	switch req.Policy {
	case calibration.RetentionDeleteAfter, calibration.RetentionKeepBounded, calibration.RetentionKeepAll:
	default:
		return fmt.Errorf("unknown retention policy %q", req.Policy)
	}
	if err := h.config.SetUserField("capture_retention", req.Policy); err != nil {
		branchkit.Logf("voice", "set capture retention save error: %v", err)
	}
	_ = h.plugin.SettingsRefresh()
	return nil
}

// handleSetClipDisposition stamps a clip's review verdict (""|confirmed|misspoke)
// via the platform and refreshes the Calibration tab. This is the human axis the
// machine can't derive — did the recording faithfully say the prompt? "confirmed"
// (said it right) marks a vetted example (a hard case when the clip is fragile);
// "misspoke" (said it wrong) excludes it from the fragility verdict + training.
// Whether the engine heard it right is already the matched flag + fragility verdict.
func (h *Host) handleSetClipDisposition(req *struct {
	File        string `json:"file"`
	Disposition string `json:"disposition"`
}) error {
	found, err := calibration.SetDisposition(req.File, req.Disposition)
	if err != nil {
		branchkit.Logf("voice", "set clip disposition error: %v", err)
		return err
	}
	if !found {
		return fmt.Errorf("clip %q is not in the corpus index — only labeled clips can be curated", req.File)
	}
	h.invalidateRecordingsCache()
	_ = h.plugin.SettingsRefresh()
	return nil
}

// handleClearCaptures wipes all captured audio + the corpus index (privacy action)
// and refreshes the Calibration tab. Fragility readouts are unaffected.
func (h *Host) handleClearCaptures(_ *struct{}) error {
	removed, err := calibration.ClearAll()
	if err != nil {
		branchkit.Logf("voice", "clear captured audio error: %v", err)
		return err
	}
	h.invalidateRecordingsCache()
	_ = h.plugin.SettingsRefresh()
	branchkit.Logf("voice", "captures cleared: %d removed", removed)
	return nil
}

// handleReprobeFragility re-runs the fragility ladder over already-kept clips and
// appends fresh readings to each clip's history — the manual re-probe trigger
// behind the Recordings UI. With a `target`, it re-probes only that target's clips
// (the group's button); empty re-probes the whole kept corpus (the "Re-probe all"
// button). The slow probe runs in the background; the Recordings list refreshes
// when it finishes. Re-measuring is only meaningful while the clips still exist, so
// this reports how many it queued (0 under delete-after retention, where nothing is
// kept) rather than gating on the policy.
func (h *Host) handleReprobeFragility(req *struct {
	Target string `json:"target"`
}) error {
	queued := h.calib.ReprobeCorpus(req.Target, func() {
		h.invalidateRecordingsCache()
		_ = h.plugin.SettingsRefresh()
	})
	branchkit.Logf("voice", "reprobing %q: queued=%v", req.Target, queued)
	return nil
}

// handleScoreBias runs the bias strength sweep over already-kept clips and
// appends a per-clip flip-threshold score to each clip's bias-score history — the
// manual "Score bias" trigger behind the Recordings UI. With a `target`, it scores
// only that target's clips; empty scores the whole kept corpus. The slow sweep runs
// in the background; the Recordings list refreshes when it finishes.
func (h *Host) handleScoreBias(req *struct {
	Target string `json:"target"`
}) error {
	queued := h.calib.ScoreBiasCorpus(req.Target, func() {
		h.invalidateRecordingsCache()
		_ = h.plugin.SettingsRefresh()
	})
	branchkit.Logf("voice", "scoring %q: queued=%v", req.Target, queued)
	return nil
}

// handleApplyBias applies the corpus-level strength recommendation to the
// platform's never-standalone bias setting via recognition.bias.apply, which
// stamps `calibration` provenance. The platform refuses over a manually-set
// value unless forced; the refusal re-renders the Apply button as an explicit
// "Replace manual setting" so the user consents before their hand-set value is
// overwritten.
func (h *Host) handleApplyBias(req *struct {
	Strength float64 `json:"strength"`
	Force    bool    `json:"force"`
}) error {
	resp, err := h.plugin.RecognitionBiasApply(branchkit.RecognitionBiasApplyRequest{Strength: req.Strength, Force: &req.Force})
	switch {
	case err != nil:
		h.setBiasApplyStatus("Apply failed: "+err.Error(), false)
	case !resp.Applied:
		h.setBiasApplyStatus("The bias strength is currently set by hand — replace it?", true)
	default:
		h.setBiasApplyStatus(fmt.Sprintf("Applied strength %g — set by calibration.", req.Strength), false)
	}
	_ = h.plugin.SettingsRefresh()
	return nil
}

// handleSetBias is the MANUAL half of the bias control (the toggle + slider
// on the Recordings tab; moved from the platform commands page 2026-09-11).
// It calls the platform's recognition.bias.set, which stamps `manual`
// provenance and pushes the new weights to the recognizer immediately.
func (h *Host) handleSetBias(req *struct {
	Enabled  *bool    `json:"enabled"`
	Strength *float64 `json:"strength"`
}) error {
	if _, err := h.plugin.RecognitionBiasSet(branchkit.RecognitionBiasSetRequest{Enabled: req.Enabled, Strength: req.Strength}); err != nil {
		h.setBiasApplyStatus("Set failed: "+err.Error(), false)
	} else {
		h.setBiasApplyStatus("", false)
	}
	_ = h.plugin.SettingsRefresh()
	return nil
}

// currentBias reads the live bias setting for rendering the manual control.
// Zero-value on error: the control renders "off / default", and the first
// interaction round-trips the truth.
func (h *Host) currentBias() (enabled bool, strength float64, provenance string) {
	resp, err := h.plugin.RecognitionBiasGet()
	if err != nil {
		return false, 0, "default"
	}
	return resp.Enabled, resp.Strength, resp.Provenance
}

// handleDeleteClip removes one kept recording (the WAV + its corpus row) and
// refreshes the Calibration tab. The per-clip delete users reach for surgical
// curation — drop a junk take without clearing the whole library.
func (h *Host) handleDeleteClip(req *struct {
	File string `json:"file"`
}) error {
	if req.File == "" {
		return fmt.Errorf("file is required")
	}
	h.calib.DeleteClip(req.File)
	h.invalidateRecordingsCache()
	_ = h.plugin.SettingsRefresh()
	return nil
}

// handleDeleteTarget removes every kept recording for one target (the per-group
// bulk delete) and refreshes the Calibration tab.
func (h *Host) handleDeleteTarget(req *struct {
	Target string `json:"target"`
}) error {
	if req.Target == "" {
		return fmt.Errorf("target is required")
	}
	n := h.calib.DeleteTarget(req.Target)
	h.invalidateRecordingsCache()
	_ = h.plugin.SettingsRefresh()
	branchkit.Logf("voice", "deleted target %q: %d clip(s)", req.Target, n)
	return nil
}

// handleSetRecordingsView switches the recordings list between the grouped-by-
// command view and the flat newest-first ("most recent") view, then refreshes the
// Calibration tab. The cache holds both shapes, so this only re-renders.
func (h *Host) handleSetRecordingsView(req *struct {
	View string `json:"view"`
}) error {
	h.setRecordingsView(req.View)
	_ = h.plugin.SettingsRefresh()
	return nil
}

// handleExportClips bundles the kept recordings (labeled, non-misspoke) + a
// NeMo-format manifest into a tar.gz and hands it to the user in Downloads, so
// they can share it to improve the model. Writes a local file only — nothing
// leaves the device until the user sends it. The result/error is shown as a
// status line under the recordings controls.
//
// The archive is built HERE, in this plugin's own data dir, because the clips
// and the manifest format are this plugin's. Only the last step is the
// platform's: `plugin.data.export` copies one file out to Downloads and reveals
// it, which is the one thing the sandbox denies a plugin on purpose — egress to
// the user's filesystem. The platform used to build the tar itself, which meant
// it also had to know the corpus schema and the training-manifest layout.
func (h *Host) handleExportClips(req *struct {
	ConfirmedOnly bool `json:"confirmed_only"`
}) error {
	name, count, err := calibration.BuildExportArchive(req.ConfirmedOnly)
	if err != nil {
		h.setExportStatus("Export failed: " + err.Error())
		_ = h.plugin.SettingsRefresh()
		return nil
	}

	resp, err := h.plugin.PluginDataExport(branchkit.PluginDataExportRequest{Path: name})
	if err != nil {
		h.setExportStatus("Export failed: " + err.Error())
	} else {
		noun := "clips"
		if count == 1 {
			noun = "clip"
		}
		h.setExportStatus(fmt.Sprintf("Exported %d %s → %s", count, noun, resp.Path))
	}
	// The staged copy has served its purpose once it is in Downloads; leaving it
	// would silently double every export's disk cost inside our own data dir.
	calibration.RemoveExportArchive(name)
	_ = h.plugin.SettingsRefresh()
	return nil
}

// The command_recognition pipeline record is seeded authoritatively by
// plugin.json (stage voice.sherpa_commands, model sherpa-offline-nemo,
// lifecycle_mode continuous, min-silence 0.10 — the silero VAD end-of-speech
// hangover tuned for ~100ms word-end→emit latency, dynamic-grammar on,
// cancel_delay_ms 800 to drain the Sherpa end-of-hold flush). The plugin no
// longer reconstructs the record at startup — that rewrite existed for engine
// selection (Vosk vs Sherpa) and its boot race against the platform's collection
// seeding was the "record drift" failure (2026-07-02). With one engine and an
// authoritative seed, startup only pre-warms; capture toggles a single param.

// Command-recognition prewarm is now part of the unified boot warmup —
// see startVoiceWarmup in readiness.go (warms every engine + drives the
// per-engine readiness HUD).

// patchCommandCaptureDir sets (or, when empty, clears) the `dump-segments` param
// on the seeded command_recognition record's Sherpa stage, then re-warms so the
// change takes effect. This is the ONLY record mutation the plugin makes, and it
// runs from the calibration path long after boot — no seed race, no retry loop.
func (h *Host) patchCommandCaptureDir(dir string) bool {
	rec, err := h.plugin.Get("_platform.pipelines", "command_recognition")
	if err != nil || rec == nil {
		branchkit.Logf("voice", "capture: command_recognition record unavailable (%v)", err)
		return false
	}
	var record map[string]any
	if err := json.Unmarshal(rec.Payload, &record); err != nil {
		branchkit.Logf("voice", "capture: parse record: %v", err)
		return false
	}
	stages, _ := record["stages"].([]any)
	for _, s := range stages {
		sm, ok := s.(map[string]any)
		if !ok || sm["stage"] != "voice.sherpa_commands" {
			continue
		}
		params, ok := sm["params"].(map[string]any)
		if !ok {
			params = map[string]any{}
			sm["params"] = params
		}
		if dir != "" {
			params["dump-segments"] = dir
		} else {
			delete(params, "dump-segments")
		}
	}
	if err := h.plugin.Put("_platform.pipelines", "command_recognition", record); err != nil {
		branchkit.Logf("voice", "capture: put record: %v", err)
		return false
	}
	h.rewarmPipeline("command_recognition")
	return true
}
