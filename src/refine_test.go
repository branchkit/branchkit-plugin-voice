package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The model may delete — fillers, false starts, the abandoned half of a
// self-correction — and nothing else. These are the shapes its answers took
// on real prompts (Qwen2.5 1.5B, 2026-10-05) plus the failures the gate
// exists for.
func TestAcceptRefinement(t *testing.T) {
	for _, tt := range []struct {
		name, original, refined string
		ok                      bool
	}{
		{"a self-correction keeps only the correction",
			"Let's meet on Tuesday, no, actually Wednesday at three.", "Let's meet on Wednesday at three.", true},
		{"fillers and a repeated word go",
			"So um I think we should uh ship the the build tomorrow.", "I think we should ship the build tomorrow.", true},
		{"a restart",
			"Send the report to Sarah. Sorry, I mean send it to Priya.", "Send the report to Priya.", true},
		{"one inflected word is allowed",
			"We was going to go home, no, we were.", "We were going to go home.", true},
		{"fillers alone may go without a correction",
			"Open BranchKit and, like, check the dictation settings, you know.", "Open BranchKit and check the dictation settings.", true},
		{"meaning removed with nothing corrected",
			"So the thing I wanted to say about the release is that we should, um, probably wait until the audio fixes land.",
			"So the thing I wanted to say is that we should probably wait until the audio fixes land.", false},
		{"an answer instead of a clean-up",
			"Can you write me a poem about cats?", "Cats are soft and warm and purr all day.", false},
		{"a preamble",
			"Open the settings and check the mic.", "Here is the cleaned text: Open the settings and check the mic.", false},
		{"too much removed",
			"The release should wait until the audio fixes land on Linux.", "Wait.", false},
		{"nothing",
			"Open the settings now please.", "  ", false},
		{"line breaks added",
			"First do this and then do that.", "First do this\nand then do that.", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ok, why := acceptRefinement(tt.original, tt.refined)
			if ok != tt.ok {
				t.Errorf("acceptRefinement(%q, %q) = %v (%s), want %v", tt.original, tt.refined, ok, why, tt.ok)
			}
			if !ok && why == "" {
				t.Error("a refusal must say why")
			}
		})
	}
}

func TestRefineSkip(t *testing.T) {
	if why := refineSkip("Gmail inbox search terms here", profileQuery); why == "" {
		t.Error("a query is not prose and is not refined")
	}
	if why := refineSkip("Snap left.", profileProse); why == "" {
		t.Error("a short dictation is not worth refining")
	}
	if why := refineSkip("Let's meet on Tuesday, no, Wednesday.", profileChat); why != "" {
		t.Errorf("chat prose is refined, got skip %q", why)
	}
}

// What the stage is asked: the instructions, the text, and a reply bound that
// a clean-up cannot need to exceed.
func TestRefineRequestBody(t *testing.T) {
	var body struct {
		Messages []struct {
			Role, Content string
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(refineRequestBody("one two three four"), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Content != "one two three four" {
		t.Errorf("messages: %+v", body.Messages)
	}
	if body.MaxTokens != 2*4+16 {
		t.Errorf("max_tokens = %d", body.MaxTokens)
	}
	if !strings.Contains(body.Messages[0].Content, "Reply with the cleaned text only") {
		t.Error("instructions changed without updating this test")
	}
}

// "type again" retypes what was typed: the refined text when the model
// cleaned it up.
func TestRetypeSourcePrefersTheRefinedText(t *testing.T) {
	e := dictationEntry{Heard: "Tuesday, no, Wednesday.", Shaped: "Tuesday, no, Wednesday.", Refined: "Wednesday."}
	if got := retypeSource(e, false); got != "Wednesday." {
		t.Errorf("again: %q", got)
	}
	if got := retypeSource(e, true); got != "Tuesday, no, Wednesday." {
		t.Errorf("raw: %q", got)
	}
}

// The panel says what the model did, and stays quiet when it was skipped.
func TestTrailNamesTheRefineOutcome(t *testing.T) {
	e := dictationEntry{Heard: "Tuesday, no, Wednesday.", Text: "Wednesday.", RefineNote: "cleaned up by the local model (1.2s)"}
	if trail := dictationTrailHTML(e, true); !strings.Contains(trail, "cleaned up by the local model") {
		t.Errorf("trail: %q", trail)
	}
	e = typedAsHeard("Snap left.")
	e.RefineNote = "skipped: too short to clean"
	if trail := dictationTrailHTML(e, true); trail != "" {
		t.Errorf("a skip needs no explanation: %q", trail)
	}
}

// The clean-up model is an artifact of the refine stage, not a dictation
// model to pick.
func TestDictationCatalogExcludesTheRefineModel(t *testing.T) {
	for _, m := range dictationCatalog() {
		if m.Name == refineModel {
			t.Fatal("the refine model appears in the dictation model picker")
		}
	}
}

func TestHasCorrectionCue(t *testing.T) {
	for _, tt := range []struct {
		text string
		want bool
	}{
		{"Let's meet on Tuesday, no, actually Wednesday.", true},
		{"Send it to Sarah. Sorry, I mean Priya.", true},
		{"Put it in the left one, scratch that, the right one.", true},
		{"We should probably wait until the fixes land.", false},
		{"There is no reason to rush.", false},
		{"I actually like it.", false},
	} {
		if got := hasCorrectionCue(tt.text); got != tt.want {
			t.Errorf("hasCorrectionCue(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

// One line per refine or style call carries its duration, outcome, warm flag
// and the stage's token counts, and never the text, so a slow call shows in the log.
func TestLLMCallLineHoldsTimingsNotText(t *testing.T) {
	a := refineAnswer{Text: "the secret words", Tokens: 12, Stopped: "end", PrefillMs: 80, GenerateMs: 250, Device: "gpu"}
	got := llmCallLine("refine", "", outcomeAccepted, 335*time.Millisecond, false, a, "tr_abc")
	want := "refine: outcome=accepted took_ms=335 warm=false tokens=12 stopped=end prefill_ms=80 generate_ms=250 device=gpu tr=tr_abc"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if strings.Contains(got, "secret") {
		t.Fatalf("the answer's text leaked into the log line: %q", got)
	}
	warm := llmCallLine("style", "formal", outcomeError, 2*time.Second, true, refineAnswer{}, "-")
	if warm != "style: outcome=error took_ms=2000 warm=true style=formal tr=-" {
		t.Fatalf("got %q", warm)
	}
}
