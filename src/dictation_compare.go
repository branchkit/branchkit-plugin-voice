package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	branchkit "github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/compare"
)

// Comparing dictation models on the user's own voice: every
// kept Read a sentence recording re-decoded by every downloaded dictation
// model, offline through the dictation stage's probe, scored by word errors
// against the sentence it reads. Verdicts are stored per (model, model
// version, recording), so a run after a new recording or a new model decodes
// only what is missing. There is no grammar in dictation, so its key is "".

// dictationBatch bounds one re-decode call: a dictation model is large and
// may compile on its first load, so batches are small and timed generously.
const dictationBatch = 10

// dictationStage is this OS's dictation stage, whose probe decodes.
func dictationStage() string {
	if runtime.GOOS == "darwin" {
		return "voice.whisperkit_stt"
	}
	return "voice.whisper_stt"
}

func dictationCompareStore() compare.Store {
	return compare.Store{Path: filepath.Join(branchkit.PluginDataDir(), "dictation-compare.jsonl")}
}

// keptDictationClips are the kept recordings, each with its sentence as the
// target.
func keptDictationClips() []compare.Clip {
	var out []compare.Clip
	for _, r := range readDictationCorpus() {
		out = append(out, compare.Clip{File: r.File, Audio: r.File, Target: r.Sentence})
	}
	return out
}

// DictationReport is one dictation comparison.
type DictationReport struct {
	Models        []compare.WordSummary  `json:"models"`
	Disagreements []compare.Disagreement `json:"disagreements"`
	Clips         int                    `json:"clips"`
}

// dictationRun is the dictation comparison as the Models tab shows it.
type dictationRun struct {
	Running  bool
	Progress string
	Report   *DictationReport
	Err      string
}

func (h *Host) dictationCompareSnapshot() dictationRun {
	h.dictCompareMu.Lock()
	defer h.dictCompareMu.Unlock()
	return h.dictCompare
}

// handleDictationModelsCompare starts the dictation comparison in the
// background; a click while one runs does nothing.
func (h *Host) handleDictationModelsCompare(_ *struct{}) error {
	h.dictCompareMu.Lock()
	if h.dictCompare.Running {
		h.dictCompareMu.Unlock()
		return nil
	}
	h.dictCompare = dictationRun{Running: true, Progress: "Starting…", Report: h.dictCompare.Report}
	h.dictCompareMu.Unlock()
	go func() {
		report, err := h.compareDictationModels(func(p string) {
			h.dictCompareMu.Lock()
			h.dictCompare.Progress = p
			h.dictCompareMu.Unlock()
			_ = h.plugin.SettingsRefresh()
		})
		h.dictCompareMu.Lock()
		if err != nil {
			h.dictCompare = dictationRun{Report: h.dictCompare.Report, Err: err.Error()}
			branchkit.Logf("voice", "dictation compare: %v", err)
		} else {
			h.dictCompare = dictationRun{Report: &report}
		}
		h.dictCompareMu.Unlock()
		_ = h.plugin.SettingsRefresh()
	}()
	return nil
}

// compareDictationModels decodes what is missing and reports.
func (h *Host) compareDictationModels(progress func(string)) (DictationReport, error) {
	clips := keptDictationClips()
	report := DictationReport{Clips: len(clips)}
	store := dictationCompareStore()
	have := store.Load()
	versions := map[string]string{}
	language := h.config.Get().Language
	for _, m := range dictationCatalog() {
		if !modelOnDisk(m.Name) {
			continue
		}
		label := factsFor(m, "").Size
		version, err := h.dictationVersion(m.Name)
		if err != nil {
			return report, fmt.Errorf("%s: %w", label, err)
		}
		missing := compare.Missing(clips, m.Name, version, "", have)
		for start := 0; start < len(missing); start += dictationBatch {
			end := min(start+dictationBatch, len(missing))
			if start == 0 {
				progress(fmt.Sprintf("Preparing %s (its first use can take a minute)", label))
			} else {
				progress(fmt.Sprintf("Listening with %s: %d of %d recordings", label, start, len(missing)))
			}
			results, err := h.redecodeDictation(m.Name, version, language, missing[start:end])
			if err != nil {
				return report, fmt.Errorf("%s: %w", label, err)
			}
			if err := store.Append(results); err != nil {
				branchkit.Logf("voice", "dictation compare: store: %v", err)
			}
			for _, r := range results {
				have[resultKey(r)] = r
			}
		}
		versions[m.Name] = version
		report.Models = append(report.Models, compare.SummarizeWords(clips, m.Name, version, "", have))
	}
	report.Disagreements = compare.DisagreementsBy(clips, versions, "", have, compare.DictationForm)
	return report, nil
}

// dictationVersion is a dictation model's version (an empty re-decode spawns
// nothing).
func (h *Host) dictationVersion(model string) (string, error) {
	resp, err := h.plugin.RecognitionRedecode(branchkit.RecognitionRedecodeRequest{
		Model: model, Stage: dictationStage(), Items: []branchkit.RedecodeItem{},
	})
	if err != nil {
		return "", err
	}
	if resp.ModelVersion == nil {
		return "", nil
	}
	return *resp.ModelVersion, nil
}

// redecodeDictation decodes clips with a dictation model and scores each
// against its sentence.
func (h *Host) redecodeDictation(model, version, language string, clips []compare.Clip) ([]compare.Result, error) {
	items := make([]branchkit.RedecodeItem, len(clips))
	for i, c := range clips {
		items[i] = branchkit.RedecodeItem{ID: c.File, Audio: c.Audio}
	}
	req := branchkit.RecognitionRedecodeRequest{Model: model, Stage: dictationStage(), Items: items}
	if language != "" {
		req.Language = &language
	}
	// A cold Core ML model compiles on its first load (about 45 s measured),
	// then each recording takes about a second on a large model.
	timeout := 3*time.Minute + time.Duration(len(clips))*15*time.Second
	resp, err := h.plugin.WithTimeout(timeout).RecognitionRedecode(req)
	if err != nil {
		return nil, err
	}
	lines := map[string]branchkit.RedecodeLine{}
	for _, l := range resp.Lines {
		lines[l.ID] = l
	}
	now := time.Now().Unix()
	out := make([]compare.Result, 0, len(clips))
	for _, c := range clips {
		errs, words := compare.WordErrors(c.Target, "")
		r := compare.Result{Model: model, Version: version, File: c.File, Target: c.Target, At: now, Errors: errs, Words: words}
		l, ok := lines[c.File]
		switch {
		case !ok:
			r.Error = "no result from the recognizer"
		case l.Error != "":
			r.Error = l.Error
		default:
			r.Heard = l.Text
			r.Errors, r.Words = compare.WordErrors(c.Target, l.Text)
			r.Right = r.Errors == 0
			if l.DecodeMs != nil {
				r.DecodeMs = *l.DecodeMs
			}
		}
		out = append(out, r)
	}
	return out, nil
}
