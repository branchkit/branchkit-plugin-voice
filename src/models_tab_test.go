package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/compare"
)

func testRef(m string) string { return "voice/" + m }
func testBytes(string) string { return "1 MB" }

// The best on this voice is the most right as a share, then the fastest, and
// only once two models were scored.
func TestBestModel(t *testing.T) {
	r := &CompareReport{Models: []compare.Summary{
		{Model: "std", Right: 9, Total: 10, MedianMs: 12},
		{Model: "light", Right: 9, Total: 10, MedianMs: 7},
	}}
	if got := bestModel(r); got != "light" {
		t.Errorf("a tie in right goes to the faster model, got %q", got)
	}
	r.Models[0].Right = 10
	if got := bestModel(r); got != "std" {
		t.Errorf("the most right wins, got %q", got)
	}
	if got := bestModel(&CompareReport{Models: r.Models[:1]}); got != "" {
		t.Errorf("one model scored has no best, got %q", got)
	}
	if bestModel(nil) != "" {
		t.Error("no report, no best")
	}
	// A model none of whose recordings decoded has no time: it must not win
	// a tie as the "fastest".
	failed := &CompareReport{Models: []compare.Summary{
		{Model: "std", Right: 0, Total: 5, MedianMs: 12},
		{Model: "light", Right: 0, Total: 5, Failed: 5},
		{Model: "other", Right: 0, Total: 5, MedianMs: 20},
	}}
	if got := bestModel(failed); got != "std" {
		t.Errorf("an all-failed model was eligible, got %q", got)
	}
}

// A failed run keeps the last good report beside its error; a good run
// replaces it.
func TestFinishedRun(t *testing.T) {
	prev := &CompareReport{Clips: 9}
	r := finishedRun(prev, CompareReport{Clips: 9}, errors.New("grammar kept changing"))
	if r.Report != prev || r.Err == "" || r.Running {
		t.Errorf("failed run: %+v", r)
	}
	r = finishedRun(prev, CompareReport{Clips: 10}, nil)
	if r.Report == prev || r.Report.Clips != 10 || r.Err != "" {
		t.Errorf("good run: %+v", r)
	}
}

// The cards carry each model's license and state; with a report they carry
// scores, and the disagreement rows have one cell per compared model, in card
// order, with a playable clip.
func TestModelsViewData(t *testing.T) {
	onDisk := map[string]string{defaultCommandModel: "183 MB", "sherpa-nemo-small": "61 MB"}
	data := modelsViewData(commandModels, onDisk, defaultCommandModel, compareRun{}, testRef, testBytes)
	if data.Compared || len(data.Cards) != 2 || !data.Cards[0].InUse || data.Cards[1].InUse || data.Cards[0].License != "CC BY 4.0" {
		t.Fatalf("before a comparison: %+v", data)
	}
	report := &CompareReport{
		Clips: 12,
		Models: []compare.Summary{
			{Model: defaultCommandModel, Right: 10, Total: 11, MedianMs: 11.4},
			{Model: "sherpa-nemo-small", Right: 8, Total: 11, MedianMs: 6.6},
		},
		Skipped: []SkippedClip{{Reason: "no grammar path"}},
		Disagreements: []compare.Disagreement{{
			Clip:  compare.Clip{File: "g/seg_1.wav", Audio: "calibration-capture/g/seg_1.wav", Target: "Snap Left"},
			Heard: map[string]string{defaultCommandModel: "SNAP LEFT", "sherpa-nemo-small": "SNAP EIGHT"},
			Right: map[string]bool{defaultCommandModel: true},
		}},
	}
	data = modelsViewData(commandModels, onDisk, "sherpa-nemo-small", compareRun{Report: report}, testRef, testBytes)
	if !data.Compared || data.Clips != 12 || data.Skipped != 1 {
		t.Errorf("report totals: %+v", data)
	}
	std := data.Cards[0]
	if !std.Scored || std.Right != 10 || std.Total != 11 || std.MedianMs != "11 ms" || !std.Best || std.InUse {
		t.Errorf("standard card: %+v", std)
	}
	if data.Cards[1].Best || !data.Cards[1].InUse {
		t.Errorf("light card: %+v", data.Cards[1])
	}
	// Every decode failed: scored, but no time per recording.
	report.Models[1] = compare.Summary{Model: "sherpa-nemo-small", Right: 0, Total: 11, Failed: 11}
	if c := modelsViewData(commandModels, onDisk, "", compareRun{Report: report}, testRef, testBytes).Cards[1]; c.MedianMs != "" {
		t.Errorf("an all-failed model shows a time: %+v", c)
	}
	report.Models[1] = compare.Summary{Model: "sherpa-nemo-small", Right: 8, Total: 11, MedianMs: 6.6}
	if len(data.Columns) != 2 || data.Columns[0] != "Standard" || data.Columns[1] != "Light" {
		t.Errorf("columns: %v", data.Columns)
	}
	d := data.Disagreements[0]
	if d.Said != "snap left" || d.AudioURL != "plugins/voice/data?file=calibration-capture%2Fg%2Fseg_1.wav" {
		t.Errorf("row: %+v", d)
	}
	if len(d.Cells) != 2 || !d.Cells[0].Right || d.Cells[1].Right || d.Cells[1].Heard != "snap eight" {
		t.Errorf("cells: %+v", d.Cells)
	}
}

// A model's sources are the distinct repositories its parts download from,
// in order, however each part names it.
func TestSourcesOf(t *testing.T) {
	got := sourcesOf([]partSource{
		{Repo: "argmaxinc/whisperkit-coreml"},
		{Repo: "argmaxinc/whisperkit-coreml"},
		{Repo: "openai/whisper-small.en"},
		{URL: "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin"},
		{URL: "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/x.tar.bz2"},
		{},
	})
	want := []string{
		"huggingface.co/argmaxinc/whisperkit-coreml",
		"huggingface.co/openai/whisper-small.en",
		"huggingface.co/ggerganov/whisper.cpp",
		"github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/x.tar.bz2",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sourcesOf = %v, want %v", got, want)
	}
}

// A download carrying two licenses names both; an unknown source names none
// rather than a partial list.
func TestDeclaredModelLicense(t *testing.T) {
	m := declaredModel{Sources: []string{"huggingface.co/argmaxinc/whisperkit-coreml", "huggingface.co/openai/whisper-small.en"}}
	if got := m.License(); got != "MIT + Apache-2.0" {
		t.Errorf("License = %q", got)
	}
	m.Sources = append(m.Sources, "example.org/unknown")
	if got := m.License(); got != "" {
		t.Errorf("an unknown source must leave the license unshown, got %q", got)
	}
}

// Every model voice declares has a license on the Models tab: its source is
// every repository its parts download from has a license read from its card.
func TestEveryDeclaredModelHasALicense(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DIR", "..")
	models := declaredModels()
	if len(models) == 0 {
		t.Fatal("no declared models read from ../plugin.json")
	}
	for _, m := range models {
		if len(m.Sources) == 0 || m.License() == "" {
			t.Errorf("%s (sources %v) has no license on the Models tab", m.Name, m.Sources)
		}
	}
}

// Dictation cards carry the license of their source; the one dictation loads
// is in use only once downloaded, and is otherwise the chosen one.
func TestDictationCards(t *testing.T) {
	models := []declaredModel{
		{Name: "large", Description: "Dictation — most accurate", Sources: []string{"huggingface.co/argmaxinc/whisperkit-coreml"}, SizeBytes: 1 << 30},
		{Name: "small", Description: "Dictation — balanced", Sources: []string{"huggingface.co/argmaxinc/whisperkit-coreml"}, SizeBytes: 1 << 28},
	}
	cards := dictationCards(models, map[string]string{"small": "466 MB"}, "large", testRef)
	if cards[0].License != "MIT" || cards[0].InUse || !cards[0].Chosen || cards[0].Downloaded {
		t.Errorf("chosen, not downloaded: %+v", cards[0])
	}
	if !cards[1].Downloaded || cards[1].InUse || cards[1].Chosen {
		t.Errorf("downloaded, not chosen: %+v", cards[1])
	}
}
