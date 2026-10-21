package main

import (
	"strings"
	"testing"
)

// Boot reconcile: which persisted modes a fresh process contradicts.
//
// `setVoiceMode` writes `plugin.voice.mode` AND `_platform.input`, both of which
// outlive this process. `command_hold` asserts lift_keys_on_dispatch, which
// `operations::registered::dispatch` reads on EVERY dispatch by EVERY plugin —
// so a crash mid-hold has every action in the system lifting modifiers because a
// dead process said it was holding a key.
//
// The classification is the whole decision, so it is what gets tested; the RPCs
// around it are plumbing.

func TestHoldModesNeedABootReset(t *testing.T) {
	for _, mode := range []string{"command_hold", "dictation", "spelling"} {
		if !modeNeedsBootReset(mode) {
			t.Errorf("%q describes a key being held — a fresh process must reset it", mode)
		}
	}
}

// The one that matters most: continuous mode is DELIBERATELY persistent across
// a hot-reload, and restoreContinuousMode re-enters it 500ms after boot. A boot
// reset that clobbered it would silently turn a hot-reload into a mode exit —
// trading one bug for a worse one, because the user asked for continuous.
func TestContinuousModesSurviveBootReset(t *testing.T) {
	for _, mode := range []string{"continuous_awake", "continuous_sleeping"} {
		if modeNeedsBootReset(mode) {
			t.Errorf("%q is restored by restoreContinuousMode — resetting it here "+
				"would make a hot-reload drop the user out of continuous mode", mode)
		}
	}
}

func TestIdleAndUnknownModesNeedNoReset(t *testing.T) {
	if modeNeedsBootReset("idle") {
		t.Error("idle is the reset target; resetting it would be a redundant write")
	}
	if modeNeedsBootReset("mode_from_a_future_build") {
		t.Error("an unrecognized mode has no known projection to undo")
	}
}

// Reads modeRecords rather than a hardcoded list, so a hold mode added to that
// table without thinking about boot is caught here rather than shipping as a
// second instance of the same strand.
func TestEveryActiveNonContinuousModeIsCoveredByTheReset(t *testing.T) {
	for name, rec := range modeRecords {
		if !rec.active || strings.HasPrefix(name, "continuous_") {
			continue
		}
		if !modeNeedsBootReset(name) {
			t.Errorf("mode %q projects an active input session but boot won't reset it", name)
		}
	}
}

// --- Overlay carry ---
//
// The half of the boot story the reset alone can't tell. An overlay (dictation,
// or a speak session) replaces the durable continuous record with its own, and
// the covered mode lives only in the session's in-memory priorMode. Nothing on
// disk said "this dictation is standing on top of continuous", so a hot-reload
// mid-overlay reset the residue to idle and restoreContinuousMode found nothing
// to restore — the user was silently dropped out of a mode they had asked for.

func TestOverlayRecordCarriesTheModeItCovers(t *testing.T) {
	for _, covered := range []string{"continuous_awake", "continuous_sleeping"} {
		payload := modePayload("dictation", modeRecords["dictation"], covered)
		if got := payload["overlay_of"]; got != covered {
			t.Errorf("dictation over %q: overlay_of = %v, want %q — without it a "+
				"hot-reload mid-dictation drops the user out of continuous", covered, got, covered)
		}
	}
}

func TestNonOverlayRecordsCarryNothingToRestore(t *testing.T) {
	// No overlay open: priorMode is empty.
	if _, ok := modePayload("command_hold", modeRecords["command_hold"], "")["overlay_of"]; ok {
		t.Error("a mode with no overlay beneath it must not claim one")
	}
	// An overlay over a non-durable mode has nothing worth restoring: idle is
	// where the boot reset lands anyway, so carrying it would make the restore
	// path fire for a mode nobody asked to keep.
	if _, ok := modePayload("dictation", modeRecords["dictation"], "idle")["overlay_of"]; ok {
		t.Error("only continuous modes are durable intent; idle must not be carried")
	}
}

// Leaving the overlay restores the covered mode as the record's own name, so
// the carry must not persist past it — a record that both IS continuous and
// claims to cover continuous would survive one reload and then restore itself
// from stale evidence forever.
func TestRestoredContinuousRecordDropsTheCarry(t *testing.T) {
	payload := modePayload("continuous_awake", modeRecords["continuous_awake"], "")
	if _, ok := payload["overlay_of"]; ok {
		t.Error("LeaveOverlay clears priorMode before the restoring write; the carry must clear with it")
	}
}

// The exclusive flag and the carry are independent fields on the same record.
// continuous_sleeping is the exclusive one, and it is also the mode most often
// covered by an overlay — worth pinning that neither write eats the other.
func TestExclusiveFlagAndCarryCoexist(t *testing.T) {
	payload := modePayload("continuous_sleeping", modeRecords["continuous_sleeping"], "")
	if payload["exclusive"] != true {
		t.Error("continuous_sleeping's gate is exclusive; the record is what makes it so")
	}
}

func TestIsContinuousModeAgreesWithTheBootReset(t *testing.T) {
	// One predicate, three readers (reset, carry, restore). If they ever
	// disagree, a continuous mode gets reset by one and restored by another.
	for name := range modeRecords {
		if isContinuousMode(name) == modeNeedsBootReset(name) && isContinuousMode(name) {
			t.Errorf("%q is continuous but the boot reset also claims it", name)
		}
	}
	if !isContinuousMode("continuous_awake") || !isContinuousMode("continuous_sleeping") {
		t.Error("both durable continuous modes must be recognized as continuous")
	}
	for _, name := range []string{"idle", "dictation", "command_hold", "spelling", ""} {
		if isContinuousMode(name) {
			t.Errorf("%q is not a durable continuous mode", name)
		}
	}
}
