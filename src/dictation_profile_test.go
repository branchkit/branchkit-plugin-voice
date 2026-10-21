package main

import "testing"

// Dictation profiles — the platform names what an utterance IS, this plugin
// decides what that means.
//
// Lived in dictated_arg_test.go until the dictated-argument arm was retired
// (2026-07-26). The two features were never related beyond both touching a
// dictation transcript: profiles shape text by declared intent, the arm held a
// command back waiting for one.
func TestApplyDictationProfile(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		profile string
		want    string
	}{
		{"query drops the trailing period WhisperKit adds to a single word",
			"Gmail.", profileQuery, "Gmail"},
		{"query drops prosody commas",
			"red, green and blue.", profileQuery, "red green and blue"},
		{"query keeps an intra-word dot — a dictated address must survive to the palette's URL parser",
			"Wikipedia.com.", profileQuery, "Wikipedia.com"},
		{"query keeps intra-word hyphens and dots together",
			"t-mobile.com", profileQuery, "t-mobile.com"},
		{"query keeps a dotted name that is not a URL — the consumer decides what it means",
			"node.js", profileQuery, "node.js"},
		{"query still strips a space-adjacent dot — prosody, not structure",
			"So. Many. Words.", profileQuery, "So Many Words"},
		{"prose is untouched — punctuation is the point when you're writing",
			"Bleed American was released.", profileProse, "Bleed American was released."},
		{"an unstamped transcript is prose",
			"Hello, world.", "", "Hello, world."},
		{"an unknown profile falls back to prose rather than guessing",
			"Hello, world.", "custom:shouty", "Hello, world."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyDictationProfile(tc.text, tc.profile); got != tc.want {
				t.Errorf("applyDictationProfile(%q, %q) = %q, want %q",
					tc.text, tc.profile, got, tc.want)
			}
		})
	}
}
