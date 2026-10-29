package settings

import (
	"encoding/json"
	"testing"
)

// The setting was a switch: a stored true or false still reads as on or
// off, and anything else is the default.
func TestSoundsModeReadsTheOldSwitch(t *testing.T) {
	for in, want := range map[string]SoundsMode{
		`true`:    SoundsOn,
		`false`:   SoundsOff,
		`"on"`:    SoundsOn,
		`"off"`:   SoundsOff,
		`"auto"`:  SoundsAuto,
		`"other"`: SoundsAuto,
	} {
		var c VoiceConfig
		if err := json.Unmarshal([]byte(`{"dictation_sounds": `+in+`}`), &c); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if c.DictationSounds != want {
			t.Errorf("%s: got %q, want %q", in, c.DictationSounds, want)
		}
	}
}
