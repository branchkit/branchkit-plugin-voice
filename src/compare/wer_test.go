package compare

import "testing"

func TestDictationWords(t *testing.T) {
	got := DictationWords("Their team said they're going, Friday. It's 3:30!")
	want := []string{"their", "team", "said", "they're", "going", "friday", "it's", "3", "30"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Case and punctuation are never errors; a substitution, an insertion and a
// deletion each count one.
func TestWordErrors(t *testing.T) {
	cases := []struct {
		ref, heard string
		errs, n    int
	}{
		{"Send it to Priya by Friday.", "send it to priya by friday", 0, 6},
		{"Send it to Priya by Friday.", "Send it to Maria by Friday.", 1, 6},
		{"Send it to Priya by Friday.", "Send it over to Priya by Friday.", 1, 6},
		{"Send it to Priya by Friday.", "Send it to Priya Friday.", 1, 6},
		{"Their team is there.", "There team is their.", 2, 4},
		{"Hello there.", "", 2, 2},
	}
	for _, c := range cases {
		errs, n := WordErrors(c.ref, c.heard)
		if errs != c.errs || n != c.n {
			t.Errorf("WordErrors(%q, %q) = %d/%d, want %d/%d", c.ref, c.heard, errs, n, c.errs, c.n)
		}
	}
}

// A failed decode counts every word of its sentence as an error and has no
// time; recordings with no verdict are left out.
func TestSummarizeWords(t *testing.T) {
	clips := []Clip{{File: "a"}, {File: "b"}, {File: "c"}}
	have := map[string]Result{
		Key("m", "v", "", "a"): {Errors: 1, Words: 10, DecodeMs: 800},
		Key("m", "v", "", "b"): {Words: 6, Error: "timeout"},
	}
	s := SummarizeWords(clips, "m", "v", "", have)
	if s.Total != 2 || s.Words != 16 || s.Errors != 7 || s.Failed != 1 || s.MedianMs != 800 {
		t.Errorf("summary = %+v", s)
	}
}
