package main

import (
	"errors"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/settings"
)

// On auto the sounds play only where no recording indicator shows, and a
// probe that fails counts as none; on and off ignore the probe.
func TestDictationSoundsFollowTheIndicatorOnAuto(t *testing.T) {
	for _, c := range []struct {
		mode      settings.SoundsMode
		available bool
		err       error
		want      bool
	}{
		{settings.SoundsAuto, false, nil, true},
		{settings.SoundsAuto, true, nil, false},
		{settings.SoundsAuto, true, errors.New("probe failed"), true},
		{"", false, nil, true},
		{"", true, nil, false},
		{settings.SoundsOn, true, nil, true},
		{settings.SoundsOff, false, nil, false},
	} {
		h := &Host{statusIndicatorAvailable: func() (bool, error) { return c.available, c.err }}
		if got := h.dictationSoundsWanted(c.mode); got != c.want {
			t.Errorf("mode %q, indicator %v, err %v: got %v, want %v", c.mode, c.available, c.err, got, c.want)
		}
	}
}
