package calibration

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// VerificationAttempt is the atomic record of one recognition attempt.
// Each record is self-contained: it captures the full grammar context at the
// time of the attempt so it remains interpretable regardless of future changes.
//
// Retune is offered only from the just-finished (in-memory) report, never from
// historical attempts: a binding frozen onto a past attempt can point at an
// entry that has since been renamed or removed. If history ever offers retune,
// resolve against the CURRENT vocabulary and handle already-renamed/removed.
type VerificationAttempt struct {
	// Identity
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Timestamp string `json:"ts"`

	// What was being tested
	Target string `json:"target"`
	Source string `json:"source"` // "calibration", "live"
	Group  string `json:"group"`  // command group (e.g. "snap", "alphabet")
	// CommandID is the stable "<owner>:<pattern>" identity of the target, so
	// per-command health aggregates across walks regardless of which selection
	// produced them (and disambiguates same-pattern-different-owner commands).
	// Empty for bare codewords (alphabet) and legacy records, which key by Target.
	CommandID string `json:"command_id,omitempty"`

	// Recognition context — point-in-time snapshot.
	//
	// The grammar snapshot is NOT here. It is per-SITTING context, identical
	// for every attempt in one, and storing it per attempt made it 88% of this
	// log (1,038,999 of 1,181,474 bytes on 2026-08-17, a quarter of records
	// carrying the full ~600-word union). It lives on the session record, which
	// `SessionID` points at.
	RecognitionPath  string `json:"path"`    // "sequential" (calibration) or "burst" (live)
	SequencePosition int    `json:"seq_pos"` // 0 = first/only word, 1+ = after narrowing

	// What happened
	Heard      string `json:"heard"`
	Matched    bool   `json:"matched"`
	DurationMs int    `json:"duration_ms"` // wall-clock time for this attempt (includes user think time)
	// MatcherRejected flags a dynamic-mode trial where the engine
	// transcribed the codeword correctly but the matcher rejected it
	// under the fixture's gate — the fixture didn't apply, the tag state
	// was wrong, the matched winner was ungated/gated_unscoped, or no
	// command pattern accepted the words. Requires a confirmed matcher
	// decision (HasMatcherDecision=true on the view); RPC-failed trials
	// fall through to plain ✗ to avoid mislabelling infra trouble as
	// filter rejection.
	//
	// Pairs with Matched=false: MatcherRejected=true → filter/scope
	// failure (heard correctly, matcher refused or scoped wrong);
	// MatcherRejected=false (with Matched=false) → acoustic miss or
	// RPC failure. Matched=true → MatcherRejected always false.
	MatcherRejected bool `json:"matcher_rejected,omitempty"`

	// MatcherDecision carries the structured matcher response captured
	// at trial-time for dynamic-mode trials. Lets Reports surface *why*
	// the filter rejected — which tiebreaker fired (winner), which gates
	// were active at decision time, and whether a partial completion
	// path was available. nil for static-mode trials and for dynamic
	// trials where the resolve RPC failed.
	MatcherDecision *MatcherDecision `json:"matcher_decision,omitempty"`

	// Verdict is the three-way command-practice score: "pass" (heard the
	// phrase AND it resolves to the intended command), "warn" (exactly one
	// of those held — said clearly but won't fire right, or misheard but
	// the matcher was robust enough to still resolve it), or "fail"
	// (neither). Empty for non-practice (codeword) trials, which score on
	// heard==target alone via Matched.
	Verdict string `json:"verdict,omitempty"`

	// Attempt context
	AttemptIndex int `json:"attempt_idx"`

	// Recognition detail
	FirstHeard string `json:"first_heard"`
	Revisions  int    `json:"revisions"`
	// HeardTallies is the mishear trail with occurrence counts — each distinct
	// thing the engine heard for this word, in first-seen order, with how many
	// times it came back (last entry = what it settled on). A word repeatedly
	// misheard as the same wrong thing shows that multiplicity (e.g. NEXT ×6)
	// rather than collapsing to a single trail step. Replaces the old deduped
	// `heards []string`; older records carry that legacy key, now ignored.
	HeardTallies []HeardTally `json:"heard_tallies,omitempty"`
	// SilentHolds counts Option+G holds during this trial that produced no
	// recognition at all (empty decode) — the "it looked like it wasn't
	// receiving input" case. Surfaced so a fragile word's true cost (mishears
	// + silent tries before it finally settled) isn't hidden.
	SilentHolds int `json:"silent_holds,omitempty"`

	// Environment
	Engine  string `json:"engine"`
	ModelID string `json:"model_id,omitempty"`

	// User correction (live source)
	UserCorrected  bool   `json:"user_corrected,omitempty"`
	IntendedTarget string `json:"intended_target,omitempty"`
	CorrectionNote string `json:"correction_note,omitempty"`

	// Future: audio corpus
	AudioRef string `json:"audio_ref,omitempty"`
}

// HeardTally is one entry in the occurrence-counted mishear trail: a distinct
// recognized value and how many times it came back across a trial's holds.
type HeardTally struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// MatcherDecision is the per-trial structured response captured from
// `commands.resolve` at Advance time. Stored alongside the attempt so
// Reports can render *why* a dynamic-mode trial failed without forcing
// the user to grep audit logs. Mirrors the wire shape of
// `ResolveTelemetry` + the resolve response's partial-match fields.
type MatcherDecision struct {
	// Winner is one of: "gated_scoped", "gated_unscoped", "ungated",
	// "no_match". When a trial's heard==target but Winner=="no_match",
	// the filter rejection is unambiguous — no command pattern in the
	// active gates accepted the words.
	Winner string `json:"winner"`
	// ActiveGates is the snapshot of active scoped tags at decision
	// time (a.k.a. `plugin.<X>.active` style). Lets the user spot a
	// fixture that didn't apply (expected gate missing) or a stale
	// mode tag leaking from a prior session.
	ActiveGates []string `json:"active_gates,omitempty"`
	// HasCompletions is true when the matcher recognized the heard
	// words as the start of a longer command — i.e., partial-match
	// path. Pairs with NextWords for the discovery view.
	HasCompletions bool `json:"has_completions,omitempty"`
	// NextWords is the matcher's partial-match continuation set: the
	// words that would extend the current partial into a complete
	// command. Empty unless HasCompletions.
	NextWords []string `json:"next_words,omitempty"`

	// The next three fields are populated by command-practice trials (the
	// preview-resolve path) and used to score whether the heard phrase
	// resolved to the *intended* command. Static/dynamic codeword trials
	// leave them zero-valued.
	//
	// Matched is the resolve envelope's top-level `matched` — true when the
	// heard words resolved to some command (no dispatch in preview mode).
	Matched bool `json:"matched,omitempty"`
	// Owner is the resolved command's owning plugin (`owner_plugin`).
	Owner string `json:"owner_plugin,omitempty"`
	// WinningPattern is the resolved command's pattern (from
	// `telemetry.winning_pattern`) — compared against the practiced
	// command's pattern to confirm identity.
	WinningPattern string `json:"winning_pattern,omitempty"`
}

// VerificationSession summarizes a completed calibration session.
type VerificationSession struct {
	ID        string `json:"id"`
	Timestamp string `json:"ts"`
	Source    string `json:"source"` // "calibration"
	Group     string `json:"group"`

	// Summary stats
	WordCount    int     `json:"word_count"`
	MatchCount   int     `json:"match_count"`
	ScorePct     float32 `json:"score_pct"`
	ElapsedSecs  float32 `json:"elapsed_secs"`
	NoInputCount int     `json:"no_input_count"` // words that timed out (not in attempts)

	// Context snapshot
	Engine          string   `json:"engine"`
	ModelID         string   `json:"model_id,omitempty"`
	GrammarSnapshot []string `json:"grammar_snapshot"`

	// Mode + context — Mode is "static" (codeword-style targets running
	// against the full engine grammar with heard==target scoring) or
	// "dynamic" (command-target fixture trial with matcher-aware
	// scoring). Context is the dotted command name surfaced in the
	// Reports breakdown, empty for static trials whose target isn't a
	// specific named command. Pre-2026-05-27 records may have
	// Mode="sandbox" — those represent calibration's now-removed
	// engine-narrowing path and should be treated as legacy historical
	// data (not currently produced).
	Mode    string `json:"mode,omitempty"`
	Context string `json:"context,omitempty"`
}

// NewID generates a random 16-byte hex ID (32 chars).
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
