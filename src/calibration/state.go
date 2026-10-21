package calibration

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// AppliedContext is the flat result of entering a command's context for a
// trial (`trial.enter_context`). Its fields map 1:1 onto the platform's
// ApplyContextResult: Kind is "static" (base / mode-gated / slotted) or
// "dynamic" (hints); Tags is the mode-gated requires_tags the platform wrote
// (empty otherwise); FixtureHandle is a dynamic command's owner-returned
// handle (empty otherwise). The platform already registered the tags/handle on
// the trial, so the host needs this only to label the live trial card.
// Only Kind is consumed — enterContextForTargetLocked sets game.Mode from it.
// Tags and FixtureHandle rode along under a comment saying the host needed them
// "only to label the live trial card"; the card never showed either, and the
// platform has already registered both on the trial anyway.
type AppliedContext struct {
	Kind string
}

// refreshDebounce is the minimum interval between coalesced settings.refresh
// calls during an active calibration game.
const refreshDebounce = 50 * time.Millisecond

// clipSettleDelay is how long after a hold ends we wait before refreshing the
// Recordings list. The dump stage writes the clip's WAV around key-up, slightly
// out of band with the pipeline-stopped signal, so we let it land on disk before
// re-scanning the capture dir.
const clipSettleDelay = 500 * time.Millisecond

// livenessTimeout is how long the calibration page can go without a heartbeat
// (an interval ping or a render touch) before the walk auto-suspends. Set above
// the page's ping cadence so a dropped ping doesn't trip it; the settings UI is
// browser-based and in-page navigation isn't surfaced to the plugin, so this
// absence-of-signal backstop is what catches "user left the page mid-walk".
const livenessTimeout = 6 * time.Second

// Frame captures a single recognition result during a trial.
type Frame struct {
	Text     string `json:"text"`
	IsFinal  bool   `json:"is_final"`
	OffsetMs int64  `json:"offset_ms"`
	// Hold numbers the trial's holds from 0: every frame of one hold carries
	// the same value. The command engine splits a hold into VAD segments, so
	// a multi-word target can arrive as several frames of one hold, and
	// FinalHeard joins them.
	Hold int `json:"hold,omitempty"`
}

// Trial isolates all recognition data for a single calibration target.
type Trial struct {
	Target    string    `json:"target"`  // the prompt the user was asked to say
	Outcome   string    `json:"outcome"` // "" (in progress), "matched", "failed", "skipped"
	Frames    []Frame   `json:"frames"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	// Heard is the acoustic signal, stamped at score time: did the engine
	// transcribe the prompt (heard ≈ prompt)? Always computable.
	Heard bool `json:"heard,omitempty"`
	// Resolves is the functional signal, stamped at score time: do the heard
	// words resolve to the intended command in its own context? nil when not
	// applicable — a bare codeword (not a command), a slotted filler (scored
	// acoustic-only), or a resolve RPC that returned no decision.
	Resolves *bool `json:"resolves,omitempty"`
	// MatcherDecision is the structured preview-resolve response captured
	// alongside Resolves. Diagnostic surface (winner + active gates +
	// completions). nil for codewords/slotted and when the resolve RPC failed.
	MatcherDecision *MatcherDecision `json:"matcher_decision,omitempty"`
	// Verdict is the three-way label ("pass"|"warn"|"fail") derived from the
	// two signals at score time. Empty until scored.
	Verdict string `json:"verdict,omitempty"`
	// SilentHolds counts holds during this trial that the engine decoded to
	// nothing (empty-decode boundary markers with no preceding content) — the
	// "wasn't receiving input" case.
	SilentHolds int `json:"silent_holds,omitempty"`
	// Bias is the live Level-1 never-standalone counterfactual for this attempt:
	// what the just-captured clip decodes to with the penalty OFF, set against the
	// live biased result the pipeline already produced. Filled asynchronously a
	// beat after the hold ends (one extra unbiased re-decode); nil until then, and
	// stays nil when capture is off, the engine isn't Sherpa, or the hold produced
	// no clip. Live-only — the trial card reads it; never persisted.
	Bias *BiasEffect `json:"-"`
}

// FinalHeard returns what the trial's last hold that heard anything heard.
// The command engine emits a hold as non-final VAD segments, so a split
// multi-word target ("snap" | "left") arrives as several frames of one hold:
// the hold's trailing segments are joined, in order, until they hold as many
// words as the target, and scored as one phrase. Only the trailing ones: a
// hold that repeats the target ("bake" | "bake") or corrects itself
// ("bike" | "bake") is scored on its last try, as before. A hold that
// carries a final frame (a streaming engine's revising partials) is read
// from its last final frame instead. Frames of different holds never join:
// each hold is its own attempt.
func (t *Trial) FinalHeard() string {
	last := -1
	for i := len(t.Frames) - 1; i >= 0; i-- {
		if t.Frames[i].Text != "" {
			last = i
			break
		}
	}
	if last < 0 {
		return ""
	}
	hold := t.Frames[last].Hold
	var segments []string
	final := ""
	for _, f := range t.Frames[:last+1] {
		if f.Hold != hold || f.Text == "" {
			continue
		}
		if f.IsFinal {
			final = f.Text
		}
		segments = append(segments, f.Text)
	}
	if final != "" {
		return final
	}
	want := len(strings.Fields(t.Target))
	from, words := len(segments), 0
	for from > 0 && (words == 0 || words < want) {
		from--
		words += len(strings.Fields(segments[from]))
	}
	return strings.Join(segments[from:], " ")
}

// Matched is the live acoustic check: the heard transcript matches the prompt
// (case-insensitive). Used for the live match indicator before scoring stamps
// the signals.
func (t *Trial) Matched() bool {
	heard := t.FinalHeard()
	return heard != "" && strings.EqualFold(heard, t.Target)
}

// Passed reports whether the trial counts as a success for outcome +
// persistence. A scored complete-command trial (Resolves set) needs both
// signals; a scored acoustic-only trial needs Heard; an unscored trial falls
// back to the live acoustic check.
func (t *Trial) Passed() bool {
	if t.Resolves != nil {
		return t.Heard && *t.Resolves
	}
	if t.Verdict != "" {
		return t.Heard
	}
	return t.Matched()
}

// Duration returns how long the trial lasted.
func (t *Trial) Duration() time.Duration {
	if t.EndedAt.IsZero() {
		if t.StartedAt.IsZero() {
			return 0
		}
		return time.Since(t.StartedAt)
	}
	return t.EndedAt.Sub(t.StartedAt)
}

// FirstHeard returns the text from the first non-empty frame.
func (t *Trial) FirstHeard() string {
	for _, f := range t.Frames {
		if f.Text != "" {
			return f.Text
		}
	}
	return ""
}

// Revisions returns the count of distinct heard texts minus one (the initial).
func (t *Trial) Revisions() int {
	if n := len(t.HeardTallies()); n > 0 {
		return n - 1
	}
	return 0
}

// HeardTallies returns the occurrence-counted mishear trail: each distinct
// non-empty thing the engine heard across this trial's holds, in first-seen
// order, with how many times it came back. The last element is what the trial
// settled on. Repeats are counted, not collapsed — six holds that each heard
// "next" yield {Text:"NEXT", Count:6}, not a single trail step. Derived purely
// from Frames (each non-empty recognition appends one).
func (t *Trial) HeardTallies() []HeardTally {
	idx := make(map[string]int)
	var out []HeardTally
	for _, f := range t.Frames {
		if f.Text == "" {
			continue
		}
		if i, ok := idx[f.Text]; ok {
			out[i].Count++
			continue
		}
		idx[f.Text] = len(out)
		out = append(out, HeardTally{Text: f.Text, Count: 1})
	}
	return out
}

// Phase is the calibration state machine's single source of truth.
//
//	Idle ──(Start)──► Playing
//	Playing ──(Finish)──► Finished
//	Playing ──(Cancel)──► Idle
//	Playing ──(Pause)──► Paused
//	Paused ──(Resume/Restart)──► Playing
//	Paused ──(Quit)──► Idle
//	Finished ──(NewGame/Run Again)──► Idle/Playing
//	Reviewing (viewing a past session from Reports)
type Phase int

const (
	PhaseIdle Phase = iota
	PhasePlaying
	PhasePaused
	PhaseFinished
	PhaseReviewing
)

// String returns a stable, lowercase name for logging + JSON.
func (p Phase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhasePlaying:
		return "playing"
	case PhasePaused:
		return "paused"
	case PhaseFinished:
		return "finished"
	case PhaseReviewing:
		return "reviewing"
	default:
		return "unknown"
	}
}

// Game manages the calibration walk state machine. `Phase` is the
// authoritative state; `IsActive` / `IsRunning` are derived from it. A walk is
// a queue of Targets (one command, a category, all, the alphabet codewords, or
// a drill set — "scope, not mode"); `Words` is the parallel prompt list the
// trial loop indexes.
type Game struct {
	Phase        Phase
	Words        []string // per-trial prompts, parallel to Targets
	WordLetters  []string // letter for each word (alphabet); empty for commands
	CurrentIndex int
	Results      []WordResult
	CurrentTrial *Trial
	HeardBuffer  string
	StartTime    *time.Time
	ElapsedSecs  float32
	Source       string // provenance: the selection that produced this walk ("alphabet"/"all"/"drill"/"category:<owner>"/a command id/"custom")
	SourcePrefix []string
	GrammarWords []string // words snapshotted onto each persisted attempt/session
	// Targets is the walk queue, parallel to Words (Words[i] == Targets[i].Prompt).
	Targets []Target
	// holdHadContent tracks whether the current Option+G hold has produced any
	// non-empty recognition yet. Reset at each hold boundary (and trial start);
	// a boundary that arrives with this still false is a silent hold. Transient.
	holdHadContent bool
	// hold numbers the current trial's holds (Frame.Hold): advanced at each
	// hold boundary, reset at trial start. Transient.
	hold int
	// Mode + Context label the CURRENT target's entered context for the live
	// trial card and the Reports breakdown: Mode is the apply result's kind
	// ("static" | "dynamic"), Context the command id (empty for bare
	// codewords). Per-target — updated as each trial enters its context.
	Mode    string
	Context string
}

// IsActive reports whether a walk is in progress (running or paused).
func (g *Game) IsActive() bool {
	return g.Phase == PhasePlaying || g.Phase == PhasePaused
}

// IsRunning is the narrower predicate: actively consuming speech frames
// (excludes Paused). Gameplay ops (ProcessFrame, Advance, Skip, Back) require it.
func (g *Game) IsRunning() bool {
	return g.Phase == PhasePlaying
}

// HasResolvableTargets reports whether the walk contains at least one
// complete-command target (one that yields a Resolves signal). Drives the
// renderer's verdict-column affordance.
func (g *Game) HasResolvableTargets() bool {
	for _, t := range g.Targets {
		if t.resolvable() {
			return true
		}
	}
	return false
}

// WordResult records the result for one calibration target.
type WordResult struct {
	Word      string  `json:"word"`
	Letter    string  `json:"letter"`     // the letter this codeword represents (alphabet)
	CommandID string  `json:"command_id"` // the target's "<owner>:<pattern>" id (empty for codewords)
	Passed    bool    `json:"passed"`
	Trial     Trial   `json:"trial"`
	TimeSecs  float32 `json:"time_secs"`
	// Binding is the vocabulary entry this row tested, copied from the target so
	// the finished report can retune it (rename/revert) without reconstructing
	// {collection, key}. nil for literal-pattern rows, which self-gate to no
	// retune. Live-only — never persisted onto the immutable VerificationAttempt.
	Binding *VocabBinding `json:"-"`
}

// currentCommandID returns the current target's command id, or "" when out of
// range or for a bare codeword.
func (g *Game) currentCommandID() string {
	if g.CurrentIndex >= 0 && g.CurrentIndex < len(g.Targets) {
		return g.Targets[g.CurrentIndex].CommandID
	}
	return ""
}

// lastCommandID returns the command id of the final queued target — the command
// the walk just exhausted when the cursor runs off the end. Drives command-level
// chaining ("what comes after the command I just finished").
func (g *Game) lastCommandID() string {
	if n := len(g.Targets); n > 0 {
		return g.Targets[n-1].CommandID
	}
	return ""
}

// AppendTargets extends the live walk queue (lazy command-level chaining): the
// next catalog command's expanded targets are tacked onto the current walk
// instead of starting a fresh game, so the sitting's Results accrue as one
// continuous list. When the cursor had already run off the end (the walk was
// about to finish), a fresh trial starts for the now-current target so the walk
// continues seamlessly. WordLetters is left as-is (chained commands carry no
// alphabet letters; the trial loop bounds-checks it).
//
// Chaining is lazy on purpose: never pre-expand the whole filtered catalog's
// targets up front (the hint cross product alone is 676 per command). The
// pre-expanded path is where the old 15-command silent-cap bug lived; guard
// any future unified queue against silent truncation.
func (g *Game) AppendTargets(targets []Target) {
	if len(targets) == 0 {
		return
	}
	atEnd := g.CurrentIndex >= len(g.Words)
	g.Targets = append(g.Targets, targets...)
	for _, t := range targets {
		g.Words = append(g.Words, t.Prompt)
	}
	if atEnd {
		g.startNewTrial()
	}
}

// currentBinding returns a fresh copy of the current target's vocabulary binding
// (nil for a literal-pattern target). Copied, not aliased, so a later retune that
// reassigns the target's binding pointer can't mutate a recorded result.
func (g *Game) currentBinding() *VocabBinding {
	if g.CurrentIndex >= 0 && g.CurrentIndex < len(g.Targets) {
		if b := g.Targets[g.CurrentIndex].Binding; b != nil {
			cp := *b
			return &cp
		}
	}
	return nil
}

func NewGame() Game {
	return Game{}
}

// currentTarget returns the current trial's target, or (zero, false) when out
// of range.
func (g *Game) currentTarget() (Target, bool) {
	if g.CurrentIndex < 0 || g.CurrentIndex >= len(g.Targets) {
		return Target{}, false
	}
	return g.Targets[g.CurrentIndex], true
}

// Start begins a fresh walk over the given targets. Per-game provenance
// (WordLetters / Source / SourcePrefix / GrammarWords / Mode / Context) is
// reset here and repopulated by the caller (StartWalk / Restart) + per-target
// context entry.
func (g *Game) Start(targets []Target) {
	g.Targets = targets
	g.Words = Prompts(targets)
	g.Phase = PhasePlaying
	g.CurrentIndex = 0
	g.Results = nil
	g.WordLetters = nil
	g.Source = ""
	g.SourcePrefix = nil
	g.GrammarWords = nil
	g.Mode = ""
	g.Context = ""
	g.HeardBuffer = ""
	now := time.Now()
	g.StartTime = &now
	g.ElapsedSecs = 0
	g.startNewTrial()
}

func (g *Game) Cancel() {
	g.Phase = PhaseIdle
	g.Words = nil
	g.Targets = nil
	g.Results = nil
	g.CurrentTrial = nil
	g.HeardBuffer = ""
}

func (g *Game) MatchedCount() int {
	count := 0
	for _, r := range g.Results {
		if r.Passed {
			count++
		}
	}
	return count
}

func (g *Game) AttemptedCount() int {
	count := 0
	for _, r := range g.Results {
		if len(r.Trial.Frames) > 0 {
			count++
		}
	}
	return count
}

// startNewTrial begins a fresh trial for the current target.
func (g *Game) startNewTrial() {
	if g.CurrentIndex >= len(g.Words) {
		g.CurrentTrial = nil
		return
	}
	g.CurrentTrial = &Trial{
		Target:    g.Words[g.CurrentIndex],
		StartedAt: time.Now(),
	}
	g.HeardBuffer = ""
	g.holdHadContent = false
	g.hold = 0
}

// MarkHoldBoundary records the end of an Option+G hold. A hold with no
// non-empty recognition counts as a silent hold on the current trial.
func (g *Game) MarkHoldBoundary() {
	if g.CurrentTrial != nil && !g.holdHadContent {
		g.CurrentTrial.SilentHolds++
	}
	g.holdHadContent = false
	g.hold++
}

// ProcessFrame appends a recognition frame to the current trial. No auto-advance.
func (g *Game) ProcessFrame(transcript string, isFinal bool) {

	if g.CurrentIndex >= len(g.Words) || g.CurrentTrial == nil {
		return
	}

	if transcript != "" {
		g.holdHadContent = true
	}

	offsetMs := int64(0)
	if !g.CurrentTrial.StartedAt.IsZero() {
		offsetMs = time.Since(g.CurrentTrial.StartedAt).Milliseconds()
	}

	g.CurrentTrial.Frames = append(g.CurrentTrial.Frames, Frame{
		Text:     transcript,
		IsFinal:  isFinal,
		OffsetMs: offsetMs,
		Hold:     g.hold,
	})
	// The live "Heard" line shows what scoring will read: the hold so far,
	// its segments joined.
	g.HeardBuffer = g.CurrentTrial.FinalHeard()
}

// MatchStatus returns whether the current trial's last heard text matches the prompt.
func (g *Game) MatchStatus() bool {
	if g.CurrentTrial == nil {
		return false
	}
	return g.CurrentTrial.Matched()
}

// Advance finalizes the current trial and moves to the next target.
// Returns true if the walk is now finished. Scoring (the two signals) must be
// stamped by the caller before Advance reads the outcome.
func (g *Game) Advance() bool {
	if !g.IsRunning() || g.CurrentIndex >= len(g.Words) || g.CurrentTrial == nil {
		return false
	}

	g.CurrentTrial.EndedAt = time.Now()
	if g.CurrentTrial.Passed() {
		g.CurrentTrial.Outcome = "matched"
	} else {
		g.CurrentTrial.Outcome = "failed"
	}

	passed := g.CurrentTrial.Outcome == "matched"

	letter := ""
	if g.CurrentIndex < len(g.WordLetters) {
		letter = g.WordLetters[g.CurrentIndex]
	}

	g.Results = append(g.Results, WordResult{
		Word:      g.Words[g.CurrentIndex],
		Letter:    letter,
		CommandID: g.currentCommandID(),
		Passed:    passed,
		Trial:     *g.CurrentTrial,
		TimeSecs:  float32(g.CurrentTrial.Duration().Seconds()),
		Binding:   g.currentBinding(),
	})

	g.CurrentIndex++
	if g.CurrentIndex >= len(g.Words) {
		return true
	}
	g.startNewTrial()
	return false
}

// Retry clears the current trial's frames for a fresh attempt on the same target.
func (g *Game) Retry() {
	if !g.IsRunning() || g.CurrentTrial == nil {
		return
	}
	g.startNewTrial()
}

// RetargetCurrent repoints the current target after an in-walk retune: it
// replaces the old surface form (oldKey) with newKey in the prompt and starts a
// fresh trial so the next hold tests it. For a bare codeword the prompt IS the
// key, so it moves wholesale; for a list slot only the embedded word changes
// ("scroll down three" → "scroll down trio"). The binding's key advances too.
// No-op when not running or out of range.
func (g *Game) RetargetCurrent(oldKey, newKey string) {
	if !g.IsRunning() || g.CurrentIndex < 0 || g.CurrentIndex >= len(g.Words) {
		return
	}
	tgt := &g.Targets[g.CurrentIndex]
	newPrompt := strings.Replace(tgt.Prompt, oldKey, newKey, 1)
	g.Words[g.CurrentIndex] = newPrompt
	if tgt.Pattern == tgt.Prompt {
		tgt.Pattern = newPrompt
	}
	tgt.Prompt = newPrompt
	if tgt.Binding != nil {
		tgt.Binding = &VocabBinding{Collection: tgt.Binding.Collection, Key: newKey}
	}
	g.startNewTrial()
}

// Skip marks the current target as skipped and advances.
func (g *Game) Skip() bool {
	if !g.IsRunning() || g.CurrentIndex >= len(g.Words) || g.CurrentTrial == nil {
		return false
	}

	g.CurrentTrial.EndedAt = time.Now()
	g.CurrentTrial.Outcome = "skipped"

	letter := ""
	if g.CurrentIndex < len(g.WordLetters) {
		letter = g.WordLetters[g.CurrentIndex]
	}

	g.Results = append(g.Results, WordResult{
		Word:      g.Words[g.CurrentIndex],
		Letter:    letter,
		CommandID: g.currentCommandID(),
		Passed:    false,
		Trial:     *g.CurrentTrial,
		TimeSecs:  float32(g.CurrentTrial.Duration().Seconds()),
		Binding:   g.currentBinding(),
	})

	g.CurrentIndex++
	if g.CurrentIndex >= len(g.Words) {
		return true
	}
	g.startNewTrial()
	return false
}

// currentGroup returns the current target's group label, or "" when out of
// range or for a standalone (ungrouped) target.
func (g *Game) currentGroup() string {
	if g.CurrentIndex >= 0 && g.CurrentIndex < len(g.Targets) {
		return g.Targets[g.CurrentIndex].Group
	}
	return ""
}

// SkipGroup marks every remaining target in the current target's group as
// skipped and advances past them, so a big command expansion (the apps list,
// the hint codeword cross product) can be dismissed as a unit instead of
// skipped one entry at a time. A standalone (ungrouped) target falls back to a
// single Skip. Returns true if the walk is now finished.
func (g *Game) SkipGroup() bool {
	if !g.IsRunning() || g.CurrentIndex >= len(g.Words) {
		return false
	}
	group := g.currentGroup()
	if group == "" {
		return g.Skip()
	}
	for g.IsRunning() && g.CurrentIndex < len(g.Words) && g.currentGroup() == group {
		if g.Skip() {
			return true
		}
	}
	return g.CurrentIndex >= len(g.Words)
}

// Back goes back to the previous target, removing its result so it can be retried.
func (g *Game) Back() {
	if !g.IsRunning() || g.CurrentIndex <= 0 {
		return
	}
	g.CurrentIndex--
	if len(g.Results) > g.CurrentIndex {
		g.Results = g.Results[:g.CurrentIndex]
	}
	g.startNewTrial()
}

// currentMatchesFilter reports whether the current target's prompt contains the
// (case-insensitive) filter term — the same match the rail's client-side filter
// uses. An empty filter or an out-of-range cursor matches (nothing to skip).
func (g *Game) currentMatchesFilter(filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" || g.CurrentIndex < 0 || g.CurrentIndex >= len(g.Words) {
		return true
	}
	return strings.Contains(strings.ToLower(g.Words[g.CurrentIndex]), filter)
}

// SkipForwardToFilter advances past targets whose prompt doesn't match filter
// (marking them skipped) until the current target matches or the walk ends, so
// Next/Skip step through only the filtered (visible) rail. No-op for an empty
// filter or when the current target already matches. Returns true if finished.
func (g *Game) SkipForwardToFilter(filter string) bool {
	if strings.TrimSpace(filter) == "" {
		return false
	}
	for g.IsRunning() && !g.currentMatchesFilter(filter) {
		if g.Skip() {
			return true
		}
	}
	return false
}

// BackToFilter steps back past non-matching targets until the current target
// matches filter or the start is reached, so Back lands on the previous visible
// rail entry. No-op for an empty filter or when the current target matches.
func (g *Game) BackToFilter(filter string) {
	if strings.TrimSpace(filter) == "" {
		return
	}
	for g.CurrentIndex > 0 && !g.currentMatchesFilter(filter) {
		g.Back()
	}
}

func (g *Game) Finish() {
	g.Phase = PhaseFinished
	if g.StartTime != nil {
		g.ElapsedSecs = float32(time.Since(*g.StartTime).Seconds())
	}
	// Finalize current trial if in progress.
	if g.CurrentTrial != nil && g.CurrentIndex < len(g.Words) {
		g.CurrentTrial.EndedAt = time.Now()
		if g.CurrentTrial.Outcome == "" {
			if g.CurrentTrial.Passed() {
				g.CurrentTrial.Outcome = "matched"
			} else {
				g.CurrentTrial.Outcome = "failed"
			}
		}
		letter := ""
		if g.CurrentIndex < len(g.WordLetters) {
			letter = g.WordLetters[g.CurrentIndex]
		}
		g.Results = append(g.Results, WordResult{
			Word:      g.Words[g.CurrentIndex],
			Letter:    letter,
			CommandID: g.currentCommandID(),
			Passed:    g.CurrentTrial.Outcome == "matched",
			Trial:     *g.CurrentTrial,
			TimeSecs:  float32(g.CurrentTrial.Duration().Seconds()),
			Binding:   g.currentBinding(),
		})
		g.CurrentIndex++
	}
	// Fill remaining targets as skipped with empty trials.
	for g.CurrentIndex < len(g.Words) {
		letter := ""
		if g.CurrentIndex < len(g.WordLetters) {
			letter = g.WordLetters[g.CurrentIndex]
		}
		g.Results = append(g.Results, WordResult{
			Word:      g.Words[g.CurrentIndex],
			Letter:    letter,
			CommandID: g.currentCommandID(),
			Passed:    false,
			Trial: Trial{
				Target:  g.Words[g.CurrentIndex],
				Outcome: "skipped",
			},
			Binding: g.currentBinding(),
		})
		g.CurrentIndex++
	}
	g.CurrentTrial = nil
}

// --- State: calibration domain state with own mutex ---

// State holds calibration-domain state protected by its own mutex.
type State struct {
	mu         sync.Mutex
	game       Game
	tabVisible bool

	// OnGameFinished is called when a calibration game completes.
	//
	// Callback, not an import: session owns CalibrationConsumed, calibration
	// only reports game end — keeps session↔calibration acyclic. It runs from
	// finishGame with THIS s.mu held and main.go's hook takes session's lock,
	// so the order is calibration's lock, then session's; never call into
	// calibration while holding session's.
	OnGameFinished func()

	// OnAttemptsChanged fires after the walk persists one or more attempts
	// (incremental live writes). The settings layer uses it to invalidate its
	// cached command catalog so the sidebar's per-command health updates as the
	// sitting progresses. nil-safe.
	OnAttemptsChanged func()

	// OnClipsChanged fires when the set of captured clips on disk may have
	// changed (a hold completed, a game was abandoned/swept). The settings layer
	// uses it to invalidate its cached Recordings list so a clip appears live as
	// you record it. nil-safe.
	OnClipsChanged func()

	// NextCommandID returns the catalog command that follows currentID in the
	// sidebar's display order, honoring the live search `filter` (empty = no
	// filter). Returns "" at the end of the (filtered) catalog. Drives
	// command-level chaining: when a catalog-activated walk exhausts one command,
	// the next filtered command's targets are appended. nil-safe (no chaining).
	NextCommandID func(currentID, filter string) string

	// Platform is the typed SDK surface calibration calls (settings re-render,
	// recognition.redecode). *branchkit.Plugin in production, set by main.go;
	// nil leaves both unwired.
	Platform Platform

	// GetPhonetics returns the current phonetic entries from the session state.
	GetPhonetics func() []struct {
		Word   string
		Letter *byte
	}

	// SettingsRedirect navigates the settings UI to a tab declared by this plugin.
	SettingsRedirect func(tab string) error

	// Append writes a record to a log-kind collection via the plugin SDK.
	Append func(collection string, payload any) error

	// TrialBegin opens a platform calibration trial scope. Returns the trial_id.
	// Set by main.go.
	TrialBegin func() (string, error)
	// TrialEnd closes a trial scope, releases registered fixtures, clears the
	// mode-gated tags it wrote, and clears `_platform.calibration.active`.
	TrialEnd func(trialID string) error
	// ApplyContext enters a command's context for a trial
	// (`trial.enter_context`): the platform platform-writes a mode-gated
	// command's requires_tags or forwards a dynamic command's fixture, and
	// returns the flat {kind, tags, fixture_handle} it entered. Set by main.go.
	ApplyContext func(trialID, commandID string) (AppliedContext, error)

	// ResolvePreview pipes a heard transcript through `commands.resolve` with
	// preview=true — the full matcher decision with NO side effects (no tag
	// writes, no bridge seed, no production telemetry). Computes the Resolves
	// signal for complete-command targets. Returns nil if the RPC fails.
	ResolvePreview func(heard string) *MatcherDecision
	// RenameVocab persists an in-walk retune: it renames a named_entities entry
	// (overrides.apply rename — change the surface form, keep the meaning) and
	// re-arms the recognizer's grammar so the new form is hearable. Generic over
	// the collection (alphabet codewords, number/direction/apps lists, …). Set
	// by main.go; required for RetuneTarget.
	RenameVocab func(collection, oldKey, newKey string) error
	// RevertVocab resets a named_entities entry to its plugin default
	// (overrides.apply revert) and returns the default key it fell back to — the
	// platform supplies it, so the host needn't know the default. Set by main.go;
	// required for RevertTarget.
	RevertVocab func(collection, currentKey string) (string, error)

	// CollectionKeys returns the entry keys (spoken surface forms) of a
	// named_entities collection — used to give a slotted command a real list
	// entry as its filler so the word is retunable. Set by main.go; nil-safe.
	CollectionKeys func(collection string) []string

	// EnumerateCommands returns the full command set (`commands.enumerate`) for
	// building walk targets. Set by main.go; nil-safe.
	EnumerateCommands func() []EnumeratedCommand
	// CommandSamples resolves owner-supplied calibration prompts for a command
	// (the `trial.resolve_samples` RPC → the owner's `trial_samples`
	// hook). Returns the concrete phrases to walk for a command whose vocabulary
	// the host can't derive — a third-party plugin's runtime list, a hint cross
	// product, free-text examples. Empty when the owner doesn't supply any, so
	// expansion falls back to the codeword stand-in. Set by main.go; nil-safe.
	CommandSamples func(commandID string) []string
	// LoadAttempts returns all persisted verification attempts — used by the
	// drill selection to find never-tested / weak commands. Set by main.go.
	LoadAttempts func() []VerificationAttempt

	// EngineName returns the active command-recognition engine id, stamped onto
	// each persisted attempt/session. nil-safe via engineName().
	EngineName func() string
	// CommandModel returns the command model the live stage loads, so the
	// re-decode probe decodes with the same one. nil-safe via commandModel().
	CommandModel func() string

	// CaptureEnabled reports whether trial-audio capture should run.
	CaptureEnabled func() bool
	// AutoAdvanceEnabled reports whether a green (heard-correctly) trial should
	// advance to the next target automatically, without a "Next" click. nil-safe
	// via autoAdvanceEnabled() — defaults to off.
	AutoAdvanceEnabled func() bool
	// SetCaptureDir points the command stage's `--dump-segments` at `dir`,
	// reporting whether the stage actually took it. A false here means no clips
	// will be written — the caller must not then claim capture is on.
	SetCaptureDir func(dir string) bool
	// RetentionPolicy returns the configured capture-retention policy and
	// whether it was actually resolved from the user's config (vs a pre-Ready
	// default). Unresolved means "do not delete" — see retentionPolicy().
	RetentionPolicy func() (policy string, resolved bool)

	// trialID is set when TrialBegin succeeds and cleared when TrialEnd runs.
	// Protected by `mu`.
	trialID string
	// enteredContext is the CommandID whose context the open trial holds ("" for
	// codewords / no command). Consecutive targets that share it reuse the same
	// trial instead of re-applying an identical context — which for a dynamic
	// (hint) command avoids re-pushing the whole fixture per pair and racing its
	// async release. Protected by `mu`.
	enteredContext string

	// captureDir is the active game's trial-audio capture dir ("" = capture off).
	captureDir string

	// chaining is true when the active walk was started by activating a single
	// catalog command (a sidebar click). On command-exhaust such a walk appends
	// the next filtered catalog command instead of finishing — walking the
	// sidebar command-to-command. Bulk walks (Test all / category / drill /
	// alphabet) leave it false and finish when their pre-built queue runs out.
	chaining bool
	// catalogFilter is the server's copy of the sidebar search term, posted with
	// each catalog interaction. Command-level chaining (including the hands-free
	// speech path, which carries no client filter) reads it so "next command"
	// honors the active filter.
	catalogFilter string
	// sittingID is the session id for the current sitting — created lazily on the
	// first persisted attempt (so it ties to real data, not a bare page open),
	// shared by every attempt of the sitting, and stamped on the session record
	// at close. Cleared when the sitting closes.
	//
	// A sitting's session opens on the first RECORDED attempt (selecting a
	// catalog row enters context but records nothing) and closes on Stop or
	// page-leave. There is deliberately no end-of-walk Results screen:
	// per-command health is the live report (decided 2026-06-21; a
	// snapshot-on-Stop table was rejected because it reintroduces the
	// start/finish boundary the live catalog dissolves).
	sittingID string
	// flushedThrough is how many of game.Results have already been persisted
	// (incremental live writes). Advanced as attempts flush; clamped back on Back
	// so a re-walked target re-persists as a fresh attempt.
	flushedThrough int

	// lastSeenAt is when the calibration page was last known to be open — bumped
	// by a render of the calibration tab and by the page's heartbeat ping. The
	// liveness loop auto-suspends an active walk when this goes stale (the user
	// navigated away). Zero when no walk is running. Protected by `mu`.
	lastSeenAt time.Time

	// Refresh coalescer state.
	refreshDirty   atomic.Bool
	refreshDirtyCh chan struct{}
	refreshFlushCh chan struct{}
	refreshStarted atomic.Bool
}

// NewState creates a calibration state.
func NewState() *State {
	return &State{
		game:           NewGame(),
		refreshDirtyCh: make(chan struct{}, 1),
		refreshFlushCh: make(chan struct{}, 1),
	}
}

// StartRefreshCoalescer launches the background loop that coalesces
// settings.refresh calls during active calibration. Idempotent.
func (s *State) StartRefreshCoalescer() {
	if !s.refreshStarted.CompareAndSwap(false, true) {
		return
	}
	go s.refreshLoop()
	go s.livenessLoop()
}

// RecordSeen marks the calibration page as currently open. Called on every
// render of the calibration tab and on the page's heartbeat ping; the liveness
// loop reads the resulting timestamp to detect a left-behind walk.
func (s *State) RecordSeen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeenAt = time.Now()
}

// livenessLoop auto-suspends an active walk once the calibration page stops
// reporting in (the user navigated away — browser back, closed the window, a
// tab switch). The settings UI is browser-based and in-page navigation isn't an
// event the plugin can subscribe to, so this absence-of-heartbeat backstop is
// the reliable signal. Started once by StartRefreshCoalescer.
func (s *State) livenessLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.suspendIfStale()
	}
}

// suspendIfStale closes an active walk's sitting when the calibration page
// hasn't reported in within livenessTimeout (the user navigated away). Under the
// live-dashboard model a sitting closes on leave: per-command attempts were
// already persisted incrementally, so finishGame just writes the session summary,
// tears down the trial scope, and ends the walk — which also stops the speech
// interceptor from swallowing Option+G. Re-opening the page lands on the catalog
// with health current. No-op when nothing is active.
func (s *State) suspendIfStale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.game.IsActive() {
		return
	}
	if s.lastSeenAt.IsZero() || time.Since(s.lastSeenAt) <= livenessTimeout {
		return
	}
	s.finishGame()
	branchkit.Logf("voice", "calibration: sitting closed — page left (no heartbeat for %s)", livenessTimeout)
}

// MarkRefreshDirty signals that the calibration UI needs re-rendering.
func (s *State) MarkRefreshDirty() {
	s.refreshDirty.Store(true)
	select {
	case s.refreshDirtyCh <- struct{}{}:
	default:
	}
}

// FlushRefresh forces the coalescer to dispatch on the next loop iteration.
func (s *State) FlushRefresh() {
	s.refreshDirty.Store(true)
	select {
	case s.refreshFlushCh <- struct{}{}:
	default:
	}
}

// refreshLoop sleeps until something is marked dirty, then waits out the
// debounce (so a burst of marks is one refresh) unless a flush cuts it
// short. It used to wake on a 50 ms ticker forever to check the flag:
// ~13 wakeups a second while nothing happened (measured 2026-10-07).
func (s *State) refreshLoop() {
	for {
		select {
		case <-s.refreshFlushCh:
		case <-s.refreshDirtyCh:
			t := time.NewTimer(refreshDebounce)
			select {
			case <-t.C:
			case <-s.refreshFlushCh:
				t.Stop()
			}
		}
		if !s.refreshDirty.Swap(false) {
			continue
		}
		if s.Platform != nil {
			_ = s.Platform.SettingsRefresh()
		}
	}
}

// WithLock runs fn while holding the calibration mutex.
func (s *State) WithLock(fn func(game *Game, tabVisible bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.game, s.tabVisible)
}

// IsActive returns whether calibration is currently active.
func (s *State) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.game.IsActive()
}

// SetTabVisible sets the calibration tab visibility flag.
func (s *State) SetTabVisible(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tabVisible = v
}

// SetTabVisibleIfNotActive sets tabVisible only if calibration is not active.
func (s *State) SetTabVisibleIfNotActive(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.game.IsActive() {
		s.tabVisible = v
	}
}

// SnapshotForRender returns a snapshot of calibration state for settings rendering.
func (s *State) SnapshotForRender() (active bool, game Game) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.game.IsActive(), s.game
}

// ProcessSpeechFrame ingests a recognition frame into the current trial during an
// active calibration walk (so the live "Heard" display + green-match update), and
// reports whether it consumed the frame, so the caller suppresses normal command
// processing. It does NOT detect the hold boundary: the command engine (sherpa,
// continuous VAD) emits the recognized words as non-final segments and only
// signals end-of-hold by stopping the pipeline on key-up — handled by
// HandleHoldEnd. False when no walk is running, the tab isn't visible, or the
// frame is empty (a boundary marker).
//
// It used to return *InterceptResult, whose two fields the only caller discarded
// with `_ = result` — nil-vs-non-nil was the entire payload.
func (s *State) ProcessSpeechFrame(transcript string, isFinal bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.game.IsRunning() || !s.tabVisible {
		return false
	}
	if transcript == "" {
		return false
	}

	s.game.ProcessFrame(transcript, isFinal)
	s.MarkRefreshDirty()
	return true
}

// HandleHoldEnd marks the end of an Option+G hold during a calibration walk. This
// is the boundary the command engine signals by stopping the ephemeral command
// pipeline (key-up), NOT by an is_final transcript frame — the sherpa continuous
// engine never sends one. It tallies a silent hold when the hold produced no
// recognition, and (auto-advance on) advances when the trial landed green. The
// speech path carries no filter, so advance/chaining use the stored catalog
// filter. No-op when no walk is running / the tab isn't visible.
func (s *State) HandleHoldEnd() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.game.IsRunning() || !s.tabVisible {
		return
	}

	s.game.MarkHoldBoundary()
	// Kick the live L1 bias counterfactual for the hold that just ended, before
	// any auto-advance moves the cursor off this trial (the probe captures the
	// finishing trial's identity now and stamps it back only if it's still live).
	s.scheduleBiasProbe()
	if s.autoAdvanceEnabled() && s.game.MatchStatus() {
		s.advanceLocked("")
	}

	if !s.game.IsActive() {
		s.FlushRefresh()
	} else {
		s.MarkRefreshDirty()
	}

	// The dump stage just wrote this hold's clip; refresh the Recordings list
	// once it lands so it shows up live.
	s.scheduleClipRefresh()
}

// scheduleClipRefresh invalidates the cached Recordings list and pushes a
// settings refresh after the clip's WAV has had time to land on disk. Off the
// game lock — it's a timer + lock-free signals.
func (s *State) scheduleClipRefresh() {
	if s.OnClipsChanged == nil {
		return
	}
	go func() {
		time.Sleep(clipSettleDelay)
		s.OnClipsChanged()
		s.FlushRefresh()
	}()
}

// CancelViaSpeech cancels the active calibration game from a speech command.
func (s *State) CancelViaSpeech() {
	s.mu.Lock()
	defer s.mu.Unlock()
	wasActive := s.game.IsActive()
	s.game.Cancel()
	if wasActive {
		s.endTrial()
		s.stopCapture(false)
	}
}

// engineName returns the active command-recognition engine id, or "" when the
// accessor isn't wired (tests construct State directly).
func (s *State) engineName() string {
	if s.EngineName == nil {
		return ""
	}
	return s.EngineName()
}

// commandModel returns the model the live command stage loads, or
// OfflineModelName when the accessor isn't wired (tests construct State
// directly).
func (s *State) commandModel() string {
	if s.CommandModel == nil {
		return OfflineModelName
	}
	return s.CommandModel()
}

// beginTrial opens a calibration trial scope and stores the returned trial_id.
// Must be called with s.mu held. No-op when TrialBegin is unwired (tests).
//
// Any trial already open is closed first — so calling beginTrial as each target
// enters its context auto-tears-down the prior target's scope (clearing its
// mode-gated tags and releasing its fixture) before the new one's tags/fixture
// go live.
func (s *State) beginTrial() string {
	if s.TrialBegin == nil {
		return ""
	}
	if s.trialID != "" {
		s.endTrial()
	}
	id, err := s.TrialBegin()
	if err != nil {
		branchkit.Logf("voice", "calibration: trial_begin failed: %v", err)
		return ""
	}
	s.trialID = id
	return id
}

// endTrial closes the trial scope if one is open. Must be called with s.mu
// held. Safe to call when no trial is open (no-op).
func (s *State) endTrial() {
	if s.trialID == "" || s.TrialEnd == nil {
		s.trialID = ""
		return
	}
	if err := s.TrialEnd(s.trialID); err != nil {
		branchkit.Logf("voice", "calibration: trial_end(%s) failed: %v", s.trialID, err)
	}
	s.trialID = ""
	s.enteredContext = ""
}

// enterCurrentTargetContextLocked enters the current target's context. If the
// open trial already holds that command's context (consecutive targets of the
// same command — e.g. the spread of pairs for one dynamic hint command), it's
// reused as-is. Otherwise it opens a fresh trial scope (auto-closing the prior,
// which clears its tags / releases its fixture) and enters the new context.
// Must be called with s.mu held.
func (s *State) enterCurrentTargetContextLocked() {
	tgt, ok := s.game.currentTarget()
	if !ok {
		return
	}
	if s.trialID != "" && tgt.CommandID == s.enteredContext {
		return
	}
	trialID := s.beginTrial()
	s.enterContextForTargetLocked(trialID, tgt)
	s.enteredContext = tgt.CommandID
}

// enterContextForTargetLocked enters a single target's context for a trial via
// `trial.enter_context` and records the kind/name on the game for the live card.
// A bare codeword (no command id) needs no context — only the trial scope's
// `_platform.calibration.active` gate, already set by beginTrial. Apply
// failures degrade the trial (logged) rather than abort it. Must be called with
// s.mu held.
func (s *State) enterContextForTargetLocked(trialID string, tgt Target) {
	if tgt.CommandID == "" || trialID == "" || s.ApplyContext == nil {
		s.game.Mode = "static"
		s.game.Context = ""
		return
	}
	ctx, err := s.ApplyContext(trialID, tgt.CommandID)
	if err != nil {
		branchkit.Logf("voice", "calibration: enter_context(%s) failed: %v — degraded trial", tgt.CommandID, err)
		s.game.Mode = "static"
		s.game.Context = ""
		return
	}
	s.game.Mode = ctx.Kind
	s.game.Context = tgt.CommandID
}

// GetCurrentTarget returns the current prompt and index for logging.
// Returns ("", -1) if calibration is not active.
func (s *State) GetCurrentTarget() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.game.IsActive() {
		return "", -1
	}
	target := ""
	if s.game.CurrentIndex < len(s.game.Words) {
		target = s.game.Words[s.game.CurrentIndex]
	}
	return target, s.game.CurrentIndex
}

// StartWalk starts a calibration walk over the given targets. `letters` is the
// parallel alphabet-letter list (nil for command walks); `source` is the
// selection provenance. Returns (active, redirect). No-op (redirect=false)
// when targets is empty.
func (s *State) StartWalk(targets []Target, letters []string, source string) (active bool, redirect bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(targets) == 0 {
		return s.game.IsActive(), false
	}
	s.game.Start(targets)
	s.game.WordLetters = letters
	s.game.Source = source
	s.game.GrammarWords = s.game.Words
	s.chaining = false
	s.sittingID = ""
	s.flushedThrough = 0
	s.lastSeenAt = time.Now()
	s.startCapture()
	s.enterCurrentTargetContextLocked()
	return s.game.IsActive(), true
}

// ActivateCommand starts a walk for one catalog command and arms command-level
// chaining: when this command's targets exhaust, the next filtered catalog
// command is appended (see chainNextLocked). `filter` is the live sidebar search
// term (stored for chaining, including the speech path); `variant` tunes a
// dynamic command's expansion ("single" walks each codeword alone). Selecting a
// command records nothing on its own — it just arms the next hold. No-op
// (redirect=false) when the command resolves to no targets.
func (s *State) ActivateCommand(id, filter, variant string) (active bool, redirect bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := s.targetsForCommandLocked(id, variant)
	if len(targets) == 0 {
		return s.game.IsActive(), false
	}
	if s.game.IsActive() {
		// Jumping to another command abandons the in-progress walk. Its attempts
		// were already persisted live (sidebar health holds), so this just tears
		// down the open trial scope; no session summary for a mid-walk jump.
		s.endTrial()
		s.stopCapture(false)
	}
	s.game.Start(targets)
	s.game.Source = id
	s.game.GrammarWords = s.game.Words
	s.chaining = true
	s.catalogFilter = filter
	s.sittingID = ""
	s.flushedThrough = 0
	s.lastSeenAt = time.Now()
	s.startCapture()
	s.enterCurrentTargetContextLocked()
	return s.game.IsActive(), true
}

// targetsForCommandLocked resolves one catalog command id into its concrete walk
// targets — the same expansion StartWalk's selection path uses (dynamic hint
// cross product / single codewords, list-slot entries). Empty when the id is
// unknown or no commands are enumerable. Must be called with s.mu held.
func (s *State) targetsForCommandLocked(id, variant string) []Target {
	var enum []EnumeratedCommand
	if s.EnumerateCommands != nil {
		enum = s.EnumerateCommands()
	}
	targets := TargetsFromIDs(enum, []string{id})
	targets = s.expandDynamicTargets(targets, variant == "single")
	targets = s.expandListSlotTargets(targets)
	return targets
}

// chainNextLocked appends the next filtered catalog command's targets to the
// live walk and enters its context, when this is a chaining (catalog-activated)
// walk and a next command exists. Returns true when it chained (the walk
// continues). Must be called with s.mu held and the cursor past the end.
func (s *State) chainNextLocked() bool {
	if !s.chaining || s.NextCommandID == nil {
		return false
	}
	nextID := s.NextCommandID(s.game.lastCommandID(), s.catalogFilter)
	if nextID == "" {
		return false
	}
	targets := s.targetsForCommandLocked(nextID, "")
	if len(targets) == 0 {
		return false
	}
	s.game.AppendTargets(targets)
	s.enterCurrentTargetContextLocked()
	return true
}

// StartGame starts an acoustic-only walk over the given literal words. Thin
// convenience used by tests.
func (s *State) StartGame(words []string) {
	targets := make([]Target, len(words))
	for i, w := range words {
		targets[i] = CodewordTarget(w)
	}
	s.StartWalk(targets, nil, "custom")
}

// scoreCurrentTrialLocked stamps the two signals (Heard/Resolves), the matcher
// decision, and the derived verdict onto the current trial before Advance reads
// its outcome. Resolves runs only for complete-command targets. Must be called
// with s.mu held.
// advanceLocked scores the current trial, finalizes it, and moves to the next
// target — skipping targets filtered out by the live rail filter, and finishing
// the walk or entering the next target's context as appropriate. Shared by the
// "advance" action and the speech-loop auto-advance. Must be called with s.mu
// held and the game active.
func (s *State) advanceLocked(filter string) {
	s.scoreCurrentTrialLocked()
	gameOver := s.game.Advance()
	if !gameOver {
		gameOver = s.game.SkipForwardToFilter(filter)
	}
	s.flushAttemptsLocked()
	if gameOver {
		if s.chainNextLocked() {
			return
		}
		s.finishGame()
	} else {
		s.enterCurrentTargetContextLocked()
	}
}

// flushAttemptsLocked persists any walk Results not yet written — the live,
// incremental replacement for batch-at-finish. Called after each advance/skip
// (and once more at finish for the tail), so leaving mid-sitting keeps every
// command already walked and the sidebar's per-command health updates as the
// walk progresses. Skipped/empty trials aren't written (they carry no attempt),
// but still advance the cursor so they aren't reconsidered. The sitting id is
// created lazily on the first real attempt. Must be called with s.mu held.
func (s *State) flushAttemptsLocked() {
	if s.Append == nil {
		s.flushedThrough = len(s.game.Results)
		return
	}
	prefix := ""
	if len(s.game.SourcePrefix) > 0 {
		prefix = s.game.SourcePrefix[0]
	}
	group := groupLabel(s.game.Source, prefix)
	engine := s.engineName()
	ts := time.Now().UTC().Format(time.RFC3339)
	wrote := false
	for i := s.flushedThrough; i < len(s.game.Results); i++ {
		res := s.game.Results[i]
		trial := &res.Trial
		if len(trial.Frames) == 0 {
			continue // no recognition — counted as no-input on the session, not an attempt
		}
		if s.sittingID == "" {
			s.sittingID = NewID()
		}
		s.Append("plugin.voice.verification_attempts", &VerificationAttempt{
			ID:               NewID(),
			SessionID:        s.sittingID,
			Timestamp:        ts,
			Target:           res.Word,
			CommandID:        res.CommandID,
			Source:           "calibration",
			Group:            group,
			RecognitionPath:  "sequential",
			SequencePosition: 0,
			Heard:            trial.FinalHeard(),
			Matched:          trial.Passed(),
			Verdict:          trial.Verdict,
			MatcherDecision:  trial.MatcherDecision,
			DurationMs:       int(trial.Duration().Milliseconds()),
			AttemptIndex:     0,
			FirstHeard:       trial.FirstHeard(),
			Revisions:        trial.Revisions(),
			HeardTallies:     trial.HeardTallies(),
			SilentHolds:      trial.SilentHolds,
			Engine:           engine,
		})
		wrote = true
	}
	s.flushedThrough = len(s.game.Results)
	if wrote && s.OnAttemptsChanged != nil {
		s.OnAttemptsChanged()
	}
}

// autoAdvanceEnabled reports the auto-advance preference (off when the accessor
// is unwired, e.g. tests that don't set it).
func (s *State) autoAdvanceEnabled() bool {
	return s.AutoAdvanceEnabled != nil && s.AutoAdvanceEnabled()
}

func (s *State) scoreCurrentTrialLocked() {
	t := s.game.CurrentTrial
	if t == nil {
		return
	}
	tgt, ok := s.game.currentTarget()
	if !ok {
		return
	}
	heard := t.FinalHeard()
	var dec *MatcherDecision
	if tgt.resolvable() && s.ResolvePreview != nil {
		dec = s.ResolvePreview(heard)
	}
	v := ScoreTrial(heard, tgt, dec)
	t.Heard = v.Heard
	t.Resolves = v.Resolves
	t.MatcherDecision = dec
	t.Verdict = v.Label()
}

// representableSurfaceForm is the lexicon pre-screen: a retune candidate the
// command engine can actually decode is lowercase letters (a multi-word entry
// like an app name may carry spaces). The engine silently drops a surface form
// it can't represent, so rejecting an unrepresentable candidate
// here — digits, punctuation, non-ASCII — saves the user a fixture recompile +
// retest on a word that was never going to be heard. Deterministic; needs no
// model files. (A richer membership check against the engine's BPE/HLG lexicon
// is a possible v2; the grammar provider BPE-encodes coined lowercase words, so
// they decode either way — only unrepresentable character shapes are a true drop.)
func representableSurfaceForm(s string) error {
	if s == "" {
		return fmt.Errorf("replacement required")
	}
	for _, r := range s {
		if r == ' ' {
			continue // multi-word surface forms (e.g. app names) are allowed
		}
		if r < 'a' || r > 'z' {
			return fmt.Errorf("%q can't be recognized — use letters a–z only", s)
		}
	}
	return nil
}

// RetuneTarget closes the calibration loop in place: it renames the vocabulary
// entry the current target tests (its VocabBinding) to `newKey`, re-arms the
// grammar so the new surface form is recognizable under the same context that
// exposed the failure, and re-tests the current target — without leaving the
// walk. Works for any retunable target (an alphabet codeword, a list slot like
// number/direction/apps); the binding says which collection + entry to rename.
//
// The re-arm is release-free: for a dynamic (hint) target the live fixture is
// refreshed in place on the SAME open trial (the platform's fixture release is
// async and would race a reapply); RenameVocab already commits the vocabulary
// for static targets. Requires a running walk, a retunable current target, and
// the RenameVocab callback.
//
// Report retune is rename-now, batch re-test later (variant A). The
// rejected-for-v1 alternative, a live one-word retest inside the popup
// (variant B), needs a transient trial scope with its own speech routing
// through the calibration gate — the higher-risk part — and is a mostly-UI
// follow-up on this same seam if the batch round-trip proves annoying.
func (s *State) RetuneTarget(newKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.game.IsRunning() {
		return fmt.Errorf("no active walk to retune")
	}
	newKey = strings.ToLower(strings.TrimSpace(newKey))
	if err := representableSurfaceForm(newKey); err != nil {
		return err
	}
	tgt, ok := s.game.currentTarget()
	if !ok || tgt.Binding == nil {
		return fmt.Errorf("the current prompt isn't a retunable vocabulary entry")
	}
	b := *tgt.Binding
	if b.Key == newKey {
		return nil // no-op
	}
	if s.RenameVocab == nil {
		return fmt.Errorf("vocabulary rename unavailable")
	}
	if err := s.RenameVocab(b.Collection, b.Key, newKey); err != nil {
		return fmt.Errorf("save override: %w", err)
	}

	// Re-arm: a dynamic (hint) target's live fixture must refresh in place to
	// carry the new codeword; RenameVocab already committed the vocabulary for
	// static targets (alphabet re-push / list commit).
	if tgt.CommandID != "" && tgt.Tier == TierDynamic {
		s.refreshCurrentContextLocked()
	}

	// Replace the old surface form in the prompt, update the binding, and start
	// a fresh trial.
	s.game.RetargetCurrent(b.Key, newKey)
	return nil
}

// RevertTarget resets the current target's vocabulary entry to its plugin
// default (the inverse of a retune), re-arms the grammar, and re-points the
// prompt at the default word. The platform supplies the default key, so the host
// needn't know it. No-op (returns nil) when the entry is already the default.
func (s *State) RevertTarget() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.game.IsRunning() {
		return fmt.Errorf("no active walk to revert")
	}
	tgt, ok := s.game.currentTarget()
	if !ok || tgt.Binding == nil {
		return fmt.Errorf("the current prompt isn't a revertable vocabulary entry")
	}
	if s.RevertVocab == nil {
		return fmt.Errorf("vocabulary revert unavailable")
	}
	b := *tgt.Binding
	defaultKey, err := s.RevertVocab(b.Collection, b.Key)
	if err != nil {
		return fmt.Errorf("revert: %w", err)
	}
	if defaultKey == "" || defaultKey == b.Key {
		return nil // already at the plugin default
	}

	if tgt.CommandID != "" && tgt.Tier == TierDynamic {
		s.refreshCurrentContextLocked()
	}
	s.game.RetargetCurrent(b.Key, defaultKey)
	return nil
}

// refreshCurrentContextLocked re-applies the current target's context on the
// SAME open trial so a mid-walk codeword override flows into a dynamic (hint)
// target's fixture without a release→reapply toggle (the platform's fixture
// release is async and would race the reapply). No-op for a bare codeword (no
// fixture) or when no trial is open. Must be called with s.mu held.
func (s *State) refreshCurrentContextLocked() {
	tgt, ok := s.game.currentTarget()
	if !ok || tgt.CommandID == "" || s.trialID == "" || s.ApplyContext == nil {
		return
	}
	if _, err := s.ApplyContext(s.trialID, tgt.CommandID); err != nil {
		branchkit.Logf("voice", "calibration: refresh_context(%s) failed: %v", tgt.CommandID, err)
	}
}

// GetCurrentIndex returns the game's current target index.
func (s *State) GetCurrentIndex() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.game.CurrentIndex
}

// GotoTarget repositions the walk cursor to target n, preserving the parallel
// invariant that Results[i] is target i's outcome. Jumping back truncates the
// later results (so those targets can be re-walked); jumping forward marks the
// skipped-over targets as "skipped" (like stepping Skip). No-op when not
// running, out of range, or already current.
func (g *Game) GotoTarget(n int) {
	if !g.IsRunning() || n < 0 || n >= len(g.Words) || n == g.CurrentIndex {
		return
	}
	if n < g.CurrentIndex {
		g.CurrentIndex = n
		if len(g.Results) > n {
			g.Results = g.Results[:n]
		}

		g.startNewTrial()
		return
	}
	for g.IsRunning() && g.CurrentIndex < n {
		if g.Skip() {
			return
		}
	}
}

// GotoTarget jumps the walk cursor to target index n (a group-rail click),
// enters that target's context, and refreshes the UI. A backward jump may
// truncate already-persisted Results, so flushedThrough is clamped (the
// re-walked targets re-persist as fresh attempts); a forward jump's skipped
// targets flush as usual. No-op when not running.
func (s *State) GotoTarget(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.game.IsRunning() {
		return
	}
	s.game.GotoTarget(n)
	if s.flushedThrough > len(s.game.Results) {
		s.flushedThrough = len(s.game.Results)
	}
	s.flushAttemptsLocked()
	s.enterCurrentTargetContextLocked()
	s.FlushRefresh()
}

// GetResults returns a copy of the game results.
func (s *State) GetResults() []WordResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WordResult, len(s.game.Results))
	copy(out, s.game.Results)
	return out
}

// ActiveCommandID returns the command id the live walk's cursor sits on (""
// when no walk is active or the cursor is on a bare codeword). Drives the
// sidebar's active-row highlight + the right-pane health readout.
func (s *State) ActiveCommandID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.game.IsActive() {
		return ""
	}
	return s.game.currentCommandID()
}

// HandleCalibrate processes a non-start calibration action. Start is handled by
// StartWalk / ActivateCommand. `filter` is the live catalog search term — when
// set, advance/skip/back move through only the matching (visible) targets, and
// command-level chaining honors it. Returns (active, redirect); redirect is
// always false here (only Start/Activate redirect).
func (s *State) HandleCalibrate(action, filter string) (active bool, redirect bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Catalog interactions carry the live search term; remember it so command
	// chaining (incl. the filter-less speech path) honors the active filter.
	if filter != "" {
		s.catalogFilter = filter
	}

	switch action {
	case "cancel":
		// Explicit throw-away: end the walk without a session summary. Per-command
		// attempts already flushed live stay (they're real data); the in-progress
		// trial and the session record are discarded.
		wasActive := s.game.IsActive()
		s.game.Cancel()
		s.chaining = false
		s.sittingID = ""
		s.flushedThrough = 0
		if wasActive {
			s.endTrial()
			s.stopCapture(false) // restore the pipeline; cancelled games aren't analyzed
		}

	case "stop":
		// Close the sitting: persist + summarize, return to the catalog (the
		// banner renders from the Finished phase). Replaces the old pause menu.
		if s.game.IsActive() {
			s.finishGame()
		}

	case "dismiss":
		// Clear the post-stop banner / reports: Finished or Idle → fresh catalog.
		s.endTrial()
		s.game = NewGame()
		s.chaining = false
		s.sittingID = ""
		s.flushedThrough = 0

	case "advance":
		if s.game.IsActive() {
			s.advanceLocked(filter)
		}

	case "retry":
		if s.game.IsActive() {
			s.game.Retry()
		}

	case "skip":
		if s.game.IsActive() {
			gameOver := s.game.Skip()
			if !gameOver {
				gameOver = s.game.SkipForwardToFilter(filter)
			}
			s.flushAttemptsLocked()
			if gameOver {
				if !s.chainNextLocked() {
					s.finishGame()
				}
			} else {
				s.enterCurrentTargetContextLocked()
			}
		}

	case "skip_group":
		if s.game.IsActive() {
			gameOver := s.game.SkipGroup()
			if !gameOver {
				gameOver = s.game.SkipForwardToFilter(filter)
			}
			s.flushAttemptsLocked()
			if gameOver {
				if !s.chainNextLocked() {
					s.finishGame()
				}
			} else {
				s.enterCurrentTargetContextLocked()
			}
		}

	case "back":
		if s.game.IsActive() {
			s.game.Back()
			s.game.BackToFilter(filter)
			// A re-walked target re-persists as a fresh attempt.
			if s.flushedThrough > len(s.game.Results) {
				s.flushedThrough = len(s.game.Results)
			}
			s.enterCurrentTargetContextLocked()
		}

	case "view_reports":
		if s.game.Phase == PhaseIdle || s.game.Phase == PhaseFinished {
			s.game.Phase = PhaseReviewing
		}
	}

	return s.game.IsActive(), false
}

// finishGame closes the sitting: it finalizes any in-progress trial, tears down
// the trial scope, flushes the remaining attempts (the bulk of them already
// persisted incrementally as the walk ran), and writes one session record
// summarizing the whole sitting. Triggered by Stop, by reaching the end of the
// (chained or bulk) walk, and by the page-left liveness backstop. Must be called
// with s.mu held.
func (s *State) finishGame() {
	s.game.Finish()
	// Close the current target's trial scope (clears its tags, releases its
	// fixture) and restore the command pipeline + analyze captured audio.
	s.endTrial()
	s.stopCapture(true)

	// Persist any attempts not yet flushed (the just-finished trial + the tail).
	s.flushAttemptsLocked()
	s.writeSessionRecordLocked()

	s.sittingID = ""
	s.flushedThrough = 0
	s.chaining = false

	if s.OnGameFinished != nil {
		s.OnGameFinished()
	}
}

// writeSessionRecordLocked writes the sitting's session summary, keyed by the
// sitting id. Skipped when no attempt was ever recorded (sittingID unset) — a
// sitting with no real holds is noise, not a session. Summary stats span the
// whole sitting (every chained command). Must be called with s.mu held.
func (s *State) writeSessionRecordLocked() {
	if s.Append == nil || s.sittingID == "" {
		return
	}
	prefix := ""
	if len(s.game.SourcePrefix) > 0 {
		prefix = s.game.SourcePrefix[0]
	}
	noInputCount := 0
	for _, res := range s.game.Results {
		if len(res.Trial.Frames) == 0 {
			noInputCount++
		}
	}
	s.Append("plugin.voice.verification_sessions", &VerificationSession{
		ID:           s.sittingID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Source:       "calibration",
		Group:        groupLabel(s.game.Source, prefix),
		WordCount:    len(s.game.Words),
		MatchCount:   s.game.MatchedCount(),
		ScorePct:     scorePct(s.game.MatchedCount(), s.game.AttemptedCount()),
		ElapsedSecs:  s.game.ElapsedSecs,
		NoInputCount: noInputCount,
		Engine:       s.engineName(),
		// The grammar snapshot lives HERE and only here: it is per-sitting
		// context, identical for every attempt in one.
		GrammarSnapshot: s.game.GrammarWords,
		Mode:            s.game.Mode,
		Context:         s.game.Context,
	})
}

// groupLabel returns the most specific label for this calibration group.
func groupLabel(source, prefix string) string {
	if prefix != "" {
		return prefix
	}
	if source != "" {
		return source
	}
	return "alphabet"
}

func scorePct(matched, attempted int) float32 {
	if attempted == 0 {
		return 0
	}
	return float32(matched) / float32(attempted) * 100
}
