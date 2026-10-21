package main

import "testing"

func TestChatProfileDropsOnlyTheTrailingPeriod(t *testing.T) {
	cases := map[string]string{
		"On my way.":                  "On my way",
		"Sure, sounds good. See you.": "Sure, sounds good. See you",
		"Wait... really?":             "Wait... really?",
		"see you at 5 p.m.":           "see you at 5 p.m",
		"no period here":              "no period here",
	}
	for in, want := range cases {
		if got := applyDictationProfile(in, profileChat); got != want {
			t.Errorf("chat(%q) = %q, want %q", in, got, want)
		}
	}
}

// Capitalisation belongs to the insert step (insertion.go), which can see the
// caret's surroundings. A profile must never touch it.
func TestChatProfileLeavesCapitalisationAlone(t *testing.T) {
	if got := applyDictationProfile("Hello there.", profileChat); got != "Hello there" {
		t.Fatalf("got %q", got)
	}
	if got := applyDictationProfile("hello there.", profileChat); got != "hello there" {
		t.Fatalf("got %q", got)
	}
}
