package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/branchkit/plugin-sdk-go"

	"github.com/branchkit/branchkit-plugin-voice/internal/logcapture"
)

// What the user said never reaches voice's own log lines.
//
// Voice's `branchkit.Logf` lines go to stderr, which the platform writes to
// platform.log and show-all. The platform reduces plugin stderr to its size
// in a release build unless the user turned recording on, but the text was
// still produced, and a dev build keeps it. These tests drive every path that
// used to print a transcript with a phrase no log line would otherwise
// contain, and fail if any word of it comes out.

// spokenPhrase is what the user "said". Its words appear in no log format
// string, codeword list or command name, so finding one in the output means
// the transcript leaked.
const spokenPhrase = "Marigold Zebra Quantum"

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

// assertNoSpokenWords fails for each spoken word found in the log output,
// case-insensitively (the discovery title upper-cases the words it shows).
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

// resolvePlatform answers commands.resolve with a canned response, so the
// real orchestration path (callResolveDefault → commands.resolve → match)
// runs without a platform.
type resolvePlatform struct {
	resp  branchkit.CommandsResolveResponse
	calls atomic.Int32
}

func (p *resolvePlatform) CommandsResolve(branchkit.CommandsResolveRequest) (*branchkit.CommandsResolveResponse, error) {
	p.calls.Add(1)
	r := p.resp
	return &r, nil
}

// CommandsResolvePhrase answers with the canned resolve as the phrase's only
// step when it matched, or as where the phrase stopped when it did not.
func (p *resolvePlatform) CommandsResolvePhrase(req branchkit.CommandsResolvePhraseRequest) (*branchkit.CommandsResolvePhraseResponse, error) {
	p.calls.Add(1)
	var resolved branchkit.ResolveResult
	b, _ := json.Marshal(p.resp)
	_ = json.Unmarshal(b, &resolved)
	if !p.resp.Matched {
		return &branchkit.CommandsResolvePhraseResponse{Stopped: &branchkit.PhraseStop{Resolved: resolved}}, nil
	}
	n := min(max(p.resp.ConsumedCount, 1), len(req.Words))
	return &branchkit.CommandsResolvePhraseResponse{Steps: []branchkit.PhraseStep{
		{Words: req.Words[:n], Resolved: resolved},
	}}, nil
}

// resolveCalls is how many times the platform was asked to resolve (a phrase
// or a single command).
func (p *resolvePlatform) resolveCalls() int { return int(p.calls.Load()) }

func (p *resolvePlatform) CollectionPut(branchkit.CollectionPutRequest) (*branchkit.CollectionPutResponse, error) {
	return &branchkit.CollectionPutResponse{}, nil
}

func (p *resolvePlatform) CollectionDeleteRecords(branchkit.CollectionDeleteRecordsRequest) (*branchkit.CollectionDeleteRecordsResponse, error) {
	return &branchkit.CollectionDeleteRecordsResponse{}, nil
}

func (p *resolvePlatform) VocabularyCommit() error { return nil }
func (p *resolvePlatform) OutputState(branchkit.OutputStateRequest) (*branchkit.OutputStateResponse, error) {
	return &branchkit.OutputStateResponse{}, nil
}
func (p *resolvePlatform) OutputClear(branchkit.OutputClearRequest) (*branchkit.OutputClearResponse, error) {
	return &branchkit.OutputClearResponse{}, nil
}

func orchestratingHost(t *testing.T, resp branchkit.CommandsResolveResponse) *Host {
	t.Helper()
	h := newTestHost()
	resetState(h)
	h.sess.Platform = &resolvePlatform{resp: resp}
	h.sess.InitRPC()
	h.setVoiceMode("continuous_awake")
	return h
}

func snapAction() *branchkit.Action {
	at := "placement.snap"
	return &branchkit.Action{Type: branchkit.ActionKindPlugin, ActionType: &at}
}

func commandTranscript(text string) *branchkit.OnTranscriptRequest {
	conf := 0.8
	return &branchkit.OnTranscriptRequest{
		Pipeline:     "command_recognition",
		Text:         text,
		Confidence:   &conf,
		WordScores:   []float64{1.5, -0.4, 2.25},
		WordOnsetsMs: []int{100, 400, 800},
	}
}

func TestCommandTranscriptMatchedLogsNoSpokenWords(t *testing.T) {
	h := orchestratingHost(t, branchkit.CommandsResolveResponse{
		Matched: true, ConsumedCount: 3, Action: snapAction(),
	})
	var resp any
	logged := captureStderr(t, func() {
		var err error
		resp, err = h.handleOnTranscript(commandTranscript(spokenPhrase))
		if err != nil {
			t.Errorf("handleOnTranscript: %v", err)
		}
	})
	assertNoSpokenWords(t, "matched command", logged)

	// The path really ran: the platform got its action, and the History ring
	// (in memory, for the HUD) still carries the phrase — only the log is
	// kept free of it.
	if out, ok := resp.(*branchkit.OnTranscriptResponse); !ok || len(out.Actions) != 1 {
		t.Fatalf("expected one action back, got %#v", resp)
	}
	hist := h.snapshotHistory()
	if len(hist) != 1 || hist[0].Phrase != strings.ToLower(spokenPhrase) {
		t.Fatalf("history ring should hold the matched phrase, got %+v", hist)
	}

	// Flagging it a mishear names the record, not the words.
	logged = captureStderr(t, func() {
		if err := h.handleMarkWrong(&struct {
			ID string `json:"id"`
		}{ID: hist[0].ID}); err != nil {
			t.Errorf("handleMarkWrong: %v", err)
		}
	})
	assertNoSpokenWords(t, "mark wrong", logged)
}

func TestCommandTranscriptUnmatchedLogsNoSpokenWords(t *testing.T) {
	// No match but a partial: discovery opens titled with the words heard.
	h := orchestratingHost(t, branchkit.CommandsResolveResponse{HasCompletions: true})
	logged := captureStderr(t, func() {
		h.handleOnTranscript(commandTranscript(spokenPhrase))
	})
	assertNoSpokenWords(t, "unmatched command", logged)
}

func TestSuppressedCommandTranscriptLogsNoSpokenWords(t *testing.T) {
	h := orchestratingHost(t, branchkit.CommandsResolveResponse{})
	h.sess.SuppressCommandRecognition()
	logged := captureStderr(t, func() {
		h.handleOnTranscript(commandTranscript(spokenPhrase))
	})
	assertNoSpokenWords(t, "suppressed command", logged)
}

func TestSpeakSessionTranscriptsLogNoSpokenWords(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
	}{
		{"stop phrase", " speak over"},
		{"cancel phrase", " speak cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := orchestratingHost(t, branchkit.CommandsResolveResponse{})
			h.setVoiceMode("dictation")
			h.sess.SetSpeakDictating(true)
			req := commandTranscript(spokenPhrase + tc.tail)
			req.WordOnsetsMs = []int{100, 400, 800, 1200, 1500}
			req.WordScores = []float64{1, 1, 1, 1, 1}
			logged := captureStderr(t, func() {
				h.handleOnTranscript(req)
			})
			assertNoSpokenWords(t, "speak "+tc.name, logged)
		})
	}
}

func dictationTranscript(t *testing.T, text string) branchkit.PipelineTranscriptEventParams {
	t.Helper()
	return branchkit.PipelineTranscriptEventParams{Pipeline: "dictation", Text: text, IsFinal: true}
}

func TestDictationTranscriptsLogNoSpokenWords(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *Host)
	}{
		{"typed at the cursor", func(h *Host) { h.sess.StartDictationRecording() }},
		{"dropped while not accepting", func(h *Host) {}},
		{"speak session", func(h *Host) {
			h.sess.StartDictationRecording()
			h.sess.SetSpeakDictating(true)
		}},
		{"speak cancelled", func(h *Host) {
			h.sess.StartDictationRecording()
			h.sess.SetSpeakDictating(true)
			h.sess.DiscardSpeakTranscript()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHost()
			resetState(h)
			tc.setup(h)
			logged := captureStderr(t, func() {
				h.handlePipelineTranscript(dictationTranscript(t, spokenPhrase))
			})
			assertNoSpokenWords(t, "dictation "+tc.name, logged)
		})
	}
}
