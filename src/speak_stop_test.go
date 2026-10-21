package main

import (
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/speech"
)

// A speak stop phrase split across two command transcripts by the VAD
// ("SPEAK" | "OVER" at the 0.10 s min-silence) must still stop speak
// dictation, the same as when both words land in one transcript.

// speakingHost opens a speak session the way `speak start` does, with the
// command-recognition transcripts routed to the speak-stop detector.
func speakingHost(t *testing.T) *Host {
	t.Helper()
	h := newTestHost()
	resetState(h)
	postActionWithPhase(h, t, "voice.speak", "start")
	h.setVoiceMode("dictation")
	if !h.sess.GetSpeakDictating() {
		t.Fatal("setup: expected a live speak session")
	}
	return h
}

func TestSpeakStopPhraseSplitAcrossTranscriptsStops(t *testing.T) {
	h := speakingHost(t)
	h.handleCommandTranscript("hello speak", false, nil, []uint64{100, 600}, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("half a stop phrase must not stop dictation")
	}
	h.handleCommandTranscript("over", false, nil, []uint64{1050}, nil, nil)
	if h.sess.GetSpeakDictating() {
		t.Fatal(`"speak" | "over" split across two transcripts must stop speak dictation`)
	}
}

func TestSpeakCancelPhraseSplitAcrossTranscriptsCancels(t *testing.T) {
	h := speakingHost(t)
	h.handleCommandTranscript("speak", false, nil, []uint64{600}, nil, nil)
	h.handleCommandTranscript("cancel", false, nil, []uint64{1000}, nil, nil)
	if h.sess.GetSpeakDictating() {
		t.Fatal(`"speak" | "cancel" split across two transcripts must cancel speak dictation`)
	}
}

// Only the previous transcript's LAST word joins this transcript's FIRST
// word: the pair must still be adjacent.
func TestSpeakStopSplitNeedsAdjacentWords(t *testing.T) {
	h := speakingHost(t)
	h.handleCommandTranscript("speak", false, nil, []uint64{600}, nil, nil)
	h.handleCommandTranscript("the over", false, nil, []uint64{900, 1200}, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("non-adjacent words across a split must not stop dictation")
	}

	h = speakingHost(t)
	h.handleCommandTranscript("speak the", false, nil, []uint64{600, 900}, nil, nil)
	h.handleCommandTranscript("over", false, nil, []uint64{1200}, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("a word that is not the previous transcript's last must not pair across a split")
	}
}

// A split pair counts only when the second word starts soon after the
// first: "...to speak." <long pause> "Over the weekend..." is dictation.
func TestSpeakStopSplitTooFarApartDoesNotStop(t *testing.T) {
	h := speakingHost(t)
	h.handleCommandTranscript("speak", false, nil, []uint64{600}, nil, nil)
	h.handleCommandTranscript("over the weekend", false, nil, []uint64{600 + speakStopSplitMaxMs + 1, 9000, 9300}, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("words far apart in time must not pair across a split")
	}
}

// Without onsets on both sides there is no evidence the words were adjacent
// in time, so a split pair does not count (a missed stop is recoverable; a
// false stop truncates dictation).
func TestSpeakStopSplitWithoutOnsetsDoesNotStop(t *testing.T) {
	h := speakingHost(t)
	h.handleCommandTranscript("speak", false, nil, nil, nil, nil)
	h.handleCommandTranscript("over", false, nil, []uint64{900}, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("a split pair without the first word's onset must not stop dictation")
	}

	h = speakingHost(t)
	h.handleCommandTranscript("speak", false, nil, []uint64{600}, nil, nil)
	h.handleCommandTranscript("over", false, nil, nil, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("a split pair without the second word's onset must not stop dictation")
	}
}

// The empty is_final marker is the end of the recognition audio; a word
// before it never pairs with one after it.
func TestSpeakStopCarryClearedByEndMarker(t *testing.T) {
	h := speakingHost(t)
	h.handleCommandTranscript("speak", false, nil, []uint64{600}, nil, nil)
	h.handleCommandTranscript("", true, nil, nil, nil, nil)
	h.handleCommandTranscript("over", false, nil, []uint64{900}, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("a word before the end marker must not pair with one after it")
	}
}

// A new speak session starts with nothing carried from the last one.
func TestSpeakStopCarryClearedByNewSession(t *testing.T) {
	h := speakingHost(t)
	h.handleCommandTranscript("speak", false, nil, []uint64{600}, nil, nil)
	postActionWithPhase(h, t, "voice.speak", "stop")
	postActionWithPhase(h, t, "voice.speak", "start")
	h.setVoiceMode("dictation")
	h.handleCommandTranscript("over", false, nil, []uint64{900}, nil, nil)
	if !h.sess.GetSpeakDictating() {
		t.Fatal("a word from the previous speak session must not pair with this one")
	}
}

func TestSpeakPhraseSpan(t *testing.T) {
	prev := &speakWord{word: "speak", onset: 600, hasOnset: true}
	cases := []struct {
		name      string
		prev      *speakWord
		words     []string
		onsets    []uint64
		wantStart int
		wantFound bool
	}{
		{"within one transcript", nil, []string{"speak", "over"}, []uint64{100, 400}, 0, true},
		{"within one transcript, carry ignored", prev, []string{"a", "speak", "over"}, []uint64{900, 1000, 1300}, 1, true},
		{"across the split", prev, []string{"over", "and"}, []uint64{1000, 1300}, -1, true},
		{"across, at the limit", prev, []string{"over"}, []uint64{600 + speakStopSplitMaxMs}, -1, true},
		{"across, past the limit", prev, []string{"over"}, []uint64{600 + speakStopSplitMaxMs + 1}, 0, false},
		{"across, onset not after the carried word", prev, []string{"over"}, []uint64{600}, 0, false},
		{"across, misaligned onsets", prev, []string{"over", "x"}, []uint64{1000}, 0, false},
		{"no carry", nil, []string{"over"}, []uint64{1000}, 0, false},
		{"punctuation tolerated", &speakWord{word: "speak.", onset: 600, hasOnset: true}, []string{"over."}, []uint64{1000}, -1, true},
	}
	for _, tc := range cases {
		start, found := speakPhraseSpan(tc.prev, tc.words, tc.onsets, speech.StopPhrases)
		if found != tc.wantFound || (found && start != tc.wantStart) {
			t.Errorf("%s: speakPhraseSpan = (%d, %v), want (%d, %v)", tc.name, start, found, tc.wantStart, tc.wantFound)
		}
	}
}

// A split pair cuts the audio at the CARRIED word's onset: that is where the
// phrase begins.
func TestSpeakStopSplitCutoff(t *testing.T) {
	prev := &speakWord{word: "speak", onset: 600, hasOnset: true}
	got := speakPhraseCutoffMs(prev, []uint64{1000}, -1)
	if got == nil || *got != 600-speakStopCutoffPadMs {
		t.Fatalf("split cutoff = %v, want %d", got, 600-speakStopCutoffPadMs)
	}
	got = speakPhraseCutoffMs(prev, []uint64{1000, 2000}, 1)
	if got == nil || *got != 2000-speakStopCutoffPadMs {
		t.Fatalf("in-transcript cutoff = %v, want %d", got, 2000-speakStopCutoffPadMs)
	}
}
