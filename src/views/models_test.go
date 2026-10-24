package views

import (
	"context"
	"strings"
	"testing"
)

func renderModels(t *testing.T, data ModelsViewData) string {
	t.Helper()
	var b strings.Builder
	if err := ModelsTab(data).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Before a comparison the tab offers one; each card names its model and
// license, the model in use carries the badge, and another downloaded model
// offers Use.
func TestModelsTabBeforeComparing(t *testing.T) {
	html := renderModels(t, ModelsViewData{Cards: []ModelCard{
		{Name: "std", Label: "Standard", Title: "NeMo FastConformer-CTC large", License: "CC BY 4.0", Source: "https://example.org/std", Downloaded: true, InUse: true},
		{Name: "light", Label: "Light", Title: "NeMo Conformer-CTC small", License: "CC BY 4.0", Source: "https://example.org/light", Downloaded: true},
		{Name: "gone", Label: "Other", Title: "Not downloaded", License: "CC BY 4.0", Source: "https://example.org/x", Ref: "voice/gone"},
	}})
	for _, want := range []string{"Compare on my voice", "command_models/compare", "In use", "NeMo FastConformer-CTC large", "CC BY 4.0", "example.org/std", "Use this model", "voice/gone"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "right<") {
		t.Error("a score shown before any comparison")
	}
	// The sandboxed tab frame cannot open a window: no link that would do nothing.
	if strings.Contains(html, "target=\"_blank\"") || strings.Contains(html, "href=\"https://example.org") {
		t.Error("the license source rendered as a link the frame cannot open")
	}
}

// While it runs the tab shows its progress and no button; after, the best
// model is marked and its Use button is primary, and the disagreement rows
// play the clip and show what each model heard.
func TestModelsTabRunningAndCompared(t *testing.T) {
	running := renderModels(t, ModelsViewData{Running: true, Progress: "Listening with Light: 50 of 120 recordings"})
	if !strings.Contains(running, "Listening with Light: 50 of 120 recordings") || strings.Contains(running, "command_models/compare") {
		t.Errorf("running state: %s", running)
	}
	html := renderModels(t, ModelsViewData{
		Compared: true, Clips: 12, Skipped: 1,
		Cards: []ModelCard{
			{Name: "std", Label: "Standard", Title: "Std", Downloaded: true, Scored: true, Best: true, Right: 10, Total: 11, MedianMs: "11 ms"},
			{Name: "light", Label: "Light", Title: "Light", Downloaded: true, InUse: true, Scored: true, Right: 8, Total: 11, MedianMs: "7 ms"},
		},
		Columns:       []string{"Standard", "Light"},
		Disagreements: []DisagreementRow{{Said: "snap left", AudioURL: "plugins/voice/data?file=x.wav", Cells: []HeardCell{{Heard: "snap left", Right: true}, {Heard: "snap eight"}}}},
	})
	for _, want := range []string{"Compared on 11 confirmed recordings", "1 left out", "Run again", "Best on your voice", `class="small primary"`, "11 ms per recording", "Where they disagree · 1", `src="plugins/voice/data?file=x.wav"`, "snap eight"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The dictation and clean-up models are listed with their licenses and
// sources; the chosen dictation model not yet downloaded says so.
func TestModelsTabListsDictationModels(t *testing.T) {
	html := renderModels(t, ModelsViewData{
		Dictation: []ModelCard{
			{Name: "large", Label: "Large", Title: "Dictation — most accurate", License: "MIT", Source: "huggingface.co/argmaxinc/whisperkit-coreml", Chosen: true, Ref: "voice/large"},
			{Name: "small", Label: "Small", Title: "Dictation — balanced", License: "MIT", Source: "huggingface.co/argmaxinc/whisperkit-coreml", Downloaded: true},
		},
		Helpers: []ModelCard{{Name: "qwen", Label: "Qwen", Title: "Dictation clean-up", License: "Apache-2.0", Source: "huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF", Downloaded: true}},
	})
	for _, want := range []string{">Dictation<", "Chosen · not downloaded", "voice/large", "model/set", "MIT", "huggingface.co/argmaxinc/whisperkit-coreml", "Dictation clean-up and styles", "Apache-2.0", "Downloaded"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Say a phrase: listening says what to do and that nothing runs; a phrase
// said just now shows each model's hearing and offers them as answers, a
// typed answer and Discard.
func TestModelsTabSayNow(t *testing.T) {
	html := renderModels(t, ModelsViewData{SayNow: SayNowView{Armed: true, Key: "opt+g"}})
	if !strings.Contains(html, "Hold opt+g and say one command: it won") || !strings.Contains(html, "say_now/stop") {
		t.Errorf("armed: %s", html)
	}
	html = renderModels(t, ModelsViewData{SayNow: SayNowView{
		Columns: []string{"Standard", "Light"},
		Rows: []SayNowRow{{File: "saynow-1/seg_0.wav", AudioURL: "plugins/voice/data?file=a.wav",
			Heard: []string{"snap right", ""}, Choices: []string{"snap right"}, Signal: "sayNowOther0"}},
	}})
	for _, want := range []string{"Say a phrase", "say_now/start", "Said just now", "nothing", "Which was right?", "say_now/confirm", `data-bind="sayNowOther0"`, "$sayNowOther0", "say_now/dismiss", "Discard"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Read a sentence shows the sentence to read and that it won't be typed; a
// recording waits with Keep and Misread; once compared, the differing
// sentences show each model's text and its word errors.
func TestModelsTabDictationCompare(t *testing.T) {
	armed := renderModels(t, ModelsViewData{Dictation: []ModelCard{{Name: "large"}}, DictationCompare: DictationCompareView{Armed: true, Key: "opt+t", Sentence: "Hello there."}})
	if !strings.Contains(armed, "Hold opt+t and read: “Hello there.” It won") || !strings.Contains(armed, "read_sentence/stop") {
		t.Errorf("armed: %s", armed)
	}
	html := renderModels(t, ModelsViewData{
		Dictation: []ModelCard{{Name: "large", Title: "Large", WordsPct: "97%", MedianMs: "1.1 s", Best: true, Downloaded: true}, {Name: "small", Title: "Small", Downloaded: true, InUse: true}},
		DictationCompare: DictationCompareView{
			Recorded: 4, Compared: true, Columns: []string{"Large", "Small"},
			Pending: []ReadClipRow{{File: "dictation-recordings/s/2.wav", AudioURL: "plugins/voice/data?file=x", Sentence: "Goodbye."}},
			Rows:    []DictationRow{{Sentence: "It is a fixed set.", AudioURL: "plugins/voice/data?file=y", Cells: []DictationCell{{Heard: "It is a fixed set."}, {Heard: "It is a fix set.", Errors: 1}}}},
		},
	})
	for _, want := range []string{"4 sentences recorded", "read_sentence/start", "read_sentence/keep", "read_sentence/discard", "dictation_models/compare", "Run again", "97%", "of words right", "1.1 s per sentence", "Best on your voice", "Where they differ · 1", "1 wrong", "It is a fix set."} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}
