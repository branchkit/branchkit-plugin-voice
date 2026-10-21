package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	"github.com/branchkit/branchkit-plugin-voice/compare"
	branchkit "github.com/branchkit/plugin-sdk-go"
)

// Comparing command models on the user's own voice: every
// confirmed recording re-decoded by every downloaded command model, through
// the live structured grammar, offline in the platform's probe (the live
// engine is untouched). Verdicts are stored per (model, model version,
// grammar, clip), so a run after a new recording or a new model decodes only
// what is missing, and a grammar change starts the comparison afresh.

// compareBatch bounds one re-decode call: each call loads the model and
// builds the grammar once, then decodes its clips in turn.
const compareBatch = 50

// compareStore keeps the verdicts beside the capture folder, not in it:
// clearing all recordings wipes calibration-capture/ (calibration.ClearAll),
// and a verdict for a clip that is gone is simply never read again.
func compareStore() compare.Store {
	return compare.Store{Path: filepath.Join(branchkit.PluginDataDir(), "model-compare.jsonl")}
}

// confirmedClips are the recordings the user listened to and confirmed, with
// what they said: the comparison's ground truth.
func confirmedClips() []compare.Clip {
	var out []compare.Clip
	for _, r := range calibration.ExportableRows(calibration.ReadCorpus(), true) {
		out = append(out, compare.Clip{File: r.File, Audio: calibration.DataRelPath(r.File), Target: r.Target})
	}
	return out
}

// SkippedClip is a confirmed recording no model could decode under the
// current grammar (its words were renamed or retired), left out of every total.
type SkippedClip struct {
	Clip   compare.Clip `json:"clip"`
	Reason string       `json:"reason"`
}

// CompareReport is one comparison: totals per downloaded command model, the
// clips the models heard differently, and the clips skipped.
type CompareReport struct {
	Models        []compare.Summary      `json:"models"`
	Disagreements []compare.Disagreement `json:"disagreements"`
	Skipped       []SkippedClip          `json:"skipped"`
	Clips         int                    `json:"clips"` // confirmed recordings considered
}

// compareAttempts bounds how often a comparison restarts because the live
// grammar changed under it.
const compareAttempts = 3

// compareCommandModels runs (or completes) the comparison and reports it.
// One at a time: a second call while one runs is refused rather than queued.
// The live grammar changes as plugins push vocabulary; when it changes during
// a run, the run starts again on the new grammar, so a report only ever sets
// models decoded under one grammar side by side.
func (h *Host) compareCommandModels(progress func(string)) (CompareReport, error) {
	if !h.compareMu.TryLock() {
		return CompareReport{}, fmt.Errorf("a comparison is already running")
	}
	defer h.compareMu.Unlock()
	for attempt := 1; attempt <= compareAttempts; attempt++ {
		report, moved, err := h.compareOnce(progress)
		if err != nil || !moved {
			return report, err
		}
		branchkit.Logf("voice", "compare: the command grammar changed during the run (attempt %d); starting again", attempt)
	}
	return CompareReport{}, fmt.Errorf("the command grammar kept changing during the comparison; try again")
}

// compareOnce is one pass under the grammar live at its start. moved reports
// that a re-decode ran under a different grammar: its verdicts are stored
// under the grammar they really used, and the pass is abandoned.
func (h *Host) compareOnce(progress func(string)) (report CompareReport, moved bool, err error) {
	clips := confirmedClips()
	report = CompareReport{Clips: len(clips)}
	full := true
	grammar, err := h.plugin.PipelinesGrammar(branchkit.PipelinesGrammarRequest{Full: &full})
	if err != nil {
		return report, false, fmt.Errorf("live grammar: %w", err)
	}
	acceptor, haveDAG := compare.NewAcceptor(grammar.VocabularyUpdate)
	// The platform's version of the structured grammar, the one each re-decode
	// reports back. Without a structured grammar (the platform's toggle off)
	// re-decodes use the flat union and report none; the export's own hash
	// then stands in.
	grammarID := compare.GrammarID(grammar.VocabularyUpdate)
	if grammar.GrammarVersion != nil && *grammar.GrammarVersion != "" {
		grammarID = *grammar.GrammarVersion
	}
	var eligible []compare.Clip
	for _, c := range clips {
		if reason := acceptor.SkipReason(c.Target, haveDAG); reason != "" {
			report.Skipped = append(report.Skipped, SkippedClip{Clip: c, Reason: reason})
			continue
		}
		eligible = append(eligible, c)
	}

	store := compareStore()
	have := store.Load()
	versions := map[string]string{}
	for _, m := range commandModels {
		if !modelOnDisk(m.Name) {
			continue
		}
		version, err := h.redecodeVersion(m.Name)
		if err != nil {
			return report, false, fmt.Errorf("%s: %w", m.Name, err)
		}
		missing := compare.Missing(eligible, m.Name, version, grammarID, have)
		for start := 0; start < len(missing); start += compareBatch {
			end := min(start+compareBatch, len(missing))
			progress(fmt.Sprintf("Listening with %s: %d of %d recordings", m.Label, start, len(missing)))
			results, err := h.redecodeClips(m.Name, version, grammarID, missing[start:end])
			if err != nil {
				return report, false, fmt.Errorf("%s: %w", m.Name, err)
			}
			if err := store.Append(results); err != nil {
				branchkit.Logf("voice", "compare: store: %v", err)
			}
			for _, r := range results {
				have[resultKey(r)] = r
				if r.Grammar != grammarID {
					moved = true
				}
			}
			if moved {
				return report, true, nil
			}
		}
		versions[m.Name] = version
		report.Models = append(report.Models, compare.Summarize(eligible, m.Name, version, grammarID, have))
	}
	report.Disagreements = compare.Disagreements(eligible, versions, grammarID, have)
	return report, false, nil
}

// resultKey mirrors the store's key so freshly decoded verdicts join the
// loaded ones.
func resultKey(r compare.Result) string {
	return compare.Key(r.Model, r.Version, r.Grammar, r.File)
}

// redecodeVersion is the model file's version (an empty re-decode spawns
// nothing and returns only that).
func (h *Host) redecodeVersion(model string) (string, error) {
	resp, err := h.plugin.RecognitionRedecode(branchkit.RecognitionRedecodeRequest{
		Model: model, Stage: calibration.ProbeStage, Items: []branchkit.RedecodeItem{},
	})
	if err != nil {
		return "", err
	}
	if resp.ModelVersion == nil {
		return "", nil
	}
	return *resp.ModelVersion, nil
}

// redecodeClips decodes clips with model through the live structured grammar
// and returns their verdicts, each stamped with the grammar the platform says
// it decoded under (expected stands in when it reports none). A clip the probe
// returned no line for is recorded as failed, so it is not decoded again and
// again.
func (h *Host) redecodeClips(model, version, expected string, clips []compare.Clip) ([]compare.Result, error) {
	structured := true
	items := make([]branchkit.RedecodeItem, len(clips))
	for i, c := range clips {
		items[i] = branchkit.RedecodeItem{ID: c.File, Audio: c.Audio, Structured: &structured}
	}
	// The default call timeout (10 s) is for quick calls; a batch loads the
	// model, builds the grammar and decodes up to compareBatch clips.
	timeout := 60*time.Second + time.Duration(len(clips))*2*time.Second
	resp, err := h.plugin.WithTimeout(timeout).RecognitionRedecode(branchkit.RecognitionRedecodeRequest{
		Model: model, Stage: calibration.ProbeStage, Items: items,
	})
	if err != nil {
		return nil, err
	}
	lines := map[string]branchkit.RedecodeLine{}
	for _, l := range resp.Lines {
		lines[l.ID] = l
	}
	grammarID := expected
	if resp.GrammarVersion != nil && *resp.GrammarVersion != "" {
		grammarID = *resp.GrammarVersion
	}
	now := time.Now().Unix()
	out := make([]compare.Result, 0, len(clips))
	for _, c := range clips {
		r := compare.Result{Model: model, Version: version, Grammar: grammarID, File: c.File, Target: c.Target, At: now}
		l, ok := lines[c.File]
		switch {
		case !ok:
			r.Error = "no result from the recognizer"
		case l.Error != "":
			r.Error = l.Error
		default:
			r.Heard = l.Text
			r.Right = compare.Right(l.Text, c.Target)
			if l.DecodeMs != nil {
				r.DecodeMs = *l.DecodeMs
			}
			r.Weakest = compare.Weakest(l.WordMargins)
		}
		out = append(out, r)
	}
	return out, nil
}

// compareRun is the comparison as the Models tab shows it: running with its
// progress, or its last report or error.
type compareRun struct {
	Running  bool
	Progress string
	Report   *CompareReport
	Err      string
}

// finishedRun is the state a run leaves: its report when it succeeded; when it
// failed, the error beside the last good report (a failed run's report is
// partial or empty, and must not stand in for the results it never got).
func finishedRun(previous *CompareReport, report CompareReport, err error) compareRun {
	if err != nil {
		return compareRun{Report: previous, Err: err.Error()}
	}
	return compareRun{Report: &report}
}

// compareSnapshot is the comparison's state now.
func (h *Host) compareSnapshot() compareRun {
	h.compareStateMu.Lock()
	defer h.compareStateMu.Unlock()
	return h.compareState
}

// handleCommandModelsCompare starts the comparison from the Models tab. A
// settings method must return at once, so it runs in the background and the
// tab re-renders as it reports progress and when it ends; a click while one
// runs does nothing.
func (h *Host) handleCommandModelsCompare(_ *struct{}) error {
	h.compareStateMu.Lock()
	if h.compareState.Running {
		h.compareStateMu.Unlock()
		return nil
	}
	h.compareState = compareRun{Running: true, Progress: "Starting…", Report: h.compareState.Report}
	h.compareStateMu.Unlock()
	go func() {
		report, err := h.compareCommandModels(func(p string) {
			h.compareStateMu.Lock()
			h.compareState.Progress = p
			h.compareStateMu.Unlock()
			_ = h.plugin.SettingsRefresh()
		})
		h.compareStateMu.Lock()
		h.compareState = finishedRun(h.compareState.Report, report, err)
		h.compareStateMu.Unlock()
		if err != nil {
			branchkit.Logf("voice", "compare: %v", err)
		}
		_ = h.plugin.SettingsRefresh()
	}()
	return nil
}
