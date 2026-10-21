package calibration

import (
	"reflect"
	"testing"
)

func TestHeardTallies(t *testing.T) {
	tr := &Trial{Frames: []Frame{
		{Text: "first"},
		{Text: "next"},
		{Text: "next"},
		{Text: ""}, // empty frames are ignored
		{Text: "next"},
		{Text: "vest"},
	}}
	got := tr.HeardTallies()
	want := []HeardTally{{"first", 1}, {"next", 3}, {"vest", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HeardTallies = %+v, want %+v", got, want)
	}
	// Revisions counts distinct values minus the initial.
	if r := tr.Revisions(); r != 2 {
		t.Errorf("Revisions = %d, want 2", r)
	}
}

func TestHeardTalliesSingleRepeat(t *testing.T) {
	// A word misheard as the same wrong thing every time, never settled —
	// the multiplicity must survive (not collapse to one step).
	tr := &Trial{Frames: []Frame{{Text: "next"}, {Text: "next"}, {Text: "next"}}}
	got := tr.HeardTallies()
	if len(got) != 1 || got[0].Text != "next" || got[0].Count != 3 {
		t.Errorf("HeardTallies = %+v, want [{next 3}]", got)
	}
}

func newPlayingGame(word string) *Game {
	g := &Game{Words: []string{word}, Phase: PhasePlaying}
	g.startNewTrial()
	return g
}

func TestSilentHoldsCounted(t *testing.T) {
	g := newPlayingGame("vest")

	// Hold 1: nothing recognized → boundary with no content = silent.
	g.MarkHoldBoundary()
	// Hold 2: heard "next" (a mismatch), then boundary = not silent.
	g.ProcessFrame("next", false)
	g.MarkHoldBoundary()
	// Hold 3: nothing again = silent.
	g.MarkHoldBoundary()
	// Hold 4: heard "vest" as a non-empty final (content + boundary in one).
	g.ProcessFrame("vest", true)
	g.MarkHoldBoundary()

	if g.CurrentTrial.SilentHolds != 2 {
		t.Errorf("SilentHolds = %d, want 2", g.CurrentTrial.SilentHolds)
	}
	tallies := g.CurrentTrial.HeardTallies()
	want := []HeardTally{{"next", 1}, {"vest", 1}}
	if !reflect.DeepEqual(tallies, want) {
		t.Errorf("HeardTallies = %+v, want %+v", tallies, want)
	}
}

func TestHoldEndMarksSilentHold(t *testing.T) {
	s := NewState()
	s.SetTabVisible(true)
	s.StartGame([]string{"vest"})

	// Empty frames are ignored by the ingest path.
	if s.ProcessSpeechFrame("", false) {
		t.Error("empty frame should not be consumed")
	}
	// Hold boundary (key-up) with nothing heard this hold → silent.
	s.HandleHoldEnd()
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentTrial.SilentHolds != 1 {
			t.Errorf("SilentHolds = %d, want 1", g.CurrentTrial.SilentHolds)
		}
	})

	// A real recognition followed by its hold boundary is NOT silent.
	s.ProcessSpeechFrame("next", false)
	s.HandleHoldEnd()
	s.WithLock(func(g *Game, _ bool) {
		if g.CurrentTrial.SilentHolds != 1 {
			t.Errorf("SilentHolds after a heard hold = %d, want 1 (unchanged)", g.CurrentTrial.SilentHolds)
		}
	})
}
