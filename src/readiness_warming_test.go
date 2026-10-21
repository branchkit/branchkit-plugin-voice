package main

import "testing"

// A dictation run that has to start its stage cold says so, and the notice
// goes when the stage is up: dictation is not ready in between, so the next
// hold says so too. Other pipelines warming are not dictation's business.
func TestDictationWarmingShowsUntilWarmed(t *testing.T) {
	h := newTestHost()
	h.dictationReady.Store(true)
	h.warmMu.Lock()
	h.warmReady["dictation"] = true
	h.warmMu.Unlock()

	h.handlePipelineWarming("command_recognition")
	if !h.dictationReady.Load() || h.dictationWarmGen != 0 {
		t.Fatalf("another pipeline warming must not touch dictation (ready=%v gen=%d)",
			h.dictationReady.Load(), h.dictationWarmGen)
	}

	h.handlePipelineWarming("dictation")
	if h.dictationReady.Load() {
		t.Fatal("a dictation stage starting cold means dictation is not ready")
	}
	gen := h.dictationWarmGen
	if gen == 0 {
		t.Fatal("the starting notice must be showing")
	}
	h.handlePipelineWarming("dictation")
	if h.dictationWarmGen != gen {
		t.Fatalf("a second warming must not open a second notice (%d then %d)", gen, h.dictationWarmGen)
	}

	h.handlePipelineWarmed("dictation")
	if !h.dictationReady.Load() {
		t.Fatal("warmed means dictation is ready")
	}
	if h.dictationWarmGen != 0 {
		t.Fatal("warmed must close the starting notice")
	}
}
