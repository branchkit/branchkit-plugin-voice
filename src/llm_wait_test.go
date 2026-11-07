package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

// stageCaller answers stages.request the way the platform would, and keeps
// what it was asked.
type stageCaller struct {
	answer string
	err    error
	method string
	req    branchkit.StagesRequestRequest
}

func (c *stageCaller) Call(method string, params any, result any) error {
	c.method = method
	// Read the params as the platform would, off the wire.
	sent, _ := json.Marshal(params)
	_ = json.Unmarshal(sent, &c.req)
	if c.err != nil {
		return c.err
	}
	body, _ := json.Marshal(refineAnswer{Text: c.answer, Device: "cpu"})
	raw, _ := json.Marshal(branchkit.StagesRequestResponse{Body: body})
	return json.Unmarshal(raw, result)
}

// llmTestHost is a test host whose deadline-bound calls go to c, recording
// the deadline each call asked for.
func llmTestHost(c *stageCaller, asked *[]time.Duration) *Host {
	h := newTestHost()
	h.callsWithin = func(d time.Duration) branchkit.Methods {
		*asked = append(*asked, d)
		return branchkit.NewMethods(c)
	}
	return h
}

// A clean-up waits at most refineTimeout in all, not the SDK's 10 s default:
// the call carries it as its own deadline, and the stage is asked for the
// same budget.
func TestRefineCallUsesItsOwnBound(t *testing.T) {
	c := &stageCaller{answer: "Let's meet on Wednesday."}
	var asked []time.Duration
	h := llmTestHost(c, &asked)
	text := "Let's meet on Tuesday, no, Wednesday."
	out, note := h.refineDictation(text, profileProse)
	if len(asked) != 1 || asked[0] != refineTimeout {
		t.Fatalf("refine asked for deadlines %v, want [%v]", asked, refineTimeout)
	}
	if refineTimeout != 8*time.Second {
		t.Errorf("refineTimeout = %v, want 8s", refineTimeout)
	}
	if c.method != branchkit.MethodStagesRequest || c.req.Stage != refineStage {
		t.Errorf("called %s on stage %q", c.method, c.req.Stage)
	}
	if c.req.TimeoutMs == nil || *c.req.TimeoutMs != int(refineTimeout/time.Millisecond) {
		t.Errorf("stage budget %v, want %d ms", c.req.TimeoutMs, refineTimeout/time.Millisecond)
	}
	if out != "Let's meet on Wednesday." {
		t.Errorf("typed %q (%s)", out, note)
	}
}

// A rewrite's bound is styleTimeout, no longer than the 10 s the SDK's
// default used to impose, on the call and on the stage alike.
func TestStyleCallUsesItsOwnBound(t *testing.T) {
	c := &stageCaller{answer: "Could you call Sam at five tomorrow?"}
	var asked []time.Duration
	h := llmTestHost(c, &asked)
	h.styleText("can you call Sam at five tomorrow", styleFormal)
	if len(asked) != 1 || asked[0] != styleTimeout {
		t.Fatalf("style asked for deadlines %v, want [%v]", asked, styleTimeout)
	}
	if styleTimeout > 10*time.Second {
		t.Errorf("styleTimeout = %v, past the 10 s it can wait today", styleTimeout)
	}
	if c.req.TimeoutMs == nil || *c.req.TimeoutMs != int(styleTimeout/time.Millisecond) {
		t.Errorf("stage budget %v, want %d ms", c.req.TimeoutMs, styleTimeout/time.Millisecond)
	}
}

// A call that runs out of time types what was heard, and the record says so.
func TestRefineTimeoutTypesTheText(t *testing.T) {
	c := &stageCaller{err: errors.New("call stages.request timed out after 8s")}
	var asked []time.Duration
	h := llmTestHost(c, &asked)
	text := "Let's meet on Tuesday, no, Wednesday."
	out, note := h.refineDictation(text, profileProse)
	if out != text {
		t.Errorf("typed %q, want the unrefined text", out)
	}
	if !strings.HasPrefix(note, "not refined: ") || !strings.Contains(note, "timed out") {
		t.Errorf("record note %q", note)
	}
	sc := &stageCaller{err: errors.New("call stages.request timed out after 10s")}
	var sAsked []time.Duration
	sh := llmTestHost(sc, &sAsked)
	if out, ok, note := sh.styleText(text, styleFormal); out != text || ok || !strings.HasPrefix(note, "not styled: ") {
		t.Errorf("style on timeout: %q %v %q", out, ok, note)
	}
}
