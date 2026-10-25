package speech

import (
	"github.com/branchkit/plugin-sdk-go"
	"strings"
	"testing"
)

func TestStripSpeakPhrases(t *testing.T) {
	// Leading wake/start phrases, plus trailing stop phrases as a text-domain
	// BACKSTOP to the audio cutoff: the cut
	// usually removes the phrase audio so the trailing strip is a no-op, but it
	// catches the paths the cut can't cover (mis-heard/split phrase, nil-cutoff
	// escape, Linux whisper-stt).
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "strip leading speak",
			input: "speak hello world",
			want:  "hello world",
		},
		{
			name:  "strip leading Speak (case insensitive)",
			input: "Speak hello world",
			want:  "hello world",
		},
		{
			name:  "strip leading branch wake",
			input: "Branch wake hello world",
			want:  "hello world",
		},
		{
			name:  "strip leading start listening",
			input: "start listening hello world",
			want:  "hello world",
		},
		{
			name:  "no phrases to strip",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "trailing stop phrase stripped (backstop)",
			input: "hello world speak over",
			want:  "hello world",
		},
		{
			name:  "trailing stop phrase with punctuation",
			input: "hello world, speak over.",
			want:  "hello world",
		},
		{
			name:  "doubled trailing phrase cleared",
			input: "note this down speak over speak over",
			want:  "note this down",
		},
		{
			name:  "trailing 'over' alone is NOT a phrase pair (kept)",
			input: "the meeting is over",
			want:  "the meeting is over",
		},
		{
			name:  "branch stop variant stripped",
			input: "save the file branch stop",
			want:  "save the file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripSpeakPhrases(tt.input)
			if got != tt.want {
				t.Errorf("StripSpeakPhrases(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRepair(t *testing.T) {
	c := New(branchkit.NewDetachedPlugin(), func() bool { return true }, func() bool { return false })
	c.list = []Correction{
		{ID: "test1", MatchPattern: "helo", Replacement: "hello", IsRegex: false, Enabled: true},
	}
	for _, tt := range []struct {
		name, in string
		speak    bool
		want     string
	}{
		{"dictation does not strip speak phrases", "speak helo world", false, "speak hello world"},
		{"speak strips the leading phrase and corrects", "speak helo world", true, "hello world"},
		{"speak with only a correction", "helo", true, "hello"},
		{"all caps fixed after corrections", "HELLO THERE", false, "Hello there"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.Repair(tt.in, tt.speak); got != tt.want {
				t.Errorf("Repair(%q, %v) = %q, want %q", tt.in, tt.speak, got, tt.want)
			}
		})
	}
}

// Spoken punctuation runs only when its setting is on, and after the user's
// corrections, so a rule can claim a phrase holding a mark word first.
func TestRepairSpokenPunctuation(t *testing.T) {
	on := false
	c := New(branchkit.NewDetachedPlugin(), func() bool { return true }, func() bool { return on })
	c.list = []Correction{
		{ID: "test1", MatchPattern: "comma separated", Replacement: "comma-separated", Enabled: true},
	}
	if got := c.Repair("hi comma there", false); got != "hi comma there" {
		t.Errorf("setting off: Repair = %q, want the text unchanged", got)
	}
	on = true
	if got := c.Repair("hi comma there", false); got != "hi, there" {
		t.Errorf("setting on: Repair = %q, want %q", got, "hi, there")
	}
	if got := c.Repair("a comma separated list", false); got != "a comma-separated list" {
		t.Errorf("a correction runs first: Repair = %q, want %q", got, "a comma-separated list")
	}
}

// The two behaviours that moved out of the WhisperKit stage. Both used to run
// in Swift with no surface to see or change them; these tests are the record
// that they still happen, and where.

func TestHallucinationFilterBlanksStockPhrasesOnly(t *testing.T) {
	corr := DefaultCorrections()
	blanked := []string{
		"Thank you.", "thanks for watching", "You", "so.", "Bye.", "...",
		"  The End.  ",
		// Punctuation alone, what noise can decode to.
		".", " . ", "?", "!", ",", "-", "…", ". . .", "?!", "\n.\n",
	}
	for _, in := range blanked {
		if got := ApplyCorrections(in, corr); strings.TrimSpace(got) != "" {
			t.Errorf("ApplyCorrections(%q) = %q, want blank", in, got)
		}
	}
	// The whole point of anchoring: the same words inside real speech survive.
	kept := []string{
		"thank you for the coffee",
		"so I opened the file",
		"say bye to the old layout",
		"you should see the end of the list",
		"Done.",
		"... and then",
		"3.",
		"$",
	}
	for _, in := range kept {
		if got := ApplyCorrections(in, corr); got != in {
			t.Errorf("ApplyCorrections(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestFixAllCaps(t *testing.T) {
	cases := map[string]string{
		"OPEN THE FILE":       "Open the file",
		"HELLO. GOODBYE.":     "Hello. Goodbye.",
		"open the file":       "open the file",
		"Send the PDF please": "Send the PDF please",
		"":                    "",
		"123 456":             "123 456",
	}
	for in, want := range cases {
		if got := FixAllCaps(in); got != want {
			t.Errorf("FixAllCaps(%q) = %q, want %q", in, got, want)
		}
	}
}

// A saved set from before the hallucination row existed must GAIN it —
// otherwise the filter silently disappears on every machine that already had
// corrections, which is exactly what a wholesale replace would have done.
func TestSeedMissingDefaultsAddsTheRowOnceToOlderSets(t *testing.T) {
	existing := []Correction{{ID: "a", MatchPattern: "helo", Replacement: "hello", Enabled: true}}
	seeded, added := seedMissingDefaults(existing, 0)
	if want := 1 + len(hallucinationDefaults()); !added || len(seeded) != want {
		t.Fatalf("expected the hallucination rows to be seeded, got %d rows, want %d (added=%v)", len(seeded), want, added)
	}
	if got := ApplyCorrections("Thank you.", seeded); strings.TrimSpace(got) != "" {
		t.Errorf("seeded set does not filter the stock phrase: %q", got)
	}
	// Already current: no duplicate, and a user who deleted the row keeps it
	// deleted.
	again, addedAgain := seedMissingDefaults(seeded, correctionsSeedVersion)
	if addedAgain || len(again) != len(seeded) {
		t.Errorf("re-seeding at the current version must be a no-op, got %d rows (added=%v)", len(again), addedAgain)
	}
}

// A set saved at version 1 gains only the punctuation-only row (version 2).
// The version-1 row a user deleted stays deleted: seeding by pattern alone
// would have brought it back on this bump.
func TestSeedMissingDefaultsAddsOnlyRowsNewerThanTheStoredVersion(t *testing.T) {
	existing := []Correction{{ID: "a", MatchPattern: "helo", Replacement: "hello", Enabled: true}}
	seeded, added := seedMissingDefaults(existing, 1)
	if !added || len(seeded) != 2 {
		t.Fatalf("expected one row seeded onto a version-1 set, got %d rows (added=%v)", len(seeded), added)
	}
	if got := seeded[0].MatchPattern; got != `^[\s\p{P}]+$` {
		t.Errorf("seeded row = %q, want the punctuation-only row", got)
	}
	if got := ApplyCorrections(".", seeded); got != "" {
		t.Errorf("seeded set does not blank a lone period: %q", got)
	}
	if got := ApplyCorrections("Thank you.", seeded); got != "Thank you." {
		t.Errorf("the deleted version-1 row came back: %q", got)
	}
}

// On an upgraded install the new row lands where a fresh install has it:
// after the seeded rows, before the user's own. Appended after a user rule
// "period" -> ".", it would blank the ".." that rule makes of "Period.".
func TestSeedMissingDefaultsInsertsBeforeUserRules(t *testing.T) {
	var existing []Correction
	for _, d := range DefaultCorrections() {
		if d.MatchPattern != `^[\s\p{P}]+$` {
			existing = append(existing, d)
		}
	}
	nSeeded := len(existing)
	user := Correction{ID: "u", MatchPattern: "period", Replacement: ".", Enabled: true, Source: SourceUser}
	existing = append(existing, user)

	upgraded, added := seedMissingDefaults(existing, 1)
	if !added || len(upgraded) != nSeeded+2 {
		t.Fatalf("expected one row seeded, got %d rows (added=%v)", len(upgraded), added)
	}
	if got := upgraded[nSeeded].MatchPattern; got != `^[\s\p{P}]+$` {
		t.Errorf("row %d = %q, want the punctuation-only row before the user rule", nSeeded, got)
	}
	if got := upgraded[nSeeded+1].ID; got != "u" {
		t.Errorf("user rule moved: last row is %q", got)
	}

	fresh := append(DefaultCorrections(), user)
	for _, set := range [][]Correction{upgraded, fresh} {
		if got := ApplyCorrections("Period.", set); got != ".." {
			t.Errorf(`ApplyCorrections("Period.") = %q, want ".."`, got)
		}
	}
}

// After a FAILED read of the saved rules, the list is the shipped defaults
// and Set replaces the whole list, so saving would erase every rule the user
// had. Set refuses instead and reads again.
func TestSetRefusesAfterAFailedRead(t *testing.T) {
	c := New(branchkit.NewDetachedPlugin(), func() bool { return true }, func() bool { return false }) // every read fails
	c.LoadFromCollection()
	if err := c.Set([]Correction{{MatchPattern: "foo", Replacement: "bar"}}); err == nil {
		t.Fatal("Set must refuse an edit made on top of defaults after a failed read")
	}
	for _, r := range c.Get() {
		if r.MatchPattern == "foo" {
			t.Fatal("the refused edit must not replace the in-memory list either")
		}
	}
}
