package speech

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/branchkit/plugin-sdk-go"
)

// Corrections is the dictation-correction set: the rules, their persistence
// in the corrections collection, and the decoder-bias push that follows
// every change. Build it with New; the platform handle and the all-caps
// switch arrive as arguments, so a Corrections cannot exist unwired.
type Corrections struct {
	plugin *branchkit.Plugin
	// allCapsFix answers the user's "fix all caps" setting; a config read
	// passed in so this package need not know where settings live.
	allCapsFix func() bool

	mu   sync.Mutex
	list []Correction
	// loadErr is set when reading the saved rules FAILED (not when there
	// were none). The list is then the shipped defaults, and Set replaces
	// the whole list, so saving an edit made on top of the defaults would
	// erase every rule the user had. Set refuses while it is set.
	loadErr error

	// vocab is the user's plain word list (vocabulary.go), and vocabLoadErr
	// the same read-failure guard as loadErr, for that list.
	vocab        []string
	vocabLoadErr error
}

// New starts from the shipped defaults; LoadFromCollection replaces them
// with what the platform has once RPC is available.
func New(p *branchkit.Plugin, allCapsFix func() bool) *Corrections {
	return &Corrections{plugin: p, allCapsFix: allCapsFix, list: DefaultCorrections()}
}

// AllCapsFix reports whether the all-caps repair is enabled.
func (c *Corrections) AllCapsFix() bool { return c.allCapsFix() }

// Correction represents a text replacement rule applied to transcriptions.
type Correction struct {
	ID           string `json:"id"`
	MatchPattern string `json:"match"`
	Replacement  string `json:"replacement"`
	IsRegex      bool   `json:"isRegex"`
	Enabled      bool   `json:"enabled"`

	// Source records where this rule came from: SourceSeeded (shipped with
	// the plugin), SourceUser (typed by hand) or SourceLearned (created from
	// an observed mistake).
	//
	// Without it the table mixes shipped policy with your own edits and
	// nothing distinguishes them — which made "Reset to Defaults" an
	// all-or-nothing button that also discarded your rules, so nobody would
	// ever press it, so the one recovery affordance was effectively dead. It
	// also left shipped behaviour illegible: the hallucination filter (which
	// blanks a transcript reading only "you" or "so") sat indistinguishable
	// from something you typed months ago and forgot, and that rule has
	// silently eaten a dictated word before.
	//
	// Empty on a record written before this field existed; `classifySource`
	// derives it rather than asking anyone.
	Source string `json:"source,omitempty"`
	// LearnedAt stamps a SourceLearned rule, so it can be listed by age and
	// pruned. Zero for everything else.
	LearnedAt int64 `json:"learnedAt,omitempty"`
}

const (
	SourceSeeded  = "seeded"
	SourceUser    = "user"
	SourceLearned = "learned"
)

// classifySource fills in provenance for a rule that predates the field.
//
// Derived, never asked: a pattern that matches something this version ships is
// seeded, and anything else can only have been added by hand. Being wrong in
// the safe direction matters more than being exactly right — mislabelling a
// user rule as seeded would expose it to a restore, so an unknown pattern is
// always treated as the user's.
func classifySource(c Correction) string {
	if c.Source != "" {
		return c.Source
	}
	for _, d := range DefaultCorrections() {
		if d.MatchPattern == c.MatchPattern {
			return SourceSeeded
		}
	}
	return SourceUser
}

// DefaultCorrections is what a FRESH install starts with.
//
// The bar for shipping a rule here is deliberately high, and it rose on
// 2026-09-06 when this same vocabulary started biasing the decoder
// (`speech.BiasVocabulary` → the STT stage's logits filter). A shipped rule is
// no longer an inert regex that simply never matches for someone who does not
// use the word — it is a thumb on the scale of what they are likely to be
// heard saying. Boosting a term a user has never heard of is pure downside.
//
// So a default must clear all three:
//
//  1. Whisper genuinely mishears it — a real, reproducible failure, not a
//     preference about spelling.
//  2. Many people say it. Not "many people in this codebase".
//  3. Low collision risk. The replacement must not plausibly overwrite an
//     ordinary English word someone might actually have said.
//
// This list was previously twenty-two entries and was, honestly, one
// developer's personal vocabulary shipped to everyone: QuickBase, basetypes,
// go-stack, qb-docs-mcp, half a dozen `@agent` names and a set of Claude Code
// slash commands. Those belong in a user's own corrections, not in the
// product. They are NOT removed from anyone's existing set — corrections live
// in `plugin.voice.corrections` and load from there; trimming this function
// only changes what a NEW install begins with.
//
// Rejected on rule 3 rather than rule 2, and worth recording so they are not
// re-added: `NATS` (would rewrite "gnats"/"nats" in ordinary speech) and
// `templ` (collides with "temple", and the library is niche).
func DefaultCorrections() []Correction {
	entries := []struct {
		match, repl string
		isRegex     bool
	}{
		// Widely used, reliably misheard, and nothing ordinary collides.
		{`(?i)cube\s*c(?:tl|tle)`, "kubectl", true},
		{`(?i)sq\s*lite`, "SQLite", true},
		{`(?i)play\s*wri(?:ght|te)`, "Playwright", true},
	}
	for _, h := range hallucinationDefaults() {
		entries = append(entries, struct {
			match, repl string
			isRegex     bool
		}{h.match, h.repl, h.isRegex})
	}
	result := make([]Correction, len(entries))
	for i, e := range entries {
		result[i] = Correction{
			ID: SimpleID(), MatchPattern: e.match, Replacement: e.repl,
			IsRegex: e.isRegex, Enabled: true, Source: SourceSeeded,
		}
	}
	return result
}

// correctionsSeedVersion is bumped when a new DEFAULT correction should reach
// installs that already have a saved set. Without it, `LoadFromCollection`
// replaces the defaults wholesale and a newly-seeded row only ever appears for
// a fresh install — which for the hallucination filter would have meant
// silently losing it on every machine that already had corrections saved.
//
// Seeding is one-way per version: a user who deletes a seeded row does not get
// it back, because the stored version was already bumped when it was added,
// and each row is seeded only into a set older than the version that added it
// (its `since`).
const correctionsSeedVersion = 2

// hallucinationDefaults is what the WhisperKit stage used to do in Swift, with
// no way to see or change it: on silence the model emits a stock phrase
// ("Thank you.", "Thanks for watching.") and the stage blanked the transcript.
//
// It is a correction now, which makes it three things it was not: visible in
// the Dictation tab, disable-able by anyone who dictates the word "so" and
// wants it kept, and applied where every other text rule already is. The
// pattern is ANCHORED — it replaces a transcript that is ONLY the stock
// phrase, never the word inside a sentence.
//
// The second row blanks a transcript made only of punctuation and whitespace:
// on noise the model can return a lone "." (seen 2026-10-11, a 1.8 s hold of
// loud noise), which the first row's "..." did not cover, so a period was
// typed at the caret. It is its own row rather than a wider first pattern so
// it reaches installs that already hold the first one (seeding keys on the
// pattern) and can be turned off on its own.
//
// `since` is the correctionsSeedVersion that introduced the row.
func hallucinationDefaults() []struct {
	match, repl string
	isRegex     bool
	since       int
} {
	return []struct {
		match, repl string
		isRegex     bool
		since       int
	}{
		{`(?i)^\s*(?:you|thank you|thanks for watching|the end|bye|so|\.\.\.)\s*[.!?]?\s*$`, "", true, 1},
		{`^[\s\p{P}]+$`, "", true, 2},
	}
}

var idCounter uint64

// SimpleID generates a unique ID for corrections.
func SimpleID() string {
	t := uint64(time.Now().UnixNano())
	seq := atomic.AddUint64(&idCounter, 1)
	mixed := t*6364136223846793005 + seq
	return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x",
		uint32(mixed>>32), uint16(mixed>>16), uint16(mixed)&0x0FFF,
		(uint16(seq)&0x3FFF)|0x8000, t&0xFFFFFFFFFFFF)
}

func (c *Corrections) save(corr []Correction) error {
	return c.plugin.Put("plugin.voice.corrections", "singleton", map[string]any{
		"corrections":  corr,
		"seed_version": correctionsSeedVersion,
	})
}

// Get returns a copy of the current rules.
func (c *Corrections) Get() []Correction {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]Correction, len(c.list))
	copy(result, c.list)
	return result
}

// Set replaces the rules and persists them.
func (c *Corrections) Set(corr []Correction) error {
	c.mu.Lock()
	failed := c.loadErr
	c.mu.Unlock()
	if failed != nil {
		// Read again so the next edit starts from the real rules, but don't
		// save this one: it was made on top of the defaults.
		c.LoadFromCollection()
		return fmt.Errorf("your saved corrections could not be read (%v), so this change "+
			"was not saved; reopen the list and make it again", failed)
	}
	c.mu.Lock()
	c.list = corr
	c.mu.Unlock()
	return c.save(corr)
}

// LoadFromCollection loads corrections from the collection system.
// Falls back to migrating from the legacy corrections.json file.
func (c *Corrections) LoadFromCollection() {
	rec, err := c.plugin.Get("plugin.voice.corrections", "singleton")
	if err != nil {
		branchkit.Logf("voice", "corrections collection read error: %v", err)
		c.mu.Lock()
		c.loadErr = err
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.loadErr = nil
	c.mu.Unlock()
	if rec != nil {
		var wrapper struct {
			Corrections []Correction `json:"corrections"`
			SeedVersion int          `json:"seed_version"`
		}
		if err := json.Unmarshal(rec.Payload, &wrapper); err != nil {
			branchkit.Logf("voice", "corrections collection parse error: %v", err)
			return
		}
		for i := range wrapper.Corrections {
			wrapper.Corrections[i].Source = classifySource(wrapper.Corrections[i])
		}
		loaded, seeded := seedMissingDefaults(wrapper.Corrections, wrapper.SeedVersion)
		c.mu.Lock()
		c.list = loaded
		c.mu.Unlock()
		if seeded {
			if err := c.save(loaded); err != nil {
				branchkit.Logf("voice", "corrections seed save error: %v", err)
			}
		}
	}
}

// seedMissingDefaults inserts defaults introduced since `stored`, skipping any
// whose pattern is already present. Returns the set and whether anything was
// added.
//
// A new row goes in after the existing seeded rows and before the first rule of
// the user's own, where a fresh install has it (DefaultCorrections first). Order
// matters because rules apply in sequence: appended after a user rule
// "period" -> ".", the punctuation-only row would blank the ".." that rule
// makes of "Period.", which a fresh install types.
func seedMissingDefaults(existing []Correction, stored int) ([]Correction, bool) {
	if stored >= correctionsSeedVersion {
		return existing, false
	}
	have := make(map[string]bool, len(existing))
	for _, c := range existing {
		have[c.MatchPattern] = true
	}
	var missing []Correction
	for _, e := range hallucinationDefaults() {
		if e.since <= stored || have[e.match] {
			continue
		}
		missing = append(missing, Correction{
			ID: SimpleID(), MatchPattern: e.match, Replacement: e.repl,
			IsRegex: e.isRegex, Enabled: true, Source: SourceSeeded,
		})
	}
	if len(missing) == 0 {
		return existing, false
	}
	at := len(existing)
	for i, c := range existing {
		if classifySource(c) != SourceSeeded {
			at = i
			break
		}
	}
	out := make([]Correction, 0, len(existing)+len(missing))
	out = append(out, existing[:at]...)
	out = append(out, missing...)
	out = append(out, existing[at:]...)
	return out, true
}

// ApplyCorrections applies all enabled corrections to text.
func ApplyCorrections(text string, corr []Correction) string {
	result := text
	for _, c := range corr {
		if !c.Enabled || c.MatchPattern == "" {
			continue
		}
		if c.IsRegex {
			re, err := regexp.Compile(c.MatchPattern)
			if err != nil {
				continue
			}
			result = re.ReplaceAllString(result, c.Replacement)
		} else {
			// Case-insensitive plain text replacement
			lower := strings.ToLower(result)
			patternLower := strings.ToLower(c.MatchPattern)
			var b strings.Builder
			start := 0
			for {
				pos := strings.Index(lower[start:], patternLower)
				if pos < 0 {
					break
				}
				absPos := start + pos
				b.WriteString(result[start:absPos])
				b.WriteString(c.Replacement)
				start = absPos + len(c.MatchPattern)
			}
			b.WriteString(result[start:])
			result = b.String()
		}
	}
	return result
}

// --- RPC handlers ---

func (c *Corrections) HandleCorrectionsAdd(_ *struct{}) error {
	corr := c.Get()
	newCorr := Correction{
		ID: SimpleID(), MatchPattern: "", Replacement: "",
		IsRegex: false, Enabled: true, Source: SourceUser,
	}
	corr = append(corr, newCorr)
	if err := c.Set(corr); err != nil {
		return fmt.Errorf("failed to save")
	}
	c.PushBias()
	return nil
}

func (c *Corrections) HandleCorrectionUpdate(req *struct {
	ID          string  `json:"id"`
	Match       *string `json:"match,omitempty"`
	Replacement *string `json:"replacement,omitempty"`
	IsRegex     *bool   `json:"isRegex,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}) error {
	corr := c.Get()
	for i := range corr {
		if corr[i].ID == req.ID {
			if req.Match != nil {
				corr[i].MatchPattern = *req.Match
			}
			if req.Replacement != nil {
				corr[i].Replacement = *req.Replacement
			}
			if req.IsRegex != nil {
				corr[i].IsRegex = *req.IsRegex
			}
			if req.Enabled != nil {
				corr[i].Enabled = *req.Enabled
			}
			break
		}
	}
	if err := c.Set(corr); err != nil {
		return fmt.Errorf("failed to save")
	}
	c.PushBias()
	return nil
}

func (c *Corrections) HandleCorrectionDelete(req *struct {
	ID string `json:"id"`
}) error {
	corr := c.Get()
	filtered := make([]Correction, 0, len(corr))
	for _, c := range corr {
		if c.ID != req.ID {
			filtered = append(filtered, c)
		}
	}
	if err := c.Set(filtered); err != nil {
		return fmt.Errorf("failed to save")
	}
	c.PushBias()
	return nil
}

// HandleCorrectionReset restores the SHIPPED rules and keeps everything else.
//
// It used to replace the whole set with the defaults, which discarded the
// user's own rules along the way — so the only recovery button in the
// corrections UI was one nobody could safely press. Provenance makes the
// narrow version possible: re-add any seeded rule that is missing or has been
// edited, leave every user and learned rule exactly where it is.
//
// There is deliberately no "delete everything" button. That is what selecting
// rows and deleting them is for, and a single click that discards work someone
// typed is not an affordance worth having.
func (c *Corrections) HandleCorrectionReset(_ *struct{}) error {
	current := c.Get()
	kept := make([]Correction, 0, len(current))
	for _, c := range current {
		if classifySource(c) != SourceSeeded {
			kept = append(kept, c)
		}
	}
	restored := append(DefaultCorrections(), kept...)
	if err := c.Set(restored); err != nil {
		return fmt.Errorf("failed to save")
	}
	c.PushBias()
	branchkit.Logf("voice", "corrections reset: %d shipped restored, %d user rules kept", len(DefaultCorrections()), len(kept))
	return nil
}

func (c *Corrections) HandleCorrectionTest(req *struct {
	Text string `json:"text"`
}) error {
	// The result reaches nobody through a method (the proxy refuses a
	// result); until a renderer shows it, the plugin log is where it lands.
	result := ApplyCorrections(req.Text, c.Get())
	branchkit.Logf("voice", "corrections test %q -> %q", req.Text, result)
	return nil
}

// NewLiteralCorrection builds a user-owned rule from a word that was misheard
// and the word it should have been.
//
// Stored as an ANCHORED regex rather than as a literal, which is a deliberate
// departure from the rule that learned corrections are literal
// substitutions. That rule's concern was machine-GENERATED
// regex logic eating unrelated text; this generates no logic. It is the typed
// word, `QuoteMeta`-escaped so no user input can be read as syntax, wrapped in
// word boundaries.
//
// Boundaries are the point. The plain-text path in `ApplyCorrections` is a
// case-insensitive SUBSTRING replace, so a rule correcting "so" would rewrite
// every "also" in everything you ever dictate. A rule created from one
// mistake must not have that blast radius.
//
// The boundary is only applied at an edge that is actually a word character —
// `\b` before "@datastar" would never match.
func NewLiteralCorrection(heard, correct string) (Correction, error) {
	heard = strings.TrimSpace(heard)
	correct = strings.TrimSpace(correct)
	if heard == "" || correct == "" {
		return Correction{}, fmt.Errorf("both the misheard text and the correction are required")
	}
	// Exact equality, NOT EqualFold. A case-only correction is among the most
	// useful kinds there is — `sherpa`→`Sherpa`, `branch kit`→`BranchKit` —
	// and folding case here rejected exactly those.
	if heard == correct {
		return Correction{}, fmt.Errorf("those are the same — nothing to correct")
	}
	return Correction{
		ID:           SimpleID(),
		MatchPattern: literalPattern(heard),
		Replacement:  correct,
		IsRegex:      true,
		Enabled:      true,
		Source:       SourceUser,
	}, nil
}

// literalPattern is the one shape a fix-a-word rule takes: the heard text,
// escaped, with a word boundary at each edge that is a word character.
// LiteralHeard reads it back, so the two must agree.
func literalPattern(heard string) string {
	runes := []rune(heard)
	pre, post := "", ""
	if isWordRune(runes[0]) {
		pre = `\b`
	}
	if isWordRune(runes[len(runes)-1]) {
		post = `\b`
	}
	return "(?i)" + pre + regexp.QuoteMeta(heard) + post
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// HandleDictationCorrect turns "it heard X, it should have been Y" into a rule.
//
// The manual precursor to learning corrections from edits, and deliberately
// shipped first: the signal is VOLUNTEERED rather than inferred, so it needs no
// accessibility read of another application's fields, no phonetic gate, no
// repeat threshold and no privacy surface at all. It also answers the question
// the automatic version cannot be designed without — how often anyone actually
// wants to correct a dictation.
func (c *Corrections) HandleDictationCorrect(req *struct {
	Heard   string `json:"heard"`
	Correct string `json:"correct"`
}) error {
	rule, err := NewLiteralCorrection(req.Heard, req.Correct)
	if err != nil {
		return err
	}
	corr := c.Get()
	for _, c := range corr {
		if c.MatchPattern == rule.MatchPattern {
			return fmt.Errorf("a rule for %q already exists", req.Heard)
		}
	}
	if err := c.Set(append(corr, rule)); err != nil {
		return fmt.Errorf("failed to save")
	}
	c.PushBias()
	return nil
}

// Repair is the dictation text pipeline's repair step: what was SAID, fixed
// by deterministic, visible rules. In order: the speak-mode
// stop-phrase backstop, the user's corrections (the hallucination filter is
// one of them), then the all-caps fix, after corrections so a blanked
// transcript is not re-cased. How the text should READ for its destination is
// the shape step's (the dictation profile), not this one's.
func (c *Corrections) Repair(text string, isSpeak bool) string {
	if isSpeak {
		text = StripSpeakPhrases(text)
	}
	text = ApplyCorrections(text, c.Get())
	if c.AllCapsFix() {
		text = FixAllCaps(text)
	}
	return text
}

// speakLeadingRe strips wake/start phrases from the beginning of transcription.
var speakLeadingRe = regexp.MustCompile(`(?i)^(?:branch\s+wake|start\s+listening|speak)[,.\s]*`)

// speakTrailingRe matches a trailing StopPhrase pair (tolerating punctuation
// between and around the words: "speak, over."). Derived from the canonical
// StopPhrases table so it cannot desync from the sherpa-side detector.
var speakTrailingRe = func() *regexp.Regexp {
	alts := make([]string, len(StopPhrases))
	for i, p := range StopPhrases {
		words := make([]string, len(p))
		for j, w := range p {
			words[j] = regexp.QuoteMeta(w)
		}
		alts[i] = strings.Join(words, `[\s.,;!?]+`)
	}
	return regexp.MustCompile(`(?i)[\s.,;!?]*(?:` + strings.Join(alts, "|") + `)[.,;!?\s]*$`)
}()

// StripSpeakPhrases removes leading wake/start phrases and trailing stop
// phrases from WhisperKit transcription output.
//
// The audio cutoff is the PRIMARY mechanism: a
// detected stop cuts the audio at the phrase onset so whisper usually never
// transcribes the phrase, and this trailing strip is then a no-op. It stays as
// a text-domain BACKSTOP for the paths the audio cut can't cover — the stop
// mis-heard by sherpa's closed grammar, the phrase split across two VAD
// segments (the adjacent-pair detector needs both words in one transcript), an
// escape close with no known onset (Option+T / toggle → nil cutoff), and the
// Linux whisper-stt stage, which does not honor cutoff_ms. The two-word
// adjacent-pair requirement keeps false strips of genuine "...over" endings
// rare. Repeat until stable so a doubled tail ("speak over speak over") clears.
func StripSpeakPhrases(text string) string {
	text = speakLeadingRe.ReplaceAllString(text, "")
	for {
		stripped := speakTrailingRe.ReplaceAllString(text, "")
		if stripped == text {
			break
		}
		text = stripped
	}
	return strings.TrimSpace(text)
}

// FixAllCaps rewrites a transcript that is entirely upper-case into sentence
// case. WhisperKit emits all-caps for some short utterances, and pasting
// "OPEN THE FILE" when someone said it normally is wrong every time.
//
// A transformation, not a find/replace, so it is a postprocess step rather
// than a seeded correction — the corrections system matches and substitutes
// text it can name in advance, and this one cannot be written down as a rule.
// It moved out of the WhisperKit stage with the hallucination filter: both
// were text policy living in a transducer.
//
// Only fires when EVERY letter is upper-case, so deliberate acronyms inside a
// sentence ("send the PDF") are untouched. A transcript that is all-caps
// because the speaker spelled an acronym alone ("PDF") is the accepted miss —
// it was the same before the move.
func FixAllCaps(text string) string {
	hasLetter := false
	for _, r := range text {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return text
			}
		}
	}
	if !hasLetter {
		return text
	}
	var b strings.Builder
	capitalizeNext := true
	for _, r := range strings.ToLower(text) {
		if capitalizeNext && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
			capitalizeNext = false
			continue
		}
		b.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			capitalizeNext = true
		}
	}
	return b.String()
}
