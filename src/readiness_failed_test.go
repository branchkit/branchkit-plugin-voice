package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A warm that fails says so on the readiness panel instead of leaving the
// engine on "Loading…" for good, and the panel settles once every engine is
// ready or has failed. A dictation stage that could not start closes its
// "Starting dictation…" notice rather than asking the user to keep talking.
// A later warm that succeeds clears the failure.
func TestAFailedWarmSaysSoAndSettlesThePanel(t *testing.T) {
	h := newTestHost()
	h.warmMu.Lock()
	for _, wt := range warmTargets {
		h.warmReady[wt.pipeline] = false
	}
	h.warmMu.Unlock()

	h.showDictationWarming()
	if h.dictationWarmGen == 0 {
		t.Fatal("precondition: the starting notice is showing")
	}
	// A run of the pipeline that fails while its engine is still warming is
	// the run's business: the same event, but not a failed warm.
	h.handlePipelineError("dictation", "mic: device disconnected")
	if row := rowFor(t, h, "dictation"); row != "Loading…" {
		t.Fatalf("a run error must not mark the warming engine failed, got %q", row)
	}
	h.handlePipelineError("dictation", "warm failed: voice.whisperkit_stt: no stage on this OS")
	if h.dictationWarmGen != 0 {
		t.Fatal("a dictation stage that could not start must close its starting notice")
	}
	row := rowFor(t, h, "dictation")
	if !strings.Contains(row, "Couldn't start") || !strings.Contains(row, "no stage on this OS") {
		t.Fatalf("the dictation row must say it could not start and why, got %q", row)
	}
	if h.warmupSettled() {
		t.Fatal("command_recognition is still loading: the panel is not settled")
	}

	h.handlePipelineWarmed("command_recognition")
	if !h.warmupSettled() {
		t.Fatal("every engine is ready or failed: the panel is settled")
	}
	if got := h.warmupTitle(); !strings.Contains(got, "Dictation voice engine") || !strings.Contains(got, "couldn't start") {
		t.Fatalf("the settled title names the engine that failed, got %q", got)
	}
	// A permission report redraws the panel with the same settled heading,
	// not "Warming up voice…".
	if title, _ := h.warmupHeading(); title != h.warmupTitle() {
		t.Fatalf("the heading after a failure is the settled title, got %q", title)
	}

	// An error for an engine already up is a run's business, not the panel's.
	h.handlePipelineError("command_recognition", "pipeline error: stage exited")
	if row := rowFor(t, h, "command_recognition"); !strings.Contains(row, "Ready") {
		t.Fatalf("a run error after warm-up must not mark the engine failed, got %q", row)
	}

	h.handlePipelineWarmed("dictation")
	if row := rowFor(t, h, "dictation"); !strings.Contains(row, "Ready") {
		t.Fatalf("a later successful warm clears the failure, got %q", row)
	}
	if got := h.warmupTitle(); got != h.readyTitle() {
		t.Fatalf("all ready again = the ready title, got %q", got)
	}
}

func rowFor(t *testing.T, h *Host, pipeline string) string {
	t.Helper()
	for _, s := range h.readinessSections() {
		for _, it := range s.Items {
			if it.ID == pipeline && it.Subtitle != nil {
				return *it.Subtitle
			}
		}
	}
	t.Fatalf("no readiness row for %s", pipeline)
	return ""
}

// The readiness panel's events reach voice only if its manifest declares
// them: a handler subscribed to an undeclared platform event never fires,
// and a failed warm would again show "Loading…" for good.
func TestTheReadinessEventsAreDeclared(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Consumes struct {
			Events []struct {
				Pattern string `json:"pattern"`
			} `json:"events"`
		} `json:"consumes"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, e := range m.Consumes.Events {
		declared[e.Pattern] = true
	}
	for _, ev := range []string{
		"_platform.pipeline.warming",
		"_platform.pipeline.warmed",
		"_platform.pipeline.error",
	} {
		if !declared[ev] {
			t.Errorf("plugin.json does not consume %s, so its handler never fires", ev)
		}
	}
}
