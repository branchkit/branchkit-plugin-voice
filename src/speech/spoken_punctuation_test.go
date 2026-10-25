package speech

import "testing"

func TestSpokenPunctuation(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"no mark words", "Hello there, how are you?", "Hello there, how are you?"},
		{"comma and period", "Dear John comma thanks period", "Dear John, thanks."},
		{"question mark at the end", "how are you question mark", "how are you?"},
		{"exclamation point", "Great exclamation point", "Great!"},
		{"absorbs Whisper's punctuation around the word", "Hello, comma, how are you?", "Hello, how are you?"},
		{"absorbs Whisper's period after the word", "Thanks period.", "Thanks."},
		{"Whisper capitalised the word", "Thanks. Period. See you.", "Thanks. See you."},
		{"capitalises after a sentence mark", "done period now the next one", "done. Now the next one"},
		{"lowers Whisper's invented capital after a comma", "Hello comma. How are you?", "Hello, how are you?"},
		{"keeps a name's capital after a comma", "Hello comma. Paris is nice.", "Hello, Paris is nice."},
		{"colon and semicolon", "the list colon apples semicolon pears", "the list: apples; pears"},
		{"Whisper's semi-colon spelling", "one semi-colon two", "one; two"},
		{"full stop", "the end full stop", "the end."},
		{"quotes", "he said open quote hello close quote and left", `he said "hello" and left`},
		{"parentheses", "a test open paren maybe close paren works", "a test (maybe) works"},
		{"closing mark then sentence mark", "he said open quote hi close quote period", `he said "hi".`},
		{"open paren after a comma keeps the comma", "well, open paren maybe close paren", "well, (maybe)"},
		{"mark word after a determiner stays", "Are these in the question mark pop-up help?", "Are these in the question mark pop-up help?"},
		{"another determiner", "move your comma over", "move your comma over"},
		{"a demonstrative ends the phrase", "I said that period", "I said that."},
		{"what question mark", "what question mark", "what?"},
		{"a spoken list", "one comma two comma three", "one, two, three"},
		{"mark word before of stays", "after a period of time", "after a period of time"},
		{"period of without a determiner stays", "period of review", "period of review"},
		{"mark word inside another word is left", "commas and periods", "commas and periods"},
		{"colon is not read inside semicolon", "semicolon", ";"},
		{"several marks in a row", "wait comma what question mark", "wait, what?"},
		{"a closing quote keeps the user's period", "he said open quote hi period close quote then left", `he said "hi." Then left`},
		{"a closing paren keeps the user's question mark", "see above question mark close paren", "see above?)"},
		{"a closing quote keeps Whisper's period", "he said hi. Close quote", `he said hi."`},
		{"capital after a sentence mark crosses an open quote", "done period open quote hello close quote", `done. "Hello"`},
		{"a hyphenated word Whisper wrote is left", "the values are comma-separated", "the values are comma-separated"},
		{"hyphen after the word", "period-appropriate costumes", "period-appropriate costumes"},
		{"a number after a sentence mark keeps its case", "we're done period 3pm works", "we're done. 3pm works"},
		{"Unicode around marks", "café comma naïve period", "café, naïve."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SpokenPunctuation(tt.in); got != tt.want {
				t.Errorf("SpokenPunctuation(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
