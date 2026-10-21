package speech

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/branchkit/plugin-sdk-go"
)

// The dictation vocabulary: words the user says, listed plainly.
//
// Before this, the only way to teach dictation a word was to write a
// correction, a regex describing how Whisper gets it wrong, because the bias
// vocabulary was derived from correction targets alone. Nobody writes a
// regex for "Ghostty" until they have seen it misheard several times, and
// most people never would. A word here goes straight into the same logits
// boost the correction targets feed (bias.go); nothing is rewritten after
// the fact, so a plain list cannot damage a transcript the way a bad pattern
// can.

const vocabularyCollection = "plugin.voice.dictation_vocabulary"

// maxVocabularyWordLen bounds one entry. Every subword of every entry is
// boosted, so a pasted paragraph would boost most of English.
const maxVocabularyWordLen = 48

// Vocabulary returns a copy of the user's word list, in the order added.
func (c *Corrections) Vocabulary() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.vocab...)
}

// LoadVocabulary reads the word list. A read FAILURE is remembered so the
// next edit refuses rather than saving a list built on top of nothing, the
// same guard Set keeps for the corrections.
func (c *Corrections) LoadVocabulary() {
	rec, err := c.plugin.Get(vocabularyCollection, "singleton")
	if err != nil {
		branchkit.Logf("voice", "dictation vocabulary read error: %v", err)
		c.mu.Lock()
		c.vocabLoadErr = err
		c.mu.Unlock()
		return
	}
	var words []string
	if rec != nil {
		var wrapper struct {
			Words []string `json:"words"`
		}
		if err := json.Unmarshal(rec.Payload, &wrapper); err != nil {
			branchkit.Logf("voice", "dictation vocabulary parse error: %v", err)
			c.mu.Lock()
			c.vocabLoadErr = err
			c.mu.Unlock()
			return
		}
		words = wrapper.Words
	}
	c.mu.Lock()
	c.vocab = words
	c.vocabLoadErr = nil
	c.mu.Unlock()
}

func (c *Corrections) setVocabulary(words []string) error {
	c.mu.Lock()
	failed := c.vocabLoadErr
	c.mu.Unlock()
	if failed != nil {
		c.LoadVocabulary()
		return fmt.Errorf("your saved vocabulary could not be read (%v), so this change "+
			"was not saved; reopen the list and make it again", failed)
	}
	if err := c.plugin.Put(vocabularyCollection, "singleton", map[string]any{"words": words}); err != nil {
		return fmt.Errorf("failed to save")
	}
	c.mu.Lock()
	c.vocab = words
	c.mu.Unlock()
	return nil
}

// ParseVocabularyInput splits what the user typed into entries. Commas and
// newlines separate words, so a list pasted from anywhere works; spaces do
// not, because "Claude Code" is one thing the user says.
func ParseVocabularyInput(text string) ([]string, error) {
	var out []string
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		w := strings.Join(strings.Fields(part), " ")
		if w == "" {
			continue
		}
		if utf8.RuneCountInString(w) > maxVocabularyWordLen {
			return nil, fmt.Errorf("%q is too long for one entry (%d characters at most); "+
				"separate words with commas", w, maxVocabularyWordLen)
		}
		if !strings.ContainsFunc(w, unicode.IsLetter) {
			return nil, fmt.Errorf("%q has no letters in it, so it cannot be said", w)
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("type a word to add")
	}
	return out, nil
}

// HandleVocabularyAdd adds one or more words. A word already present in any
// capitalisation is skipped: the spelling the user typed first is the one
// they want, and a second casing would boost different tokens for the same
// word.
func (c *Corrections) HandleVocabularyAdd(req *struct {
	Text string `json:"text"`
}) error {
	words, err := ParseVocabularyInput(req.Text)
	if err != nil {
		return err
	}
	current := c.Vocabulary()
	have := make(map[string]bool, len(current))
	for _, w := range current {
		have[strings.ToLower(w)] = true
	}
	next := current
	for _, w := range words {
		if have[strings.ToLower(w)] {
			continue
		}
		have[strings.ToLower(w)] = true
		next = append(next, w)
	}
	if len(next) == len(current) {
		return nil
	}
	if err := c.setVocabulary(next); err != nil {
		return err
	}
	c.PushBias()
	return nil
}

// HandleVocabularyRemove removes a word from everything that teaches it: the
// vocabulary list (any capitalisation) and the fix-a-word rules that write
// it. A hand-written pattern rule is left alone; it lives under Advanced,
// where it was written, and is removed there.
func (c *Corrections) HandleVocabularyRemove(req *struct {
	Word string `json:"word"`
}) error {
	key := strings.ToLower(strings.TrimSpace(req.Word))
	if key == "" {
		return nil
	}
	current := c.Vocabulary()
	next := make([]string, 0, len(current))
	for _, w := range current {
		if strings.ToLower(w) != key {
			next = append(next, w)
		}
	}
	corr := c.Get()
	keep := make([]Correction, 0, len(corr))
	for _, r := range corr {
		if _, literal := LiteralHeard(r); literal && classifySource(r) != SourceSeeded &&
			strings.ToLower(strings.TrimSpace(r.Replacement)) == key {
			continue
		}
		keep = append(keep, r)
	}
	if len(next) == len(current) && len(keep) == len(corr) {
		return nil
	}
	if len(next) != len(current) {
		if err := c.setVocabulary(next); err != nil {
			return err
		}
	}
	if len(keep) != len(corr) {
		if err := c.Set(keep); err != nil {
			return fmt.Errorf("failed to save")
		}
	}
	c.PushBias()
	return nil
}

// LiteralHeard reports the text a fix-a-word rule matches, when the rule is
// one: exactly the shape NewLiteralCorrection writes. Anything else is a
// pattern someone wrote by hand.
func LiteralHeard(c Correction) (string, bool) {
	if !c.IsRegex {
		return "", false
	}
	body, ok := strings.CutPrefix(c.MatchPattern, "(?i)")
	if !ok {
		return "", false
	}
	body = strings.TrimPrefix(body, `\b`)
	body = strings.TrimSuffix(body, `\b`)
	var heard strings.Builder
	escaped := false
	for _, r := range body {
		if r == '\\' && !escaped {
			escaped = true
			continue
		}
		escaped = false
		heard.WriteRune(r)
	}
	h := heard.String()
	if h == "" || literalPattern(h) != c.MatchPattern {
		return "", false
	}
	return h, true
}

// WordRow is one word dictation is taught, as the settings tab lists it: the
// spelling, the mishearings fix-a-word rules repair into it, and whether a
// hand-written pattern also writes it.
type WordRow struct {
	Word       string
	Fixes      []WordFix
	HasPattern bool
}

// WordFix is one fix-a-word rule under its word.
type WordFix struct {
	ID    string
	Heard string
}

// WordRows gathers the vocabulary and the correction targets into one list,
// one row per word (case-insensitive), the vocabulary's order first. Shipped
// rules are left out: they are policy, shown under Advanced. So are rules that
// write nothing (the hallucination filter) and disabled ones.
func WordRows(corr []Correction, vocab []string) []WordRow {
	var rows []WordRow
	index := map[string]int{}
	row := func(word string) *WordRow {
		key := strings.ToLower(word)
		if i, ok := index[key]; ok {
			return &rows[i]
		}
		index[key] = len(rows)
		rows = append(rows, WordRow{Word: word})
		return &rows[len(rows)-1]
	}
	for _, w := range vocab {
		row(w)
	}
	var extra []Correction
	for _, c := range corr {
		w := strings.TrimSpace(c.Replacement)
		if !c.Enabled || w == "" || classifySource(c) == SourceSeeded {
			continue
		}
		if _, listed := index[strings.ToLower(w)]; !listed {
			extra = append(extra, c)
			continue
		}
		addRule(row(w), c)
	}
	// Words that only a rule writes come after the list, alphabetically, so
	// the order does not shift with the order rules were made in.
	sort.SliceStable(extra, func(i, j int) bool {
		return strings.ToLower(extra[i].Replacement) < strings.ToLower(extra[j].Replacement)
	})
	for _, c := range extra {
		addRule(row(strings.TrimSpace(c.Replacement)), c)
	}
	return rows
}

func addRule(r *WordRow, c Correction) {
	if heard, ok := LiteralHeard(c); ok {
		r.Fixes = append(r.Fixes, WordFix{ID: c.ID, Heard: heard})
		return
	}
	r.HasPattern = true
}
