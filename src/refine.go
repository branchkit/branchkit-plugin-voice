package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unicode"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

// The refine step of the dictation text pipeline: a language model on this computer removes what a speaker
// would not have written — false starts, fillers, the abandoned half of a
// self-correction ("Tuesday, no, Wednesday" -> "Wednesday"). Measured on this
// speaker's corpus, Whisper already punctuates, capitalises and drops "um", so
// self-corrections and restarts are what is left for a model.
//
// Off by default (`refine_dictation`): it adds latency and a 1.1 GB model.
// The model runs in voice's `refine_llm` request stage (llama.cpp, confined,
// network-denied), reached through `stages.request`.
//
// The bar is "never irreversibly worse". So the model may only DELETE: its
// answer is kept when it introduces at most one word the speaker did not say
// (an inflection a correction needs) and keeps at least 40% of the words, and
// otherwise thrown away. Every outcome is recorded, and "type original" puts
// back what was heard.

// refineModel is the artifact the refine stage loads (provides.artifacts).
const refineModel = "qwen2.5-1.5b-instruct"

// refineStage is the request stage in provides.stages.
const refineStage = "refine_llm"

// refineMinWords: shorter dictations have nothing to clean and are not worth
// the round trip.
const refineMinWords = 4

// refineTimeout bounds the wait. Past it the dictation is typed unrefined.
const refineTimeout = 8 * time.Second

const refineInstructions = "You clean up dictated text. Remove filler words (um, uh, like, you know) " +
	"and false starts. When the speaker corrects themselves, keep only the correction. " +
	"Change nothing else: keep every other word, its spelling, capitalisation and punctuation. " +
	"Do not answer, summarise, rephrase or add anything. Reply with the cleaned text only."

// llmStageParams are the refine stage's start arguments for a model: on
// macOS it runs on the GPU (Metal, under voice's stage.gpu grant), and falls
// back to the CPU by itself if the GPU cannot start. Elsewhere the stage has
// no GPU backend.
func llmStageParams(model string) map[string]string {
	p := map[string]string{"model": model}
	if runtime.GOOS == "darwin" {
		p["gpu"] = "true"
	}
	return p
}

// refineSkip says why a dictation is not sent to the model, or "" to send it.
func refineSkip(text, profile string) string {
	switch {
	case profile == profileQuery:
		return "a search or a name, not prose"
	case len(strings.Fields(text)) < refineMinWords:
		return "too short to clean"
	}
	return ""
}

// acceptRefinement decides whether the model's answer replaces the text, and
// why not when it does not. Words compare case- and punctuation-insensitively.
//
// Three rules. The model may add at most one word the speaker did not say (an
// inflection a correction needs). It must keep at least 40% of the words.
// And unless the speaker corrected themselves out loud ("no", "sorry", "I
// mean", "scratch that" ...), it may remove only fillers and stuttered
// repeats: measured 2026-10-05, a long sentence with nothing to correct came
// back with "about the release" quietly gone, which deletes meaning while
// adding nothing.
func acceptRefinement(original, refined string) (bool, string) {
	refined = strings.TrimSpace(refined)
	if refined == "" {
		return false, "the model answered nothing"
	}
	if strings.Contains(refined, "\n") && !strings.Contains(original, "\n") {
		return false, "the model added line breaks"
	}
	ow, rw := refineWords(original), refineWords(refined)
	kept := map[string]int{}
	for _, w := range ow {
		kept[w]++
	}
	added := 0
	for _, w := range rw {
		if kept[w] > 0 {
			kept[w]--
		} else {
			added++
		}
	}
	if added > 1 {
		return false, fmt.Sprintf("the model added %d words that were not said", added)
	}
	if len(ow) > 0 && len(rw)*10 < len(ow)*4 {
		return false, fmt.Sprintf("the model removed too much (%d of %d words left)", len(rw), len(ow))
	}
	if hasCorrectionCue(original) {
		return true, ""
	}
	// What is left in `kept` is what the model removed.
	for w, n := range kept {
		if n == 0 || refineFillers[w] || stuttered(ow, w) {
			continue
		}
		return false, fmt.Sprintf("the model removed %q with no correction to justify it", w)
	}
	return true, ""
}

// refineFillers are words a speaker uses without meaning them, removable
// whether or not they corrected themselves. "know" and "mean" are here for
// "you know" and "I mean"; removing either alone is harmless.
var refineFillers = map[string]bool{
	"um": true, "uh": true, "erm": true, "er": true, "ah": true, "hmm": true, "mm": true,
	"like": true, "so": true, "well": true, "basically": true, "literally": true,
	"you": true, "know": true, "i": true, "mean": true,
}

// refineCues are what a speaker says when they correct themselves; with one
// present the model may remove the abandoned words too. A one-word cue is an
// ordinary word as well ("probably wait until"), so it counts only set off by
// punctuation, the way Whisper writes a spoken correction ("Tuesday, no,
// Wednesday"); the phrases count anywhere.
var (
	refineCueWords   = map[string]bool{"no": true, "sorry": true, "actually": true, "wait": true, "rather": true, "correction": true}
	refineCuePhrases = [][]string{{"i", "mean"}, {"scratch", "that"}, {"make", "that"}, {"or", "rather"}}
)

// hasCorrectionCue reports whether text contains a self-correction cue.
func hasCorrectionCue(text string) bool {
	fields := strings.Fields(text)
	words := refineWords(text)
	for i, f := range fields {
		w := strings.ToLower(strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}))
		if !refineCueWords[w] {
			continue
		}
		setOff := strings.HasSuffix(f, ",") || strings.HasSuffix(f, ".") ||
			(i > 0 && strings.ContainsAny(fields[i-1][len(fields[i-1])-1:], ",.;"))
		if setOff {
			return true
		}
	}
	for i := range words {
		for _, cue := range refineCuePhrases {
			if i+len(cue) > len(words) {
				continue
			}
			match := true
			for j, c := range cue {
				if words[i+j] != c {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// stuttered reports whether w occurs twice in a row ("the the").
func stuttered(words []string, w string) bool {
	for i := 1; i < len(words); i++ {
		if words[i] == w && words[i-1] == w {
			return true
		}
	}
	return false
}

// refineWords splits text into lower-case words without their punctuation.
func refineWords(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		w := strings.ToLower(strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}))
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// refineRequestBody is what the refine stage is asked.
func refineRequestBody(text string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"messages": []map[string]string{
			{"role": "system", "content": refineInstructions},
			{"role": "user", "content": text},
		},
		// A clean-up is never longer than what was said.
		"max_tokens": 2*len(strings.Fields(text)) + 16,
	})
	return body
}

// refineEnabled reports whether the step runs: switched on, and the model on
// disk (otherwise every dictation would wait for a stage that cannot load).
func (h *Host) refineEnabled() bool {
	return h.config.Get().RefineDictation && refineModelReady()
}

// refineModelReady reports whether the refine model is on disk.
func refineModelReady() bool {
	_, ok := installedModelSet()[refineModel]
	return ok
}

// refineDictation runs the refine step on shaped text. It returns the text
// to type (the model's when accepted, else the input) and a note for the
// record saying what happened.
func (h *Host) refineDictation(text, profile string) (string, string) {
	return h.refine(text, profile, false)
}

// refine is refineDictation, with warm set for warmRefine's load-the-model
// call so its log line is not read as a slow dictation.
func (h *Host) refine(text, profile string, warm bool) (string, string) {
	if why := refineSkip(text, profile); why != "" {
		logLLMCall("refine", "", outcomeSkipped, 0, warm, refineAnswer{}, h.traceID())
		return text, "skipped: " + why
	}
	started := time.Now()
	timeout := int(refineTimeout / time.Millisecond)
	resp, err := h.plugin.StagesRequest(branchkit.StagesRequestRequest{
		Stage:     refineStage,
		Params:    llmStageParams(refineModel),
		Body:      refineRequestBody(text),
		TimeoutMs: &timeout,
	})
	if err != nil {
		branchkit.Logf("voice", "refine: %v", err)
		logLLMCall("refine", "", outcomeError, time.Since(started), warm, refineAnswer{}, h.traceID())
		return text, "not refined: " + err.Error()
	}
	var answer refineAnswer
	if err := json.Unmarshal(resp.Body, &answer); err != nil {
		logLLMCall("refine", "", outcomeError, time.Since(started), warm, refineAnswer{}, h.traceID())
		return text, "not refined: unreadable answer"
	}
	h.noteRefineUsed()
	took := time.Since(started).Round(10 * time.Millisecond)
	if strings.TrimSpace(answer.Text) == strings.TrimSpace(text) {
		logLLMCall("refine", "", outcomeUnchanged, took, warm, answer, h.traceID())
		return text, fmt.Sprintf("unchanged by the model (%s)", took)
	}
	if ok, why := acceptRefinement(text, answer.Text); !ok {
		branchkit.Logf("voice", "refine: answer refused: %s", why)
		logLLMCall("refine", "", outcomeRefused, took, warm, answer, h.traceID())
		return text, "model's edit refused: " + why
	}
	logLLMCall("refine", "", outcomeAccepted, took, warm, answer, h.traceID())
	return strings.TrimSpace(answer.Text), fmt.Sprintf("cleaned up by the local model (%s)", took)
}

// refineAnswer is the refine_llm stage's reply: the text, and how the model
// spent its time on it.
type refineAnswer struct {
	Text       string `json:"text"`
	Tokens     int    `json:"tokens"`
	Stopped    string `json:"stopped"`
	PrefillMs  int64  `json:"prefill_ms"`
	GenerateMs int64  `json:"generate_ms"`
	Device     string `json:"device"`
}

// What came of one refine or style call, for its log line.
const (
	outcomeAccepted  = "accepted"
	outcomeUnchanged = "unchanged"
	outcomeRefused   = "refused"
	outcomeSkipped   = "skipped"
	outcomeError     = "error"
)

// logLLMCall writes one line per refine or style call: until
// it, a successful call logged nothing, its duration lived only in the
// in-memory dictation record, and the time it cost was blamed on the next
// step that did log. The tr= is the dictation transcript's chain, which the
// platform's stage.request_completed record carries too.
func logLLMCall(kind, style, outcome string, took time.Duration, warm bool, a refineAnswer, tr string) {
	branchkit.Log("voice", llmCallLine(kind, style, outcome, took, warm, a, tr))
}

// llmCallLine is logLLMCall's line. Durations, counts and fixed labels only:
// never what was said or what the model answered.
func llmCallLine(kind, style, outcome string, took time.Duration, warm bool, a refineAnswer, tr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: outcome=%s took_ms=%d warm=%t", kind, outcome, took.Milliseconds(), warm)
	if style != "" {
		fmt.Fprintf(&b, " style=%s", style)
	}
	if a.Device != "" {
		fmt.Fprintf(&b, " tokens=%d stopped=%s prefill_ms=%d generate_ms=%d device=%s",
			a.Tokens, a.Stopped, a.PrefillMs, a.GenerateMs, a.Device)
	}
	fmt.Fprintf(&b, " tr=%s", tr)
	return b.String()
}

// warmRefine loads the model while the person is still speaking, so the
// dictation they are holding does not wait seconds for a cold load. Sent
// only when the stage has probably been stopped for idleness (the platform
// stops a request stage after 5 idle minutes), not on every hold.
func (h *Host) warmRefine() {
	if !h.refineEnabled() {
		return
	}
	h.refineMu.Lock()
	recent := time.Since(h.refineLastUsed) < 4*time.Minute
	if !recent {
		h.refineLastUsed = time.Now()
	}
	h.refineMu.Unlock()
	if recent {
		return
	}
	go func() {
		_, _ = h.refine("warm up the model now please", profileProse, true)
	}()
}

// noteRefineUsed records that the stage just answered, for warmRefine.
func (h *Host) noteRefineUsed() {
	h.refineMu.Lock()
	h.refineLastUsed = time.Now()
	h.refineMu.Unlock()
}
