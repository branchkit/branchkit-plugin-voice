package session

import (
	"github.com/branchkit/branchkit-plugin-voice/hud"
)

// ApplyOrchestrateResults applies everything one orchestration produced
// EXCEPT its actions: discovery mutations, the session event log, and the
// main HUD push.
//
// The actions are deliberately not here. They go back to the platform on the
// `on_transcript` response and the platform executes them, which is what
// makes their provenance structural — they arrived as the answer to a call
// the platform made, so there is nothing to authorise and no credential to
// carry. This function used to loop over them calling `dispatch`, which made
// voice a second authenticated caller and made command routing
// indistinguishable from one plugin reaching into another.
//
// Tag writes are not here either, and never were: the platform applies
// sets_tag_writes / clears_tag_writes inside `commands.resolve`, before it
// returns. That is why ordering is inherited rather than arranged — by the
// time the next resolve is issued, the previous one's tags are already live.
func (s *State) ApplyOrchestrateResults(resp OrchestrateResponse) {
	if resp.Discovery != nil {
		s.applyDiscoveryMutationLocally(resp.Discovery)
	}
	if resp.Outcome != nil {
		s.hud.PushOutcome(*resp.Outcome)
	}

	for _, evt := range resp.Events {
		s.notifyEvent(evt.Type, evt.Data)
	}

	s.hud.PushMainHUD()
}

// applyDiscoveryMutationLocally applies a full DiscoveryMutation directly via
// the discovery state machine (no RPC to platform).
func (s *State) applyDiscoveryMutationLocally(dm *DiscoveryMutation) {
	if dm == nil {
		return
	}
	if dm.Open != nil && *dm.Open {
		title := "COMMANDS"
		if dm.Title != nil {
			title = *dm.Title
		}
		phrase := ""
		if dm.Phrase != nil {
			phrase = *dm.Phrase
		}
		footer := "Discovering possibilities..."
		if dm.Footer != nil {
			footer = *dm.Footer
		}
		countdownSecs := 0
		if dm.Countdown != nil && *dm.Countdown {
			countdownSecs = 8
			if dm.CountdownSeconds != nil {
				countdownSecs = int(*dm.CountdownSeconds)
			}
		}
		onExpire := hud.OnExpire{Close: true}
		if dm.OnExpire != nil {
			onExpire = *dm.OnExpire
		}
		s.hud.OpenDiscovery(title, dm.Sections, phrase, footer, countdownSecs, onExpire)
	} else if dm.Open != nil && !*dm.Open {
		s.hud.CloseDiscovery()
	}
}
