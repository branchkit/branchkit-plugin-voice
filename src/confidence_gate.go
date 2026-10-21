package main

// The command confidence gate. The command engine decodes
// against a closed grammar, so speech that is not a command is often forced
// onto the nearest command's words; the worst word's margin separates those
// forced decodes from real commands. This decides what a decode below the
// floor does. The floor is measured per model with `just margin-gate`.

type gateVerdict int

const (
	gatePass gateVerdict = iota // at or above the floor, or not gated here
	gateFlag                    // shadow: runs, marked as below the floor
	gateDrop                    // enforce: not run
)

// commandGate applies the configured gate to one decode. Wake candidates
// (continuous_sleeping) and speak-session stop phrases (dictation) are
// separate regimes with their own planned gates, and a decode without
// margins has nothing to judge, so those always pass.
func commandGate(mode string, worst *float64, gate string, floor float64) gateVerdict {
	if worst == nil || mode == "continuous_sleeping" || mode == "dictation" || *worst >= floor {
		return gatePass
	}
	switch gate {
	case "shadow":
		return gateFlag
	case "enforce":
		return gateDrop
	}
	return gatePass
}
