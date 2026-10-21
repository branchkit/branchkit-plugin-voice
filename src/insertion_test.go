package main

import "testing"

// The rules ported from the shell's InsertionContext tests, plus the
// mid-sentence ones it never had. Whisper capitalises and punctuates every
// transcript as a whole sentence, so the realistic input is "And then we
// left." rather than "and then we left".
func TestFitToCaret(t *testing.T) {
	ctx := func(before, after string) *caretContext { return &caretContext{Before: before, After: after} }
	none := func(string) bool { return false }
	for _, tt := range []struct {
		name string
		text string
		ctx  *caretContext
		want string
	}{
		// An unreadable field is left alone.
		{"unknown context", "Hello there.", nil, "Hello there."},
		{"empty text", "", ctx("We left", ""), ""},

		{"start of field capitalises, no space", "hello there", ctx("", ""), "Hello there"},
		{"blank field counts as start", "hello there", ctx("   ", ""), "Hello there"},

		// The seam that motivated this: consecutive dictations.
		{"after a sentence gets a space", "Can I go?", ctx("Look in the folder.", ""), " Can I go?"},
		{"after a sentence and a space", "Can I go?", ctx("Look in the folder. ", ""), "Can I go?"},
		{"after a sentence capitalises", "then we left", ctx("We went to the shop.", ""), " Then we left"},
		{"after a question", "then we left", ctx("Really?", ""), " Then we left"},
		{"terminator behind a quote", "then", ctx("he said \"stop.\" ", ""), "Then"},

		// Mid-sentence: no capital Whisper put there.
		{"continuation is lowered", "And then we left.", ctx("We went to the shop", ""), " and then we left."},
		{"after a comma", "But not today.", ctx("Maybe tomorrow, ", ""), "but not today."},
		{"existing space not doubled", "and then", ctx("We went to the shop ", ""), "and then"},
		{"pronoun I keeps its capital", "I think so.", ctx("Well", ""), " I think so."},
		{"I'm keeps its capital", "I'm sure.", ctx("Well", ""), " I'm sure."},
		{"acronym kept", "NASA launched it.", ctx("Then", ""), " NASA launched it."},
		{"mixed case kept", "iPhone sales fell.", ctx("Then", ""), " iPhone sales fell."},

		// Openers and leading punctuation.
		{"after an opener", "however", ctx("text (", ""), "however"},
		{"after a slash", "bin", ctx("/usr/", ""), "bin"},
		{"leading punctuation attaches", ", and then", ctx("We left", ""), ", and then"},
		{"leading ellipsis attaches", "...anyway", ctx("We left", ""), "...anyway"},

		// Not prose: never re-cased.
		{"path at start", "/usr/local/bin", ctx("", ""), "/usr/local/bin"},
		{"flag at start", "--verbose", ctx("", ""), "--verbose"},
		{"number at start", "3 items", ctx("", ""), "3 items"},

		// The far side of the caret.
		// "Quickly" keeps Whisper's capital: it is not a continuation word,
		// and a word the rule cannot tell from a name is left alone.
		{"space before a following word", "Quickly.", ctx("We ", "ran home"), "Quickly "},
		{"period dropped before a lowercase continuation", "Very quickly.", ctx("We ran ", "home"), "very quickly "},
		{"period kept before a capital", "We left.", ctx("", "Then it rained."), "We left. "},
		{"period kept at the end", "We left.", ctx("", ""), "We left."},
		{"no space before punctuation", "the shop", ctx("We went to ", ". Then"), "the shop"},
		{"ellipsis kept", "And so...", ctx("Well, ", "maybe"), "and so... "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := fitToCaret(tt.text, tt.ctx, none); got != tt.want {
				t.Errorf("fitToCaret(%q, %+v) = %q, want %q", tt.text, tt.ctx, got, tt.want)
			}
		})
	}
}

// A word the user taught dictation carries its own capital: mid-sentence it
// is not a sentence start to undo.
func TestFitToCaretKeepsTaughtWords(t *testing.T) {
	taught := func(w string) bool { return w == "Ghostty" }
	got := fitToCaret("Ghostty crashed.", &caretContext{Before: "Then"}, taught)
	if got != " Ghostty crashed." {
		t.Errorf("got %q", got)
	}
	got = fitToCaret("Then it crashed.", &caretContext{Before: "Ghostty"}, taught)
	if got != " then it crashed." {
		t.Errorf("got %q", got)
	}
}

// Whisper capitalises a continuation's first word, and a name looks the same.
// Only words that cannot be names are lowered; a name keeps its capital.
func TestFitToCaretKeepsNamesMidSentence(t *testing.T) {
	none := func(string) bool { return false }
	for _, tt := range []struct{ text, before, want string }{
		{"Paris is lovely.", "We flew to", " Paris is lovely."},
		{"Priya said yes.", "Then", " Priya said yes."},
		{"Kubernetes crashed.", "And then", " Kubernetes crashed."},
		{"Will you come?", "So", " Will you come?"},
		{"Running late.", "Sorry,", " Running late."},
		{"And then we left.", "We ate", " and then we left."},
		{"It’s fine.", "Well", " it’s fine."},
		{"Don't worry.", "Honestly,", " don't worry."},
	} {
		if got := fitToCaret(tt.text, &caretContext{Before: tt.before}, none); got != tt.want {
			t.Errorf("fitToCaret(%q after %q) = %q, want %q", tt.text, tt.before, got, tt.want)
		}
	}
}
