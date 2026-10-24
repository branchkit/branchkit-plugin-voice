package main

import (
	"encoding/json"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/views"
	"github.com/branchkit/plugin-sdk-go"
)

// Voice's step on the platform's Setup page.
//
// The platform owns the page: where a step sits, the "N of M done" count,
// whether it starts open. Voice owns what the step is about — its body is
// voice's Get started tab (`start` in plugin.json), and voice REPORTS its
// state with `plugin.report_setup` whenever it changes. Reports are dropped
// when this process restarts, so they are sent again at start.
//
// One step, not one per section: each step frames its tab, so two steps on
// one tab would show it twice. The step's state folds the
// sections together in startStep.

const (
	setupStepStart = "start"

	setupStateDone       = "done"
	setupStateNeedsYou   = "needs_you"
	setupStateInProgress = "in_progress"
)

// setupReporter sends a step's state only when it changed since the last
// send. The platform also ignores a repeat, but most of what triggers a
// re-check (every collection update) changes nothing, and a call per update
// is a call for nothing.
type setupReporter struct {
	mu   sync.Mutex
	last map[string][2]string
	send func(step, state, summary string) error
	// models is the models section's last state and summary, so a check
	// that ticks can re-fold the step without asking the platform again.
	models *[2]string
}

func (r *setupReporter) setModels(state, summary string) {
	r.mu.Lock()
	r.models = &[2]string{state, summary}
	r.mu.Unlock()
}

func (r *setupReporter) modelsState() (state, summary string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.models == nil {
		return "", "", false
	}
	return r.models[0], r.models[1], true
}

func (r *setupReporter) report(step, state, summary string) {
	r.mu.Lock()
	if r.last == nil {
		r.last = map[string][2]string{}
	}
	cur := [2]string{state, summary}
	if prev, ok := r.last[step]; ok && prev == cur {
		r.mu.Unlock()
		return
	}
	r.last[step] = cur
	r.mu.Unlock()
	if err := r.send(step, state, summary); err != nil {
		branchkit.Logf("voice", "setup: report %s=%s failed: %v", step, state, err)
		// Forget it, so the next check tries again.
		r.mu.Lock()
		delete(r.last, step)
		r.mu.Unlock()
	}
}

func (h *Host) sendSetupReport(step, state, summary string) error {
	req := branchkit.PluginReportSetupRequest{
		Step:  step,
		State: branchkit.SetupStepState(state),
	}
	if summary != "" {
		req.Summary = &summary
	}
	return h.plugin.PluginReportSetup(req)
}

// modelFacts is what the setup picker says about one dictation model: plain
// facts, no engine names.
type modelFacts struct {
	Size      string // "Large", "Small", "Base"
	Bytes     string // "1.51 GB"
	Languages string // "English" or "Many languages"
	Note      string // "most accurate (Whisper large-v3 turbo)"
}

func factsFor(m declaredModel, onDiskSize string) modelFacts {
	f := modelFacts{Bytes: humanBytes(m.SizeBytes), Languages: "Many languages"}
	if onDiskSize != "" {
		f.Bytes = onDiskSize
	}
	name := strings.ToLower(m.Name)
	switch {
	case strings.Contains(name, "large"):
		f.Size = "Large"
	case strings.Contains(name, "medium"):
		f.Size = "Medium"
	case strings.Contains(name, "small"):
		f.Size = "Small"
	case strings.Contains(name, "base"):
		f.Size = "Base"
	case strings.Contains(name, "tiny"):
		f.Size = "Tiny"
	default:
		f.Size = m.Name
	}
	if strings.HasSuffix(name, ".en") || strings.Contains(strings.ToLower(m.Description), "english") {
		f.Languages = "English"
	}
	// The manifest's description reads "Dictation — most accurate (...)";
	// the part after the dash is the plain fact.
	if i := strings.LastIndex(m.Description, "— "); i >= 0 {
		f.Note = strings.TrimSpace(m.Description[i+len("— "):])
	}
	return f
}

// modelsStep is where "Speech models" stands: downloading, done when the
// dictation model that actually loads is on disk, else waiting on the person.
// downloading is true only for a download of one of this plugin's models;
// otherBusy is another plugin's download running now, which voice's
// downloads wait behind (see otherPluginDownload).
func modelsStep(downloading, otherBusy bool, active string, onDisk map[string]string, catalog []declaredModel) (state, summary string) {
	if downloading {
		return setupStateInProgress, "Downloading…"
	}
	size, ok := onDisk[active]
	if !ok {
		if otherBusy {
			return setupStateNeedsYou, "Another plugin is downloading; download yours when it finishes"
		}
		return setupStateNeedsYou, "Choose a size and download it"
	}
	label := active
	for _, m := range catalog {
		if m.Name == active {
			f := factsFor(m, size)
			label = f.Size + " · " + f.Bytes
		}
	}
	for _, m := range commandModels {
		if _, cmds := onDisk[m.Name]; cmds {
			return setupStateDone, label + " · commands on"
		}
	}
	return setupStateDone, label + " · commands not downloaded"
}

// readModelDownload reads the platform's in-flight download record. A
// missing or unreadable record means nothing is downloading.
func (h *Host) readModelDownload() ModelDownloadData {
	resp, err := h.plugin.CollectionGet(branchkit.CollectionGetRequest{Name: "_platform.model_download"})
	if err != nil || resp == nil {
		return ModelDownloadData{}
	}
	return parseModelDownload(resp.Data)
}

// parseModelDownload accepts the record itself or a map holding it under
// its singleton id.
func parseModelDownload(raw json.RawMessage) ModelDownloadData {
	var md ModelDownloadData
	if json.Unmarshal(raw, &md) == nil && (md.Downloading || md.Progress > 0 || md.Error != "" || len(md.Failures) > 0) {
		return md
	}
	var byID map[string]ModelDownloadData
	if json.Unmarshal(raw, &byID) == nil {
		for _, v := range byID {
			return v
		}
	}
	return md
}

// refreshSetup re-checks the step and reports it if it changed. Called at
// start and whenever something it depends on may have moved: a download
// update, a model chosen or deleted, a check ticked or failed.
func (h *Host) refreshSetup() {
	dl := h.readModelDownload()
	prefix := h.plugin.ID() + "/"
	h.setup.setModels(modelsStep(ownDownloadRunning(&dl, prefix), otherPluginDownload(&dl, prefix) != "", resolveDictationModel(h.config.Get().Model), installedModelSet(), dictationCatalog()))
	h.reportStart()
}

// reportStart folds the last models state with the checks and reports the
// step. Before the models were first read there is nothing to fold.
func (h *Host) reportStart() {
	mState, mSummary, ok := h.setup.modelsState()
	if !ok {
		return
	}
	tState, tSummary, tried := tryStep(h.health.snapshot(time.Now()).results)
	state, summary := startStep(mState, mSummary, tState, tSummary, tried)
	h.setup.report(setupStepStart, state, summary)
}

// startStep folds Get started's sections into the one Setup step: models
// first (nothing works without them), then a failed check (a silent
// microphone needs the person). The checks never hold the step open
// otherwise: they are optional, and they tick by themselves with use.
func startStep(mState, mSummary, tState, tSummary string, tried bool) (state, summary string) {
	if mState != setupStateDone {
		return mState, mSummary
	}
	if tried && tState == setupStateNeedsYou {
		return tState, tSummary
	}
	return mState, mSummary
}

// --- Get started, the step's body -----------------------------------------

func (h *Host) renderGetStarted(dl *ModelDownloadData, active json.RawMessage) string {
	cfg := h.config.Get()
	return views.Render(views.GetStarted(views.GetStartedViewData{
		Mic:    h.micMenuView(cfg.InputDevice, cfg.InputDeviceName),
		Models: h.setupModelsData(dl),
		Try:    h.setupTryData(active),
	}))
}

func (h *Host) setupModelsData(dl *ModelDownloadData) views.SetupModelsViewData {
	onDisk := installedModelSet()
	current := resolveDictationModel(h.config.Get().Model)
	var rows []views.SetupModelView
	for _, m := range dictationCatalog() {
		size, downloaded := onDisk[m.Name]
		f := factsFor(m, size)
		rows = append(rows, views.SetupModelView{
			Name:       m.Name,
			Size:       f.Size,
			Bytes:      f.Bytes,
			Languages:  f.Languages,
			Note:       f.Note,
			Chosen:     m.Name == current,
			Downloaded: downloaded,
			Ref:        h.modelRef(m.Name),
		})
	}
	cmdRows := h.commandModelRows(onDisk)
	prefix := h.plugin.ID() + "/"
	own := ownDownloadRunning(dl, prefix)
	pct := 0
	if own {
		pct = int(dl.Progress * 100)
	}
	return views.SetupModelsViewData{
		Languages:     languageRows(h.config.Get().Language),
		Models:        rows,
		Commands:      cmdRows,
		Downloading:   own,
		OtherDownload: otherPluginDownload(dl, prefix),
		ProgressPct:   pct,
		DownloadError: setupDownloadError(dl, prefix, current, onDisk),
	}
}

// commandModelRows are Get started's command model rows, in commandModels'
// order. InUse marks the model the stage loads now, which is not the chosen
// one while the chosen one is still to be downloaded.
func (h *Host) commandModelRows(onDisk map[string]string) []views.SetupModelView {
	declared := map[string]int64{}
	for _, m := range declaredModels() {
		declared[m.Name] = m.SizeBytes
	}
	chosen := h.chosenCommandModel()
	inUse := commandModelOnDisk(chosen, func(n string) bool { _, ok := onDisk[n]; return ok })
	var rows []views.SetupModelView
	for _, m := range commandModels {
		size, ok := declared[m.Name]
		if !ok {
			continue // not declared in this manifest: nothing to download
		}
		_, downloaded := onDisk[m.Name]
		rows = append(rows, views.SetupModelView{
			Name:       m.Name,
			Size:       m.Label,
			Bytes:      humanBytes(size),
			Languages:  "English",
			Note:       m.Note,
			Chosen:     m.Name == chosen,
			InUse:      downloaded && m.Name == inUse,
			Downloaded: downloaded,
			Ref:        h.modelRef(m.Name),
		})
	}
	return rows
}

// ownDownloadRunning reports whether the platform is fetching one of this
// plugin's models now (refPrefix is "<plugin id>/"). The platform keeps one
// download record for every plugin, so a running download is voice's
// progress only when its ref says so.
func ownDownloadRunning(dl *ModelDownloadData, refPrefix string) bool {
	return dl != nil && dl.Downloading && strings.HasPrefix(dl.Model, refPrefix)
}

// otherPluginDownload is what Get started says while another plugin's
// download runs ("Another plugin (announcements) is downloading kokoro"), or
// "". The platform runs downloads one at a time through one queue (the
// platform's services/model_download.rs), and its record shows only the
// running one: a Download click now would wait out of sight, behind a bar
// that is not voice's. So voice's Download buttons stay off until it
// finishes, and Get started says why instead of hiding them silently. A
// record from a platform that predates the model ref cannot be told apart,
// and reads as another plugin's: nothing claims it is voice's.
func otherPluginDownload(dl *ModelDownloadData, refPrefix string) string {
	if dl == nil || !dl.Downloading || strings.HasPrefix(dl.Model, refPrefix) {
		return ""
	}
	plugin, model, ok := strings.Cut(dl.Model, "/")
	if !ok || plugin == "" || model == "" {
		return "Another plugin is downloading a model"
	}
	return "Another plugin (" + plugin + ") is downloading " + model
}

// setupDownloadError is the failure Get started shows, or "". The platform
// keeps one download record for every plugin, so a failure counts only when
// it names one of this plugin's models (refPrefix is "<plugin id>/"), and
// only while it still stands in the way: the failed model is not on disk,
// and, for a dictation model, the chosen dictation model is not on disk
// either — picking a model already downloaded starts no new download, so
// the failure would otherwise stay until that model was asked for again.
// The record's current model is checked first, then every earlier failure
// the platform keeps beside it: "Download Recommended" queues two models,
// and the second one starting must not hide why the first one stopped.
func setupDownloadError(dl *ModelDownloadData, refPrefix, chosen string, onDisk map[string]string) string {
	if dl == nil {
		return ""
	}
	refs := []string{dl.Model}
	var earlier []string
	for ref := range dl.Failures {
		if ref != dl.Model {
			earlier = append(earlier, ref)
		}
	}
	sort.Strings(earlier)
	refs = append(refs, earlier...)
	for _, ref := range refs {
		why := dl.failureOf(ref)
		if why == "" || !strings.HasPrefix(ref, refPrefix) {
			continue
		}
		failed := strings.TrimPrefix(ref, refPrefix)
		if _, ok := onDisk[failed]; ok {
			continue
		}
		if !isCommandModel(failed) {
			if _, ok := onDisk[chosen]; ok {
				continue
			}
		}
		return why
	}
	return ""
}

// activeKey returns the chord bound to an action, from the platform's
// derived binding table, or "" when nothing binds it.
func activeKey(active json.RawMessage, action string) string {
	var table struct {
		Entries []struct {
			Source  string `json:"source"`
			Trigger string `json:"trigger"`
			Action  string `json:"action"`
		} `json:"entries"`
	}
	if json.Unmarshal(active, &table) != nil || len(table.Entries) == 0 {
		var byID map[string]json.RawMessage
		if json.Unmarshal(active, &byID) == nil {
			if inner, ok := byID["singleton"]; ok {
				return activeKey(inner, action)
			}
		}
		return ""
	}
	for _, e := range table.Entries {
		if e.Action != action || (e.Source != "" && e.Source != "keyboard") {
			continue
		}
		chord := strings.TrimSpace(e.Trigger)
		if i := strings.IndexByte(chord, ' '); i >= 0 {
			chord = chord[:i]
		}
		if chord != "" {
			return chord
		}
	}
	return ""
}

// keyName is a default chord written out in this OS's words, for prose:
// "⌥T" on macOS, "Alt+T" elsewhere.
func keyName(chord string) string {
	sep := "+"
	if runtime.GOOS == "darwin" {
		sep = ""
	}
	return strings.Join(keyCaps(chord, runtime.GOOS), sep)
}

// keyCaps renders a chord in this OS's own words: glyphs on macOS, names
// elsewhere, modifiers in the order each OS writes them.
func keyCaps(chord, goos string) []string {
	mac := goos == "darwin"
	var ctrl, alt, shift, cmd bool
	key := ""
	for _, part := range strings.Split(strings.ToLower(chord), "+") {
		switch part {
		case "ctrl", "control":
			ctrl = true
		case "alt", "opt", "option":
			alt = true
		case "shift":
			shift = true
		case "cmd", "command", "super", "win", "meta":
			cmd = true
		default:
			key = part
		}
	}
	var caps []string
	add := func(on bool, glyph, word string) {
		if on {
			if mac {
				caps = append(caps, glyph)
			} else {
				caps = append(caps, word)
			}
		}
	}
	add(ctrl, "⌃", "Ctrl")
	add(alt, "⌥", "Alt")
	add(shift, "⇧", "Shift")
	superWord := "Super"
	if goos == "windows" {
		superWord = "Win"
	}
	add(cmd, "⌘", superWord)
	switch key {
	case "":
	case "space":
		caps = append(caps, "Space")
	case "escape", "esc":
		if mac {
			caps = append(caps, "esc")
		} else {
			caps = append(caps, "Esc")
		}
	default:
		if len(key) == 1 {
			caps = append(caps, strings.ToUpper(key))
		} else {
			caps = append(caps, strings.ToUpper(key[:1])+key[1:])
		}
	}
	return caps
}

func (h *Host) setupTryData(active json.RawMessage) views.SetupTryViewData {
	dictate := activeKey(active, "voice.dictation")
	if dictate == "" {
		dictate = "opt+t"
	}
	command := activeKey(active, "voice.command")
	if command == "" {
		command = "opt+g"
	}
	hv := h.health.snapshot(time.Now())
	commandsOnDisk := modelOnDisk(h.commandModelName())
	return views.SetupTryViewData{
		DictateKeys:    keyCaps(dictate, runtime.GOOS),
		CommandKeys:    keyCaps(command, runtime.GOOS),
		Mic:            hv.results.Mic,
		MicNote:        hv.micNote,
		Dictation:      hv.results.Dictation,
		Commands:       hv.results.Commands,
		Listening:      hv.listening,
		Heard:          hv.heard,
		Phrase:         commandCheckPhrase,
		CommandsOnDisk: commandsOnDisk,
		Linux:          runtime.GOOS == "linux",
	}
}
