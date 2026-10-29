package main

import (
	"encoding/json"
	"errors"
	"fmt"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

func (h *Host) getCommandFilter() string {
	h.commandFilterMu.Lock()
	defer h.commandFilterMu.Unlock()
	return h.commandFilter
}

// The Command Editor's selected command (catalog + inspector):
// clicking a catalog row selects it; the
// inspector shows its reading, test, and the door into the editor.
func (h *Host) getSelectedRule() string {
	h.selectedRuleMu.Lock()
	defer h.selectedRuleMu.Unlock()
	return h.selectedRule
}

type ruleSelectRequest struct {
	Canonical string `json:"canonical"`
}

func (h *Host) handleRuleSelect(req *ruleSelectRequest) error {
	h.selectedRuleMu.Lock()
	h.selectedRule = req.Canonical
	h.selectedRuleMu.Unlock()
	// The inspector shows ONE thing. Selecting a different command while the
	// editor is open closes the editor (the same as Cancel), instead of
	// leaving the edited row and the selected row both highlighted with the
	// form still showing the first. Clicking the row being edited keeps it.
	h.edState.mu.Lock()
	if h.edState.editor.Open && h.edState.editor.EditCanonical != req.Canonical {
		h.edState.editor.Open = false
		h.edState.editor.Recording = false
		h.edState.editor.SaveError = ""
	}
	h.edState.mu.Unlock()
	return nil
}

// --- Group A: Models (choose one, download one) ---

type modelSetRequest struct {
	Name string `json:"name"`
}

// handleModelSet records the chosen dictation model in THIS plugin's settings,
// which is where the pipeline reads it (`dictationRunParams`).
//
// It used to only send `set_model:` to the Swift shell, which stored the name in
// its own UserDefaults and reported it back through a platform collection for
// this tab to render. That made the picker cosmetic: the selection round-tripped
// through two other processes and never reached the setting that decides which
// model the stt stage actually loads.
func (h *Host) handleModelSet(req *modelSetRequest) error {
	if err := h.config.SetUserField("model", req.Name); err != nil {
		return err
	}
	// A different size may not be downloaded yet.
	go h.refreshSetup()
	return nil
}

type commandModelSetRequest struct {
	Name string `json:"name"`
}

// handleCommandModelSet records the chosen command model and moves the
// command stage onto it. The stage loads the model a run names
// (commandRecognitionRunParams), so a hold started after this uses it; a
// continuous listening run already going is restarted on it here, and the
// pool is warmed with it so the next hold does not pay a cold start. The
// name is applied as given, not re-read: the settings mirror may not have
// seen the write yet.
func (h *Host) handleCommandModelSet(req *commandModelSetRequest) error {
	if !isCommandModel(req.Name) {
		return fmt.Errorf("%q is not a command model", req.Name)
	}
	if err := h.config.SetUserField("command_model", req.Name); err != nil {
		return err
	}
	if modelOnDisk(req.Name) && h.claimCommandModel(req.Name, true) {
		h.moveCommandStage(req.Name)
	}
	go h.refreshSetup()
	return nil
}

// handleModelPick is a dictation row picked on Get started: the model becomes
// the chosen one and, when it is not on this computer yet, its download
// starts, in one press. Only a dictation model this OS can load is taken:
// the setting is what the dictation pipeline loads.
func (h *Host) handleModelPick(req *modelSetRequest) error {
	if !isDictationModel(req.Name) {
		return fmt.Errorf("%q is not a dictation model", req.Name)
	}
	if err := h.handleModelSet(req); err != nil {
		return err
	}
	return h.downloadIfMissing(req.Name)
}

// handleCommandModelPick is a command row picked on Get started: chosen as by
// handleCommandModelSet (the stage stays on a model it has until the chosen
// one is on this computer), and downloaded when it is not yet.
func (h *Host) handleCommandModelPick(req *commandModelSetRequest) error {
	if err := h.handleCommandModelSet(req); err != nil {
		return err
	}
	return h.downloadIfMissing(req.Name)
}

// downloadIfMissing asks the platform for one of this plugin's models unless
// it is on this computer already or is the download running now: a second
// request for it would wait in the platform's queue and run again for
// nothing.
func (h *Host) downloadIfMissing(name string) error {
	if modelOnDisk(name) {
		return nil
	}
	ref := h.modelRef(name)
	if !needsDownload(ref, h.readModelDownload()) {
		return nil
	}
	return h.plugin.ArtifactDownload(branchkit.ArtifactDownloadRequest{Ref: ref})
}

// needsDownload is downloadIfMissing's decision for a model not on this
// computer: it is asked for unless the platform's record shows it running.
func needsDownload(ref string, running ModelDownloadData) bool {
	return !(running.Downloading && running.Model == ref)
}

// isDictationModel reports whether name is a dictation model this OS can load.
func isDictationModel(name string) bool {
	for _, m := range dictationCatalog() {
		if m.Name == name {
			return true
		}
	}
	return false
}

// syncCommandModel moves the command stage when the model it should load has
// changed since it was last put on one: the settings arrived after the boot
// warm-up used the default, they changed, or a chosen model's download
// finished.
func (h *Host) syncCommandModel() {
	model := h.commandModelName()
	if h.claimCommandModel(model, false) {
		h.moveCommandStage(model)
	}
}

// claimCommandModel decides, and records in the same step, whether the stage
// moves to want. Deciding and recording under one lock is what keeps a Use
// click from moving it twice: the settings write fires the change hook
// (syncCommandModel) while the click's own handler is still running, and
// whichever claims first moves the stage; the other finds it done.
func (h *Host) claimCommandModel(want string, explicit bool) bool {
	h.cmdModelMu.Lock()
	defer h.cmdModelMu.Unlock()
	applied, move := claimCommandModelState(h.cmdModelApplied, want, explicit)
	h.cmdModelApplied = applied
	return move
}

// claimCommandModelState is claimCommandModel's decision: the model now
// recorded as applied, and whether the stage moves. Nothing moves to the model
// already applied. Before anything was applied ("") only an explicit choice
// moves the stage; a settings refresh leaves it to the warm-up, which puts the
// stage on the right model itself.
func claimCommandModelState(applied, want string, explicit bool) (string, bool) {
	if applied == want || (applied == "" && !explicit) {
		return applied, false
	}
	return want, true
}

// noteCommandModel records the command model the stage was put on.
func (h *Host) noteCommandModel(model string) {
	h.cmdModelMu.Lock()
	h.cmdModelApplied = model
	h.cmdModelMu.Unlock()
}

// moveCommandStage puts the command stage on model, once claimCommandModel
// has decided it: a warm stage for the next hold, and continuous listening
// restarted on it when it is running.
func (h *Host) moveCommandStage(model string) {
	go func() {
		if _, err := h.plugin.PipelinesWarm(branchkit.PipelinesWarmRequest{
			Name:           "command_recognition",
			ParamOverrides: map[string]json.RawMessage{"model": mustJSON(model)},
		}); err != nil {
			branchkit.Logf("voice", "command model: warm %s failed: %v", model, err)
		}
	}()
	if h.cmdRecWanted() {
		h.pipelinesStop(branchkit.PipelinesStopRequest{Name: "command_recognition"})
		if err := h.runPipeline(commandRecognitionRunParams(model, false)); err != nil {
			branchkit.Logf("voice", "command model: restart on %s failed: %v", model, err)
			return
		}
		branchkit.Logf("voice", "command model: continuous listening restarted on %s", model)
	}
}

type modelDownloadRequest struct {
	Ref string `json:"ref"`
}

// The download itself is the platform's job (`artifact.download`): it runs
// `branchkit-cli artifact download` and reports progress on
// `_platform.model_download`. A refusal (not one of voice's models, no
// downloader in this install) is returned to the button rather than dropped,
// so the button never reports a download that is not happening.
func (h *Host) handleModelDownload(req *modelDownloadRequest) error {
	return h.plugin.ArtifactDownload(branchkit.ArtifactDownloadRequest{Ref: req.Ref})
}

// handleModelDownloadRecommended fetches the two models a fresh install needs:
// the default dictation model and the chosen command model.
//
// Both refs are derived from this plugin's own id, because both models are
// declared in this plugin's `provides.models`. The dictation ref was still
// spelled `whisperkit/…` from the era of a compiled-in CLI catalog, and that
// namespace stopped resolving when models moved under per-plugin roots — so
// this button, and every Download button in the models tab, had been failing
// with "Unknown model".
func (h *Host) handleModelDownloadRecommended(_ *struct{}) error {
	if err := h.plugin.ArtifactDownload(branchkit.ArtifactDownloadRequest{Ref: h.modelRef(resolveDictationModel(defaultDictationModel))}); err != nil {
		return err
	}
	return h.plugin.ArtifactDownload(branchkit.ArtifactDownloadRequest{Ref: h.modelRef(h.commandModelName())})
}

// --- Group B: Platform operation proxies ---

type commandsDeleteRequest struct {
	Canonical string `json:"canonical"`
}

func (h *Host) handleCommandsDelete(req *commandsDeleteRequest) error {
	return h.plugin.CommandsDelete(branchkit.CommandsDeleteRequest{Canonical: req.Canonical})
}

type commandsResetRequest struct {
	Canonical string `json:"canonical"`
}

func (h *Host) handleCommandsReset(req *commandsResetRequest) error {
	return h.plugin.CommandsReset(branchkit.CommandsResetRequest{Canonical: req.Canonical})
}

type modelDeleteRequest struct {
	Ref string `json:"ref"`
}

func (h *Host) handleModelDelete(req *modelDeleteRequest) error {
	err := h.plugin.ArtifactDelete(branchkit.ArtifactDeleteRequest{Ref: req.Ref})
	// Deleting the model in use undoes the Speech models step.
	go h.refreshSetup()
	return err
}

// --- Group C: Plugin-local state ---

type commandFilterRequest struct {
	CommandFilter string `json:"commandFilter"`
}

func (h *Host) handleSettingsCommandFilter(req *commandFilterRequest) error {
	h.commandFilterMu.Lock()
	h.commandFilter = req.CommandFilter
	h.commandFilterMu.Unlock()
	return nil
}

// errRefineModelMissing refuses turning clean-up on while its model is not
// downloaded: it would take no effect, and the page offers no switch then.
var errRefineModelMissing = errors.New("download the clean-up model first, on the Dictation tab")

// handleConfigUpdate relays a settings patch, refusing to turn clean-up on
// without its model. Turning it off, and every other field, passes through.
func (h *Host) handleConfigUpdate(req *map[string]json.RawMessage) error {
	if raw, ok := (*req)["refine_dictation"]; ok {
		var on bool
		if json.Unmarshal(raw, &on) == nil && on && !refineModelReady() {
			return errRefineModelMissing
		}
	}
	return h.config.HandleUpdateConfig(req)
}
