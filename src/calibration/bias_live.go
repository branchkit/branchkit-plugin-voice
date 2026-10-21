package calibration

import (
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Live Level-1 never-standalone bias introspection: after each calibration hold,
// re-decode the just-captured clip with the penalty OFF and diff it against the
// live (biased) result the pipeline already produced. Where they differ, the
// never-standalone bias flipped this command's decode (off: FOUR → on: PAGE), and
// the live trial card surfaces it. One extra decode per attempt, run off the game
// lock a beat after the hold so the clip has landed on disk. The bias word set is
// the platform's — the probe just declines it (apply_bias:false) here — so when
// the live penalty is off the two decodes match and nothing is shown.

// scheduleBiasProbe kicks off the live counterfactual for the hold that just
// ended. It snapshots the finishing trial's identity + live decode under the game
// lock, then re-decodes its clip unbiased off the lock and — if that trial is
// still the one on the card — stamps the BiasEffect and refreshes. Must hold s.mu.
// No-op unless capture is on, the engine is the Sherpa offline path, and the hold
// produced a real decode (a silent hold has nothing to compare against).
func (s *State) scheduleBiasProbe() {
	if s.captureDir == "" || s.Platform == nil {
		return
	}
	if s.engineName() != SherpaOfflineEngine {
		return
	}
	t := s.game.CurrentTrial
	if t == nil {
		return
	}
	liveHeard := strings.ToUpper(strings.TrimSpace(t.FinalHeard()))
	if liveHeard == "" {
		return // silent hold — no live decode to diff against
	}
	go s.runBiasProbe(filepath.Base(s.captureDir), t.Target, t.StartedAt, liveHeard)
}

// runBiasProbe (off the game lock) waits for the clip to land, finds the WAV the
// just-ended hold wrote, re-decodes it unbiased, and stamps the resulting
// counterfactual onto the trial — but only if it's still the live one (an
// auto-advance or retry since the hold drops the stale result).
func (s *State) runBiasProbe(gameRel, target string, trialStart time.Time, liveHeard string) {
	time.Sleep(clipSettleDelay)

	wav := s.latestClipSince(gameRel, trialStart)
	if wav == "" {
		return // no clip captured for this hold
	}
	off, _, err := NewProber().DecodeClipUnbiased(s.probeViaPlatform, wav)
	if err != nil {
		branchkit.Logf("voice", "calibration: live bias probe failed: %v", err)
		return
	}
	if off == "" {
		return
	}
	effect := &BiasEffect{
		Target:  target,
		File:    wav,
		Off:     off,
		On:      liveHeard,
		Changed: off != liveHeard,
	}

	s.mu.Lock()
	if t := s.game.CurrentTrial; t != nil && t.StartedAt.Equal(trialStart) {
		t.Bias = effect
		s.mu.Unlock()
		s.FlushRefresh()
		return
	}
	s.mu.Unlock()
}

// latestClipSince returns the capture-root-relative path of the newest clip the
// game's dump stage wrote at or after `since` (the just-ended hold's clip), or ""
// when none. Reads the per-game manifest through the platform, then picks the clip
// with pickLatestClip.
func (s *State) latestClipSince(gameRel string, since time.Time) string {
	return pickLatestClip(s.rpcReadManifest(gameRel), gameRel, since)
}

// pickLatestClip returns the capture-root-relative path of the newest manifest
// clip written at or after `since`, or "" when none. Mirrors the corpus labeling's
// window logic — a clip's ts is set at key-up, after the trial began — so the
// trial's StartedAt cleanly bounds out clips from earlier targets. Pure.
func pickLatestClip(lines []captureManifestLine, gameRel string, since time.Time) string {
	var best string
	var bestTs time.Time
	for _, ln := range lines {
		if ln.File == "" || ln.Ts == "" {
			continue
		}
		ts, err := parseManifestTs(ln.Ts)
		if err != nil || ts.Before(since) {
			continue
		}
		if best == "" || ts.After(bestTs) {
			best, bestTs = ln.File, ts
		}
	}
	if best == "" {
		return ""
	}
	return path.Join(gameRel, best)
}
