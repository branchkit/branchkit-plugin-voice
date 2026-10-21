package session

import (
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/internal/logcapture"
)

// The orchestration paths never print what the user said.
// The main package drives the whole transcript path; these cover the branches
// of orchestrate it cannot reach with one canned resolve — scope-dropped
// words, a per-word fallthrough, and a tie — and assert the same thing: no
// word of the utterance in the log output.

var spokenWords = []string{"marigold", "zebra", "quantum"}

// captureStderr runs fn and returns everything written to stderr meanwhile,
// by any goroutine — where branchkit.Logf writes.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	out, err := logcapture.Stderr(fn)
	if err != nil {
		t.Fatalf("capture stderr: %v", err)
	}
	return out
}

func assertNoSpokenWords(t *testing.T, path, logged string) {
	t.Helper()
	lower := strings.ToLower(logged)
	for _, w := range spokenWords {
		if strings.Contains(lower, w) {
			t.Errorf("%s: log output contains the spoken word %q:\n%s", path, w, logged)
		}
	}
	if strings.TrimSpace(logged) == "" {
		t.Errorf("%s: logged nothing — the path under test did not run", path)
	}
}

func spokenRequest() *OrchestrateRequest {
	return &OrchestrateRequest{
		Transcript: strings.Join(spokenWords, " "),
		Words:      append([]string{}, spokenWords...),
		Mode:       "command_hold",
		ActiveTags: []string{"plugin.placement.snap_mode"},
	}
}

func TestScopeDroppedWordsLogNoSpokenWords(t *testing.T) {
	s := newTestState()
	s.Platform = &tieRecorder{resolveResp: &resolveResponse{
		ScopedTags: []string{"plugin.placement.snap_mode"},
	}}
	s.InitRPC()
	logged := captureStderr(t, func() { s.OrchestrateInternal(spokenRequest()) })
	assertNoSpokenWords(t, "scope-dropped", logged)
}

func TestFallthroughLogsNoSpokenWords(t *testing.T) {
	// Every word matches on its own: three actions from a three-word input.
	s := newTestState()
	s.Platform = &tieRecorder{resolveResp: &resolveResponse{
		Matched: true, ConsumedCount: 1, Action: opaqueAction("letter"),
	}}
	s.InitRPC()
	var resp OrchestrateResponse
	logged := captureStderr(t, func() { resp = s.OrchestrateInternal(spokenRequest()) })
	if len(resp.Actions) != 3 {
		t.Fatalf("expected a per-word fallthrough of 3 actions, got %d", len(resp.Actions))
	}
	assertNoSpokenWords(t, "fallthrough", logged)
}

func TestTieLogsNoSpokenWords(t *testing.T) {
	s := newTestState()
	rec := &tieRecorder{resolveResp: tieResolveResp("spotify", "youtube")}
	s.Platform = rec
	s.InitRPC()
	logged := captureStderr(t, func() { s.OrchestrateInternal(spokenRequest()) })
	assertNoSpokenWords(t, "tie opened", logged)
	if !s.IsTieActive() {
		t.Fatal("a tie should be active")
	}

	// Picking a choice re-resolves the original words.
	s.mu.Lock()
	token := s.tieChoices[0].Token
	s.mu.Unlock()
	rec.resolveResp = &resolveResponse{Matched: true, Action: pluginAction("spotify.play", "")}
	logged = captureStderr(t, func() {
		if s.ResolveTie(token) == nil {
			t.Error("the pick should resolve to a winner")
		}
	})
	assertNoSpokenWords(t, "tie picked", logged)
}
