package main

import (
	"encoding/json"
	branchkit "github.com/branchkit/plugin-sdk-go"
	"strings"
	"testing"
)

func resetDictationRing(h *Host) {
	h.dictationMu.Lock()
	h.dictationEntries = nil
	h.dictationMu.Unlock()
}

func TestDictationRingIsNewestFirstAndBounded(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	for i := 0; i < dictationHistoryCap+5; i++ {
		h.recordDictation(typedAsHeard(strings.Repeat("x", i+1)))
	}
	got := h.snapshotDictations()
	if len(got) != dictationHistoryCap {
		t.Fatalf("ring should cap at %d, got %d", dictationHistoryCap, len(got))
	}
	if len(got[0].Text) != dictationHistoryCap+5 {
		t.Fatalf("newest entry should be first, got %q", got[0].Text)
	}
	if len(got[len(got)-1].Text) != 6 {
		t.Fatalf("oldest surviving entry wrong, got %q", got[len(got)-1].Text)
	}
}

// Empty and whitespace-only transcripts are not dictations. Whisper returns
// empty for silence, and a ring full of blanks buries the ones worth
// recovering.
func TestDictationRingSkipsEmpty(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	h.recordDictation(typedAsHeard(""))
	h.recordDictation(typedAsHeard("   "))
	h.recordDictation(typedAsHeard("\n\t "))
	if n := len(h.snapshotDictations()); n != 0 {
		t.Fatalf("expected empty ring, got %d entries", n)
	}
	h.recordDictation(typedAsHeard("  real text  "))
	got := h.snapshotDictations()
	if len(got) != 1 || got[0].Text != "real text" {
		t.Fatalf("expected trimmed single entry, got %+v", got)
	}
}

func TestDictationEntriesHaveDistinctIDs(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	for i := 0; i < 5; i++ {
		h.recordDictation(typedAsHeard("entry"))
	}
	seen := map[string]bool{}
	for _, e := range h.snapshotDictations() {
		if e.ID == "" {
			t.Fatal("entry has no id")
		}
		if seen[e.ID] {
			t.Fatalf("duplicate id %q — ids key nothing useful if they collide", e.ID)
		}
		seen[e.ID] = true
	}
}

// The panel must escape what it renders: a dictation is arbitrary user text
// and lands inside HUD HTML.
func TestDictationPanelEscapesUserText(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	h.recordDictation(typedAsHeard(`<script>alert("x")</script> & "quoted"`))
	out := h.renderDictationHistoryHTML()
	if strings.Contains(out, "<script>") {
		t.Fatal("raw <script> reached the panel HTML")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("expected escaped output, got: %s", out)
	}
}

func TestDictationPanelEmptyState(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	if out := h.renderDictationHistoryHTML(); !strings.Contains(out, "No dictations yet") {
		t.Fatalf("expected an empty state, got: %s", out)
	}
}

// "type again" with nothing dictated this session does nothing, and the
// command it answers is registered.
func TestRetypeLastIsRegisteredAndQuietWhenEmpty(t *testing.T) {
	h := newTestHost()
	if _, ok := actionHandlersForTest(h)["voice.retype_last"]; !ok {
		t.Fatal("voice.retype_last has no handler")
	}
	if _, err := h.handleRetypeLast(&branchkit.OnActionRequest{Action: "voice.retype_last"}); err != nil {
		t.Fatalf("empty history: %v", err)
	}
}

// typedAsHeard is a dictation no step changed: typed exactly as heard.
func typedAsHeard(text string) dictationEntry {
	return dictationEntry{Heard: text, Repaired: text, Shaped: text, Text: text}
}

func dictationTranscriptWith(t *testing.T, fields map[string]any) branchkit.PipelineTranscriptEventParams {
	t.Helper()
	m := map[string]any{"pipeline": "dictation", "is_final": true}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// Decoded the way the SDK's typed helper decodes the wire payload.
	var ev branchkit.PipelineTranscriptEventParams
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// One dictation through the steps, against a detached plugin (nothing is
// typed and the caret cannot be read): the record holds each step's output
// and what chose the profile.
func TestDictationRecordsEachStep(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	h.sess.StartDictationRecording()
	h.sess.DrainDictation()
	h.handlePipelineTranscript(dictationTranscriptWith(t, map[string]any{
		"text": "Gmail.", "dictation_profile": "query", "focused_app": "com.google.Chrome",
		"confidence": 0.4, "word_scores": []float64{0.4},
	}))
	got := h.snapshotDictations()
	if len(got) != 1 {
		t.Fatalf("want one record, got %d", len(got))
	}
	e := got[0]
	if e.Heard != "Gmail." || e.Repaired != "Gmail." || e.Shaped != "Gmail" || e.Text != "Gmail" {
		t.Errorf("steps: heard %q repaired %q shaped %q text %q", e.Heard, e.Repaired, e.Shaped, e.Text)
	}
	if e.Confidence == nil || *e.Confidence != 0.4 || len(e.WordScores) != 1 {
		t.Errorf("the stage's scores were not kept: %v %v", e.Confidence, e.WordScores)
	}
	if e.Profile != "query" || e.ProfileFrom != "mode" || e.App != "com.google.Chrome" || e.Caret != "none" {
		t.Errorf("facts: %+v", e)
	}
	if trail := dictationTrailHTML(e, true); !strings.Contains(trail, "formatted as query (from the active mode)") {
		t.Errorf("trail does not say what shaped it: %q", trail)
	}
}

// A transcript a correction reduced to nothing is still recorded: "where did
// my word go?" is exactly the question the panel answers.
func TestDictationRecordsATranscriptARuleRemoved(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	h.sess.StartDictationRecording()
	h.sess.DrainDictation()
	h.handlePipelineTranscript(dictationTranscriptWith(t, map[string]any{"text": "So."}))
	got := h.snapshotDictations()
	if len(got) != 1 || got[0].Heard != "So." || got[0].Text != "" {
		t.Fatalf("want the blanked transcript recorded, got %+v", got)
	}
	if trail := dictationTrailHTML(got[0], true); !strings.Contains(trail, "removed by a correction rule") {
		t.Errorf("trail: %q", trail)
	}
}

// Focus moved while it transcribed: kept on the clipboard, and the record
// says so.
func TestDictationRecordsAKeptTranscript(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	h.sess.StartDictationRecording()
	h.sess.DrainDictation()
	h.handlePipelineTranscript(dictationTranscriptWith(t, map[string]any{
		"text": "Meet at noon.", "focused_app": "com.apple.mail", "focused_app_now": "com.tinyspeck.slackmacgap",
	}))
	got := h.snapshotDictations()
	if len(got) != 1 || !got[0].Kept || got[0].Text != "Meet at noon." {
		t.Fatalf("want a kept record, got %+v", got)
	}
}

// "type again" retypes the shaped text (its seam is refitted); "type
// original" retypes what was heard.
func TestRetypeSource(t *testing.T) {
	e := dictationEntry{Heard: " Gmail. ", Repaired: "Gmail.", Shaped: "Gmail", Text: "gmail"}
	if got := retypeSource(e, false); got != "Gmail" {
		t.Errorf("again: %q", got)
	}
	if got := retypeSource(e, true); got != "Gmail." {
		t.Errorf("raw: %q", got)
	}
}

// A dictation typed exactly as heard needs no explanation.
func TestDictationTrailIsEmptyWhenNothingChanged(t *testing.T) {
	if trail := dictationTrailHTML(typedAsHeard("Hello there."), true); trail != "" {
		t.Errorf("want no trail, got %q", trail)
	}
}

// Typing that fails is recorded on the entry, so the Dictation tab does not
// show the text as typed once the overlay has closed. The
// detached plugin refuses every call, so input.type_text fails here.
func TestDictationRecordsATypingFailure(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	h.sess.StartDictationRecording()
	h.sess.DrainDictation()
	h.handlePipelineTranscript(dictationTranscriptWith(t, map[string]any{"text": "Hello there."}))
	got := h.snapshotDictations()
	if len(got) != 1 || got[0].TypeError == "" {
		t.Fatalf("want the typing failure on the entry, got %+v", got)
	}
	// "type again" retypes the newest dictation only, so only its note
	// offers it; the phrase is the retype command's (commands.json).
	trail := dictationTrailHTML(got[0], true)
	if !strings.Contains(trail, "not typed:") || !strings.Contains(trail, "say “type again”") {
		t.Errorf("trail should say it was not typed and how to retry: %q", trail)
	}
	if older := dictationTrailHTML(got[0], false); !strings.Contains(older, "not typed:") || strings.Contains(older, "type again") {
		t.Errorf("an older entry says it was not typed, without the retry: %q", older)
	}
}

// A "type again" that lands clears the failure.
func TestRetypeThatLandsClearsTheTypingFailure(t *testing.T) {
	h := newTestHost()
	resetDictationRing(h)
	e := typedAsHeard("Hello there.")
	e.TypeError = "input refused"
	h.recordDictation(e)
	h.clearTypingFailure(h.snapshotDictations()[0].ID)
	if got := h.snapshotDictations()[0].TypeError; got != "" {
		t.Errorf("a retype that landed should clear the failure, got %q", got)
	}
	if trail := dictationTrailHTML(h.snapshotDictations()[0], true); strings.Contains(trail, "not typed") {
		t.Errorf("trail still says not typed: %q", trail)
	}
}
