package main

import (
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/plugin-sdk-go"
)

// The insert step of the dictation text pipeline: fit a
// transcript into the text already around the caret.
//
// Dictation produces a sentence; a text field is already mid-thought. Without
// this the two are simply concatenated and the user repairs the seam by hand
// every time: two dictations in a row join as "folder.Can I", a continuation
// starts with a capital, a doubled space follows a period.
//
// The platform reads the few characters either side of the caret
// (`native.caret_context`, never a password field) and this file decides what
// they imply. It used to live in the shell as InsertionContext.swift, on a
// paste path nothing had called since April, so no OS did any of it.
//
// Profiles shape punctuation for the destination; this shapes the SEAM: the
// space on either side, the capital at a sentence start (and its absence
// mid-sentence), and a sentence period that the text after the caret makes
// wrong.

// caretContext is what the platform reports around the caret.
type caretContext struct {
	Before string
	After  string
}

// insertionOpeners already imply that no space follows them: "(" then
// "however" is "(however".
const insertionOpeners = "([{“‘\"'/-@#*_~<|\\$"

// pronounI are the words that keep their capital mid-sentence.
var pronounI = map[string]bool{"I": true, "I'm": true, "I'll": true, "I've": true, "I'd": true}

// fitToCaret shapes text for insertion between ctx.Before and ctx.After.
//
// A nil context means the field could not be read, and the text is returned
// untouched: guessing from no information is how you get a stray space in the
// one app that does not report its contents. keepCase names words whose
// capital is not the sentence's (the user's vocabulary and correction
// targets), so a mid-sentence "BranchKit" or "Ghostty" stays as it is.
func fitToCaret(text string, ctx *caretContext, keepCase func(word string) bool) string {
	if ctx == nil || text == "" {
		return text
	}
	out := text
	first, _ := utf8.DecodeRuneInString(out)
	startsWithLetter := unicode.IsLetter(first)

	// Leading space, unless the field ends in whitespace or an opener, or the
	// text starts with punctuation. An empty field gets none.
	if last, _ := utf8.DecodeLastRuneInString(ctx.Before); ctx.Before != "" {
		if !unicode.IsSpace(last) && !strings.ContainsRune(insertionOpeners, last) &&
			!unicode.IsPunct(first) && !unicode.IsSpace(first) {
			out = " " + out
		}
	}

	// Capitalisation, only when the text starts with a letter: a path, a
	// flag or a number is not prose and is never re-cased.
	if startsWithLetter {
		switch {
		case strings.TrimSpace(ctx.Before) == "" || precedingEndsSentence(ctx.Before):
			out = capitaliseFirstLetter(out)
		case continuesSentence(ctx.Before):
			// Whisper capitalises the first word of every transcript; mid-
			// sentence that capital is wrong. Only a plain capitalised word
			// is lowered ("And", not "NASA", "iPhone" or "I'm"), and never
			// one the user taught as a word.
			out = lowerFirstWord(out, keepCase)
		}
	}

	// Trailing side: a space before a following word, and no sentence period
	// when the sentence visibly carries on after the caret.
	afterFirst, _ := utf8.DecodeRuneInString(strings.TrimLeft(ctx.After, " \t"))
	if ctx.After != "" {
		if unicode.IsLower(afterFirst) && strings.HasSuffix(out, ".") && !strings.HasSuffix(out, "..") {
			out = strings.TrimSuffix(out, ".")
		}
		next, _ := utf8.DecodeRuneInString(ctx.After)
		if (unicode.IsLetter(next) || unicode.IsDigit(next)) && !strings.HasSuffix(out, " ") {
			out += " "
		}
	}
	return out
}

// precedingEndsSentence reports whether the text before the caret ends a
// sentence: a terminator, possibly followed by whitespace and by a closing
// quote or bracket ("...done.\" ").
func precedingEndsSentence(before string) bool {
	s := strings.TrimRightFunc(before, unicode.IsSpace)
	s = strings.TrimRight(s, "\"'”’)]")
	last, _ := utf8.DecodeLastRuneInString(s)
	return s != "" && (last == '.' || last == '!' || last == '?')
}

// continuesSentence reports whether the text before the caret is plainly
// mid-sentence: its last visible character is a letter, a digit, or a comma,
// semicolon or colon. After an opener or a dash nothing is assumed.
func continuesSentence(before string) bool {
	s := strings.TrimRightFunc(before, unicode.IsSpace)
	last, _ := utf8.DecodeLastRuneInString(s)
	return s != "" && (unicode.IsLetter(last) || unicode.IsDigit(last) || strings.ContainsRune(",;:", last))
}

func capitaliseFirstLetter(s string) string {
	for i, r := range s {
		if unicode.IsLetter(r) {
			if !unicode.IsLower(r) {
				return s
			}
			return s[:i] + string(unicode.ToUpper(r)) + s[i+utf8.RuneLen(r):]
		}
	}
	return s
}

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

// lowerFirstWord lowers the first word's capital when that capital can only be
// Whisper's sentence start: the word is a continuation word (above) written
// with an upper-case first letter and nothing else upper-case. A name, an
// acronym, the pronoun I, and anything keepCase claims are left alone.
func lowerFirstWord(s string, keepCase func(string) bool) string {
	start := strings.IndexFunc(s, unicode.IsLetter)
	if start < 0 {
		return s
	}
	end := start + strings.IndexFunc(s[start:], func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\'' && r != '’'
	})
	if end < start {
		end = len(s)
	}
	word := strings.TrimRight(s[start:end], "'’")
	if pronounI[word] || (keepCase != nil && keepCase(word)) {
		return s
	}
	if !continuationWords[strings.ReplaceAll(strings.ToLower(word), "’", "'")] {
		return s
	}
	first, size := utf8.DecodeRuneInString(word)
	if !unicode.IsUpper(first) {
		return s
	}
	for _, r := range word[size:] {
		if unicode.IsLetter(r) && !unicode.IsLower(r) {
			return s
		}
	}
	return s[:start] + string(unicode.ToLower(first)) + s[start+size:]
}

// fitDictationToCaret applies the insert step to text about to be typed, and
// says for the log whether the caret could be read ("read") or not ("none").
// An unreadable caret is ordinary (a terminal, a canvas, a password box) and
// leaves the text as it is. It reads the caret now: for text not tied to a
// release (a retype).
func (h *Host) fitDictationToCaret(text string) (string, string) {
	resp, err := h.plugin.NativeCaretContext()
	return h.fitToCaretResponse(text, resp, err)
}

// fitReleasedDictationToCaret is fitDictationToCaret for a dictation's
// transcript: it uses the caret read started at release (prefetchCaret,
// handed over by takeCaretPrefetch), waiting for it if it is still running,
// and reads now when there is none. Reading after the transcript put the whole read in the felt
// latency, and in a terminal the read copies the scrollback: +254 ms p50,
// +602 ms p90 over 35 Ghostty dictations (2026-10-08). Started
// at release it runs during the ~1 s of transcription instead. The caret is
// where it was at release, which is where the person was dictating.
func (h *Host) fitReleasedDictationToCaret(text string, p *caretPrefetch) (string, string) {
	if p != nil {
		<-p.done
		return h.fitToCaretResponse(text, p.resp, p.err)
	}
	return h.fitDictationToCaret(text)
}

func (h *Host) fitToCaretResponse(text string, resp *branchkit.NativeCaretContextResponse, err error) (string, string) {
	if err != nil || resp == nil || resp.Context == nil {
		return text, "none"
	}
	ctx := &caretContext{Before: resp.Context.Before, After: resp.Context.After}
	return fitToCaret(text, ctx, h.taughtWord()), "read"
}

// caretPrefetch is one caret read started at a dictation's release.
type caretPrefetch struct {
	started time.Time
	done    chan struct{}
	resp    *branchkit.NativeCaretContextResponse
	err     error
}

// A prefetch older than this belongs to a dictation whose transcript never
// came (an empty one, a dropped one), never to the next.
const caretPrefetchMaxAge = 20 * time.Second

var (
	caretMu      sync.Mutex
	caretPending *caretPrefetch
)

// prefetchCaret starts reading the caret context, for the transcript of the
// dictation just released to use.
func (h *Host) prefetchCaret() {
	p := &caretPrefetch{started: time.Now(), done: make(chan struct{})}
	caretMu.Lock()
	caretPending = p
	caretMu.Unlock()
	go func() {
		p.resp, p.err = h.plugin.NativeCaretContext()
		close(p.done)
	}()
}

// takeCaretPrefetch hands over the pending read, once; nil when there is
// none or it is too old to describe this dictation.
func (h *Host) takeCaretPrefetch() *caretPrefetch {
	caretMu.Lock()
	defer caretMu.Unlock()
	p := caretPending
	caretPending = nil
	if p == nil || time.Since(p.started) > caretPrefetchMaxAge {
		return nil
	}
	return p
}

// taughtWord reports whether a word is one the user taught dictation (their
// vocabulary or a correction's replacement), compared exactly: those words
// carry their own capitals.
func (h *Host) taughtWord() func(string) bool {
	set := map[string]bool{}
	for _, w := range speech.BiasWords(h.speech.Get(), h.speech.Vocabulary()) {
		for _, f := range strings.Fields(w) {
			set[f] = true
		}
	}
	return func(word string) bool { return set[word] }
}
