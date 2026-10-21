package main

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// restartsNow runs a scheduled restart at once, and records that one was
// scheduled.
type restartsNow struct {
	mu    sync.Mutex
	count int
}

func runRestartsNow(h *Host) *restartsNow {
	r := &restartsNow{}
	h.cmdRecRestartAfter = func(_ time.Duration, f func()) {
		r.mu.Lock()
		r.count++
		r.mu.Unlock()
		f()
	}
	return r
}

func notices(h *Host) *[]branchkit.SystemNotifyRequest {
	var got []branchkit.SystemNotifyRequest
	h.systemNotify = func(req branchkit.SystemNotifyRequest) error {
		got = append(got, req)
		return nil
	}
	return &got
}

func listening(t *testing.T, h *Host) {
	t.Helper()
	postActionWithPhase(h, t, "voice.continuous_toggle", "start")
	wantMode(t, h, "continuous_sleeping")
}

// Command recognition that dies on an error while continuous listening is on
// is started again. It used to stay down while the menu bar said listening.
func TestContinuousRestartsCommandRecognitionThatFailed(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	runRestartsNow(h)
	listening(t, h)
	calls.take()

	h.handleCmdRecStopped("error", "stage exited")
	if got := calls.take(); !slices.Equal(got, []string{"run command_recognition"}) {
		t.Fatalf("a failed run is started again; calls %v", got)
	}
	wantMode(t, h, "continuous_sleeping")
}

// A reload is not a failure: the run is started again at once, and the
// failure budget is not spent on it.
func TestContinuousRestartsCommandRecognitionAfterAReload(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	scheduled := runRestartsNow(h)
	listening(t, h)
	calls.take()

	for range cmdRecRestartLimit + 2 {
		h.handleCmdRecStopped("reload", "")
	}
	if got := calls.take(); len(got) != cmdRecRestartLimit+2 {
		t.Fatalf("every reload restarts it; calls %v", got)
	}
	if scheduled.count != 0 {
		t.Fatalf("a reload restarts at once, not after a delay")
	}
	wantMode(t, h, "continuous_sleeping")
}

// Only a stop nobody asked for is acted on, and only while listening and not
// set aside for a dictation.
func TestCommandRecognitionStopsThatAreNotRestarted(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	runRestartsNow(h)

	// Not listening: a hold's run that failed is not started again.
	h.handleCmdRecStopped("error", "x")
	if got := calls.take(); len(got) != 0 {
		t.Fatalf("outside continuous listening nothing restarts; calls %v", got)
	}

	listening(t, h)
	calls.take()
	h.handleCmdRecStopped("requested", "")
	if got := calls.take(); len(got) != 0 {
		t.Fatalf("a requested stop is not undone; calls %v", got)
	}

	// Dictation set command recognition aside: its end starts it again.
	h.stopCmdRecForDictation()
	calls.take()
	h.handleCmdRecStopped("error", "x")
	if got := calls.take(); len(got) != 0 {
		t.Fatalf("command recognition set aside for dictation stays down; calls %v", got)
	}
}

// A stage that keeps failing is not restarted forever: past the budget,
// continuous listening goes off, and the user is told why.
func TestContinuousGivesUpOnCommandRecognitionThatKeepsFailing(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	runRestartsNow(h)
	got := notices(h)
	listening(t, h)
	calls.take()

	for range cmdRecRestartLimit {
		h.handleCmdRecStopped("error", "model missing")
	}
	if runs := calls.take(); len(runs) != cmdRecRestartLimit {
		t.Fatalf("restarted within the budget; calls %v", runs)
	}
	if len(*got) != 0 {
		t.Fatalf("no notice within the budget: %v", *got)
	}

	h.handleCmdRecStopped("error", "model missing")
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("not restarted past the budget; calls %v", runs)
	}
	wantMode(t, h, "idle")
	if h.sess.IsContinuousActive() {
		t.Fatal("continuous listening is off")
	}
	if len(*got) != 1 || !strings.Contains((*got)[0].Body, "model missing") {
		t.Fatalf("one notice naming the error: %v", *got)
	}
}

func TestCmdRecRestartBudgetRefillsAfterTheWindow(t *testing.T) {
	var r cmdRecRecovery
	start := time.Now()
	for i := range cmdRecRestartLimit {
		if !r.allow(start.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("restart %d is within the budget", i)
		}
	}
	if r.allow(start.Add(10 * time.Second)) {
		t.Fatal("one past the limit within the window is refused")
	}
	if !r.allow(start.Add(cmdRecRestartWindow + 3*time.Second)) {
		t.Fatal("the budget refills once the window has passed")
	}
}

// Giving up under an overlay that covers continuous listening (a speak
// session, which listens on the persistent run) leaves the overlay alone,
// and its end lands on idle. Restoring the continuous base would say
// listening while nothing listens. (An Option+G hold never sees this: it
// sets the persistent run aside, so nothing restarts or gives up under it.)
func TestGivingUpUnderAnOverlayReleasesToIdle(t *testing.T) {
	h := newTestHost()
	resetState(h)
	calls := watchPipelines(h)
	runRestartsNow(h)
	notices(h)
	listening(t, h)
	postActionWithPhase(h, t, "voice.speak", "start")
	wantMode(t, h, "dictation")
	calls.take()

	for range cmdRecRestartLimit + 1 {
		h.handleCmdRecStopped("error", "stage exited")
	}
	wantMode(t, h, "dictation")
	if h.sess.IsContinuousActive() {
		t.Fatal("continuous listening is still on under the overlay")
	}

	speakStopPhrase(h)
	wantMode(t, h, "idle")

	calls.take()
	postActionWithPhase(h, t, "voice.command", "start")
	if got := calls.take(); !slices.Contains(got, "run command_recognition") {
		t.Fatalf("the next hold runs its own recognition; calls %v", got)
	}
}
