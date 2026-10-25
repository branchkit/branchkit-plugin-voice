package speech

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SpokenPunctuation turns punctuation said as a word into the mark: "Dear
// John comma thanks period" becomes "Dear John, thanks." A repair step, off
// unless the user turns on "Say punctuation".
//
// Only sentence punctuation is converted. Its words almost never occur as
// words: in 3,069 logged dictations (2026-09-25..2026-10-25) "period",
// "comma", "semicolon" and the quote and parenthesis pairs never did,
// "question mark" twice and "colon" once. Symbol words ("slash", "dash",
// "plus") are common words too, and wait for a design of their own. Line
// breaks wait for a check of how a typed "\n" lands in each app.
//
// Whisper often punctuates around the spoken word as well ("Hello, comma,
// how"), so a mark absorbs the punctuation Whisper put beside it. A word
// right after an article or determiner ("the question mark button") or
// right before "of" ("a period of time") is left as a word: both literal
// "question mark"s in the log were "the question mark …". The miss it
// accepts: a literal word with neither around it ("the trial period.").
func SpokenPunctuation(text string) string {
	matches := spokenMarkRe.FindAllStringSubmatchIndex(text, -1)
	if matches == nil {
		return text
	}
	var out strings.Builder
	last := 0
	follow := followNone
	for _, m := range matches {
		start, end := m[0], m[1]
		if start < last || spokenWordIsLiteral(text, start, end) {
			continue
		}
		out.WriteString(applyFollow(text[last:start], &follow))
		sm := spokenMarks[normalizeSpokenMark(text[m[2]:m[3]])]
		if sm.opens {
			written := strings.TrimRight(out.String(), " \t")
			out.Reset()
			out.WriteString(written)
			if r, _ := utf8.DecodeLastRuneInString(written); written != "" && !strings.ContainsRune("([{\"\n", r) {
				out.WriteByte(' ')
			}
		} else {
			written := strings.TrimRight(out.String(), " \t")
			written = strings.TrimRight(written, whisperPunctuation+" \t")
			out.Reset()
			out.WriteString(written)
		}
		out.WriteString(sm.mark)
		// Skip Whisper's punctuation after the word, and the space before
		// whatever comes next.
		last = end
		for last < len(text) {
			r, size := utf8.DecodeRuneInString(text[last:])
			if !strings.ContainsRune(whisperPunctuation+" \t", r) {
				break
			}
			last += size
		}
		switch {
		case sm.opens:
			follow = followNone
		case sm.mark == "." || sm.mark == "?" || sm.mark == "!":
			follow = followCapital
		case sm.mark == "," || sm.mark == ";" || sm.mark == ":":
			follow = followLower
		default:
			// A closing quote or parenthesis keeps whatever the mark
			// before it asked for: `said "hi." Then` capitalises.
		}
		if last < len(text) && !sm.opens {
			r, _ := utf8.DecodeRuneInString(text[last:])
			if !strings.ContainsRune(")]}\"", r) {
				out.WriteByte(' ')
			}
		}
	}
	out.WriteString(applyFollow(text[last:], &follow))
	return out.String()
}

// whisperPunctuation is the punctuation Whisper writes beside a spoken mark
// word, which the mark replaces.
const whisperPunctuation = ",.;:!?…"

// spokenMark is a mark and which side it attaches to: an opening mark to
// the word after it, every other mark to the word before.
type spokenMark struct {
	mark  string
	opens bool
}

// spokenMarks maps each spoken form, normalised by normalizeSpokenMark, to
// its mark. Straight quotes: an app that curls quotes curls these too.
var spokenMarks = map[string]spokenMark{
	"period":            {mark: "."},
	"full stop":         {mark: "."},
	"comma":             {mark: ","},
	"question mark":     {mark: "?"},
	"exclamation mark":  {mark: "!"},
	"exclamation point": {mark: "!"},
	"colon":             {mark: ":"},
	"semicolon":         {mark: ";"},
	"open quote":        {mark: `"`, opens: true},
	"close quote":       {mark: `"`},
	"end quote":         {mark: `"`},
	"open paren":        {mark: "(", opens: true},
	"open parenthesis":  {mark: "(", opens: true},
	"close paren":       {mark: ")"},
	"close parenthesis": {mark: ")"},
}

// spokenMarkRe matches every spoken form, Whisper's spellings of them
// included ("semi-colon", "full-stop", "Question mark"). Longer forms first,
// so "semicolon" is never read as "colon".
var spokenMarkRe = regexp.MustCompile(`(?i)\b(question[\s-]+mark|exclamation[\s-]+(?:mark|point)|full[\s-]+stop|semi-?colon|open[\s-]+(?:quote|parenthesis|paren)|close[\s-]+(?:quote|parenthesis|paren)|end[\s-]+quote|period|comma|colon)\b`)

var spokenSeparatorRe = regexp.MustCompile(`[\s-]+`)

func normalizeSpokenMark(s string) string {
	s = strings.ToLower(s)
	if strings.HasPrefix(s, "semi") {
		return "semicolon"
	}
	return spokenSeparatorRe.ReplaceAllString(s, " ")
}

// literalBefore are the words after which a mark word names the mark
// instead of making it: "the question mark", "a period", "your comma".
// Articles, possessives and quantifiers only: a demonstrative or "what" is
// as often the end of a phrase ("I said that period", "what question
// mark"), and "one" begins a spoken list ("one comma two").
var literalBefore = map[string]bool{
	"a": true, "an": true, "the": true, "each": true, "every": true,
	"any": true, "no": true, "my": true, "your": true, "our": true,
	"their": true, "his": true, "her": true, "its": true, "which": true,
	"another": true,
}

// spokenWordIsLiteral reports whether the mark word at text[start:end] is
// used as a word: right after a determiner, or right before "of".
func spokenWordIsLiteral(text string, start, end int) bool {
	before := strings.TrimRight(text[:start], " \t")
	i := strings.LastIndexFunc(before, func(r rune) bool { return !unicode.IsLetter(r) })
	if literalBefore[strings.ToLower(before[i+1:])] {
		return true
	}
	return ofAfterRe.MatchString(text[end:])
}

var ofAfterRe = regexp.MustCompile(`(?i)^[ \t]+of\b`)

// follow is what a mark does to the first word after it.
type follow int

const (
	followNone follow = iota
	// followCapital capitalises it: a sentence starts.
	followCapital
	// followLower lowers a capital Whisper gave it for a sentence start it
	// invented ("Hello comma. How are you"), on the caret fit's terms:
	// continuation words only, so a name keeps its capital.
	followLower
)

// applyFollow applies f to the first word of s. A segment with no letter in
// it passes f on to the next one.
func applyFollow(s string, f *follow) string {
	i := strings.IndexFunc(s, unicode.IsLetter)
	if i < 0 {
		return s
	}
	switch *f {
	case followCapital:
		r, size := utf8.DecodeRuneInString(s[i:])
		s = s[:i] + string(unicode.ToUpper(r)) + s[i+size:]
	case followLower:
		j := strings.IndexFunc(s[i:], func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' && r != '’' })
		word := s[i:]
		if j >= 0 {
			word = s[i : i+j]
		}
		r, size := utf8.DecodeRuneInString(word)
		if IsContinuationWord(word) && unicode.IsUpper(r) && strings.ToLower(word[size:]) == word[size:] {
			s = s[:i] + string(unicode.ToLower(r)) + s[i+size:]
		}
	}
	*f = followNone
	return s
}
