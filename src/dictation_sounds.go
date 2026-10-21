package main

import (
	"runtime"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Start and stop sounds for dictation, off by default.
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
	if !h.config.Get().DictationSounds || h.plugin == nil {
		return
	}
	go func() {
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
