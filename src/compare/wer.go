package compare

import (
	"strings"
	"unicode"
)

// Dictation is scored by words, not by exact match: a sentence is long, and a
// model that gets one word wrong in twelve is far better than one that gets
// six wrong. WordErrors counts the word edits (substitutions, insertions,
// deletions) that turn the reference into what was heard, after both are
// normalized the same way: lowercased, punctuation dropped, so "Friday." and
// "friday" agree and a comma is never an error.

// DictationWords is s as the words dictation is scored on.
func DictationWords(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'' || r == '’':
			// Inside a word ("it's", "they're") an apostrophe is part of it.
			b.WriteRune('\'')
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	for i, w := range words {
		words[i] = strings.Trim(w, "'")
	}
	return words
}

// WordErrors is the word edit distance from reference to heard, and the
// reference's word count.
func WordErrors(reference, heard string) (errors, words int) {
	ref, hyp := DictationWords(reference), DictationWords(heard)
	prev := make([]int, len(hyp)+1)
	cur := make([]int, len(hyp)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ref); i++ {
		cur[0] = i
		for j := 1; j <= len(hyp); j++ {
			cost := 1
			if ref[i-1] == hyp[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(hyp)], len(ref)
}
