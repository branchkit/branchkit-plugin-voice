package calibration

import (
	"sync/atomic"
	"testing"
	"time"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

type countingPlatform struct{ refreshes atomic.Int32 }

func (p *countingPlatform) RecognitionRedecode(branchkit.RecognitionRedecodeRequest) (*branchkit.RecognitionRedecodeResponse, error) {
	return nil, nil
}

func (p *countingPlatform) SettingsRefresh() error {
	p.refreshes.Add(1)
	return nil
}

// A burst of marks is one refresh after the debounce, and with nothing
// marked the loop does nothing (it sleeps rather than polling a ticker).
func TestRefreshLoopCoalescesAndSleeps(t *testing.T) {
	p := &countingPlatform{}
	s := NewState()
	s.Platform = p
	s.StartRefreshCoalescer()

	time.Sleep(4 * refreshDebounce)
	if n := p.refreshes.Load(); n != 0 {
		t.Fatalf("refreshes with nothing marked = %d, want 0", n)
	}

	for i := 0; i < 5; i++ {
		s.MarkRefreshDirty()
	}
	time.Sleep(4 * refreshDebounce)
	if n := p.refreshes.Load(); n != 1 {
		t.Fatalf("refreshes after a burst of marks = %d, want 1", n)
	}

	s.FlushRefresh()
	deadline := time.Now().Add(refreshDebounce)
	for p.refreshes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := p.refreshes.Load(); n != 2 {
		t.Fatalf("refreshes after a flush = %d, want 2 within the debounce", n)
	}
}
