package main

import (
	"github.com/branchkit/plugin-sdk-go"
)

// Dictation types into the app that had focus when the hold ended, or not at
// all.
//
// The transcript arrives about a second after release, longer for a long
// dictation. Typing into whatever has focus then sent text to the wrong place
// whenever the user switched apps right after releasing, or a notification
// took focus: possibly a chat that sends on Return. So a transcript that lands
// somewhere else is kept instead: put on the clipboard, left in Recent
// dictations, and announced, so the user goes back and pastes it where they
// meant.
//
// The platform reports both apps on the transcript: `focused_app`, focused
// when the pipeline was asked to stop (the app the words were meant for, and
// the one the profile is chosen for), and `focused_app_now`, focused as the
// transcript is emitted. Voice used to keep its own key-up copy from a cache
// of focus events, a second reading of the same fact at a different moment.

// dictationFocusMoved reports whether focus moved between the end of the hold
// (target) and the transcript's arrival (now). Either missing means there is
// nothing to compare, and the text is typed as before.
func dictationFocusMoved(target, now string) bool {
	return target != "" && now != "" && target != now
}

// keepDictation is what happens instead of typing when focus moved: the text
// goes on the clipboard and a notice says where it was meant to go.
func (h *Host) keepDictation(text, target, focusedNow string) {
	branchkit.Logf("voice", "dictation: focus moved %s → %s; kept on the clipboard, not typed",
		appOrUnknown(target), appOrUnknown(focusedNow))
	clip := text
	if err := h.plugin.InputClipboardAction(branchkit.InputClipboardActionRequest{Action: "set", Text: &clip}); err != nil {
		branchkit.Logf("voice", "dictation: clipboard set failed: %v", err)
	}
	go func() {
		if err := h.plugin.SystemNotify(branchkit.SystemNotifyRequest{
			Title: "Dictation kept: focus moved",
			Body:  "It was for " + h.appDisplayName(target) + ". It's on your clipboard, ready to paste there.",
		}); err != nil {
			branchkit.Logf("voice", "dictation: focus-moved notice failed: %v", err)
		}
	}()
}
