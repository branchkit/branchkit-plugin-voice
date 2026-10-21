package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/compare"
	"github.com/branchkit/branchkit-plugin-voice/views"
	"github.com/branchkit/plugin-sdk-go"
)

// Reading covers the whole set before repeating: the next sentence is the
// one with the fewest kept recordings, the earliest on a tie.
func TestNextSentence(t *testing.T) {
	if got := nextSentence(nil); got != dictationSentences[0].Text {
		t.Errorf("first sentence: %q", got)
	}
	kept := []dictationRecording{{Sentence: dictationSentences[0].Text}}
	if got := nextSentence(kept); got != dictationSentences[1].Text {
		t.Errorf("after one recording: %q", got)
	}
}

// One dictation run records per arm, and only within the window; its
// transcript is claimed once; a cancelled run's mark goes stale.
func TestReadSentenceRecordsOneRun(t *testing.T) {
	var r readSentence
	now := time.Now()
	if r.recordDirForRun(now) != "" {
		t.Error("unarmed: no recording")
	}
	r.armedUntil, r.session, r.sentence = now.Add(readSentenceWindow), "dictation-recordings/session-x", "Hello there."
	if r.recordDirForRun(now) != "dictation-recordings/session-x" || r.recordDirForRun(now) != "" {
		t.Error("exactly one run records per arm")
	}
	if s, sentence, ok := r.takeRecordedTranscript(now); !ok || s != "dictation-recordings/session-x" || sentence != "Hello there." {
		t.Error("the recorded run's transcript must be claimed")
	}
	if _, _, ok := r.takeRecordedTranscript(now); ok {
		t.Error("claimed twice")
	}
	r.armedUntil = now.Add(readSentenceWindow)
	r.recordDirForRun(now)
	if _, _, ok := r.takeRecordedTranscript(now.Add(readSentenceWindow + time.Second)); ok {
		t.Error("a cancelled run's stale mark swallowed a later dictation")
	}
}

// Keep adds the recording to the dictation corpus, discard and the sweep
// delete what is not kept or waiting, and an active session is left alone.
func TestReadSentenceKeepDiscardAndSweep(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	root := dictationDataDir()
	write := func(rel string) {
		p := filepath.Join(filepath.Dir(root), rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		"dictation-recordings/session-a/1.wav", "dictation-recordings/session-a/2.wav", "dictation-recordings/session-a/manifest.jsonl",
		"dictation-recordings/session-live/1.wav",
	} {
		write(f)
	}
	h := &Host{}
	h.readSentence.pending = []readClip{
		{File: "dictation-recordings/session-a/1.wav", Sentence: "Hello there."},
		{File: "dictation-recordings/session-a/2.wav", Sentence: "Goodbye."},
	}
	h.readSentence.recording, h.readSentence.session = true, "dictation-recordings/session-live"
	if err := h.handleReadSentenceKeep(&readAnswer{File: "dictation-recordings/session-a/1.wav"}); err != nil {
		t.Fatal(err)
	}
	if got := readDictationCorpus(); len(got) != 1 || got[0].Sentence != "Hello there." {
		t.Fatalf("corpus after keep: %+v", got)
	}
	if clips := keptDictationClips(); len(clips) != 1 || clips[0].Target != "Hello there." || clips[0].Audio != "dictation-recordings/session-a/1.wav" {
		t.Errorf("kept clips: %+v", clips)
	}
	if err := h.handleReadSentenceDiscard(&readAnswer{File: "dictation-recordings/session-a/2.wav"}); err != nil {
		t.Fatal(err)
	}
	exists := func(rel string) bool { _, err := os.Stat(filepath.Join(filepath.Dir(root), rel)); return err == nil }
	for rel, want := range map[string]bool{
		"dictation-recordings/session-a/1.wav":          true,
		"dictation-recordings/session-a/2.wav":          false,
		"dictation-recordings/session-a/manifest.jsonl": false,
		"dictation-recordings/session-live/1.wav":       true,
	} {
		if exists(rel) != want {
			t.Errorf("%s exists=%v, want %v", rel, !want, want)
		}
	}
}

// Dictation cards show words right and time per sentence; the best is the
// most words right, then the fastest, once two are scored; an all-failed
// model is never best.
func TestScoreDictationCards(t *testing.T) {
	cards := []views.ModelCard{{Name: "large"}, {Name: "small"}, {Name: "base"}}
	scoreDictationCards(cards, &DictationReport{Models: []compare.WordSummary{
		{Model: "large", Words: 100, Errors: 3, Total: 8, MedianMs: 1100},
		{Model: "small", Words: 100, Errors: 9, Total: 8, MedianMs: 400},
		{Model: "base", Words: 100, Errors: 100, Total: 8, Failed: 8},
	}})
	if cards[0].WordsPct != "97%" || cards[0].MedianMs != "1.1 s" || !cards[0].Best {
		t.Errorf("large: %+v", cards[0])
	}
	if cards[1].Best || cards[1].WordsPct != "91%" {
		t.Errorf("small: %+v", cards[1])
	}
	if cards[2].Best || cards[2].MedianMs != "" {
		t.Errorf("an all-failed model: %+v", cards[2])
	}
}

// Differing recordings get a column per compared model, in catalog order,
// each with its word errors against the sentence.
func TestFillDictationRows(t *testing.T) {
	catalog := []declaredModel{{Name: "large", Description: "Dictation — most accurate"}, {Name: "small", Description: "Dictation — balanced"}}
	r := &DictationReport{
		Models: []compare.WordSummary{{Model: "small"}, {Model: "large"}},
		Disagreements: []compare.Disagreement{{
			Clip:  compare.Clip{Target: "It is a fixed set.", Audio: "dictation-recordings/s/1.wav"},
			Heard: map[string]string{"large": "It is a fixed set.", "small": "It is a fix set."},
		}},
	}
	var v views.DictationCompareView
	fillDictationRows(&v, r, catalog)
	if !v.Compared || len(v.Columns) != 2 || len(v.Rows) != 1 {
		t.Fatalf("view: %+v", v)
	}
	if c := v.Rows[0].Cells; c[0].Errors != 0 || c[1].Errors != 1 || c[1].Heard != "It is a fix set." {
		t.Errorf("cells: %+v", c)
	}
}

// A recorded run that never started, or ended with no text, clears its mark:
// the next ordinary dictation is typed, not swallowed.
func TestReadSentenceClearedMarkReleasesTheNextDictation(t *testing.T) {
	var r readSentence
	now := time.Now()
	r.armedUntil, r.session = now.Add(readSentenceWindow), "dictation-recordings/session-y"
	if r.recordDirForRun(now) == "" {
		t.Fatal("armed run must record")
	}
	r.clearRecording()
	if _, _, ok := r.takeRecordedTranscript(now); ok {
		t.Error("a cleared mark swallowed the next dictation")
	}
}

// The reading and an ordinary dictation are told apart by their pipelines: a
// dictation_sample transcript claims the reading's mark and is never typed (no
// dictation is recorded), while an ordinary dictation transcript leaves the
// mark alone and is handled as a dictation, even while a reading is pending.
func TestReadingAndDictationAreToldApartByPipeline(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", t.TempDir())
	h := newTestHost()
	resetState(h)
	h.readSentence.recording, h.readSentence.recordedAt = true, time.Now()
	h.readSentence.session, h.readSentence.sentence = "dictation-recordings/session-z", "Hello there."

	h.sess.StartDictationRecording()
	h.handlePipelineTranscript(dictationTranscript(t, "hello there"))
	if !h.readSentence.recording {
		t.Fatal("an ordinary dictation claimed the reading's mark")
	}
	if len(h.snapshotDictations()) != 1 {
		t.Fatal("the ordinary dictation was not handled as a dictation")
	}

	h.sess.StartDictationRecording()
	h.handlePipelineTranscript(branchkit.PipelineTranscriptEventParams{Pipeline: dictationSamplePipeline, Text: "Hello there.", IsFinal: true})
	if h.readSentence.recording {
		t.Error("the reading did not claim its mark")
	}
	if len(h.snapshotDictations()) != 1 {
		t.Error("the reading was handled as a dictation (it would be typed)")
	}
}

// What holds for a dictation run as a run holds for the reading's too: an
// empty final transcript finishes its session (no "dictation didn't come
// back"), and the chosen microphone applies to it.
func TestTheReadingPipelineIsADictationPipeline(t *testing.T) {
	if !isDictationPipeline(dictationSamplePipeline) || !isDictationPipeline("dictation") || isDictationPipeline("command_recognition") {
		t.Fatal("isDictationPipeline")
	}
	found := false
	for _, p := range micPipelines {
		found = found || p == dictationSamplePipeline
	}
	if !found {
		t.Error("the reading pipeline does not follow the chosen microphone")
	}
	h := newTestHost()
	resetState(h)
	lost := 0
	h.sess.SetOnDictationLost(func() { lost++ })
	h.sess.StartDictationRecording()
	h.sess.DrainDictation()
	h.handlePipelineTranscript(branchkit.PipelineTranscriptEventParams{Pipeline: dictationSamplePipeline, Text: "", IsFinal: true})
	h.sess.FinishDictationIfDraining()
	if lost != 0 {
		t.Error("an empty reading was reported as a lost dictation")
	}
}

// A reading that cold-starts the dictation stage opens the warming overlay,
// and its warmed event closes it and marks dictation ready, though the
// reading's pipeline is not one the boot warm-up tracks.
func TestTheReadingsWarmedClosesItsWarmingOverlay(t *testing.T) {
	h := newTestHost()
	resetState(h)
	h.handlePipelineWarming(dictationSamplePipeline)
	if h.dictationReady.Load() {
		t.Fatal("warming must mark dictation not ready")
	}
	h.handlePipelineWarmed(dictationSamplePipeline)
	if !h.dictationReady.Load() {
		t.Error("the reading's warmed did not mark dictation ready")
	}
	h.dictationWarmMu.Lock()
	open := h.dictationWarmGen != 0
	h.dictationWarmMu.Unlock()
	if open {
		t.Error("the reading's warmed left the warming overlay open")
	}
}
