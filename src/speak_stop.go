package main

import "sync"

// speakStopSplitMaxMs bounds a stop/cancel pair split across two command
// transcripts: the second word's onset may be at most this far after the
// first word's onset. Within one transcript adjacency already implies the
// words were close (the VAD splits on 0.10 s of silence, plugin.json
// `min-silence`), but consecutive transcripts can be any distance apart, and
// "...to speak." <pause> "Over the weekend..." is dictation, not a stop. A
// false stop truncates dictation; a missed one is recoverable by saying it
// again, so the bound is tight. Onset to onset covers the first word's own
// length (~0.3-0.45 s for "speak") plus the pause that caused the split.
const speakStopSplitMaxMs = 1000

// speakWord is the last word of the previous command transcript in a speak
// session, with its onset (shared chunk timebase) when the engine supplied
// one.
type speakWord struct {
	word     string
	onset    uint64
	hasOnset bool
}

// speakStopCarry holds the previous transcript's last word, so a stop phrase
// the VAD split across two transcripts ("SPEAK" | "OVER") still pairs.
type speakStopCarry struct {
	mu   sync.Mutex
	last *speakWord
}

// swap returns the carried word and replaces it with this transcript's last
// word. An empty transcript leaves the carry as it is: a blank segment says
// nothing about adjacency, and the time bound still applies.
func (c *speakStopCarry) swap(words []string, onsets []uint64) *speakWord {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.last
	if n := len(words); n > 0 {
		w := &speakWord{word: words[n-1]}
		if len(onsets) == n {
			w.onset, w.hasOnset = onsets[n-1], true
		}
		c.last = w
	}
	return prev
}

// reset forgets the carried word: at a new speak session, after a stop or
// cancel, and at the end-of-audio marker.
func (c *speakStopCarry) reset() {
	c.mu.Lock()
	c.last = nil
	c.mu.Unlock()
}

// speakPhraseSpan finds the earliest adjacent pair from table, letting the
// previous transcript's last word (prev) pair with this transcript's first.
// start is the index of the pair's first word in words, or -1 when the pair
// spans the split. A split pair counts only with onsets on both sides (this
// transcript's onsets aligned 1:1 with its words) and the second word
// starting after the first, within speakStopSplitMaxMs. found is false when
// no pair matches.
func speakPhraseSpan(prev *speakWord, words []string, onsets []uint64, table [][]string) (start int, found bool) {
	if prev != nil && prev.hasOnset && len(words) > 0 && len(onsets) == len(words) &&
		onsets[0] > prev.onset && onsets[0]-prev.onset <= speakStopSplitMaxMs {
		w0 := stripWordPunctuation(prev.word)
		w1 := stripWordPunctuation(words[0])
		for _, phrase := range table {
			if w0 == phrase[0] && w1 == phrase[1] {
				return -1, true
			}
		}
	}
	if i := speakPhraseIndex(words, table); i >= 0 {
		return i, true
	}
	return 0, false
}

// speakPhraseCutoffMs is speakStopCutoffMs for a span from speakPhraseSpan:
// a split pair cuts at the carried word's onset, where the phrase begins.
func speakPhraseCutoffMs(prev *speakWord, onsets []uint64, start int) *uint64 {
	if start < 0 {
		if prev == nil || !prev.hasOnset {
			return nil
		}
		return speakStopCutoffMs([]uint64{prev.onset}, 0)
	}
	return speakStopCutoffMs(onsets, start)
}
