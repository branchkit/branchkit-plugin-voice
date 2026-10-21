package calibration

import (
	"reflect"
	"testing"
)

// bucketByPrompt splits labeled clips into the noise-ladder input (clips that
// decoded == their prompt) and the mishears (decode != prompt — the max-fragile
// clips). Bucketing is by PROMPT, so a mishear lands under the word it was meant
// to be rather than being dropped (the old decode-based grouping discarded them).
func TestBucketByPrompt(t *testing.T) {
	rows := []CorpusEntry{
		{File: "g/seg_00000.wav", Target: "COPY", Heard: "COPY", Matched: true},
		{File: "g/seg_00001.wav", Target: "BAT QUENCH", Heard: "BAT QUENCH", Matched: true},
		{File: "g/seg_00002.wav", Target: "BAT QUENCH", Heard: "BACK QUENCH", Matched: false}, // mishear
		{File: "g/seg_00003.wav", Target: "COPY", Heard: "COPY", Matched: true},
		{File: "", Target: "COPY", Heard: "COPY", Matched: true},            // no file, skipped
		{File: "g/seg_00005.wav", Target: "", Heard: "HUH", Matched: false}, // no prompt, skipped
	}

	laddered, mishears := bucketByPrompt(rows)

	wantLaddered := map[string][]string{
		"COPY":       {"g/seg_00000.wav", "g/seg_00003.wav"},
		"BAT QUENCH": {"g/seg_00001.wav"},
	}
	if !reflect.DeepEqual(laddered, wantLaddered) {
		t.Errorf("laddered:\n got  %v\n want %v", laddered, wantLaddered)
	}

	if len(mishears) != 1 || mishears[0].File != "g/seg_00002.wav" || mishears[0].Heard != "BACK QUENCH" {
		t.Errorf("mishears = %+v, want the one BACK QUENCH clip", mishears)
	}
}

func TestBucketByPrompt_AllMishears(t *testing.T) {
	rows := []CorpusEntry{
		{File: "g/0.wav", Target: "COPY", Heard: "NOPE", Matched: false},
	}
	laddered, mishears := bucketByPrompt(rows)
	if len(laddered) != 0 {
		t.Errorf("want no laddered clips, got %v", laddered)
	}
	if len(mishears) != 1 {
		t.Errorf("want 1 mishear, got %d", len(mishears))
	}
}

// An unresolved retention setting must never resolve to a DELETING policy.
//
// Regression guard for 2026-08-16, when a game finished ~19s after a plugin
// restart, the settings mirror was not Ready, GetVoiceConfig handed back the
// declared default (keep_bounded), and the finish path pruned a curated corpus
// to the newest clip per word — nine confirmed clips gone, unrecoverably.
//
// The rule under test: uncertainty resolves to keep_all. Keeping too much is a
// disk-space problem the user can fix; deleting too much cannot be undone.
func TestRetentionPolicyNeverDeletesWhenUnresolved(t *testing.T) {
	cases := []struct {
		name string
		cb   func() (string, bool)
	}{
		{"unwired", nil},
		{"not resolved yet (mirror pre-Ready)", func() (string, bool) { return RetentionKeepBounded, false }},
		{"resolved but empty", func() (string, bool) { return "", true }},
		{"resolved to delete_after, unresolved flag", func() (string, bool) { return RetentionDeleteAfter, false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &State{RetentionPolicy: tc.cb}
			if got := s.retentionPolicy(); got != RetentionKeepAll {
				t.Fatalf("retentionPolicy() = %q, want %q — an uncertain policy must not delete",
					got, RetentionKeepAll)
			}
		})
	}
}

// A policy the user actually chose is honored, including the deleting ones.
// The fix above must not turn retention into a no-op.
func TestRetentionPolicyHonorsAResolvedChoice(t *testing.T) {
	for _, want := range []string{RetentionKeepAll, RetentionKeepBounded, RetentionDeleteAfter} {
		s := &State{RetentionPolicy: func() (string, bool) { return want, true }}
		if got := s.retentionPolicy(); got != want {
			t.Fatalf("retentionPolicy() = %q, want %q", got, want)
		}
	}
}
