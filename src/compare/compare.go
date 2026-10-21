// Package compare scores command models on the user's own confirmed
// recordings: each clip re-decoded by each downloaded model through the live
// structured grammar, the verdict kept per (model, model version, grammar,
// clip) so a re-run decodes only what is missing. Models are
// only ever compared under the same grammar: a verdict from before a grammar
// change is history, not a result.
//
// A verdict means what `just voice-regress` means by one: right when the
// decoded text equals the clip's target, lowercased and with whitespace
// collapsed; a target the current grammar cannot produce is skipped, not
// counted wrong, since no model could decode it.
package compare

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Clip is one confirmed recording.
type Clip struct {
	File   string // corpus-relative path, the clip's identity
	Audio  string // path relative to the plugin's data dir, for the re-decode
	Target string // what was said
}

// Result is one model's verdict on one clip.
type Result struct {
	Model   string `json:"model"`
	Version string `json:"version"` // hash of the model file, from the re-decode
	Grammar string `json:"grammar"` // GrammarID of the grammar decoded under
	File    string `json:"file"`
	Target  string `json:"target"`
	Heard   string `json:"heard"`
	Right   bool   `json:"right"`
	// DecodeMs is the recognizer's time on this clip, model load excluded.
	DecodeMs float64 `json:"decode_ms"`
	// Weakest is the least certain decoded word's margin, absent when the
	// recognizer reported none.
	Weakest *float64 `json:"weakest,omitempty"`
	// Error is the re-decode's failure for this clip, when it failed.
	Error string `json:"error,omitempty"`
	// Errors and Words score a dictation verdict: word edits from the
	// target to what was heard, and the target's word count (WordErrors).
	// Zero for a command verdict, which is right or wrong as a whole.
	Errors int   `json:"errors,omitempty"`
	Words  int   `json:"words,omitempty"`
	At     int64 `json:"at"` // unix seconds
}

// Normalize is voice-regress's comparison form: lowercased, whitespace
// collapsed.
func Normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Right reports whether heard is target.
func Right(heard, target string) bool {
	return Normalize(heard) == Normalize(target)
}

// Weakest is the smallest margin, or nil when there are none.
func Weakest(margins []float64) *float64 {
	if len(margins) == 0 {
		return nil
	}
	w := margins[0]
	for _, m := range margins[1:] {
		if m < w {
			w = m
		}
	}
	return &w
}

// Store is the results file: one JSON Result per line, appended.
type Store struct{ Path string }

// Key identifies a verdict: the same clip under a changed model file or a
// changed grammar is a new verdict, the old one stays as history.
func Key(model, version, grammar, file string) string {
	return model + "\x00" + version + "\x00" + grammar + "\x00" + file
}

// GrammarID names a grammar by its content: a hash of the live grammar export
// (`pipelines.grammar` full, the payload the re-decode is stamped from).
func GrammarID(vocabularyUpdate []byte) string {
	sum := sha256.Sum256(vocabularyUpdate)
	return hex.EncodeToString(sum[:8])
}

// Load reads every stored verdict, the last one written winning per key. A
// missing or unreadable file is no verdicts yet.
func (s Store) Load() map[string]Result {
	out := map[string]Result{}
	f, err := os.Open(s.Path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Result
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Model != "" && r.File != "" {
			out[Key(r.Model, r.Version, r.Grammar, r.File)] = r
		}
	}
	return out
}

// Append writes results to the end of the file.
func (s Store) Append(results []Result) error {
	if len(results) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, r := range results {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	return w.Flush()
}

// Missing is the clips with no stored verdict for (model, version, grammar),
// in order.
func Missing(clips []Clip, model, version, grammar string, have map[string]Result) []Clip {
	var out []Clip
	for _, c := range clips {
		if _, ok := have[Key(model, version, grammar, c.File)]; !ok {
			out = append(out, c)
		}
	}
	return out
}

// Summary is one model's totals over the clips compared.
type Summary struct {
	Model    string  `json:"model"`
	Version  string  `json:"version"`
	Right    int     `json:"right"`
	Total    int     `json:"total"`
	MedianMs float64 `json:"median_ms"`
	Failed   int     `json:"failed"` // re-decodes that errored, counted wrong
}

// Summarize totals model's verdicts (at version, under grammar) over clips. A
// clip without a verdict is left out of the total, so a partly run comparison
// reads as what it covers.
func Summarize(clips []Clip, model, version, grammar string, have map[string]Result) Summary {
	s := Summary{Model: model, Version: version}
	var ms []float64
	for _, c := range clips {
		r, ok := have[Key(model, version, grammar, c.File)]
		if !ok {
			continue
		}
		s.Total++
		if r.Right {
			s.Right++
		}
		if r.Error != "" {
			s.Failed++
			continue
		}
		ms = append(ms, r.DecodeMs)
	}
	if len(ms) > 0 {
		sort.Float64s(ms)
		s.MedianMs = ms[len(ms)/2]
	}
	return s
}

// Disagreement is a clip the models heard differently, for the side-by-side.
type Disagreement struct {
	Clip  Clip              `json:"clip"`
	Heard map[string]string `json:"heard"` // model -> text
	Right map[string]bool   `json:"right"` // model -> verdict
}

// Disagreements is every clip where the models' texts differ under grammar, in
// clip order. versions names the version of each model to read; a model with
// no verdict on a clip leaves the clip out (nothing to compare yet).
func Disagreements(clips []Clip, versions map[string]string, grammar string, have map[string]Result) []Disagreement {
	return DisagreementsBy(clips, versions, grammar, have, Normalize)
}

// DictationForm is how dictation texts are compared for disagreement: by
// their words, so two models that differ only in punctuation or case agree.
func DictationForm(s string) string {
	return strings.Join(DictationWords(s), " ")
}

// DisagreementsBy is Disagreements with the comparison form given: Normalize
// for commands, DictationForm for dictation.
func DisagreementsBy(clips []Clip, versions map[string]string, grammar string, have map[string]Result, form func(string) string) []Disagreement {
	models := make([]string, 0, len(versions))
	for m := range versions {
		models = append(models, m)
	}
	sort.Strings(models)
	var out []Disagreement
	for _, c := range clips {
		d := Disagreement{Clip: c, Heard: map[string]string{}, Right: map[string]bool{}}
		complete := true
		for _, m := range models {
			r, ok := have[Key(m, versions[m], grammar, c.File)]
			if !ok {
				complete = false
				break
			}
			d.Heard[m] = r.Heard
			d.Right[m] = r.Right
		}
		if !complete || len(models) < 2 {
			continue
		}
		first := form(d.Heard[models[0]])
		for _, m := range models[1:] {
			if form(d.Heard[m]) != first {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// WordSummary is one dictation model's totals over the recordings compared.
type WordSummary struct {
	Model    string  `json:"model"`
	Version  string  `json:"version"`
	Words    int     `json:"words"`  // reference words, over the recordings with a verdict
	Errors   int     `json:"errors"` // word edits, a failed decode counting every word
	Total    int     `json:"total"`  // recordings with a verdict
	MedianMs float64 `json:"median_ms"`
	Failed   int     `json:"failed"`
}

// SummarizeWords totals model's dictation verdicts (at version, under
// grammar) over clips, as Summarize does for commands.
func SummarizeWords(clips []Clip, model, version, grammar string, have map[string]Result) WordSummary {
	s := WordSummary{Model: model, Version: version}
	var ms []float64
	for _, c := range clips {
		r, ok := have[Key(model, version, grammar, c.File)]
		if !ok {
			continue
		}
		s.Total++
		s.Words += r.Words
		if r.Error != "" {
			s.Failed++
			s.Errors += r.Words
			continue
		}
		s.Errors += r.Errors
		ms = append(ms, r.DecodeMs)
	}
	if len(ms) > 0 {
		sort.Float64s(ms)
		s.MedianMs = ms[len(ms)/2]
	}
	return s
}
