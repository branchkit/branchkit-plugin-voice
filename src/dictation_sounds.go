package main

import (
	"runtime"
	"time"

	"github.com/branchkit/plugin-sdk-go"

	"github.com/branchkit/branchkit-plugin-voice/settings"
)

// Start and stop sounds for dictation. On "auto", the default, they play
// only where the platform reports no visible recording indicator, the same
// probe that gates continuous listening: there nothing else says the
// microphone is live.
//
// The stop sound waits out the pipeline's post-roll (stop_tail_ms, 200 ms in
// plugin.json): played at key-up it would land in the very audio being
// transcribed. The start sound plays as capture begins, so its tail does
// reach the recording; measured on 200 recorded dictations before this was
// built, see the issue.
const stopSoundDelay = 250 * time.Millisecond

// dictationSoundNames are each OS's own sounds for "listening" and "done":
// names the platform's play_sound resolves there.
func dictationSoundNames() (start, stop string) {
	switch runtime.GOOS {
	case "windows":
		return "Speech On", "Speech Off"
	case "linux":
		return "message", "complete"
	default:
		return "Tink", "Pop"
	}
}

func (h *Host) playDictationSound(name string) {
	mode := h.config.Get().DictationSounds
	if mode == settings.SoundsOff || h.plugin == nil {
		return
	}
	go func() {
		if !h.dictationSoundsWanted(mode) {
			return
		}
		if err := h.plugin.NativePlaySound(branchkit.NativePlaySoundRequest{Name: name}); err != nil {
			branchkit.Logf("voice", "dictation sound %q failed: %v", name, err)
		}
	}()
}

func (h *Host) dictationStartSound() {
	start, _ := dictationSoundNames()
	h.playDictationSound(start)
}

func (h *Host) dictationStopSound() {
	_, stop := dictationSoundNames()
	time.AfterFunc(stopSoundDelay, func() { h.playDictationSound(stop) })
}

// dictationSoundsWanted reports whether the sounds play under mode. On auto
// they play unless a recording indicator is known to be showing: a probe
// that fails counts as none, since the sound may be the only sign.
func (h *Host) dictationSoundsWanted(mode settings.SoundsMode) bool {
	switch mode {
	case settings.SoundsOn:
		return true
	case settings.SoundsOff:
		return false
	}
	available, err := h.statusIndicatorAvailable()
	return err != nil || !available
}

// soundsChoice is the setting as the Dictation tab's chooser shows it; an
// unset value is the default, auto.
func soundsChoice(mode settings.SoundsMode) string {
	if mode == settings.SoundsOn || mode == settings.SoundsOff {
		return string(mode)
	}
	return string(settings.SoundsAuto)
}
