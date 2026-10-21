package main

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/branchkit/plugin-sdk-go"

	"github.com/branchkit/branchkit-plugin-voice/views"
)

// The microphone BranchKit listens through.
//
// The choice is a param of the `_platform.mic` stage in both of voice's
// pipelines, dictation and command recognition: `device` (the platform's
// device id) and `device-name`. The platform passes stage params to the mic
// on every OS: the macOS shell selects the device by its CoreAudio UID, the
// mic-source stage sets PULSE_SOURCE on Linux and matches the name
// elsewhere. A device that is not connected falls back to the system
// default until it returns.

// micPipelines are the pipelines whose `_platform.mic` stage follows the
// choice.
var micPipelines = []string{"dictation", dictationSamplePipeline, "command_recognition"}

// applyInputDevice writes the configured microphone onto the mic stage of
// voice's pipelines, so the next run of each captures from it. Run at boot
// (the platform re-seeds the records from plugin.json, which carries no
// device) and on every change. A change also moves a live always-listening
// command session over now, rather than at its next restart; a hold or a
// dictation starts a fresh run and picks the device up anyway.
func (h *Host) applyInputDevice(moveLive bool) {
	cfg := h.config.Get()
	for _, name := range micPipelines {
		rec, err := h.plugin.Get("_platform.pipelines", name)
		if err != nil || rec == nil {
			branchkit.Logf("voice", "microphone: pipeline %s unavailable (%v)", name, err)
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(rec.Payload, &record); err != nil {
			continue
		}
		if !setMicStageDevice(record, cfg.InputDevice, cfg.InputDeviceName) {
			continue
		}
		if err := h.plugin.Put("_platform.pipelines", name, record); err != nil {
			branchkit.Logf("voice", "microphone: put %s: %v", name, err)
		}
	}
	// Only the persistent run: a dictation or a command hold that set it
	// aside brings it back on the record just written.
	if moveLive && h.cmdRecWanted() {
		// Stop, then run: the platform starts the new run once the old one
		// has drained, so no audio is lost between them.
		if _, err := h.plugin.PipelinesStop(branchkit.PipelinesStopRequest{Name: "command_recognition"}); err != nil {
			branchkit.Logf("voice", "microphone: stop command_recognition: %v", err)
		}
		h.runCommandRecognition(false)
	}
}

// setMicStageDevice sets (or, for an empty id, clears) the device params on
// a pipeline record's `_platform.mic` stage. Reports whether anything changed.
func setMicStageDevice(record map[string]any, uid, name string) bool {
	stages, _ := record["stages"].([]any)
	changed := false
	for _, s := range stages {
		sm, ok := s.(map[string]any)
		if !ok || sm["stage"] != "_platform.mic" {
			continue
		}
		params, ok := sm["params"].(map[string]any)
		if !ok {
			params = map[string]any{}
		}
		before, _ := json.Marshal(params)
		if uid == "" {
			delete(params, "device")
			delete(params, "device-name")
		} else {
			params["device"] = uid
			params["device-name"] = name
		}
		if len(params) == 0 {
			delete(sm, "params")
		} else {
			sm["params"] = params
		}
		after, _ := json.Marshal(params)
		if string(before) != string(after) {
			changed = true
		}
	}
	return changed
}

// handleMicrophoneSet is the Dictation tab's Microphone menu: store the
// choice and move the pipelines to it.
func (h *Host) handleMicrophoneSet(req *struct {
	UID string `json:"uid"`
}) error {
	name := ""
	if req.UID != "" {
		mics, _, _ := h.micMenu("")
		for _, m := range mics {
			if m.UID == req.UID {
				name = m.Name
			}
		}
	}
	raw := map[string]json.RawMessage{}
	raw["input_device"], _ = json.Marshal(req.UID)
	raw["input_device_name"], _ = json.Marshal(name)
	if err := h.config.HandleUpdateConfig(&raw); err != nil {
		return err
	}
	branchkit.Logf("voice", "microphone: %q (%s)", name, req.UID)
	go h.applyInputDevice(true)
	return nil
}

// micMenuView is the Get started tab's Microphone menu.
func (h *Host) micMenuView(uid, name string) views.MicMenuView {
	mics, defaultName, connected := h.micMenu(uid)
	return views.MicMenuView{Mics: mics, UID: uid, Name: name, Connected: connected, DefaultName: defaultName}
}

// micLabel names the microphone in use, for the Dictation tab's pointer.
func micLabel(m views.MicMenuView) string {
	switch {
	case m.UID == "" && m.DefaultName != "":
		return m.DefaultName + " (system default)"
	case m.UID == "":
		return "system default"
	case !m.Connected:
		return m.Name + " (not connected; using the system default)"
	default:
		return m.Name
	}
}

// micMenu builds the Microphone menu: the connected input devices, the
// system default's name, and whether the chosen device is connected.
func (h *Host) micMenu(chosen string) (mics []views.MicOption, defaultName string, connected bool) {
	devices, err := h.plugin.NativeAudioDevices()
	if err != nil {
		return nil, "", false
	}
	for _, d := range devices {
		if !d.IsInput || d.UID == "" || strings.HasPrefix(d.UID, "CADefaultDeviceAggregate") {
			continue
		}
		mics = append(mics, views.MicOption{UID: d.UID, Name: d.Name})
		if d.IsDefaultInput {
			defaultName = d.Name
		}
		if d.UID == chosen {
			connected = true
		}
	}
	sort.Slice(mics, func(i, j int) bool { return strings.ToLower(mics[i].Name) < strings.ToLower(mics[j].Name) })
	return mics, defaultName, connected
}
