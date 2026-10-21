package main

import "testing"

func TestCommandGate(t *testing.T) {
	m := func(v float64) *float64 { return &v }
	cases := []struct {
		name  string
		mode  string
		worst *float64
		gate  string
		want  gateVerdict
	}{
		{"above the floor passes", "command_hold", m(-2), "enforce", gatePass},
		{"at the floor passes", "command_hold", m(-6), "enforce", gatePass},
		{"below, enforce drops", "command_hold", m(-8), "enforce", gateDrop},
		{"below, shadow flags", "command_hold", m(-8), "shadow", gateFlag},
		{"below, off passes", "command_hold", m(-8), "off", gatePass},
		{"no margins passes", "command_hold", nil, "enforce", gatePass},
		{"wake candidates are not gated here", "continuous_sleeping", m(-20), "enforce", gatePass},
		{"speak stop phrases are not gated here", "dictation", m(-20), "enforce", gatePass},
		{"awake continuous mode is gated", "continuous_awake", m(-8), "enforce", gateDrop},
	}
	for _, c := range cases {
		if got := commandGate(c.mode, c.worst, c.gate, -6); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
