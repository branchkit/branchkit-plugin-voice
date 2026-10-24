package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// "Check it works" is a health check: three things voice can prove on its own,
// with no other plugin installed — the microphone delivers sound, dictation
// types, and a command is heard and matched. A command that does something
// visible (snapping a window) would need another plugin; the check proves
// voice works, not what the rest of the fleet can do.
//
// The microphone check runs on every hold, not only during setup: a hold
// that captured digital silence (the wrong input device, a muted one) turns
// the step back to "needs you" on the Setup page, and the next hold with
// sound clears it.

const (
	checkPass = "pass"
	checkFail = "fail"

	// healthFile records each check's last result, so the step stays done
	// across restarts. Results only, never what was said.
	healthFile = "setup-health.json"

	// A hold shorter than this proves nothing about the microphone.
	minMicHoldMS = 500
	// The last chunks of a hold cross the pipeline bridge just after the
	// release; wait this long before asking how loud the hold was.
	micSettle = 300 * time.Millisecond
	// At or below this the audio is digital silence. A quiet room still
	// measures well above it; zeros (a dead, muted or unpermitted input)
	// read as the platform's floor, -120 dBFS.
	silentAtDB = -100.0

	// How long "Check commands" listens before giving up.
	commandCheckWindow = 2 * time.Minute
	// After a pass, the rest of that hold is still swallowed, so a trailing
	// word cannot run as a command.
	commandCheckGrace = 1500 * time.Millisecond
)

// commandCheckPhrase is what the check asks for: one of voice's own
// commands (it opens the command list), live in every context, so the check
// needs nothing but voice. TestCommandCheckPhraseIsAnUngatedVoiceCommand
// holds it to commands.json.
const commandCheckPhrase = "show help"

// commandCheckAccepts is every wording of that same command.
var commandCheckAccepts = map[string]bool{"show help": true, "show commands": true}

// healthResults is what is persisted: each check's last result, "" for
// never tried.
type healthResults struct {
	Mic       string `json:"mic,omitempty"`
	Dictation string `json:"dictation,omitempty"`
	Commands  string `json:"commands,omitempty"`
}

// setupHealth is the check's live state.
type setupHealth struct {
	mu      sync.Mutex
	loaded  bool
	results healthResults
	// micNote says why the microphone check failed.
	micNote string
	// silentNoticeAt is when the silent-microphone notice last showed.
	silentNoticeAt time.Time
	// The command check: listening until, swallowing frames until (the
	// grace after a pass), and what it last heard.
	listenUntil  time.Time
	swallowUntil time.Time
	heard        string
	// holdStart is when the current hold began (Unix ms), 0 between holds.
	holdStart int64
}

func healthPath() string {
	dir := branchkit.PluginDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, healthFile)
}

// load reads the persisted results once. Callers hold mu.
func (s *setupHealth) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	if p := healthPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &s.results)
		}
	}
}

// save writes the results. Callers hold mu.
func (s *setupHealth) save() {
	p := healthPath()
	if p == "" {
		return
	}
	b, err := json.Marshal(s.results)
	if err != nil {
		return
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		branchkit.Logf("voice", "setup: saving the health check failed: %v", err)
	}
}

// healthView is one snapshot for rendering and reporting.
type healthView struct {
	results   healthResults
	micNote   string
	listening bool
	heard     string
}

func (s *setupHealth) snapshot(now time.Time) healthView {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return healthView{
		results:   s.results,
		micNote:   s.micNote,
		listening: now.Before(s.listenUntil),
		heard:     s.heard,
	}
}

// set records one check's result; false when nothing changed.
func (s *setupHealth) set(field *string, result string) bool {
	if *field == result {
		return false
	}
	*field = result
	s.save()
	return true
}

func (s *setupHealth) recordMic(result, note string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	changed := s.micNote != note
	s.micNote = note
	return s.set(&s.results.Mic, result) || changed
}

func (s *setupHealth) recordDictation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s.set(&s.results.Dictation, checkPass)
}

// startListening arms the command check.
func (s *setupHealth) startListening(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	s.listenUntil = now.Add(commandCheckWindow)
	s.heard = ""
}

// intercepts says whether a command transcript belongs to the check rather
// than to the matcher: while listening, and for the grace after a pass.
func (s *setupHealth) intercepts(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.listenUntil) || now.Before(s.swallowUntil)
}

// heardCommand scores one heard phrase. `matched` is the matcher's preview
// verdict: the phrase resolves to a command, nothing run.
func (s *setupHealth) heardCommand(now time.Time, heard string, matched bool) (passed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if !now.Before(s.listenUntil) {
		return false // the grace after a pass: swallowed, not scored
	}
	s.heard = heard
	if matched && commandCheckAccepts[heard] {
		s.listenUntil = time.Time{}
		s.swallowUntil = now.Add(commandCheckGrace)
		s.set(&s.results.Commands, checkPass)
		return true
	}
	// Not it: keep listening, a little longer.
	s.listenUntil = now.Add(commandCheckWindow)
	return false
}

// micVerdict reads one hold's loudest moment. No audio at all is a failure
// too: nothing reached BranchKit while the key was held.
func micVerdict(peakDB *float64) (result, note string) {
	switch {
	case peakDB == nil:
		return checkFail, "No sound reached BranchKit during your last hold. Check that BranchKit may use the microphone (Permissions, above), then try again."
	case *peakDB <= silentAtDB:
		return checkFail, "Your last hold was silent: BranchKit got no sound at all. Your computer is probably recording from a different microphone than the one you spoke into, or it is muted. Choose your microphone on voice's Get started tab, then try again."
	default:
		return checkPass, ""
	}
}

// tryStep is where "Check it works" stands, for the Setup page. Never tried at
// all: nothing to report (an optional step reads as optional).
func tryStep(r healthResults) (state, summary string, report bool) {
	if r.Mic == checkFail {
		return setupStateNeedsYou, "Your microphone is silent", true
	}
	if r.Mic == "" && r.Dictation == "" && r.Commands == "" {
		return "", "", false
	}
	if r.Mic == checkPass && r.Dictation == checkPass && r.Commands == checkPass {
		return setupStateDone, "Microphone, dictation and commands work", true
	}
	var works, left []string
	for _, c := range []struct{ name, result string }{
		{"microphone", r.Mic},
		{"dictation", r.Dictation},
		{"commands", r.Commands},
	} {
		if c.result == checkPass {
			works = append(works, c.name)
		} else {
			left = append(left, c.name)
		}
	}
	verb := " works"
	if len(works) > 1 {
		verb = " work"
	}
	return setupStateInProgress, capitalize(joinAnd(works)) + verb + " · " + joinAnd(left) + " not tried yet", true
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// --- Wiring --------------------------------------------------------------

// reportTry re-reports the Setup step a check feeds and redraws the tab.
func (h *Host) reportTry() {
	h.reportStart()
	if h.plugin != nil {
		go h.plugin.SettingsRefresh()
	}
}

// markTried is called after a dictation was typed at the cursor.
func (h *Host) markTried() {
	if h.health.recordDictation() {
		h.reportTry()
	}
}

// holdBegan marks the start of a hold that captures audio.
func (h *Host) holdBegan() {
	h.health.mu.Lock()
	h.health.holdStart = time.Now().UnixMilli()
	h.health.mu.Unlock()
	h.warnIfMicrophoneBlocked()
}

// holdEnded asks the platform how loud the hold was and records the
// microphone check.
func (h *Host) holdEnded() {
	h.health.mu.Lock()
	start := h.health.holdStart
	h.health.holdStart = 0
	h.health.mu.Unlock()
	if start == 0 || time.Now().UnixMilli()-start < minMicHoldMS || h.plugin == nil {
		return
	}
	go func() {
		time.Sleep(micSettle)
		since := int(start)
		resp, err := h.plugin.PipelinesAudioLevel(branchkit.PipelinesAudioLevelRequest{SinceMs: &since})
		if err != nil {
			branchkit.Logf("voice", "setup: audio level unavailable: %v", err)
			return
		}
		result, note := micVerdict(resp.PeakDb)
		if result == checkFail {
			branchkit.Logf("voice", "setup: last hold reached no sound (peak %s)", fmtPeak(resp.PeakDb))
			h.noticeSilentMic(note)
		}
		if h.health.recordMic(result, note) {
			h.reportTry()
		}
	}()
}

// silentMicNoticeEvery spaces the silent-microphone notice: a muted or
// wrong input makes every hold silent, and one notice says it.
const silentMicNoticeEvery = 10 * time.Minute

// noticeSilentMic says, outside Setup, that a hold captured digital silence.
// The only cue used to be a flat pill meter, and a silent
// dictation types nothing, so the user saw nothing happen and no reason.
func (h *Host) noticeSilentMic(note string) {
	h.health.mu.Lock()
	if !h.health.silentNoticeAt.IsZero() && time.Since(h.health.silentNoticeAt) < silentMicNoticeEvery {
		h.health.mu.Unlock()
		return
	}
	h.health.silentNoticeAt = time.Now()
	h.health.mu.Unlock()
	if err := h.plugin.SystemNotify(branchkit.SystemNotifyRequest{
		Title: "No sound from your microphone",
		Body:  note,
	}); err != nil {
		branchkit.Logf("voice", "silent-mic notice failed: %v", err)
	}
}

func fmtPeak(p *float64) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprintf("%.1f dB", *p)
}

// checkCommandTranscript scores a command hold's transcript while the
// check listens. Nothing is run: the matcher is asked in preview. Every
// decoded segment arrives as a non-final frame and the hold ends with an
// empty final marker, so any frame with words is a whole segment.
func (h *Host) checkCommandTranscript(text string) {
	heard := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	if heard == "" {
		return
	}
	matched := false
	if h.plugin != nil {
		preview := true
		resp, err := h.plugin.CommandsResolve(branchkit.CommandsResolveRequest{
			Preview: &preview,
			Words:   strings.Fields(heard),
		})
		if err != nil {
			branchkit.Logf("voice", "setup: command check preview failed: %v", err)
		} else {
			matched = resp.Matched
		}
	}
	passed := h.health.heardCommand(time.Now(), heard, matched)
	branchkit.Logf("voice", "setup: command check heard %d words, matched=%v passed=%v", len(strings.Fields(heard)), matched, passed)
	h.reportTry()
}

type setupCheckRequest struct{}

// handleSetupCheck is the tab's "Check commands" button.
func (h *Host) handleSetupCheck(_ *setupCheckRequest) error {
	h.health.startListening(time.Now())
	go h.plugin.SettingsRefresh()
	// Redraw when the window lapses, so the tab stops saying it listens.
	time.AfterFunc(commandCheckWindow+time.Second, func() { _ = h.plugin.SettingsRefresh() })
	return nil
}
