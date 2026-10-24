package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/compare"
	"github.com/branchkit/branchkit-plugin-voice/views"
)

// renderModels draws the Models tab from the declared command models, what is
// on disk, and the comparison's state.
func (h *Host) renderModels(dl *ModelDownloadData, active json.RawMessage) string {
	onDisk := installedModelSet()
	prefix := h.plugin.ID() + "/"
	declared := map[string]int64{}
	for _, m := range declaredModels() {
		declared[m.Name] = m.SizeBytes
	}
	data := modelsViewData(commandModelsDeclared(declared), onDisk, h.commandModelName(), h.compareSnapshot(), func(m string) string { return h.modelRef(m) }, func(n string) string { return humanBytes(declared[n]) })
	// A command card's license and sources are its artifact's, every part
	// counted (the model's and Silero VAD's).
	byName := map[string]declaredModel{}
	for _, m := range declaredModels() {
		byName[m.Name] = m
	}
	for i := range data.Cards {
		if m, ok := byName[data.Cards[i].Name]; ok && m.License() != "" {
			data.Cards[i].License = m.License()
			data.Cards[i].Source = strings.Join(m.Sources, ", ")
		}
	}
	data.Dictation = dictationCards(dictationCatalog(), onDisk, resolveDictationModel(h.config.Get().Model), h.modelRef)
	var helpers []declaredModel
	for _, m := range catalogForThisOS() {
		if m.Name == refineModel || m.Name == styleModel {
			helpers = append(helpers, m)
		}
	}
	dictKey := activeKey(active, "voice.dictation")
	if dictKey == "" {
		dictKey = "opt+t"
	}
	data.DictationCompare = h.dictationCompareView(dictKey)
	scoreDictationCards(data.Dictation, h.dictationCompareSnapshot().Report)
	data.Helpers = dictationCards(helpers, onDisk, "", h.modelRef)
	for i := range data.Helpers {
		data.Helpers[i].Label = helperLabels[data.Helpers[i].Name]
	}
	armed, pending, sayErr := h.sayNow.snapshot(time.Now())
	key := activeKey(active, "voice.command")
	if key == "" {
		key = "opt+g"
	}
	var downloaded []commandModel
	for _, m := range commandModels {
		if _, ok := onDisk[m.Name]; ok {
			downloaded = append(downloaded, m)
		}
	}
	data.SayNow = sayNowView(armed, key, sayErr, downloaded, pending)
	data.Downloading = ownDownloadRunning(dl, prefix)
	data.OtherDownload = otherPluginDownload(dl, prefix)
	return views.Render(views.ModelsTab(data))
}

// sayNowView is the Say a phrase section: a column per downloaded command
// model, a row per phrase waiting for its answer, offering each distinct thing
// a model heard as the answer.
func sayNowView(armed bool, key, err string, models []commandModel, pending []sayNowClip) views.SayNowView {
	v := views.SayNowView{Armed: armed, Key: key, Error: err}
	for _, m := range models {
		v.Columns = append(v.Columns, m.Label)
	}
	for i, c := range pending {
		row := views.SayNowRow{
			File:     c.File,
			AudioURL: "plugins/voice/data?file=" + url.QueryEscape(c.Audio),
			Signal:   fmt.Sprintf("sayNowOther%d", i),
		}
		seen := map[string]bool{}
		for _, m := range models {
			h := c.Heard[m.Name]
			row.Heard = append(row.Heard, h)
			if h != "" && !seen[h] {
				seen[h] = true
				row.Choices = append(row.Choices, h)
			}
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// helperLabels name the dictation helper models on their cards.
var helperLabels = map[string]string{refineModel: "Clean-up", styleModel: "Styles"}

// dictationCards are cards for models listed with their licenses but not
// compared: the dictation models (inUse names the one dictation loads, which
// is also the chosen one) and the clean-up and style models (inUse "").
func dictationCards(models []declaredModel, onDisk map[string]string, inUse string, ref func(string) string) []views.ModelCard {
	var out []views.ModelCard
	for _, m := range models {
		size, downloaded := onDisk[m.Name]
		f := factsFor(m, size)
		out = append(out, views.ModelCard{
			Name: m.Name, Label: f.Size, Title: m.Description,
			License: m.License(), Source: strings.Join(m.Sources, ", "),
			Bytes: f.Bytes, Ref: ref(m.Name),
			Downloaded: downloaded, InUse: downloaded && m.Name == inUse,
			Chosen: m.Name == inUse,
		})
	}
	return out
}

// commandModelsDeclared is commandModels less any this manifest does not
// declare (nothing to download for those).
func commandModelsDeclared(declared map[string]int64) []commandModel {
	var out []commandModel
	for _, m := range commandModels {
		if _, ok := declared[m.Name]; ok {
			out = append(out, m)
		}
	}
	return out
}

// modelsViewData is the Models tab's data, pure over its inputs so the
// scoring, the best pick and the disagreement columns are testable.
func modelsViewData(models []commandModel, onDisk map[string]string, inUse string, run compareRun, ref, bytes func(string) string) views.ModelsViewData {
	data := views.ModelsViewData{Running: run.Running, Progress: run.Progress, Error: run.Err}
	scores := map[string]compare.Summary{}
	if run.Report != nil {
		data.Compared = true
		data.Clips = run.Report.Clips
		data.Skipped = len(run.Report.Skipped)
		for _, s := range run.Report.Models {
			scores[s.Model] = s
		}
	}
	best := bestModel(run.Report)
	labels := map[string]string{}
	for _, m := range models {
		_, downloaded := onDisk[m.Name]
		c := views.ModelCard{
			Name: m.Name, Label: m.Label, Title: m.Title, License: m.License, Source: m.Source,
			Bytes: bytes(m.Name), Ref: ref(m.Name),
			Downloaded: downloaded, InUse: downloaded && m.Name == inUse, Best: m.Name == best,
		}
		if s, ok := scores[m.Name]; ok && s.Total > 0 {
			c.Scored, c.Right, c.Total = true, s.Right, s.Total
			if s.Total > s.Failed {
				c.MedianMs = fmt.Sprintf("%.0f ms", s.MedianMs)
			}
		}
		data.Cards = append(data.Cards, c)
		labels[m.Name] = m.Label
	}
	if run.Report == nil {
		return data
	}
	// Columns follow the cards, for the models the comparison covered.
	var cols []string
	for _, m := range models {
		if _, ok := scores[m.Name]; ok {
			cols = append(cols, m.Name)
			data.Columns = append(data.Columns, labels[m.Name])
		}
	}
	for _, d := range run.Report.Disagreements {
		row := views.DisagreementRow{
			Said:     compare.Normalize(d.Clip.Target),
			AudioURL: "plugins/voice/data?file=" + url.QueryEscape(d.Clip.Audio),
		}
		for _, m := range cols {
			row.Cells = append(row.Cells, views.HeardCell{Heard: compare.Normalize(d.Heard[m]), Right: d.Right[m]})
		}
		data.Disagreements = append(data.Disagreements, row)
	}
	return data
}

// bestModel is the model that did best on this voice: the most right (as a
// share, the totals can differ while one is still partly run), then the
// fastest. A model none of whose recordings decoded is not scored here: it
// has no time to compare and nothing it heard. "" unless at least two models
// were scored.
func bestModel(r *CompareReport) string {
	if r == nil {
		return ""
	}
	best, scored := "", 0
	bestShare, bestMs := -1.0, math.Inf(1)
	for _, s := range r.Models {
		if s.Total == 0 || s.Failed == s.Total {
			continue
		}
		scored++
		share := float64(s.Right) / float64(s.Total)
		if share > bestShare || (share == bestShare && s.MedianMs < bestMs) {
			best, bestShare, bestMs = s.Model, share, s.MedianMs
		}
	}
	if scored < 2 {
		return ""
	}
	return best
}

// dictationCompareView is the Read a sentence and dictation comparison state
// as the tab draws it.
func (h *Host) dictationCompareView(key string) views.DictationCompareView {
	h.readSentence.mu.Lock()
	v := views.DictationCompareView{
		Armed:     time.Now().Before(h.readSentence.armedUntil),
		Sentence:  h.readSentence.sentence,
		Key:       key,
		ReadError: h.readSentence.err,
	}
	for _, c := range h.readSentence.pending {
		v.Pending = append(v.Pending, views.ReadClipRow{
			File: c.File, AudioURL: dataAudioURL(c.File), Sentence: c.Sentence, Heard: c.Live,
		})
	}
	h.readSentence.mu.Unlock()
	v.Recorded = len(readDictationCorpus())
	run := h.dictationCompareSnapshot()
	v.Running, v.Progress, v.Error = run.Running, run.Progress, run.Err
	fillDictationRows(&v, run.Report, dictationCatalog())
	return v
}

// fillDictationRows adds a report's columns and differing recordings to v,
// columns in catalog order (the cards' order).
func fillDictationRows(v *views.DictationCompareView, r *DictationReport, catalog []declaredModel) {
	if r == nil {
		return
	}
	v.Compared = true
	scored := map[string]bool{}
	for _, s := range r.Models {
		scored[s.Model] = true
	}
	var cols []string
	for _, m := range catalog {
		if scored[m.Name] {
			cols = append(cols, m.Name)
			v.Columns = append(v.Columns, factsFor(m, "").Size)
		}
	}
	for _, d := range r.Disagreements {
		row := views.DictationRow{Sentence: d.Clip.Target, AudioURL: dataAudioURL(d.Clip.Audio)}
		for _, m := range cols {
			errs, _ := compare.WordErrors(d.Clip.Target, d.Heard[m])
			row.Cells = append(row.Cells, views.DictationCell{Heard: d.Heard[m], Errors: errs})
		}
		v.Rows = append(v.Rows, row)
	}
}

// scoreDictationCards puts a dictation report's scores on the cards: words
// right as a share and time per sentence, and the best on this voice (most
// words right, then fastest, once two models are scored; a model whose
// decodes all failed is not eligible).
func scoreDictationCards(cards []views.ModelCard, r *DictationReport) {
	if r == nil {
		return
	}
	byModel := map[string]compare.WordSummary{}
	for _, s := range r.Models {
		byModel[s.Model] = s
	}
	best, bestShare, bestMs, scored := "", -1.0, math.Inf(1), 0
	for _, s := range r.Models {
		if s.Words == 0 || s.Failed == s.Total {
			continue
		}
		scored++
		share := float64(s.Words-s.Errors) / float64(s.Words)
		if share > bestShare || (share == bestShare && s.MedianMs < bestMs) {
			best, bestShare, bestMs = s.Model, share, s.MedianMs
		}
	}
	if scored < 2 {
		best = ""
	}
	for i := range cards {
		s, ok := byModel[cards[i].Name]
		if !ok || s.Words == 0 {
			continue
		}
		cards[i].Scored = true
		cards[i].WordsPct = fmt.Sprintf("%.0f%%", 100*float64(max(s.Words-s.Errors, 0))/float64(s.Words))
		if s.Total > s.Failed {
			cards[i].MedianMs = fmt.Sprintf("%.1f s", s.MedianMs/1000)
		}
		cards[i].Best = cards[i].Name == best
	}
}

// dataAudioURL plays a recording from this plugin's data dir on a tab.
func dataAudioURL(rel string) string {
	return "plugins/voice/data?file=" + url.QueryEscape(rel)
}
