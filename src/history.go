package main

import (
	"errors"
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Recent-command history — the data behind the Option+B "History" HUD tab.
//
// Every matched command is recorded here with its recognition margin, so the
// panel can surface WHAT fired and HOW SURE the engine was. This is the
// transparency surface for the confidence-gating work: a mis-heard word that
// fired a command (the "hello"→"hi" click) lands here with a deeply-negative
// worst-word margin, visibly distinct from a confident real command. A later
// "that was wrong" flag on a row turns this passive view into the labeled
// ground truth a confidence floor is tuned against.
//
// It is a pure VIEW of orchestrate output the plugin already handles — no new
// event subscription, timer, or sensor — so it does not trip the
// one-in-one-out sensing freeze.

const historyCap = 20

// recognitionLogName is the durable per-match log (log preset, PerCollectionDisk,
// by_field "id"). Every matched command is put here with its margin; the flag
// patches verdict="wrong". The margin distribution + verdicts accrue on-device
// from normal use — the data a confidence floor is tuned against, no longer
// bounded by show-all's ~2-day margin window.
//
// It records only when the user turns it on (collections/recognition_log.json:
// off by default, 30 days' retention). The in-memory ring
// below feeds the History HUD either way.
const recognitionLogName = "plugin.voice.recognition_log"

// recognitionLogCheckInterval bounds how stale voice's view of the
// recognition_log recording flag may be. Nothing tells a plugin when the user
// flips a log's recording toggle, so voice re-reads it at most this often.
const recognitionLogCheckInterval = 30 * time.Second

// recognitionLogOn reports whether recognition_log is recording, reading the
// flag from the platform at most once per recognitionLogCheckInterval.
//
// Asking first is what keeps recording-off quiet. The append itself would be
// refused, but every refusal is a warn-level state.write_denied line in
// platform.log — one per spoken command, on every install that never opted
// in. A failed read answers true, so the append runs and the platform
// decides, as it did before this check existed.
func (h *Host) recognitionLogOn() bool {
	h.recLogMu.Lock()
	on := h.recLogOn
	fresh := !h.recLogCheckedAt.IsZero() && time.Since(h.recLogCheckedAt) < recognitionLogCheckInterval
	h.recLogMu.Unlock()
	if fresh {
		return on
	}
	// Not under the lock: it is a round trip to the platform.
	on, err := h.recognitionLogRecording()
	if err != nil {
		return true
	}
	h.recLogMu.Lock()
	h.recLogOn, h.recLogCheckedAt = on, time.Now()
	h.recLogMu.Unlock()
	return on
}

// noteRecognitionLogOff records a refused append: recording was switched off
// since the last read. Later matches skip the append until the next read.
func (h *Host) noteRecognitionLogOff() {
	h.recLogMu.Lock()
	h.recLogOn, h.recLogCheckedAt = false, time.Now()
	h.recLogMu.Unlock()
}

func (h *Host) recognitionLogRecordingDefault() (bool, error) {
	resp, err := h.plugin.PrivacyGetRecording(branchkit.PrivacyGetRecordingRequest{Name: recognitionLogName})
	if err != nil {
		return false, err
	}
	return resp.Enabled, nil
}

type historyEntry struct {
	ID     string // stable record id — the collection key the flag patches
	At     time.Time
	Phrase string   // the lowercased transcript that matched
	Action string   // dotted action type of the first dispatched action
	Mode   string   // voice mode active at match time
	Conf   *float64 // mean margin (coarse; display only)
	Worst  *float64 // min per-word margin — the gate-relevant number
	Wrong  bool     // set by the "that was wrong" flag (labeling)
	// Skipped: the confidence gate (enforce) dropped this decode, so nothing
	// ran. Ring-only: never written to the recognition_log, whose unflagged
	// rows are the genuine-command population the floor is measured from.
	Skipped bool
	// Logged: the match was written to the recognition_log, so ID is its key
	// there. False when recording was off (ID is ring-local, and there is no
	// record for a flag to annotate).
	Logged bool

	// Trace is the utterance's chain id (tr_), the key the platform's
	// `_platform.pipeline.executed` report is joined on.
	Trace string
	// Actions is the type of every action handed to the platform, in order
	// ("copy that paste that" is two). Action is the first of them.
	Actions []string
	// Stop says why resolving stopped before the last word ("miss", "tie"),
	// and Unresolved holds the words it left. A row with no Actions and a
	// Stop is a command that was heard and did not resolve: ring-only, like
	// Skipped.
	Stop       string
	Unresolved string
	// Results is what the platform reported each action did, filled when its
	// report arrives (nil until then). Dropped is set instead when the
	// platform discarded the turn: voice answered too late.
	Results []actionResult
	Dropped string
}

// actionResult is one action's outcome as the platform reported it.
type actionResult struct {
	ActionType string
	Status     string // ok, error, not_handled, denied, simulated
	Handler    string
	Denial     string
}

// commandRecord is what one command utterance produced, for the History
// ring and the recognition_log.
type commandRecord struct {
	Phrase      string
	Actions     []string
	Mode        string
	Conf, Worst *float64
	Trace       string
	Stop        string
	Unresolved  []string
}

// recordMatch appends a matched command to the in-memory ring (for the HUD) and,
// when the user has recording on, durably to the recognition_log (for
// threshold analysis). recognition_log is a
// keyed compacted-changelog log: Append *introduces* the match record — the
// server mints its `id` (also the fold key) and returns it — with an empty
// verdict; a later flag annotates that same id (see handleMarkWrong). Repaints
// the panel if open so the row appears live.
func (h *Host) recordMatch(r commandRecord) {
	phrase, mode, conf, worst := r.Phrase, r.Mode, r.Conf, r.Worst
	action := ""
	if len(r.Actions) > 0 {
		action = r.Actions[0]
	}
	// Durable match record, when the user has recording on. A number field is
	// omitted (not NaN) when the engine carried no per-word signal, so
	// analysis distinguishes "no margin" from a real value. Append introduces
	// a keyed record and returns its id (the key the flag annotates).
	id, logged := "", false
	if h.recognitionLogOn() {
		payload := recordPayload(phrase, action, mode, "", conf, worst)
		rid, err := h.plugin.Append(recognitionLogName, payload)
		switch {
		case errors.Is(err, branchkit.ErrRecordingDisabled):
			h.noteRecognitionLogOff()
		case err != nil:
			branchkit.Logf("voice", "recognition_log: append failed: %v", err)
		default:
			id, logged = rid, true
		}
	}
	if id == "" {
		id = strconv.FormatInt(time.Now().UnixNano(), 10) // ring-local fallback id
	}

	h.historyMu.Lock()
	entry := historyEntry{
		ID: id, At: time.Now(), Phrase: phrase, Action: action,
		Mode: mode, Conf: conf, Worst: worst, Logged: logged,
		Trace: r.Trace, Actions: r.Actions, Stop: r.Stop,
		Unresolved: strings.Join(r.Unresolved, " "),
	}
	claimed := false
	if p, ok := h.takePendingLocked(r.Trace); ok {
		p.applyTo(&entry)
		claimed = true
	}
	h.historyEntries = append(h.historyEntries, entry)
	if len(h.historyEntries) > historyCap {
		h.historyEntries = h.historyEntries[len(h.historyEntries)-historyCap:]
	}
	h.historyMu.Unlock()

	if claimed {
		h.announceFailure(entry)
	}
	if h.hud.IsHudOpen() && h.hud.CurrentMainMode() == "history" {
		h.hud.PushMainHUD()
	}
}

// recordUnresolved shows a command that was heard and resolved to nothing
// (a miss or a tie) in the History panel: the "I said it and nothing
// happened" case, which used to leave no row at all. Ring only: the
// recognition_log is the population of commands that fired.
func (h *Host) recordUnresolved(r commandRecord) {
	h.historyMu.Lock()
	h.historyEntries = append(h.historyEntries, historyEntry{
		ID: strconv.FormatInt(time.Now().UnixNano(), 10), At: time.Now(),
		Phrase: r.Phrase, Mode: r.Mode, Conf: r.Conf, Worst: r.Worst,
		Trace: r.Trace, Stop: r.Stop, Unresolved: strings.Join(r.Unresolved, " "),
	})
	if len(h.historyEntries) > historyCap {
		h.historyEntries = h.historyEntries[len(h.historyEntries)-historyCap:]
	}
	h.historyMu.Unlock()
	if h.hud.IsHudOpen() && h.hud.CurrentMainMode() == "history" {
		h.hud.PushMainHUD()
	}
}

// pendingReport is a platform report waiting for the row it belongs to.
type pendingReport struct {
	trace, outcome, reason string
	results                []actionResult
	at                     time.Time
}

// pendingCap and pendingTTL bound reports no row ever claims (a turn voice
// did not record, such as a miss).
const (
	pendingCap = 8
	pendingTTL = time.Minute
)

func (p pendingReport) applyTo(e *historyEntry) {
	if p.outcome == "dropped" {
		e.Dropped = p.reason
		if e.Dropped == "" {
			e.Dropped = "dropped"
		}
		return
	}
	e.Results = p.results
}

// takePendingLocked removes and returns the report waiting for trace.
// Caller holds historyMu.
func (h *Host) takePendingLocked(trace string) (pendingReport, bool) {
	if trace == "" {
		return pendingReport{}, false
	}
	for i, p := range h.pendingExecuted {
		if p.trace == trace {
			h.pendingExecuted = append(h.pendingExecuted[:i], h.pendingExecuted[i+1:]...)
			return p, true
		}
	}
	return pendingReport{}, false
}

// applyExecuted joins the platform's report of what a turn's actions did to
// the History entry that turn recorded. Usually the entry is already there:
// the platform runs the actions after voice answers. A drop is the
// exception: the platform gives up while voice is still deciding, so the
// report arrives first and waits (pendingExecuted) for recordMatch to claim
// it. Returns whether it joined a row now.
func (h *Host) applyExecuted(trace, outcome, reason string, results []actionResult) bool {
	_, found := h.joinExecuted(trace, outcome, reason, results)
	return found
}

// joinExecuted is applyExecuted returning a copy of the row it joined.
func (h *Host) joinExecuted(trace, outcome, reason string, results []actionResult) (historyEntry, bool) {
	if trace == "" {
		return historyEntry{}, false
	}
	report := pendingReport{trace: trace, outcome: outcome, reason: reason, results: results, at: time.Now()}
	h.historyMu.Lock()
	found := false
	var joined historyEntry
	for i := len(h.historyEntries) - 1; i >= 0; i-- {
		e := &h.historyEntries[i]
		if e.Trace != trace || len(e.Actions) == 0 {
			continue
		}
		report.applyTo(e)
		joined = *e
		found = true
		break
	}
	if !found {
		kept := h.pendingExecuted[:0]
		for _, p := range h.pendingExecuted {
			if time.Since(p.at) < pendingTTL {
				kept = append(kept, p)
			}
		}
		h.pendingExecuted = append(kept, report)
		if len(h.pendingExecuted) > pendingCap {
			h.pendingExecuted = h.pendingExecuted[len(h.pendingExecuted)-pendingCap:]
		}
	}
	h.historyMu.Unlock()
	if found && h.hud.IsHudOpen() && h.hud.CurrentMainMode() == "history" {
		h.hud.PushMainHUD()
	}
	return joined, found
}

// handlePipelineExecuted takes the platform's `_platform.pipeline.executed`
// report for a command turn and records it on that turn's History row.
func (h *Host) handlePipelineExecuted(p branchkit.PipelineExecutedEventParams) {
	if p.Pipeline != "command_recognition" {
		return
	}
	results := make([]actionResult, 0, len(p.Actions))
	failed := 0
	for _, a := range p.Actions {
		results = append(results, actionResult{ActionType: a.ActionType, Status: a.Status,
			Handler: derefString(a.Handler), Denial: derefString(a.Denial)})
		if !settled(a.Status) {
			failed++
		}
	}
	trace := h.traceID()
	entry, found := h.joinExecuted(trace, p.Outcome, derefString(p.Reason), results)
	if found {
		h.announceFailure(entry)
	}
	if p.Outcome == "dropped" || failed > 0 {
		branchkit.Logf("voice", "executed: outcome=%s actions=%d failed=%d joined=%v tr=%s",
			p.Outcome, len(results), failed, found, trace)
	}
}

// announceFailure says, in the person's words, that a command they gave did
// not work. The command's words were already said as its
// outcome, before the platform ran it; this follows only when the
// platform's report shows it failed, so a command that worked costs no
// wait and no second announcement.
func (h *Host) announceFailure(e historyEntry) {
	phrase := e.problemPhrase()
	if phrase == "" {
		return
	}
	branchkit.Logf("voice", "command failed: announced (%d words) tr=%s", len(strings.Fields(e.Phrase)), e.Trace)
	// During a hold the release clears the channel, so the release says it,
	// once; saying it now as well would be cut off by that clear and
	// repeated.
	if h.sess.CommandHoldActive() {
		h.sess.SetLastProblem(phrase)
		return
	}
	// Inside a mode the choices are showing; the failure leads their phrase
	// rather than replacing the list (and closing it 6 s later).
	if h.hud.LeadDiscoveryPhrase(phrase) {
		return
	}
	h.reportProblem("Didn't work", phrase)
}

// problemPhrase is the spoken sentence for a failed command, or "" when
// nothing failed.
func (e historyEntry) problemPhrase() string {
	label, _ := e.failure()
	if label == "" {
		return ""
	}
	said := "“" + e.Phrase + "”"
	if e.Dropped != "" {
		return said + " didn't run: " + e.Dropped
	}
	for _, r := range e.Results {
		switch r.Status {
		case "ok", "simulated", "asking", "cancelled":
			continue
		case "denied":
			if r.Denial != "" {
				return said + " was refused: " + r.Denial
			}
			return said + " was refused"
		case "not_handled":
			if r.Handler != "" {
				return said + " was not handled by " + r.Handler
			}
			return said + " was not handled"
		default:
			return said + " failed"
		}
	}
	return ""
}

// settled reports whether an action status is no failure: it ran, it was
// rehearsed, or it is held for the person to say go ahead ("asking") and
// they did not ("cancelled"). The platform holds a spoken command that
// cannot be undone when the person asked it to check with them first.
func settled(status string) bool {
	switch status {
	case "ok", "simulated", "asking", "cancelled":
		return true
	}
	return false
}

// held is the History row's word for a command waiting on the person, or
// one they cancelled; empty otherwise.
func (e historyEntry) held() (label, title string) {
	for _, r := range e.Results {
		switch r.Status {
		case "asking":
			return "asking", "it can't be undone, so it waits for you to say go ahead"
		case "cancelled":
			return "cancelled", "it was held for you to say go ahead, and did not run"
		}
	}
	return "", ""
}

// failure is the first thing that went wrong after the entry resolved:
// the turn dropped, or an action that did not finish ok. Empty when every
// action ran (or no report has arrived yet).
func (e historyEntry) failure() (label, title string) {
	if e.Dropped != "" {
		return "didn't run", "the platform dropped this turn: " + e.Dropped
	}
	for _, r := range e.Results {
		switch r.Status {
		case "ok", "simulated", "asking", "cancelled":
			continue
		case "denied":
			why := r.Denial
			if why == "" {
				why = "not allowed"
			}
			return "refused", r.ActionType + " was refused: " + why
		case "not_handled":
			who := r.Handler
			if who == "" {
				who = "its plugin"
			}
			return "not handled", r.ActionType + " was not handled by " + who
		default:
			return "failed", r.ActionType + " failed (" + r.Status + ")"
		}
	}
	return "", ""
}

// recordSkipped shows a decode the confidence gate dropped in the History
// panel, so a real command lost to the floor is visible. Ring only.
func (h *Host) recordSkipped(phrase, mode string, worst *float64) {
	h.historyMu.Lock()
	h.historyEntries = append(h.historyEntries, historyEntry{
		ID: strconv.FormatInt(time.Now().UnixNano(), 10), At: time.Now(),
		Phrase: phrase, Mode: mode, Worst: worst, Skipped: true,
	})
	if len(h.historyEntries) > historyCap {
		h.historyEntries = h.historyEntries[len(h.historyEntries)-historyCap:]
	}
	h.historyMu.Unlock()
	if h.hud.IsHudOpen() && h.hud.CurrentMainMode() == "history" {
		h.hud.PushMainHUD()
	}
}

// recordPayload builds a recognition_log match record. verdict is empty at
// introduce time (a later flag annotates it to "wrong" via append_keyed).
// Margins are omitted when nil. The `id` fold key is supplied by the server on
// introduce, not here.
func recordPayload(phrase, action, mode, verdict string, conf, worst *float64) map[string]any {
	p := map[string]any{"phrase": phrase, "action": action, "mode": mode, "verdict": verdict}
	if worst != nil {
		p["worst"] = *worst
	}
	if conf != nil {
		p["mean"] = *conf
	}
	return p
}

// snapshotHistory returns the ring newest-first.
func (h *Host) snapshotHistory() []historyEntry {
	h.historyMu.Lock()
	defer h.historyMu.Unlock()
	out := make([]historyEntry, len(h.historyEntries))
	for i, e := range h.historyEntries {
		out[len(h.historyEntries)-1-i] = e
	}
	return out
}

// markHistoryWrong flags the ring entry with this id as a mis-heard misfire.
// Returns the flagged entry (whose id keys the durable annotation), or nil if
// the id is gone (rolled off the in-memory ring) or already flagged.
func (h *Host) markHistoryWrong(id string) *historyEntry {
	h.historyMu.Lock()
	defer h.historyMu.Unlock()
	for i := range h.historyEntries {
		if h.historyEntries[i].ID == id && !h.historyEntries[i].Wrong {
			h.historyEntries[i].Wrong = true
			e := h.historyEntries[i]
			return &e
		}
	}
	return nil
}

// handleMarkWrong backs the per-row "✗ wrong" button. It flags the in-memory
// entry (strikes the row through) and annotates the durable recognition_log
// record's verdict to "wrong" by appending the SAME key with just the changed
// field (append_keyed — logs are append-only, but a keyed log folds the
// annotation onto the match on a compacted read). The match's margin, recorded
// at introduce time, is preserved by the fold, so the record becomes labeled
// ground truth: `scripts/margin-distribution.py` folds recognition_log into
// confirmed-wrong vs the rest to site the confidence floor.
func (h *Host) handleMarkWrong(req *struct {
	ID string `json:"id"`
}) error {
	e := h.markHistoryWrong(req.ID)
	if e == nil {
		branchkit.Logf("voice", "recognition_log: flag id=%s not found in ring (rolled off or already flagged)", req.ID)
		return nil
	}
	worst, mean := math.NaN(), math.NaN()
	if e.Worst != nil {
		worst = *e.Worst
	}
	if e.Conf != nil {
		mean = *e.Conf
	}
	// Keyed compacted-changelog log: annotate the match's id with only the
	// changed field. The compacted read folds this onto the match record,
	// recovering its margin — no need to duplicate it here. A match that was
	// never written (recording was off) has no record to annotate — its id is
	// ring-local — so the flag strikes the row in memory only.
	durable := "memory only (match not recorded)"
	if e.Logged {
		err := h.plugin.AppendKeyed(recognitionLogName, e.ID, map[string]any{"verdict": "wrong"})
		switch {
		case errors.Is(err, branchkit.ErrRecordingDisabled):
			h.noteRecognitionLogOff()
			durable = "memory only (recording off)"
		case err != nil:
			durable = "annotate failed"
			branchkit.Logf("voice", "recognition_log: annotate wrong (id=%s) failed: %v", e.ID, err)
		default:
			durable = "annotated"
		}
	}
	// The record id and a word count, not the phrase: what the user said
	// stays out of the log. With recording on, the id is the
	// phrase's key in the recognition_log.
	branchkit.Logf("voice", "recognition_log: flagged WRONG id=%s words=%d action=%s mode=%s worst=%.2f mean=%.2f (%s)",
		e.ID, len(strings.Fields(e.Phrase)), e.Action, e.Mode, worst, mean, durable)

	if h.hud.IsHudOpen() && h.hud.CurrentMainMode() == "history" {
		h.hud.PushMainHUD()
	}
	return nil
}

// actionTypeLabel pulls a dotted action type out of a resolved action, for
// the History panel's one-line label. Plugin actions carry it in
// `ActionType`; anything else is named by its variant tag.
//
// This took a `json.RawMessage` and unmarshalled into an anonymous struct
// until 2026-09-19, when `commands.resolve` and the `on_transcript` reply
// became typed.
func actionTypeLabel(a branchkit.Action) string {
	if a.Type == branchkit.ActionKindPlugin && a.ActionType != nil && *a.ActionType != "" {
		return *a.ActionType
	}
	return string(a.Type)
}

// relTime renders how long ago an entry landed, log-style.
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 2*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// marginClass buckets a worst-word margin into a color tier for the panel.
// The mishear tail sits below ~-6 (measured: the "hello"→"hi" case was -8.79,
// real commands bottomed near -1 to -2); numbers are display heuristics, not
// the eventual gate, which lands against the labeled distribution.
func marginTier(worst *float64) (cls, glyph string) {
	if worst == nil {
		return "hist-unk", "·"
	}
	switch {
	case *worst >= 0:
		return "hist-strong", "●"
	case *worst >= -3:
		return "hist-ok", "●"
	case *worst >= -6:
		return "hist-weak", "◐"
	default:
		return "hist-low", "○"
	}
}

// renderHistoryHTML builds the History panel body: a vertical list of recent
// matches, each showing the phrase, what it did, and a color-coded confidence
// margin. Rendered into the main HUD's content area.
func (h *Host) renderHistoryHTML() string {
	entries := h.snapshotHistory()
	var b []byte
	buf := func(s string) { b = append(b, s...) }

	buf(`<style>` +
		`.hist{display:flex;flex-direction:column;gap:2px;min-width:300px;font-size:12px}` +
		`.hist-row{display:flex;align-items:baseline;gap:10px;padding:5px 8px;border-radius:7px}` +
		`.hist-row:hover{background:rgba(255,255,255,0.05)}` +
		`.hist-wrong{opacity:0.5;text-decoration:line-through}` +
		`.hist-phrase{flex:1;font-weight:500;color:#fff;white-space:nowrap}` +
		`.hist-action{color:rgba(255,255,255,0.4);font-size:11px;white-space:nowrap}` +
		`.hist-time{color:rgba(255,255,255,0.3);font-size:10px;font-variant-numeric:tabular-nums;min-width:30px;text-align:right}` +
		`.hist-conf{font-variant-numeric:tabular-nums;font-size:11px;min-width:44px;text-align:right}` +
		`.hist-flag{background:transparent;border:none;color:rgba(255,255,255,0.25);font-size:12px;cursor:pointer;padding:0 2px;width:18px;text-align:center;font-family:inherit}` +
		`.hist-flag:hover{color:#ff453a}` +
		`.hist-flagged{color:#ff453a;font-size:11px;width:18px;text-align:center}` +
		`.hist-dot{font-size:10px;width:12px;text-align:center}` +
		`.hist-strong{color:#30d158}.hist-ok{color:#9be89b}` +
		`.hist-weak{color:#ffd60a}.hist-low{color:#ff453a}.hist-unk{color:rgba(255,255,255,0.3)}` +
		`.hist-empty{color:rgba(255,255,255,0.4);padding:8px;font-style:italic}` +
		`.hist-gate{color:#ffd60a;font-size:10px;white-space:nowrap}` +
		`.hist-skipped{opacity:0.6}` +
		`.hist-fail{color:#ff453a;font-size:10px;white-space:nowrap}` +
		`</style>`)

	buf(`<div class="hist">`)
	if len(entries) == 0 {
		buf(`<div class="hist-empty">No commands yet — speak a command and it appears here with its confidence.</div>`)
		buf(`</div>`)
		return string(b)
	}
	cfg := h.config.Get()
	for _, e := range entries {
		cls, glyph := marginTier(e.Worst)
		rowCls := "hist-row"
		if e.Wrong {
			rowCls += " hist-wrong"
		}
		if e.Skipped {
			rowCls += " hist-skipped"
		}
		conf := "—"
		if e.Worst != nil {
			conf = fmt.Sprintf("%+.1f", *e.Worst)
		}
		buf(`<div class="` + rowCls + `">`)
		buf(`<span class="hist-dot ` + cls + `">` + glyph + `</span>`)
		buf(`<span class="hist-phrase">` + html.EscapeString(e.Phrase) + `</span>`)
		if len(e.Actions) > 1 {
			buf(`<span class="hist-action">` + html.EscapeString(strings.Join(e.Actions, ", ")) + `</span>`)
		} else if e.Action != "" {
			buf(`<span class="hist-action">` + html.EscapeString(e.Action) + `</span>`)
		}
		if label, title := e.failure(); label != "" {
			buf(`<span class="hist-fail" title="` + html.EscapeString(title) + `">` + label + `</span>`)
		}
		if label, title := e.held(); label != "" {
			buf(`<span class="hist-gate" title="` + html.EscapeString(title) + `">` + label + `</span>`)
		}
		switch {
		case len(e.Actions) == 0 && e.Stop == "tie":
			buf(`<span class="hist-gate" title="it matched more than one command equally, so it asked which">ambiguous</span>`)
		case len(e.Actions) == 0 && e.Stop == "blocked":
			buf(`<span class="hist-gate" title="a command matched, but it is switched off; the card said why">switched off</span>`)
		case len(e.Actions) == 0 && e.Stop != "":
			buf(`<span class="hist-gate" title="no command matches these words right now">no match</span>`)
		case e.Unresolved != "":
			buf(`<span class="hist-gate" title="` + html.EscapeString("the rest was not a command: "+e.Unresolved) + `">+ unmatched</span>`)
		}
		switch {
		case e.Skipped:
			buf(`<span class="hist-gate" title="below the confidence floor, so it did not run">skipped</span>`)
		case e.Worst != nil && *e.Worst < cfg.CommandConfidenceFloor && cfg.CommandConfidenceGate == "shadow":
			buf(`<span class="hist-gate" title="below the confidence floor: the gate in enforce mode would skip it">below floor</span>`)
		}
		buf(`<span class="hist-time">` + relTime(e.At) + `</span>`)
		buf(`<span class="hist-conf ` + cls + `">` + conf + `</span>`)
		if e.Wrong {
			buf(`<span class="hist-flagged" title="flagged as a mishear">✗</span>`)
		} else if !e.Skipped && len(e.Actions) > 0 {
			// `MethodPost` builds the Datastar expression. This used to
			// interpolate by hand inside a RAW string, so the literal text
			// `" + branchkit.MethodURL("mark_wrong") + "` was emitted into the
			// attribute — the button rendered and its handler was syntactically
			// broken JavaScript, so flagging a mishear silently did nothing.
			buf(fmt.Sprintf(`<button class="hist-flag" title="that was wrong — flag this as a mishear" `+
				`data-on:click="%s">✗</button>`,
				html.EscapeString(branchkit.MethodPost("mark_wrong",
					fmt.Sprintf("{id: '%s'}", jsEscapeID(e.ID))))))
		}
		buf(`</div>`)
	}
	buf(`</div>`)
	return string(b)
}

// jsEscapeID makes a record id safe inside a JavaScript object literal.
// Ids are minted by SimpleID and are hex/dashes today, but an inline
// expression built from a value is exactly where that assumption stops being
// true quietly.
func jsEscapeID(id string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `"`, `\"`, "\n", "", "\r", "")
	return r.Replace(id)
}
