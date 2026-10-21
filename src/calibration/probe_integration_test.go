package calibration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProberAgainstFixtures exercises the real `sherpa_commands probe` binary over
// the spike fixtures. Skipped unless BK_PROBE_FIXTURES (the fixtures dir),
// BK_PROBE_BIN (the binary), and BK_PROBE_MODEL (the model dir) are set — it needs
// the built stage + model + WAVs, none present in CI. Decode-only: no machine
// side effects (no speak/play_sound/etc.).
//
// In production the re-decode runs in the platform (recognition.redecode);
// here the test supplies its own ProbeFunc that spawns the binary directly (the
// test process isn't sandboxed), so it still exercises the real ladder + analysis.
//
//	BK_PROBE_BIN=target/release/sherpa_commands \
//	BK_PROBE_MODEL="$HOME/Library/Application Support/BranchKitDev/models/voice/sherpa-offline-nemo" \
//	BK_PROBE_FIXTURES="$HOME/spike-ctc-hlg/real_fixtures" \
//	go test ./voice/src/calibration/ -run TestProberAgainstFixtures -v
func TestProberAgainstFixtures(t *testing.T) {
	fix := os.Getenv("BK_PROBE_FIXTURES")
	bin := os.Getenv("BK_PROBE_BIN")
	model := os.Getenv("BK_PROBE_MODEL")
	if fix == "" || bin == "" || model == "" {
		t.Skip("set BK_PROBE_FIXTURES, BK_PROBE_BIN, BK_PROBE_MODEL to run")
	}

	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	wavs := func(stem string) []string {
		return []string{
			filepath.Join(fix, stem+"_1.wav"),
			filepath.Join(fix, stem+"_2.wav"),
			filepath.Join(fix, stem+"_3.wav"),
		}
	}
	targets := map[string][]string{
		"COPY":       wavs("copy"),
		"BAT QUENCH": wavs("bat_quench"),
		"CAP RED":    wavs("cap_red"),
		"HARP PIT":   wavs("harp_pit"),
	}

	// Self-seed: the binary has no bundled HL.fst, so each item must carry a grammar
	// (in production the platform stamps the live committed union). The fragility
	// outcomes asserted below were calibrated against the FULL first-party command
	// vocabulary — e.g. BAT's nearest confuser BACK must be IN the grammar — so we
	// stamp that union here. Snapshot of the first-party command words (formerly the
	// HL.fst.words sidecar); update if the command vocabulary changes materially.
	grammar := []string{
		"ACCORDION", "AGAIN", "AIR", "ALL", "AND", "ATE", "BACK", "BAT", "BIGGER", "BOTTOM",
		"BRANCH", "BUILD", "CANCEL", "CAP", "CENTER", "CLEAR", "CLOSE", "COLLECTIONS", "COMMANDS", "COPY",
		"CUT", "DESK", "DESKTOP", "DONE", "DOWN", "DRUM", "EACH", "EAST", "EIGHT", "END",
		"ENTER", "ENTITIES", "EQUAL", "ESCAPE", "EVERYTHING", "EXIT", "FIND", "FINE", "FIVE", "FLOAT",
		"FOCUS", "FOR", "FORWARD", "FOUR", "FULL", "FULLSCREEN", "GO", "GUST", "HARP", "HELP",
		"HIDE", "HOME", "HUD", "INK", "INPUT", "JURY", "KIND", "LARGER", "LAYOUT", "LEFT",
		"LINE", "LISTENING", "LISTS", "LOOK", "MADE", "MAKE", "MAXIMIZE", "MISSION", "MONITOR", "MOVE",
		"MUTE", "NATIVE", "NEAR", "NEW", "NEXT", "NINE", "NORTH", "ODD", "ONE", "OPEN",
		"OUTPUT", "OVER", "OVERVIEW", "PAGE", "PASTE", "PIT", "PLEX", "PRESS", "PREVIOUS", "QUENCH",
		"RED", "REDO", "REPEAT", "RESIZE", "RETURN", "RIGHT", "ROTATE", "RUN", "SAVE", "SCROLL",
		"SELECT", "SEND", "SET", "SEVEN", "SHOW", "SIDEBAR", "SIX", "SLEEP", "SMALLER", "SNAP",
		"SOUTH", "SPACE", "SPEAK", "SPEECH", "SPEED", "SPELL", "SPLIT", "START", "STOP", "SUN",
		"SWAP", "SWITCH", "TAB", "TALL", "TEN", "THIS", "THREE", "THROW", "TILE", "TO",
		"TOGGLE", "TOO", "TOP", "TRAP", "TWO", "UNDO", "UNFLOAT", "UNMUTE", "UNTIE", "UNTILE",
		"UP", "URGE", "VEST", "VOLUME", "WAKE", "WALL", "WEST", "WHALE", "WIDE", "WINDOW",
		"WINDOWS", "WON", "WORD", "WORKSPACE", "YANK", "ZERO", "ZIP",
	}

	// Direct-spawn ProbeFunc — mirrors what the platform now does, but in-process
	// (unsandboxed) for the test. Audio paths here are absolute fixture paths.
	call := func(items []probeItem, maxActive int) ([]probeResultLine, string, error) {
		for i := range items {
			items[i].Words = grammar
		}
		req := map[string]any{"model": model, "max_active": maxActive, "items": items}
		payload, err := json.Marshal(req)
		if err != nil {
			return nil, "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, abs(bin), "probe")
		cmd.Stdin = strings.NewReader(string(payload))
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, "", fmt.Errorf("probe run failed: %v (stderr: %s)", err, strings.TrimSpace(stderr.String()))
		}
		var lines []probeResultLine
		for _, raw := range strings.Split(stdout.String(), "\n") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			var ln probeResultLine
			if json.Unmarshal([]byte(raw), &ln) == nil {
				lines = append(lines, ln)
			}
		}
		return lines, "", nil
	}

	p := &Prober{
		SNRLadder:    DefaultSNRLadder(),
		SeedsPerStep: 4,
		MaxActive:    3000,
	}

	analysis, err := p.AnalyzeTargets(call, targets)
	if err != nil {
		t.Fatalf("AnalyzeTargets: %v", err)
	}
	results := analysis.PerTarget
	byTarget := map[string]WordFragility{}
	for _, r := range results {
		byTarget[r.Target] = r
		t.Logf("%-12s held=%v mushy=%v fallSNR=%.1f reps=%d constituents=%+v",
			r.Target, r.Held, r.Mushy, r.FallSNRdB, r.Reps, r.Constituents)
	}

	if bq, ok := byTarget["BAT QUENCH"]; ok {
		if len(bq.Constituents) == 0 || bq.Constituents[0].NearestConfuser != "BACK" {
			t.Errorf("BAT QUENCH constituent[0] confuser = %+v, want BACK", bq.Constituents)
		}
	} else {
		t.Error("missing BAT QUENCH result")
	}
	if hp, ok := byTarget["HARP PIT"]; ok {
		if !hp.Mushy {
			t.Errorf("HARP PIT should be mushy; got %+v", hp)
		}
	} else {
		t.Error("missing HARP PIT result")
	}
	if cr, ok := byTarget["CAP RED"]; ok {
		if !cr.Held {
			t.Errorf("CAP RED should hold the ladder; got %+v", cr)
		}
	}
	// Robust CAP RED must rank below mushy/fragile codewords.
	if results[len(results)-1].Target != "CAP RED" {
		t.Errorf("CAP RED (robust) should rank last; order = %v", targetOrder(results))
	}
}

func targetOrder(r []WordFragility) []string {
	out := make([]string, len(r))
	for i, w := range r {
		out[i] = w.Target
	}
	return out
}
