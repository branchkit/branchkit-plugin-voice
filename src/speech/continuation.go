package speech

import "strings"

// continuationWords are the words whose capital mid-sentence can only be
// Whisper's sentence start: closed-class words (articles, conjunctions,
// prepositions, pronouns, auxiliaries) and the common adverbs a continuation
// opens with. Only these are lowered. A word not listed keeps its capital,
// because Whisper's first-word capital and a name look the same ("And then"
// and "Paris"), and keeping a wrong capital is the safer mistake: lowering
// every plain capitalised word turned "to Paris" into "to paris" (2026-10-05).
// Words that are also names ("will", "may", "mark", "bill") are left out.
var continuationWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		a an the and or but nor so yet then also too just now still even only
		because since although though while when where which who whom whose what
		that this these those there here if unless until as than whether either
		neither not no never always often sometimes usually maybe perhaps however
		otherwise instead again already actually really probably anyway plus
		like how why more most less very quite rather almost thus therefore
		meanwhile besides especially some any every each all both one
		my your his her its our their me you him them us we they it he she
		of to in on at for with from by about into onto over under after before
		between through during without within around against along toward
		towards upon is are was were be been being am do does did have has had
		can could would should shall might must
		it's that's there's here's what's who's let's don't doesn't didn't
		can't won't isn't aren't wasn't weren't we're they're you're we'll
		you'll they'll he's she's`) {
		m[w] = true
	}
	return m
}()

// IsContinuationWord reports whether word, in any case and with either
// apostrophe, is one of continuationWords.
func IsContinuationWord(word string) bool {
	return continuationWords[strings.ReplaceAll(strings.ToLower(word), "’", "'")]
}
