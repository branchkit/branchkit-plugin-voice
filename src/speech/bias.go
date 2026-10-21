package speech

import (
	"encoding/json"
	"github.com/branchkit/plugin-sdk-go"
	"sort"
	"strings"
	"unicode"
)

// Decoder biasing: the same vocabulary the corrections table already names,
// wired one layer EARLIER. The STT stage boosts these words' tokens in a
// logits filter; the decoder prompt is not used (measured and
// rejected).
//
// A correction is a repair — Whisper mishears "QuickBase" as "quick base" and
// a regex patches the string afterwards. The words it repairs toward are
// exactly the words the decoder should have been biased toward in the first
// place, so we send them to the STT stage as configuration. The regex stays as
// backstop: biasing raises the odds of the right token, it does not guarantee
// it, and it cannot reach a word mangled into something no pattern matches.
//
// Sent on the platform's config channel (`pipelines.inject`), which retains the
// last list and replays it when the dictation pipeline starts — so a stage
// respawn does not quietly drop the bias.
const (
	biasPipeline  = "dictation"
	biasStage     = "_platform.stt"
	biasEventType = "ext.branchkit.dictation_bias"
)

// BiasVocabulary derives the bias vocabulary from the correction set.
//
// The REPLACEMENT side is what matters: it is the spelling we want produced.
// Match patterns are regexes describing what Whisper gets wrong, and are
// meaningless to a decoder.
func BiasVocabulary(corr []Correction) []string {
	var targets []string
	for _, c := range corr {
		if c.Enabled {
			targets = append(targets, c.Replacement)
		}
	}
	return biasSet(targets)
}

// BiasWords is everything the decoder is biased toward: the user's
// vocabulary list and the correction targets, as one set.
func BiasWords(corr []Correction, vocab []string) []string {
	return biasSet(append(BiasVocabulary(corr), vocab...))
}

// biasSet normalises candidate words into the payload the stage receives.
func biasSet(words []string) []string {
	seen := make(map[string]bool, len(words))
	var out []string
	for _, w := range words {
		// The hallucination filter replaces with the empty string; it deletes
		// rather than names a word, so it contributes no vocabulary.
		w = strings.TrimSpace(w)
		// Leading sigils are routing syntax, not sounds — "@datastar" and
		// "/commit" are typed decorations around words the user actually says.
		// Bias toward the word; the correction still adds the sigil.
		w = strings.TrimLeft(w, "@/#")
		if w == "" || seen[w] {
			continue
		}
		// Anything with no letter at all cannot be spoken into existence.
		if !strings.ContainsFunc(w, unicode.IsLetter) {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	// Stable order so an unchanged set produces an unchanged payload and the
	// stage's own change check stays meaningful.
	sort.Strings(out)
	return out
}

// PushBias sends the current vocabulary to the dictation STT stage as
// configuration, so Whisper is nudged toward the right spelling instead of
// only being patched after mishearing it. The platform retains the last
// list and replays it at pipeline start, so this fires on change only. Not
// fatal on failure: biasing is a quality improvement, and dictation keeps
// working unbiased — logged because a silently unbiased decoder is exactly
// the thing that is hard to notice from the output.
func (c *Corrections) PushBias() {
	words := BiasWords(c.Get(), c.Vocabulary())
	payload, err := BiasPayload(words)
	if err != nil {
		branchkit.Logf("voice", "bias vocabulary: marshal failed: %v", err)
		return
	}
	pipelineName, stageName, eventType := BiasTarget()
	if _, err := c.plugin.PipelinesInject(branchkit.PipelinesInjectRequest{EventType: eventType, Name: pipelineName, Stage: stageName, Data: payload}); err != nil {
		branchkit.Logf("voice", "bias vocabulary: push failed: %v", err)
		return
	}
	branchkit.Logf("voice", "bias vocabulary: pushed %d words", len(words))
}

// BiasPayload is the wire body for the config event.
func BiasPayload(words []string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"words": words})
}

// BiasTarget reports where the vocabulary is sent — pipeline, stage, event type.
func BiasTarget() (string, string, string) {
	return biasPipeline, biasStage, biasEventType
}
