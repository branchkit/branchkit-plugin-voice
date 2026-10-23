package main

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// A trial refused for the missing privilege asks for it, once; any other
// outcome asks nothing.
func TestBeginTrialOrAskRequestsTheCalibrationPrivilegeOnce(t *testing.T) {
	forbidden := &branchkit.RPCError{Code: -32003, Kind: branchkit.ErrorKindForbidden, Message: "not allowed"}
	var asked atomic.Bool
	asks := 0
	ask := func() (string, error) { asks++; return "pending", nil }

	for i := 0; i < 3; i++ {
		id, err := beginTrialOrAsk(func() (string, error) { return "", forbidden }, ask, &asked)
		if id != "" || !errors.Is(err, branchkit.ErrForbidden) {
			t.Fatalf("refusal: got %q, %v", id, err)
		}
	}
	if asks != 1 {
		t.Fatalf("asked %d times, want once", asks)
	}

	var fresh atomic.Bool
	id, err := beginTrialOrAsk(func() (string, error) { return "t1", nil }, ask, &fresh)
	if id != "t1" || err != nil {
		t.Fatalf("open trial: got %q, %v", id, err)
	}
	other := errors.New("platform gone")
	if _, err := beginTrialOrAsk(func() (string, error) { return "", other }, ask, &fresh); err != other {
		t.Fatalf("other error: %v", err)
	}
	if asks != 1 || fresh.Load() {
		t.Fatalf("asked on a non-privilege outcome")
	}

	// A request that failed outright filed nothing: the next refusal asks again.
	var retry atomic.Bool
	failing := 0
	flaky := func() (string, error) { failing++; return "", errors.New("timeout") }
	refuse := func() (string, error) { return "", forbidden }
	beginTrialOrAsk(refuse, flaky, &retry)
	beginTrialOrAsk(refuse, flaky, &retry)
	if failing != 2 {
		t.Fatalf("a failed request was not retried: %d calls", failing)
	}
}
