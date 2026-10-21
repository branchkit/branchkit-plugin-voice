package main

// Vocabulary candidate ledger + codeword-lab report surface.
//
// The codeword-lab harness (`just codeword-lab --letter b`) writes per-slot candidate
// reports into THIS plugin's data namespace, and this plugin reads them with
// ordinary file calls, rendering them on the Phonetics tab merged with the
// candidate LEDGER: `plugin.voice.vocab_candidates`, one record per
// (slot, word) the user decided on.
//
// They used to live at a platform-named app-support path behind two platform
// RPCs, because this process could not read app-support. The reports are an
// artifact one dev harness produces FOR this plugin, so they live where this
// plugin's data lives. The harness's own
// working state — fetched wordlists, the TTS cache, difficulty.json — stays
// under app-support: that is the harness's, not ours.
//
// The ledger records decisions about WORDS (adopt/reject/retire + why);
// calibration dispositions stay ground truth about UTTERANCES. Adopting an
// alphabet candidate goes through applyPhoneticOverride — the same rename
// path as the Phonetics-tab edit box — and stamps the outgoing codeword
// `retired` so "what was B before, and why?" stays answerable.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/views"
)

const vocabCandidatesStore = "plugin.voice.vocab_candidates"

// CandidateLedgerRecord is one decision row. Record ID = "<slot>:<word>"
// (mirrored in the `id` field — the collection's by_field identity).
type CandidateLedgerRecord struct {
	ID     string `json:"id"`
	Slot   string `json:"slot"`
	Word   string `json:"word"`
	Status string `json:"status"` // adopted | rejected | retired
	Reason string `json:"reason,omitempty"`
	Ts     int64  `json:"ts,omitempty"` // decision time, unix seconds
}

func candidateRecordID(slot, word string) string { return slot + ":" + word }

func (h *Host) loadCandidateLedger() map[string]CandidateLedgerRecord {
	out := map[string]CandidateLedgerRecord{}
	resp, err := h.plugin.CollectionList(branchkit.CollectionListRequest{Name: vocabCandidatesStore})
	if err != nil {
		branchkit.Logf("voice", "candidates: ledger list failed: %v", err)
		return out
	}
	for _, rec := range resp.Records {
		var r CandidateLedgerRecord
		if err := json.Unmarshal(rec.Payload, &r); err == nil && r.Slot != "" && r.Word != "" {
			out[candidateRecordID(r.Slot, r.Word)] = r
		}
	}
	return out
}

func (h *Host) putCandidateRecord(r CandidateLedgerRecord) error {
	r.ID = candidateRecordID(r.Slot, r.Word)
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = h.plugin.CollectionPut(branchkit.CollectionPutRequest{
		Name:    vocabCandidatesStore,
		Entries: []branchkit.CollectionPutEntry{{ID: candidateRecordID(r.Slot, r.Word), Payload: payload}},
	})
	return err
}

// --- harness report access (via platform ops) ---

type labRendition struct {
	Voice string `json:"voice"`
	Rate  int    `json:"rate"`
	Heard string `json:"heard"`
	Pass  bool   `json:"pass"`
}

type labCandidate struct {
	Word       string         `json:"word"`
	Rank       int            `json:"rank"`
	Penalty    float64        `json:"penalty"`
	Nearest    [][]any        `json:"nearest"` // [word, distance] pairs
	Renditions []labRendition `json:"renditions"`
	AllPass    bool           `json:"all_pass"`
	CleanPass  bool           `json:"clean_pass"`
	Confusers  []string       `json:"confusers"`
}

type labReport struct {
	Slot          string         `json:"slot"`
	GeneratedUnix int64          `json:"generated_unix"`
	Tested        int            `json:"tested"`
	CleanPass     int            `json:"clean_pass"`
	Candidates    []labCandidate `json:"candidates"`
}

// LabReportsDir is where the codeword-lab harness deposits its per-slot
// candidate reports: this plugin's own data namespace. Named here and read by
// the harness through the same convention, so there is one spelling.
func LabReportsDir() string {
	data := branchkit.PluginDataDir()
	if data == "" {
		return ""
	}
	return filepath.Join(data, "codeword-lab", "reports")
}

// loadLabReports reads every slot report on disk, newest first.
//
// A missing directory is "the harness has not run yet", which is the normal
// state — not an error, and not worth a log line on every render.
func loadLabReports() []labReport {
	dir := LabReportsDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type dated struct {
		rep labReport
		mod int64
	}
	var found []dated
	for _, e := range entries {
		slot := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || slot == e.Name() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rep labReport
		if err := json.Unmarshal(body, &rep); err != nil {
			branchkit.Logf("voice", "candidates: report %s unparseable: %v", slot, err)
			continue
		}
		if rep.Slot == "" {
			rep.Slot = slot
		}
		var mod int64
		if info, err := e.Info(); err == nil {
			mod = info.ModTime().Unix()
		}
		found = append(found, dated{rep, mod})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mod > found[j].mod })
	out := make([]labReport, 0, len(found))
	for _, f := range found {
		out = append(out, f.rep)
	}
	return out
}

// --- decision action ---

// CandidateDecisionRequest is the typed payload for the candidates card
// buttons: adopt / reject (with reason) / clear (undo a decision).
type CandidateDecisionRequest struct {
	Slot     string `json:"slot"`
	Word     string `json:"word"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (h *Host) handleCandidateDecision(req *CandidateDecisionRequest) error {
	slot := strings.ToLower(strings.TrimSpace(req.Slot))
	word := strings.ToLower(strings.TrimSpace(req.Word))
	if slot == "" || word == "" {
		return fmt.Errorf("slot and word required")
	}
	now := time.Now().Unix()
	switch req.Decision {
	case "adopt":
		// Alphabet slots rewire the live codeword through the same path as
		// the Phonetics edit box; the outgoing word is stamped retired.
		if letter, ok := strings.CutPrefix(slot, "alphabet."); ok && len(letter) == 1 {
			prev, hadPrev := h.currentCodewordForLetter(letter)
			if err := h.applyPhoneticOverride(letter, word); err != nil {
				return err
			}
			if hadPrev && prev != word {
				if err := h.putCandidateRecord(CandidateLedgerRecord{
					Slot: slot, Word: prev, Status: "retired",
					Reason: "superseded by " + word, Ts: now,
				}); err != nil {
					branchkit.Logf("voice", "candidates: retire record failed: %v", err)
				}
			}
		}
		if err := h.putCandidateRecord(CandidateLedgerRecord{
			Slot: slot, Word: word, Status: "adopted", Ts: now,
		}); err != nil {
			return err
		}
	case "reject":
		if err := h.putCandidateRecord(CandidateLedgerRecord{
			Slot: slot, Word: word, Status: "rejected",
			Reason: strings.TrimSpace(req.Reason), Ts: now,
		}); err != nil {
			return err
		}
	case "clear":
		if _, err := h.plugin.CollectionDeleteRecords(branchkit.CollectionDeleteRecordsRequest{
			Name: vocabCandidatesStore,
			Ids:  []string{candidateRecordID(slot, word)},
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown decision %q", req.Decision)
	}
	return nil
}

// --- render ---

func (h *Host) buildCandidateSlots() []views.CandidateSlotView {
	reports := loadLabReports()
	if len(reports) == 0 {
		return nil
	}
	ledger := h.loadCandidateLedger()

	var slots []views.CandidateSlotView
	for _, rep := range reports {
		sv := views.CandidateSlotView{
			Slot: rep.Slot,
			When: candidateWhen(rep.GeneratedUnix),
		}
		if letter, ok := strings.CutPrefix(rep.Slot, "alphabet."); ok && len(letter) == 1 {
			sv.Letter = strings.ToUpper(letter)
			if cur, ok := h.currentCodewordForLetter(letter); ok {
				sv.Current = cur
			}
		}
		for _, c := range rep.Candidates {
			marks := make([]byte, 0, len(c.Renditions))
			for _, r := range c.Renditions {
				if r.Pass {
					marks = append(marks, 'Y')
				} else {
					marks = append(marks, 'x')
				}
			}
			verdict, class := "fail", "score-red"
			switch {
			case c.AllPass:
				verdict, class = "pass", "score-green"
			case c.CleanPass:
				verdict, class = "pass*", "score-yellow"
			}
			var nearest []string
			for _, pair := range c.Nearest {
				if len(pair) == 2 {
					if w, ok := pair[0].(string); ok {
						nearest = append(nearest, w)
					}
				}
			}
			row := views.CandidateRowView{
				Slot: rep.Slot, Word: c.Word,
				Marks: string(marks), Verdict: verdict, VerdictClass: class,
				Nearest:   strings.Join(nearest, ", "),
				Confusers: strings.Join(c.Confusers, ", "),
			}
			if rec, ok := ledger[candidateRecordID(rep.Slot, c.Word)]; ok {
				row.Status = rec.Status
				row.Reason = rec.Reason
			}
			// Adopted-elsewhere: the slot's current codeword may itself carry
			// a ledger row; candidate rows only need their own status.
			sv.Rows = append(sv.Rows, row)
		}
		// Ledger rows for this slot whose word is no longer in the report
		// (retired codewords, rejects from earlier runs) — history stays
		// visible so a rejected word is recognizably already-tried.
		inReport := map[string]bool{}
		for _, c := range rep.Candidates {
			inReport[c.Word] = true
		}
		var history []views.CandidateRowView
		for _, rec := range ledger {
			if rec.Slot == rep.Slot && !inReport[rec.Word] {
				history = append(history, views.CandidateRowView{
					Slot: rep.Slot, Word: rec.Word,
					Status: rec.Status, Reason: rec.Reason,
					HistoryOnly: true,
				})
			}
		}
		sort.Slice(history, func(i, j int) bool { return history[i].Word < history[j].Word })
		sv.Rows = append(sv.Rows, history...)
		slots = append(slots, sv)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Slot < slots[j].Slot })
	return slots
}

func candidateWhen(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).Format("Jan 2 15:04")
}
