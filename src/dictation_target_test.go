package main

import "testing"

// The app focused at the stop is the target; a transcript arriving with focus
// elsewhere has moved, one arriving in the same app has not, and with nothing
// to compare (either stamp missing) the text is typed as before.
func TestDictationFocusMovedComparesStopAppToNow(t *testing.T) {
	for _, tt := range []struct {
		target, now string
		moved       bool
	}{
		{"com.apple.mail", "com.apple.mail", false},
		{"com.apple.mail", "com.tinyspeck.slackmacgap", true},
		{"", "com.tinyspeck.slackmacgap", false},
		{"com.apple.mail", "", false},
	} {
		if got := dictationFocusMoved(tt.target, tt.now); got != tt.moved {
			t.Errorf("dictationFocusMoved(%q, %q) = %v, want %v", tt.target, tt.now, got, tt.moved)
		}
	}
}
