package calibration

import (
	"fmt"

	"github.com/branchkit/plugin-sdk-go"
)

// RunFragilityAnalysis re-decodes the given targets' captured WAVs through the
// Sherpa probe and returns the ladder analysis (per-target rollup + per-clip
// readings). `targets` maps a target word/codeword to its captured repetition
// WAVs. It does NOT touch game state, so it runs without the game lock — the
// decode batch is a slow subprocess and must not block gameplay.
//
// Persistence is the caller's job: the per-clip readings are written to
// plugin.voice.clip_fragility (the measured truth), and the per-target verdict is
// DERIVED from those at read time — so there is no separate per-target write to
// keep in sync. Returns an error (and analyzes nothing) when the active engine
// isn't the Sherpa offline path — the re-decode mechanism is Sherpa-only.
func (s *State) RunFragilityAnalysis(targets map[string][]string) (LadderAnalysis, error) {
	if engine := s.engineName(); engine != SherpaOfflineEngine {
		return LadderAnalysis{}, fmt.Errorf("fragility analysis needs the Sherpa offline engine (active: %q)", engine)
	}
	if len(targets) == 0 {
		return LadderAnalysis{}, nil
	}

	// The re-decode runs in the platform (recognition.redecode): the plugin
	// is sandboxed away from the model + capture dirs. We keep the ladder shaping
	// + analysis here and forward only the batch.
	return NewProber().AnalyzeTargets(s.probeViaPlatform, targets)
}

// fetchModelVersion resolves just the model version (a content hash of the model)
// via an empty-item probe — the platform short-circuits empty items and returns
// the version without spawning the recognizer. Used to stamp a game that produced
// only mishears (no laddered clips, so the analysis carries no version). Returns
// "" when unwired or the probe fails.
func (s *State) fetchModelVersion() string {
	if s.Platform == nil {
		return ""
	}
	_, version, err := s.probeViaPlatform(nil, 0)
	if err != nil {
		branchkit.Logf("voice", "clip_fragility: model-version fetch failed: %v", err)
		return ""
	}
	return version
}

// probeViaPlatform forwards a batch of re-decode items to the platform's
// recognition.redecode RPC (the platform runs the recognizer's `probe`
// subcommand — it has the model + capture access the sandboxed plugin lacks) and
// returns the NDJSON result lines. The plugin keeps the ladder shaping +
// analysis; only the file-touching spawn lives in the platform.
func (s *State) probeViaPlatform(items []probeItem, maxActive int) ([]probeResultLine, string, error) {
	if s.Platform == nil {
		return nil, "", fmt.Errorf("plugin call not wired")
	}
	wire := make([]branchkit.RedecodeItem, len(items))
	for i, it := range items {
		wire[i] = redecodeItem(it)
	}
	// max_active is sent even when 0 (the stage reads 0 as its default beam),
	// as it was before this call was typed — never omitted.
	resp, err := s.Platform.RecognitionRedecode(branchkit.RecognitionRedecodeRequest{Items: wire, Model: s.commandModel(), Stage: ProbeStage, MaxActive: &maxActive})
	if err != nil {
		return nil, "", err
	}
	lines := make([]probeResultLine, len(resp.Lines))
	for i, l := range resp.Lines {
		lines[i] = probeResultLine{ID: l.ID, Text: l.Text, Error: l.Error}
	}
	version := ""
	if resp.ModelVersion != nil {
		version = *resp.ModelVersion
	}
	return lines, version, nil
}

// redecodeItem maps a ladder item onto the wire type. ApplyBias false stays
// omitted (the platform's default), exactly as probeItem's omitempty sent it.
func redecodeItem(it probeItem) branchkit.RedecodeItem {
	out := branchkit.RedecodeItem{
		ID:    it.ID,
		Audio: it.Audio,
		// Not it.Words: on this path the PLATFORM supplies the live grammar's
		// word set; probeItem.Words exists for the integration test, which
		// spawns the stage directly and must hand it one. Copying it here was
		// forwarding a field nothing on a live path ever sets.
		BiasStrength: it.BiasStrength,
	}
	if it.ApplyBias {
		t := true
		out.ApplyBias = &t
	}
	if it.Noise != nil {
		out.Noise = &branchkit.RedecodeNoise{SnrDb: it.Noise.SNRdB, Seed: int(it.Noise.Seed)}
	}
	return out
}

// Platform is the slice of the generated SDK surface calibration calls.
// *branchkit.Plugin satisfies it; tests supply a fake.
type Platform interface {
	RecognitionRedecode(req branchkit.RecognitionRedecodeRequest) (*branchkit.RecognitionRedecodeResponse, error)
	SettingsRefresh() error
}
