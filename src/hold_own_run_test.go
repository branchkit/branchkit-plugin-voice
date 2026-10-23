package main

import (
	"slices"
	"sync"
	"testing"

	"github.com/branchkit/branchkit-plugin-voice/session"
	"github.com/branchkit/plugin-sdk-go"
)

// An Option+G hold over continuous listening listens on an ephemeral run of
// its own, as push-to-talk does: the platform treats a transcript as the
// person holding the key only when its run is ephemeral (the echo filter and
// spoken answers to an app's question key on it). The persistent run is set
// aside at the press and started again at the release, its transcripts held
// back until the hold's cleanup has restored continuous listening's mode.

// pipelineSim stands in for the platform's pipeline manager, for
// command_recognition: one run of a name at a time, `started` said as a run
// starts, and a stopped run's `stopped` held until the test delivers it, in
// the order the runs were stopped — as the platform delivers them.
type pipelineSim struct {
	t *testing.T
	h *Host

	mu       sync.Mutex
	calls    []string
	live     *simRun
	stopping []*simRun
	// persistentRuns counts persistent command_recognition runs started.
	persistentRuns int
	// refuseEphemeral answers an ephemeral run with started:false.
	refuseEphemeral bool
}

type simRun struct {
	ephemeral bool
	reason    string
}

func simulatePipelines(t *testing.T, h *Host) *pipelineSim {
	t.Helper()
	p := &pipelineSim{t: t, h: h}
	h.pipelinesRun = p.run
	h.pipelinesStop = p.stop
	h.pipelinesStatus = p.status
	return p
}

func (p *pipelineSim) status() (*branchkit.PipelinesStatusResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := &branchkit.PipelinesStatusResponse{}
	if p.live != nil {
		out.Pipelines = append(out.Pipelines, branchkit.PipelineStatusEntry{Name: "command_recognition", Ephemeral: p.live.ephemeral})
	}
	return out, nil
}

func (p *pipelineSim) run(req branchkit.PipelinesRunRequest) (*branchkit.PipelinesRunResponse, error) {
	eph := req.Ephemeral != nil && *req.Ephemeral
	kind := "persistent"
	if eph {
		kind = "ephemeral"
	}
	p.mu.Lock()
	if req.Name != "command_recognition" {
		p.calls = append(p.calls, "run "+req.Name)
		p.mu.Unlock()
		return &branchkit.PipelinesRunResponse{Started: true}, nil
	}
	if eph && p.refuseEphemeral {
		p.calls = append(p.calls, "refused command_recognition "+kind)
		p.mu.Unlock()
		reason := "refused"
		return &branchkit.PipelinesRunResponse{Started: false, Reason: &reason}, nil
	}
	if p.live != nil {
		// The platform skips it and still answers started:true.
		p.calls = append(p.calls, "skipped command_recognition "+kind)
		p.mu.Unlock()
		return &branchkit.PipelinesRunResponse{Started: true}, nil
	}
	p.calls = append(p.calls, "run command_recognition "+kind)
	p.live = &simRun{ephemeral: eph}
	if !eph {
		p.persistentRuns++
	}
	p.mu.Unlock()
	p.h.sess.NoteCmdRecStarted(eph)
	return &branchkit.PipelinesRunResponse{Started: true}, nil
}

func (p *pipelineSim) stop(req branchkit.PipelinesStopRequest) (*branchkit.PipelinesStopResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "stop "+req.Name)
	if req.Name == "command_recognition" && p.live != nil {
		p.live.reason = "requested"
		p.stopping = append(p.stopping, p.live)
		p.live = nil
	}
	return &branchkit.PipelinesStopResponse{Stopped: true}, nil
}

// fail ends the live run on its own, as a stage failure does.
func (p *pipelineSim) fail() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live == nil {
		p.t.Fatal("no live run to fail")
	}
	p.live.reason = "error"
	p.stopping = append(p.stopping, p.live)
	p.live = nil
}

// deliverStopped delivers the oldest pending `stopped`, reporting whether it
// was an ephemeral run's.
func (p *pipelineSim) deliverStopped() bool {
	p.t.Helper()
	p.mu.Lock()
	if len(p.stopping) == 0 {
		p.mu.Unlock()
		p.t.Fatal("no stopped to deliver")
	}
	r := p.stopping[0]
	p.stopping = p.stopping[1:]
	p.mu.Unlock()
	p.h.handleCommandRecognitionStopped(r.reason, "")
	return r.ephemeral
}

// deliverAllStopped delivers every pending `stopped`.
func (p *pipelineSim) deliverAllStopped() {
	p.t.Helper()
	for p.pending() > 0 {
		p.deliverStopped()
	}
}

func (p *pipelineSim) pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stopping)
}

// liveKind is the command_recognition run up now: "ephemeral",
// "persistent" or "none".
func (p *pipelineSim) liveKind() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.live == nil:
		return "none"
	case p.live.ephemeral:
		return "ephemeral"
	default:
		return "persistent"
	}
}

func (p *pipelineSim) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.calls
	p.calls = nil
	return out
}

func (p *pipelineSim) persistentStarts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persistentRuns
}

func wantLive(t *testing.T, p *pipelineSim, want string) {
	t.Helper()
	if got := p.liveKind(); got != want {
		t.Fatalf("command_recognition run up = %s, want %s", got, want)
	}
}

// holdEnds counts calibration's hold ends.
type holdEnds struct {
	mu sync.Mutex
	n  int
}

func (c *holdEnds) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func watchHoldEnds(h *Host) *holdEnds {
	c := &holdEnds{}
	h.calibHoldEnd = func() {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return c
}

// listeningHost is continuous listening, on and asleep, its persistent run
// up, against a matcher that matches "close tab" except under the
// sleeping gate (sleepingGatePlatform).
func listeningHost(t *testing.T) (*Host, *pipelineSim, *sleepingGatePlatform) {
	t.Helper()
	h := newTestHost()
	resetState(h)
	p := simulatePipelines(t, h)
	m := &sleepingGatePlatform{
		resolvePlatform: resolvePlatform{resp: branchkit.CommandsResolveResponse{
			Matched: true, ConsumedCount: 2, Action: snapAction(),
		}},
	}
	m.mode = h.sess.GetCurrentMode
	h.sess.Platform = m
	h.sess.InitRPC()
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	p.take()
	return h, p, m
}

// sleepingGatePlatform answers like the platform's matcher under the
// continuous_sleeping gate, which is exclusive: asleep, a command phrase
// matches nothing (only the wake phrase would), while the hold's mode lets
// it match. It reads voice's mode at resolve time, as the platform reads the
// mode record voice writes.
type sleepingGatePlatform struct {
	resolvePlatform
	mode func() string
}

func (p *sleepingGatePlatform) CommandsResolve(req branchkit.CommandsResolveRequest) (*branchkit.CommandsResolveResponse, error) {
	if p.mode() == "continuous_sleeping" {
		return &branchkit.CommandsResolveResponse{}, nil
	}
	return p.resolvePlatform.CommandsResolve(req)
}

func (p *sleepingGatePlatform) CommandsResolvePhrase(req branchkit.CommandsResolvePhraseRequest) (*branchkit.CommandsResolvePhraseResponse, error) {
	if p.mode() == "continuous_sleeping" {
		return &branchkit.CommandsResolvePhraseResponse{Stopped: &branchkit.PhraseStop{}}, nil
	}
	return p.resolvePlatform.CommandsResolvePhrase(req)
}

// say delivers a command_recognition transcript and returns the actions
// voice answered it with.
func say(t *testing.T, h *Host, text string) []branchkit.Action {
	t.Helper()
	resp, err := h.handleOnTranscript(&branchkit.OnTranscriptRequest{Pipeline: "command_recognition", Text: text})
	if err != nil {
		t.Fatalf("handleOnTranscript: %v", err)
	}
	return resp.(*branchkit.OnTranscriptResponse).Actions
}

// The whole hold: G down sets the persistent run aside and starts the hold's
// own ephemeral run; G up stops it and starts the persistent run again at
// once, so speech after the release is captured; only when the hold's run
// has drained does the mode return to continuous listening.
func TestHoldOverContinuousListensOnItsOwnRun(t *testing.T) {
	h, p, _ := listeningHost(t)
	ends := watchHoldEnds(h)

	postActionWithPhase(h, t, "voice.command", "start")
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition", "run command_recognition ephemeral"}) {
		t.Fatalf("G down over continuous listening: calls %v, want the persistent run stopped and an ephemeral one started", got)
	}
	wantMode(t, h, "command_hold")
	wantLive(t, p, "ephemeral")

	// The persistent run's own `stopped` arrives during the hold: it is not
	// the hold's end.
	if p.deliverStopped() {
		t.Fatal("the first stopped should be the persistent run's")
	}
	wantMode(t, h, "command_hold")
	if n := ends.count(); n != 0 {
		t.Fatalf("calibration heard %d hold ends from the persistent run's stop", n)
	}

	if got := say(t, h, "close tab"); len(got) != 1 {
		t.Fatalf("a command under the hold: %d actions, want 1", len(got))
	}

	postActionWithPhase(h, t, "voice.command", "stop")
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition", "run command_recognition persistent"}) {
		t.Fatalf("G up: calls %v, want the hold's run stopped and the persistent one started", got)
	}
	// Draining: the hold's run has not said stopped yet, and the
	// persistent run is already capturing.
	wantMode(t, h, "command_hold")
	wantLive(t, p, "persistent")

	p.deliverStopped()
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.take(); len(got) != 0 {
		t.Fatalf("the hold's cleanup touched the runs: calls %v", got)
	}
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2 (listening on, then once at the release)", got)
	}
	if n := ends.count(); n != 1 {
		t.Fatalf("calibration heard %d hold ends, want 1", n)
	}
	if h.sess.IsCmdRecAside() {
		t.Fatal("the persistent run is still set aside after the hold")
	}
	if st := h.sess.GetCmdRecPipelineState(); st != session.PipelineActive {
		t.Fatalf("command_recognition state = %v, want active", st)
	}
	if !h.sess.IsContinuousActive() {
		t.Fatal("continuous listening ended")
	}
}

// A hold released before the persistent run it set aside has finished
// winding down: that run's `stopped` must not end the hold's drain, or the
// hold's last words would be matched after the mode went back to sleeping.
func TestShortHoldWaitsForItsOwnRun(t *testing.T) {
	h, p, _ := listeningHost(t)

	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.take()

	p.deliverStopped() // the persistent run's
	wantMode(t, h, "command_hold")
	if got := p.take(); len(got) != 0 {
		t.Fatalf("the persistent run's stop ran the hold's cleanup; calls %v", got)
	}

	// The hold's last command, decoded at the end-of-hold flush.
	if got := say(t, h, "close tab"); len(got) != 1 {
		t.Fatalf("the hold's last command was dropped: %d actions", len(got))
	}
	p.deliverStopped() // the hold's run
	wantMode(t, h, "continuous_sleeping")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}

// The utterance still being decoded at G up arrives after the release, at
// the run's end-of-hold flush, and must still be matched as the hold's
// command, not dropped by the sleeping gate.
func TestCommandInFlightAtReleaseStillMatches(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.command", "start")
	p.deliverStopped() // the persistent run's
	postActionWithPhase(h, t, "voice.command", "stop")

	if got := say(t, h, "close tab"); len(got) != 1 {
		t.Fatalf("the command in flight at the release was dropped: %d actions", len(got))
	}
	p.deliverStopped()
	wantMode(t, h, "continuous_sleeping")
}

// Nothing left to say at the release: the hold's run says stopped with no
// transcript, and the mode returns to sleeping.
func TestSilentReleaseOverContinuousReturnsToSleeping(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
}

// The command was delivered while G was down; the person releases and keeps
// talking. Speech after the release never reaches the hold's run (its
// microphone stops at the release) and the persistent run comes back only
// once the mode is sleeping again, so it is not a command.
func TestSpeechAfterReleaseIsNotACommand(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.command", "start")
	if got := say(t, h, "close tab"); len(got) != 1 {
		t.Fatalf("the hold's command: %d actions, want 1", len(got))
	}
	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()

	// Later words come from the persistent run, asleep.
	if got := say(t, h, "close tab"); len(got) != 0 {
		t.Fatalf("speech after the release ran as a command: %#v", got)
	}
	wantMode(t, h, "continuous_sleeping")
}

// G down, T down, G up, T up over continuous listening: the dictation must
// not stop the hold's run, the hold's end must not bring the persistent run
// back while the dictation holds the microphone, and the dictation's end
// brings it back once.
func TestOverlappingChordsOverContinuousRestartOnce(t *testing.T) {
	h, p, _ := listeningHost(t)

	postActionWithPhase(h, t, "voice.command", "start")
	p.take()
	postActionWithPhase(h, t, "voice.dictation", "start")
	if got := p.take(); slices.Contains(got, "stop command_recognition") {
		t.Fatalf("T down stopped the hold's run; calls %v", got)
	}
	wantLive(t, p, "ephemeral")

	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "dictation")
	wantLive(t, p, "none")
	if got := p.persistentStarts(); got != 1 {
		t.Fatalf("the persistent run came back while a dictation holds the microphone (%d starts)", got)
	}

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}

// T down, G down, T up, G up over continuous listening: the dictation set
// the persistent run aside first, so the hold stops nothing; T up leaves the
// hold's run alone; G up's cleanup brings the persistent run back once.
func TestDictationThenHoldOverContinuousRestartOnce(t *testing.T) {
	h, p, _ := listeningHost(t)

	postActionWithPhase(h, t, "voice.dictation", "start")
	wantLive(t, p, "none")
	p.take()
	postActionWithPhase(h, t, "voice.command", "start")
	if got := p.take(); !slices.Equal(got, []string{"run command_recognition ephemeral"}) {
		t.Fatalf("G down under a dictation: calls %v", got)
	}

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	wantMode(t, h, "command_hold")
	wantLive(t, p, "ephemeral")
	if got := p.persistentStarts(); got != 1 {
		t.Fatalf("T up restarted the persistent run under a hold (%d starts)", got)
	}
	// The dictation is over; the hold's commands still count.
	if got := say(t, h, "close tab"); len(got) != 1 {
		t.Fatalf("a command under the hold after T up: %d actions", len(got))
	}

	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}

// T down, G down, G up, T up over continuous listening.
func TestHoldInsideDictationOverContinuousRestartOnce(t *testing.T) {
	h, p, _ := listeningHost(t)

	postActionWithPhase(h, t, "voice.dictation", "start")
	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "dictation")
	if got := p.persistentStarts(); got != 1 {
		t.Fatalf("the hold's end restarted the persistent run under a dictation (%d starts)", got)
	}

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	wantMode(t, h, "continuous_sleeping")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}

// A second press while the first hold is still draining: the persistent run
// the first release started is set aside again; the first hold's runs
// ending must not end the second hold's drain; each release starts the
// persistent run once.
func TestFastRePressOverContinuous(t *testing.T) {
	h, p, _ := listeningHost(t)

	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.take()

	postActionWithPhase(h, t, "voice.command", "start")
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition", "run command_recognition ephemeral"}) {
		t.Fatalf("re-press while draining: calls %v, want the persistent run set aside again and the new hold's run", got)
	}
	postActionWithPhase(h, t, "voice.command", "stop")

	p.deliverStopped() // persistent
	p.deliverStopped() // first hold's run
	p.deliverStopped() // persistent, started at the first release
	wantMode(t, h, "command_hold")
	if got := p.persistentStarts(); got != 3 {
		t.Fatalf("persistent runs started = %d, want 3 (listening on, then once per release)", got)
	}
	p.deliverStopped() // second hold's run
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 3 {
		t.Fatalf("the second hold's cleanup started another persistent run (%d starts)", got)
	}
}

// An aborted hold over continuous listening stops its run and gives
// continuous listening its run back at once; the runs' later `stopped`s
// change nothing.
func TestAbortOverContinuousRestoresListening(t *testing.T) {
	h, p, _ := listeningHost(t)
	h.setVoiceMode("continuous_awake")

	postActionWithPhase(h, t, "voice.command", "start")
	p.take()
	postActionWithPhase(h, t, "voice.command", "abort")
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition", "run command_recognition persistent"}) {
		t.Fatalf("abort: calls %v, want the hold's run stopped and the persistent one started", got)
	}
	wantMode(t, h, "continuous_awake")

	p.deliverAllStopped()
	wantMode(t, h, "continuous_awake")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}

// Turning continuous listening off while a released hold drains: the hold
// ends first, the persistent run its release started is stopped and not
// restarted on the way, and nothing is left claiming continuous listening.
func TestContinuousOffDuringHoldDrain(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.take()

	h.lastContinuousToggle = h.lastContinuousToggle.AddDate(-1, 0, 0)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	wantMode(t, h, "idle")
	if h.sess.IsContinuousActive() {
		t.Fatal("continuous listening still reports active")
	}
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition"}) {
		t.Fatalf("toggle off during the drain: calls %v, want the persistent run stopped", got)
	}
	p.deliverAllStopped()
	wantMode(t, h, "idle")
	wantLive(t, p, "none")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2 (listening on, then the release)", got)
	}
}

// The toggle waits while G is down, in both directions.
func TestContinuousToggleWaitsForTheRelease(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.command", "start")
	h.lastContinuousToggle = h.lastContinuousToggle.AddDate(-1, 0, 0)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	wantMode(t, h, "command_hold")
	wantLive(t, p, "ephemeral")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "continuous_sleeping")
}

// A speak session over continuous listening hears its stop phrase on
// command_recognition: a hold under it listens on its own run like any other
// hold, the persistent run comes back after, and the speak session lives on.
func TestHoldUnderSpeakOverContinuous(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.speak", "start")
	wantMode(t, h, "dictation")
	wantLive(t, p, "persistent")
	p.take()

	postActionWithPhase(h, t, "voice.command", "start")
	wantLive(t, p, "ephemeral")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "dictation")
	wantLive(t, p, "persistent")
	if !h.sess.GetSpeakDictating() {
		t.Fatal("speak session ended")
	}

	speakStopPhrase(h)
	if h.sess.GetSpeakDictating() {
		t.Fatal("the stop phrase did not end the speak session")
	}
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
}

// The hold's run refused: continuous listening gets its run back at once
// rather than nothing listening, and the release stops nothing.
func TestRefusedHoldRunRestoresListening(t *testing.T) {
	h, p, _ := listeningHost(t)
	p.refuseEphemeral = true

	postActionWithPhase(h, t, "voice.command", "start")
	if got := p.take(); !slices.Equal(got, []string{
		"stop command_recognition", "refused command_recognition ephemeral", "run command_recognition persistent",
	}) {
		t.Fatalf("calls %v", got)
	}
	postActionWithPhase(h, t, "voice.command", "stop")
	if got := p.take(); slices.Contains(got, "stop command_recognition") {
		t.Fatalf("G up stopped continuous listening's run; calls %v", got)
	}
	wantMode(t, h, "continuous_sleeping")
	p.deliverAllStopped()
	wantLive(t, p, "persistent")
}

// While a hold has the persistent run aside, no run's end restarts it: the
// hold's cleanup does that. A failure of the hold's own run is not
// continuous listening failing.
func TestRecoveryLeavesAHoldsRunAlone(t *testing.T) {
	h, p, _ := listeningHost(t)
	r := runRestartsNow(h)

	postActionWithPhase(h, t, "voice.command", "start")
	p.deliverStopped() // the persistent run's, requested
	p.fail()           // the hold's run fails
	p.deliverStopped()
	if r.count != 0 {
		t.Fatalf("a restart was scheduled during the hold (%d)", r.count)
	}
	wantLive(t, p, "none")
	wantMode(t, h, "command_hold")

	postActionWithPhase(h, t, "voice.command", "stop")
	// Nothing to stop, so no stopped comes: the backstop ends the drain.
	h.sess.FinishCommandHoldDrain(h.finishCommandHold)
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}

// A `stopped` for a run voice never saw start (one from before this process
// restarted) is handled as before runs were told apart: it ends a pending
// hold drain and reaches calibration and the restart logic.
func TestStoppedForAnUnknownRun(t *testing.T) {
	h := newTestHost()
	resetState(h)
	watchPipelines(h)
	ends := watchHoldEnds(h)

	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	h.handleCommandRecognitionStopped("requested", "")
	wantMode(t, h, "idle")
	if n := ends.count(); n != 1 {
		t.Fatalf("calibration heard %d hold ends, want 1", n)
	}
}

// Outside continuous listening, a hold that shares a speak session's run and
// outlives the session must still stop that run at its release: nothing else
// would, and an ephemeral run is a microphone open as if a key were held.
func TestHoldOutlivingASpeakSessionStopsItsRun(t *testing.T) {
	h := newTestHost()
	resetState(h)
	p := simulatePipelines(t, h)

	postActionWithPhase(h, t, "voice.speak", "start")
	wantLive(t, p, "ephemeral")
	p.take()
	postActionWithPhase(h, t, "voice.command", "start")
	if got := p.take(); slices.Contains(got, "run command_recognition ephemeral") {
		t.Fatalf("the hold started a run beside the speak session's; calls %v", got)
	}
	// Option+C ends a live speak session before its "blocked while G is
	// down" check.
	h.lastContinuousToggle = h.lastContinuousToggle.AddDate(-1, 0, 0)
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	if h.sess.GetSpeakDictating() {
		t.Fatal("the toggle did not end the speak session")
	}
	wantLive(t, p, "ephemeral") // the hold is still down

	postActionWithPhase(h, t, "voice.command", "stop")
	wantLive(t, p, "none")
	p.deliverAllStopped()
	wantMode(t, h, "idle")
}

// Push-to-talk: "speak" decoded at the end-of-hold flush starts the speak
// session's run while the hold drains. The hold's cleanup still runs on its
// own run's stopped, not 2 s later on the backstop.
func TestSpeakStartedAtTheFlushDoesNotHoldUpTheCleanup(t *testing.T) {
	h := newTestHost()
	resetState(h)
	p := simulatePipelines(t, h)

	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	postActionWithPhase(h, t, "voice.speak", "start")
	wantLive(t, p, "ephemeral") // the speak session's
	if !p.deliverStopped() {
		t.Fatal("want the hold's run's stopped")
	}
	if h.sess.CommandHoldDraining() {
		t.Fatal("the hold's cleanup waited on a run started after its release")
	}
	wantMode(t, h, "dictation")
}

// A press that lands while the previous hold's cleanup is still running
// (the cleanup runs on the notification goroutine, the press on its own):
// the new hold sets aside the persistent run the release started, and the
// old cleanup must leave it aside for the new hold rather than start it
// beside the new hold's run; the new hold's release restarts it, once.
func TestRePressDuringTheLastHoldsCleanup(t *testing.T) {
	h, p, _ := listeningHost(t)
	postActionWithPhase(h, t, "voice.command", "start")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverStopped() // the persistent run's
	p.take()

	// The last run's stopped consumes the drain; before its cleanup
	// reaches the restart, G goes down again.
	if !h.sess.CommandHoldDraining() {
		t.Fatal("want the first hold draining")
	}
	h.sess.FinishCommandHoldDrain(func() {
		postActionWithPhase(h, t, "voice.command", "start")
		h.finishCommandHold()
	})
	if got := p.take(); !slices.Equal(got, []string{"stop command_recognition", "run command_recognition ephemeral"}) {
		t.Fatalf("calls %v, want the persistent run set aside again and only the new hold's run", got)
	}
	wantLive(t, p, "ephemeral")
	if !h.sess.IsCmdRecAside() {
		t.Fatal("the old cleanup released the new hold's claim")
	}

	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 3 {
		t.Fatalf("persistent runs started = %d, want 3 (listening on, then once per release)", got)
	}
}

// Continuous listening restored after voice restarts (the mode came back
// from the record) must mark command recognition active like the toggle
// does: a hold reads that to decide whether to stop the persistent run and
// whether its cleanup restarts it. Left inactive, the first hold stopped the
// persistent run and nothing started it again.
func TestHoldAfterContinuousRestoredOnRestart(t *testing.T) {
	h := newTestHost()
	resetState(h)
	p := simulatePipelines(t, h)
	h.restoreContinuousMode("continuous_sleeping")
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if st := h.sess.GetCmdRecPipelineState(); st != session.PipelineActive {
		t.Fatalf("command_recognition state = %v after the restore, want active", st)
	}

	postActionWithPhase(h, t, "voice.command", "start")
	wantLive(t, p, "ephemeral")
	postActionWithPhase(h, t, "voice.command", "stop")
	p.deliverAllStopped()
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
}

// Voice restarted mid-hold over continuous listening, and the platform could
// not stop the dead process's hold run. Restoring continuous listening must
// replace it with a persistent run, not be skipped beside it and leave
// listening on a run the platform treats as a held key; and the old run's
// stopped must not be taken for the new run's.
func TestRestoreReplacesAnOrphanedHoldRun(t *testing.T) {
	h := newTestHost()
	resetState(h)
	p := simulatePipelines(t, h)
	p.live = &simRun{ephemeral: true} // the dead process's hold run

	h.restoreContinuousMode("continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 1 {
		t.Fatalf("persistent runs started = %d, want 1", got)
	}
	p.deliverAllStopped() // the orphan's

	quickHoldWaitsForItsOwnRun(t, h, p)
}

// Continuous listening's run survived voice's restart: the restore keeps it
// and records it, so the first hold after the restart does not take that
// run's stopped for its own.
func TestRestoreKeepsASurvivingRunAndTracksIt(t *testing.T) {
	h := newTestHost()
	resetState(h)
	p := simulatePipelines(t, h)
	p.live = &simRun{} // continuous listening's run, from before the restart

	h.restoreContinuousMode("continuous_sleeping")
	if got := p.take(); len(got) != 0 {
		t.Fatalf("the surviving run was touched; calls %v", got)
	}
	wantLive(t, p, "persistent")

	quickHoldWaitsForItsOwnRun(t, h, p)
}

// quickHoldWaitsForItsOwnRun: a hold released before the persistent run it
// set aside has said stopped ends only on its own run's stopped.
func quickHoldWaitsForItsOwnRun(t *testing.T, h *Host, p *pipelineSim) {
	t.Helper()
	postActionWithPhase(h, t, "voice.command", "start")
	wantLive(t, p, "ephemeral")
	postActionWithPhase(h, t, "voice.command", "stop")
	if p.deliverStopped() {
		t.Fatal("want the persistent run's stopped first")
	}
	wantMode(t, h, "command_hold")
	if !p.deliverStopped() {
		t.Fatal("want the hold's run's stopped")
	}
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
}

// T down, G down over continuous listening: the hold under the dictation
// listens on a run of its own, and what that run hears counts, as it does
// for the same hold without continuous listening. The dictation suppresses
// continuous listening's persistent run, not a hold's run.
func TestHoldUnderDictationOverContinuousHearsCommands(t *testing.T) {
	h, p, _ := listeningHost(t)

	postActionWithPhase(h, t, "voice.dictation", "start")
	p.take()
	postActionWithPhase(h, t, "voice.command", "start")
	if got := p.take(); !slices.Equal(got, []string{"run command_recognition ephemeral"}) {
		t.Fatalf("G down under a dictation: calls %v", got)
	}
	wantLive(t, p, "ephemeral")
	if got := say(t, h, "close tab"); len(got) != 1 {
		t.Fatalf("a command under a hold inside a dictation: %d actions, want 1", len(got))
	}

	// G up: a command still in flight on the hold's run counts too.
	postActionWithPhase(h, t, "voice.command", "stop")
	if got := say(t, h, "close tab"); len(got) != 1 {
		t.Fatalf("a command flushed at the release of a hold inside a dictation: %d actions, want 1", len(got))
	}
	p.deliverAllStopped()
	wantMode(t, h, "dictation")

	holdDictationLong(h)
	postActionWithPhase(h, t, "voice.dictation", "stop")
	wantMode(t, h, "continuous_sleeping")
	wantLive(t, p, "persistent")
	if got := p.persistentStarts(); got != 2 {
		t.Fatalf("persistent runs started = %d, want 2", got)
	}
}
