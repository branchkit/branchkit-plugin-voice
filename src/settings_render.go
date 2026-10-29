package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/branchkit/branchkit-plugin-voice/calibration"
	"github.com/branchkit/branchkit-plugin-voice/hud"
	"github.com/branchkit/branchkit-plugin-voice/speech"
	"github.com/branchkit/branchkit-plugin-voice/views"
	"github.com/branchkit/plugin-sdk-go"
)

//go:embed views/voice.css
var voiceCSS string

// --- SDK type aliases ---

type ActionTypeSchema = branchkit.ActionTypeSchema
type ActionFieldSchema = branchkit.ActionFieldSchema
type CommandRowData = branchkit.CommandRowData

type CorrectionRowData struct {
	ID           string `json:"id"`
	MatchPattern string `json:"match"`
	Replacement  string `json:"replacement"`
	IsRegex      bool   `json:"isRegex"`
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source,omitempty"`
}

// ModelDownloadData is the in-flight download progress the platform shell
// reports while branchkit-cli fetches a model. It is the only model state this
// plugin cannot see for itself: the shell runs the downloader subprocess, and
// nothing on disk shows progress mid-fetch.
//
// Everything else that used to ride on this collection is gone. `models_on_disk`
// and `downloaded` are a ReadDir of this plugin's own model namespace now
// (models.go), and `model_name` was this plugin reading its OWN setting back out
// of the Swift shell's UserDefaults — a round trip that also silently failed to
// change which model actually loads, since the pipeline reads the setting
// directly.
type ModelDownloadData struct {
	Downloading bool    `json:"downloading"`
	Progress    float64 `json:"progress"`
	// Model is the `<plugin>/<model>` ref being fetched; empty from a
	// platform that predates it.
	Model string `json:"model,omitempty"`
	// Error is why the last download of Model stopped short, in the
	// downloader's words; empty unless it failed.
	Error string `json:"error,omitempty"`
	// Failures is every model whose last download failed, by ref, with its
	// reason. Error covers only Model, and the platform's queue moves Model
	// on to the next download the moment one fails ("Download Recommended"
	// asks for two), so an earlier failure is read from here. A model's
	// entry goes when it is asked for again or arrives.
	Failures map[string]string `json:"failures,omitempty"`
}

// failureOf is why ref's last download failed, or "".
func (dl *ModelDownloadData) failureOf(ref string) string {
	if dl == nil || ref == "" {
		return ""
	}
	if dl.Model == ref {
		if dl.Downloading {
			return ""
		}
		if dl.Error != "" {
			return dl.Error
		}
	}
	return dl.Failures[ref]
}

// downloadOf is ref's download as a Download button shows it: active only
// while the platform is fetching that model, failed while its last download
// stands failed.
func downloadOf(dl *ModelDownloadData, ref string) views.ModelDownloadView {
	if dl == nil {
		return views.ModelDownloadView{}
	}
	if dl.Model == ref && dl.Downloading {
		return views.ModelDownloadView{Active: true, Pct: int(dl.Progress*100 + 0.5)}
	}
	return views.ModelDownloadView{Failed: dl.failureOf(ref)}
}

func modelDownloadFromCollection(data map[string]json.RawMessage) *ModelDownloadData {
	raw, ok := data["_platform.model_download"]
	if !ok {
		return &ModelDownloadData{}
	}
	var md ModelDownloadData
	if err := json.Unmarshal(raw, &md); err != nil {
		return &ModelDownloadData{}
	}
	return &md
}

// --- Render functions ---

func (h *Host) renderPhonetics(search string, phonetics []PhoneticEntry, commands []CommandRowData) string {
	stats, misheard := h.aggregatePhoneticStats()

	// A codeword is a user override when it differs from the plugin's seeded
	// default for that letter — this drives the User/Core badge and the
	// reset-to-default affordance. (Was derived from the user-commands list,
	// which never reflected codeword renames.)
	defaults := alphabetDefaults()

	var rows []views.PhoneticRowView
	for _, e := range phonetics {
		letter := "?"
		if e.Letter != nil {
			letter = strings.ToUpper(string(*e.Letter))
		}
		if search != "" && !strings.Contains(strings.ToLower(letter), search) && !strings.Contains(strings.ToLower(e.Word), search) {
			continue
		}

		sc, st := "score-none", "\u2014"
		mh := ""
		if s, ok := stats[e.Word]; ok && s[1] > 0 {
			pct := float32(s[0]) / float32(s[1]) * 100
			st = fmt.Sprintf("%.0f%%", pct)
			switch {
			case pct >= 80:
				sc = "score-green"
			case pct >= 50:
				sc = "score-yellow"
			default:
				sc = "score-red"
			}
		}
		if heard, ok := misheard[e.Word]; ok && len(heard) > 0 {
			mh = "Often heard as: " + strings.Join(heard, ", ")
		}

		isOverride := false
		if e.Letter != nil {
			def := defaults[strings.ToLower(string(*e.Letter))]
			isOverride = def != "" && !strings.EqualFold(def, e.Word)
		}
		bc, tt, mc := "badge-core", "Core", ""
		if isOverride {
			bc, tt, mc = "badge-user", "User", "modified"
		}

		rows = append(rows, views.PhoneticRowView{
			Letter: letter, Word: e.Word,
			ScoreClass: sc, ScoreText: st, MisheardHint: mh,
			BadgeClass: bc, TierText: tt, ModifiedClass: mc,
			IsOverride: isOverride,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Letter < rows[j].Letter })

	html := views.Render(views.Phonetics(rows))
	// Codeword-lab candidates card (only when the harness has written
	// reports): engine-tested replacement candidates + the decision ledger.
	if slots := h.buildCandidateSlots(); len(slots) > 0 {
		html += views.Render(views.Candidates(slots))
	}
	return html
}

func (h *Host) renderCalibration(calibActive bool, hasPhonetics bool, game CalibrationGame, activeCmd string) string {
	cw := ""
	if game.CurrentIndex < len(game.Words) {
		cw = game.Words[game.CurrentIndex]
	}
	reviewing := game.Phase == calibration.PhaseReviewing

	// The current target is retunable when it carries a vocabulary binding (an
	// alphabet codeword, or a list slot like number/direction/apps). The bound
	// key pre-fills the edit box; a pair / command-pattern word has no binding.
	currentRetuneKey := ""
	currentTier := ""
	currentGroup := ""
	groupPos, groupTotal := 0, 0
	var groupTargets []views.CalGroupTarget
	if calibActive && game.CurrentIndex >= 0 && game.CurrentIndex < len(game.Targets) {
		cur := game.Targets[game.CurrentIndex]
		currentTier = string(cur.Tier)
		if cur.Binding != nil {
			currentRetuneKey = cur.Binding.Key
		}
		// The active command's expansion group becomes the second column: every
		// target inside it (the hint codeword cross product, a list slot's
		// entries), with its status + two-signal result, and its position
		// (e.g. 3 of 676) so a big group reads as contained.
		if cur.Group != "" {
			currentGroup = cleanGroupLabel(cur.Group)
			for i, t := range game.Targets {
				if t.Group != cur.Group {
					continue
				}
				groupTotal++
				if i <= game.CurrentIndex {
					groupPos++
				}
				item := views.CalGroupTarget{Prompt: t.Prompt, Index: i}
				switch {
				case i < len(game.Results):
					item.Status = "done"
					tr := game.Results[i].Trial
					if tr.Outcome == "skipped" {
						item.Heard = "skip"
					} else {
						item.Heard = sigKind(tr.Heard)
						if tr.Resolves != nil {
							item.Resolves = sigKind(*tr.Resolves)
						}
					}
				case i == game.CurrentIndex:
					item.Status = "current"
				default:
					item.Status = "upcoming"
				}
				groupTargets = append(groupTargets, item)
			}
		}
	}

	// Build live trial view data
	matchStatus := false
	frameCount := 0
	trialElapsed := ""
	var biasCounterfactual *views.BiasCounterfactual
	if calibActive && game.CurrentTrial != nil {
		trial := game.CurrentTrial
		matchStatus = trial.Matched()
		frameCount = len(trial.Frames)
		if !trial.StartedAt.IsZero() {
			trialElapsed = fmt.Sprintf("%.1fs", time.Since(trial.StartedAt).Seconds())
		}
		// Live L1 bias counterfactual — surfaced only when the never-standalone
		// penalty actually flipped this clip's decode.
		if b := trial.Bias; b != nil && b.Changed {
			biasCounterfactual = &views.BiasCounterfactual{Off: b.Off, On: b.On}
		}
	}

	// The always-on sidebar catalog (cached: rebuilt only when attempts change,
	// not per recognition frame) + the active command's health readout.
	var catalog []views.CatalogGroup
	var activeDetail *views.CommandDetail
	if hasPhonetics && !reviewing {
		if snap := h.cachedCatalog(); snap != nil {
			catalog = snap.groups
			if calibActive && activeCmd != "" {
				activeDetail = snap.detailFor(activeCmd)
			}
		}
	}

	// Post-stop banner: a sitting just closed (Finished) with real attempts —
	// "Tested N commands this session", with a path to the retrospective report.
	showBanner := false
	bannerTested := 0
	if !calibActive && game.Phase == calibration.PhaseFinished {
		seen := make(map[string]bool)
		for _, r := range game.Results {
			if len(r.Trial.Frames) == 0 {
				continue
			}
			key := r.CommandID
			if key == "" {
				key = r.Word
			}
			seen[key] = true
		}
		bannerTested = len(seen)
		showBanner = bannerTested > 0
	}

	// The working set: clips whose target matches the selected command, docked
	// under the stage (the test→clip loop never leaves the tab). The GLOBAL
	// library moved to the Recordings tab (C5); calibration keeps only this
	// scoped view plus a count for the "All recordings" link row.
	rec := h.assembleRecordingsData()
	var dockedClips []views.RecordingGroup
	dockedFor := ""
	if activeDetail != nil {
		dockedFor = activeDetail.Pattern
		for _, g := range rec.RecordingGroups {
			if strings.EqualFold(g.Target, activeDetail.Pattern) {
				dockedClips = append(dockedClips, g)
			}
		}
	}

	return views.Render(views.Calibration(views.CalibrationViewData{
		DockedClips:       dockedClips,
		DockedFor:         dockedFor,
		Catalog:           catalog,
		ActiveCmd:         activeCmd,
		ActiveDetail:      activeDetail,
		GroupTargets:      groupTargets,
		CurrentTier:       currentTier,
		CurrentRetuneKey:  currentRetuneKey,
		CurrentGroup:      currentGroup,
		CurrentGroupPos:   groupPos,
		CurrentGroupTotal: groupTotal,
		IsActive:          calibActive,
		IsReviewing:       reviewing,
		ReportsSessions:   h.buildReportsSessions(reviewing),
		ReportsRollups:    h.buildReportsRollups(reviewing),
		ShowBanner:        showBanner,
		BannerTested:      bannerTested,
		CaptureSupported:  true,
		CaptureEnabled:    h.config.Get().CalibrationCapture,
		CaptureRetention:  h.captureRetentionOrDefault(),
		RecordingsShown:   rec.RecordingsShown,
		RecordingsTotal:   rec.RecordingsTotal,
		AutoAdvance:       h.config.Get().CalibrationAutoAdvance,
		HasPhonetics:      hasPhonetics,
		CurrentWord:       cw,
		CurrentIndex:      game.CurrentIndex,
		TotalWords:        len(game.Words),
		MatchedCount:      game.MatchedCount(),
		ElapsedSecs:       fmt.Sprintf("%.1fs", game.ElapsedSecs),
		HeardBuffer:       game.HeardBuffer,
		MatchStatus:       matchStatus,
		CommandKey:        keyName("opt+g"),
		FrameCount:        frameCount,
		TrialElapsed:      trialElapsed,
		CanGoBack:         game.CurrentIndex > 0,
		Bias:              biasCounterfactual,
	}))
}

// assembleRecordingsData builds the voice-library view shared by the
// Recordings tab (the system of record) and calibration's docked working set.
// Served from the clip cache, same as before the split.
func (h *Host) assembleRecordingsData() views.RecordingsViewData {
	data := views.RecordingsViewData{
		CaptureSupported: true,
		CaptureEnabled:   h.config.Get().CalibrationCapture,
		CaptureRetention: h.captureRetentionOrDefault(),
		RecordingsView:   h.currentRecordingsView(),
		ExportStatus:     h.currentExportStatus(),
	}
	var biasRecommend calibration.BiasRecommendation
	if snap := h.cachedRecordings(); snap != nil {
		data.RecordingGroups, data.RecordingsFlat, data.RecordingsTotal = snap.groups, snap.flat, snap.total
		for _, g := range data.RecordingGroups {
			data.RecordingsShown += g.ClipCount
		}
		biasRecommend = snap.biasRec
	}
	if biasRecommend.Savable+biasRecommend.Clean+biasRecommend.Never > 0 {
		data.BiasRecommendLabel = biasRecommend.Label()
	}
	data.BiasRecommendStrength = strconv.FormatFloat(biasRecommend.Strength, 'f', -1, 64)
	data.BiasRecommendOK = biasRecommend.OK
	data.BiasApplyStatus, data.BiasApplyNeedsConfirm = h.currentBiasApplyStatus()
	{
		enabled, strength, provenance := h.currentBias()
		data.BiasEnabled = enabled
		data.BiasStrength = strconv.FormatFloat(strength, 'f', -1, 64)
		switch provenance {
		case "manual":
			data.BiasProvenance = "manually set"
		case "calibration":
			data.BiasProvenance = "set by calibration"
		default:
			data.BiasProvenance = "default"
		}
	}
	return data
}

func (h *Host) renderRecordings() string {
	return views.Render(views.Recordings(h.assembleRecordingsData()))
}

// sigKind maps a boolean two-signal outcome to the group rail's display kind.
func sigKind(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// cleanGroupLabel renders an expansion group's pattern as a readable label:
// literal words pass through, and each `<slot>` collapses to its slot name —
// dropping the `:collection` / `${template}` qualifiers. "scroll down <number>"
// → "scroll down number"; the hint command's
// "<prefix:browser_hints_prefix> <suffix:browser_hints_${prefix}_strict>" →
// "prefix suffix".
func cleanGroupLabel(pattern string) string {
	if pattern == "" {
		return ""
	}
	var b strings.Builder
	for i, tok := range strings.Fields(pattern) {
		if i > 0 {
			b.WriteByte(' ')
		}
		if strings.HasPrefix(tok, "<") && strings.HasSuffix(tok, ">") {
			inner := strings.Trim(tok, "<>")
			if j := strings.IndexAny(inner, ":${|"); j >= 0 {
				inner = inner[:j]
			}
			b.WriteString(inner)
		} else {
			b.WriteString(tok)
		}
	}
	return b.String()
}

// catalogSnapshot is the heavy, cacheable part of the command catalog: the
// owner-grouped rows (display order, no per-render highlight baked in), the
// flattened display order for command-level chaining, and the raw health
// analyzers so the right-pane detail for whichever command is active can be
// built per render without re-aggregating. Rebuilt only when attempts change
// (a walk persists) or the command set changes — NOT per recognition frame, so
// the always-on sidebar doesn't re-aggregate the whole attempt log mid-utterance.
type catalogSnapshot struct {
	groups     []views.CatalogGroup
	orderedIDs []string
	enumByID   map[string]calibration.EnumeratedCommand
	health     map[string]calibration.CommandHealth
	friction   map[string]calibration.FrictionStat
	confusion  *calibration.ConfusionGraph
	fragility  map[string]views.FragilityWordView
	// lastMiss is the newest failed attempt per command id / target: the raw
	// material of the miss anatomy (P2) — what the engine actually heard.
	lastMiss map[string]lastMissInfo
	// setterByTag indexes mode entries (tag -> the command id that sets it),
	// shared by the combo tree and the test box's gated-why lookup.
	setterByTag map[string]string
}

// whyGated answers "would this phrase match inside a mode?": when the exact
// phrase is a gated command, returns its pattern and the entry word that
// opens its mode.
func (snap *catalogSnapshot) whyGated(phrase string) (child, entry string, ok bool) {
	p := strings.TrimSpace(strings.ToLower(phrase))
	if p == "" {
		return "", "", false
	}
	for _, c := range snap.enumByID {
		if strings.ToLower(c.Pattern) != p {
			continue
		}
		for _, t := range c.RequiresTags {
			if setterID, found := snap.setterByTag[t]; found && setterID != c.ID {
				if e, have := snap.enumByID[setterID]; have {
					return c.Pattern, e.Pattern, true
				}
			}
		}
	}
	return "", "", false
}

type lastMissInfo struct {
	heard    string
	when     time.Time
	filtered bool // MatcherRejected: heard right, matcher refused
}

// invalidateCatalogCache drops the cached catalog so the next render rebuilds it
// with fresh health. Called when a walk persists attempts (live per-command
// health) and when the command set changes.
func (h *Host) invalidateCatalogCache() {
	h.catalogMu.Lock()
	h.catalogCached = nil
	h.catalogMu.Unlock()
}

// cachedCatalog returns the catalog snapshot, building it on first use / after
// invalidation. nil when no commands are enumerable.
func (h *Host) cachedCatalog() *catalogSnapshot {
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	if h.catalogCached == nil {
		h.catalogCached = h.buildCatalogSnapshot()
	}
	return h.catalogCached
}

// lookupHealth resolves a command's health by id first (new records), pattern
// fallback (legacy/codeword records).
func (snap *catalogSnapshot) lookupHealth(c calibration.EnumeratedCommand) (calibration.CommandHealth, bool) {
	if h, ok := snap.health[c.ID]; ok {
		return h, true
	}
	if h, ok := snap.health[c.Pattern]; ok {
		return h, true
	}
	return calibration.CommandHealth{}, false
}

// detailFor builds the right-pane health readout for one command id (the active
// walk command). nil when the id is unknown.
func (snap *catalogSnapshot) detailFor(id string) *views.CommandDetail {
	c, ok := snap.enumByID[id]
	if !ok {
		return nil
	}
	h, found := snap.lookupHealth(c)
	d := buildCommandDetail(c, h, found && h.Attempts > 0, snap.friction, snap.confusion, snap.fragility)
	mi, ok := snap.lastMiss[c.ID]
	if !ok {
		mi, ok = snap.lastMiss[c.Pattern]
	}
	if ok {
		d.LastMissHeard = mi.heard
		d.LastMissWhen = relTimeLabel(mi.when)
		d.LastMissFiltered = mi.filtered
	}
	return d
}

// buildCatalogSnapshot assembles the unified command catalog: every enumerated
// command grouped by owner, each row with a glanceable health summary, plus the
// analyzers needed for the active command's detail. Returns nil when no commands
// are enumerable.
func (h *Host) buildCatalogSnapshot() *catalogSnapshot {
	enum := h.enumerateCommandsForPractice()
	if len(enum) == 0 {
		return nil
	}
	attempts := h.loadVerificationAttempts()
	snap := &catalogSnapshot{
		health:    calibration.CatalogHealth(attempts),
		friction:  calibration.ComputeFriction(attempts),
		confusion: calibration.ComputeConfusion(attempts, time.Time{}),
		fragility: make(map[string]views.FragilityWordView),
		enumByID:  make(map[string]calibration.EnumeratedCommand, len(enum)),
		lastMiss:  make(map[string]lastMissInfo),
	}
	for _, a := range attempts {
		if a.Matched || strings.TrimSpace(a.Heard) == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, a.Timestamp)
		if err != nil {
			continue
		}
		mi := lastMissInfo{heard: a.Heard, when: ts, filtered: a.MatcherRejected}
		for _, key := range []string{a.CommandID, a.Target} {
			if key == "" {
				continue
			}
			if prev, ok := snap.lastMiss[key]; !ok || ts.After(prev.when) {
				snap.lastMiss[key] = mi
			}
		}
	}
	for _, fw := range h.buildFragilityWords() {
		snap.fragility[strings.ToLower(fw.Target)] = fw
	}

	// The combo tree: a command whose sets_tags gate other commands is a
	// MODE ENTRY; the commands it gates nest under it instead of appearing as
	// top-level rows. One level deep, matched on the first gating tag found.
	setterByTag := make(map[string]string) // tag -> entry command id
	for _, c := range enum {
		for _, t := range c.SetsTags {
			if _, taken := setterByTag[t]; !taken {
				setterByTag[t] = c.ID
			}
		}
	}
	snap.setterByTag = setterByTag
	entryFor := func(c calibration.EnumeratedCommand) string {
		for _, t := range c.RequiresTags {
			if id, ok := setterByTag[t]; ok && id != c.ID {
				return id
			}
		}
		return ""
	}

	type acc struct {
		rows    []views.CatalogRow
		tested  int
		rateSum float32
	}
	byOwner := make(map[string]*acc)
	var owners []string
	childrenOf := make(map[string][]views.CatalogRow)

	for _, c := range enum {
		snap.enumByID[c.ID] = c
		h, found := snap.lookupHealth(c)
		tested := found && h.Attempts > 0
		accuracy := "—"
		if tested {
			accuracy = fmt.Sprintf("%.0f%%", h.Rate)
		}
		warn := tested && h.Rate < 80
		if fs, ok := snap.friction[strings.ToLower(c.Pattern)]; ok && fs.IsHigh() {
			warn = true
		}
		row := views.CatalogRow{
			ID:          c.ID,
			Pattern:     displayPattern(c),
			Tier:        string(c.Tier()),
			Accuracy:    accuracy,
			Warn:        warn,
			Description: c.Description,
		}
		if parent := entryFor(c); parent != "" {
			childrenOf[parent] = append(childrenOf[parent], row)
			if tested {
				if a := byOwner[c.Owner]; a != nil {
					a.tested++
					a.rateSum += h.Rate
				}
			}
			continue
		}
		a := byOwner[c.Owner]
		if a == nil {
			a = &acc{}
			byOwner[c.Owner] = a
			owners = append(owners, c.Owner)
		}
		a.rows = append(a.rows, row)
		if tested {
			a.tested++
			a.rateSum += h.Rate
		}
	}

	sort.Strings(owners)
	snap.groups = make([]views.CatalogGroup, 0, len(owners))
	for _, owner := range owners {
		a := byOwner[owner]
		sort.Slice(a.rows, func(i, j int) bool { return a.rows[i].Pattern < a.rows[j].Pattern })
		for i := range a.rows {
			if kids := childrenOf[a.rows[i].ID]; len(kids) > 0 {
				sort.Slice(kids, func(x, y int) bool { return kids[x].Pattern < kids[y].Pattern })
				a.rows[i].Children = kids
			}
		}
		rollup := "—"
		if a.tested > 0 {
			rollup = fmt.Sprintf("%.0f%%", a.rateSum/float32(a.tested))
		}
		for _, r := range a.rows {
			snap.orderedIDs = append(snap.orderedIDs, r.ID)
			for _, k := range r.Children {
				snap.orderedIDs = append(snap.orderedIDs, k.ID)
			}
		}
		snap.groups = append(snap.groups, views.CatalogGroup{
			Owner:  owner,
			Rows:   a.rows,
			Rollup: rollup,
			Tested: a.tested,
			Total:  len(a.rows),
		})
	}
	return snap
}

// nextCatalogCommandID returns the command following currentID in the sidebar's
// display order, honoring the live search filter (matched the same way the UI
// filters rows: owner + display pattern contains the term). Returns "" at the
// end of the filtered catalog or when currentID isn't found — so chaining stops
// rather than loops. Wired to calib.NextCommandID for command-level chaining.
func (h *Host) nextCatalogCommandID(currentID, filter string) string {
	snap := h.cachedCatalog()
	if snap == nil {
		return ""
	}
	term := strings.ToLower(strings.TrimSpace(filter))
	matches := func(id string) bool {
		if term == "" {
			return true
		}
		c, ok := snap.enumByID[id]
		if !ok {
			return false
		}
		return strings.Contains(strings.ToLower(c.Owner+" "+displayPattern(c)), term)
	}
	seenCurrent := false
	for _, id := range snap.orderedIDs {
		if seenCurrent {
			if matches(id) {
				return id
			}
			continue
		}
		if id == currentID {
			seenCurrent = true
		}
	}
	return ""
}

// dynamicCommandLabel is the friendly catalog/detail name for a dynamic (hint)
// command: its literal words (e.g. "hover") plus "hint badges", since the slots
// are the per-page hint badges. "(alphabet)" notes that calibration fills them
// from the voice codeword set.
func dynamicCommandLabel(c calibration.EnumeratedCommand) string {
	var lits []string
	for _, tok := range strings.Fields(c.Pattern) {
		if !strings.HasPrefix(tok, "<") {
			lits = append(lits, tok)
		}
	}
	label := "hint badges"
	if len(lits) > 0 {
		label = strings.Join(lits, " ") + " " + label
	}
	return label + " (alphabet)"
}

// displayPattern is the catalog/detail label for a command: a dynamic (hint)
// command shows a friendly name (its per-trial prompts are still the concrete
// codewords); everything else shows its pattern verbatim.
func displayPattern(c calibration.EnumeratedCommand) string {
	if c.Dynamic {
		return dynamicCommandLabel(c)
	}
	return c.Pattern
}

// buildCommandDetail assembles the right-pane readout for one command.
func buildCommandDetail(c calibration.EnumeratedCommand, h calibration.CommandHealth, tested bool, friction map[string]calibration.FrictionStat, confusion *calibration.ConfusionGraph, fragility map[string]views.FragilityWordView) *views.CommandDetail {
	d := &views.CommandDetail{
		Pattern:      displayPattern(c),
		Owner:        c.Owner,
		Tier:         string(c.Tier()),
		ContextLabel: contextLabel(c),
		Accuracy:     "never tested",
		LastTested:   "never",
		Category:     c.Category,
	}
	if strings.EqualFold(d.Category, d.Owner) {
		d.Category = "" // redundant with the owner already shown alongside
	}
	if tested {
		runs := "runs"
		if h.Attempts == 1 {
			runs = "run"
		}
		d.Accuracy = fmt.Sprintf("%.0f%% (%d %s)", h.Rate, h.Attempts, runs)
		d.LastTested = relTimeLabel(h.LastVerified)
	}
	if fs, ok := friction[strings.ToLower(c.Pattern)]; ok && fs.Attempts >= 2 {
		if fs.IsHigh() {
			d.Friction = "high"
		} else {
			d.Friction = "low"
		}
	}
	if fw, ok := fragility[strings.ToLower(c.Pattern)]; ok {
		d.Fragility = fw.Status
		d.FragilityDetail = fw.Detail
		d.Confuser = fw.Confusers
	}
	if d.Confuser == "" && confusion != nil {
		if edges := confusion.ByTarget[c.Pattern]; len(edges) > 0 {
			d.Confuser = edges[0].Heard
		}
	}
	return d
}

// contextLabel explains why a command needs setup before a trial.
func contextLabel(c calibration.EnumeratedCommand) string {
	switch c.Tier() {
	case calibration.TierModeGated:
		return "needs mode: " + strings.Join(c.RequiresTags, ", ")
	case calibration.TierDynamic:
		return "hint grammar — acoustic only"
	case calibration.TierSlotted:
		return "slotted — acoustic only"
	default:
		return "base context — no setup"
	}
}

// relTimeLabel renders a coarse "how long ago" for a timestamp.
func relTimeLabel(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	days := int(time.Since(t).Hours() / 24)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", days)
	}
}

// recordingsDisplayCap bounds how many clips the Recordings list renders. The
// full kept count is shown alongside, so a larger library reads as
// "showing N of M" — never a silent truncation.
const recordingsDisplayCap = 200

// recordingsSnapshot caches the rendered Recordings rows so the always-shown
// list isn't re-scanned from the capture dir on every recognition frame during a
// walk. Invalidated when clips change (a hold completes, a mark, a clear).
type recordingsSnapshot struct {
	groups []views.RecordingGroup
	flat   []views.RecordingView // ungrouped, newest-first (for the "most recent" view)
	total  int
	// biasRec folds the same bias-score log the rows read into the corpus-level
	// strength recommendation shown in the Recordings header.
	biasRec calibration.BiasRecommendation
}

// setRecordingsView switches the recordings body between "grouped" and "recent".
func (h *Host) setRecordingsView(mode string) {
	h.recordingsMu.Lock()
	if mode == "recent" {
		h.recordingsViewMode = "recent"
	} else {
		h.recordingsViewMode = "grouped"
	}
	h.recordingsMu.Unlock()
}

// currentRecordingsView reads the active recordings view mode.
func (h *Host) currentRecordingsView() string {
	h.recordingsMu.Lock()
	defer h.recordingsMu.Unlock()
	return h.recordingsViewMode
}

// lastExportStatus is the result line from the most recent clip export ("Exported
// N clips → path" or an error), shown under the recordings controls until the next
// export. Transient UI feedback, not persisted. Guarded by recordingsMu.

func (h *Host) setExportStatus(s string) {
	h.recordingsMu.Lock()
	h.lastExportStatus = s
	h.recordingsMu.Unlock()
}

func (h *Host) currentExportStatus() string {
	h.recordingsMu.Lock()
	defer h.recordingsMu.Unlock()
	return h.lastExportStatus
}

// lastBiasApplyStatus is the result line from the most recent bias-recommendation
// apply ("Applied strength 8" / the manual-value refusal / an error), shown next
// to the Apply button until the next attempt. biasApplyNeedsConfirm flags that
// the last apply was refused over a manually-set value, so the button re-renders
// as an explicit "Replace manual setting". Transient UI feedback, not persisted.
// Guarded by recordingsMu.

func (h *Host) setBiasApplyStatus(s string, needsConfirm bool) {
	h.recordingsMu.Lock()
	h.lastBiasApplyStatus = s
	h.biasApplyNeedsConfirm = needsConfirm
	h.recordingsMu.Unlock()
}

func (h *Host) currentBiasApplyStatus() (string, bool) {
	h.recordingsMu.Lock()
	defer h.recordingsMu.Unlock()
	return h.lastBiasApplyStatus, h.biasApplyNeedsConfirm
}

// invalidateRecordingsCache drops the cached Recordings list so the next render
// re-scans the capture dir (via the platform). Called after a hold captures a
// clip, after a disposition mark, and after a clear.
func (h *Host) invalidateRecordingsCache() {
	h.recordingsMu.Lock()
	h.recordingsCached = nil
	h.recordingsMu.Unlock()
}

// cachedRecordings returns the Recordings snapshot, building it on first use /
// after invalidation. Never nil (an empty library is an empty snapshot).
func (h *Host) cachedRecordings() *recordingsSnapshot {
	h.recordingsMu.Lock()
	defer h.recordingsMu.Unlock()
	if h.recordingsCached == nil {
		groups, flat, total, biasRec := h.buildRecordings()
		h.recordingsCached = &recordingsSnapshot{groups: groups, flat: flat, total: total, biasRec: biasRec}
	}
	return h.recordingsCached
}

// buildRecordings folds the kept-clip corpus into the Recordings list view rows
// (newest first, capped for display). Returns the per-target groups, the same rows
// ungrouped+newest-first (the "most recent" view), and the full kept count.
//
// The scan is a plain directory walk of this plugin's own capture data
// (calibration.ScanRecordings). It used to be a platform RPC, because the
// capture dir was platform-named under app support and this process was denied
// it; it lives in the plugin's data namespace now.
func (h *Host) buildRecordings() ([]views.RecordingGroup, []views.RecordingView, int, calibration.BiasRecommendation) {
	all := calibration.ScanRecordings()
	total := len(all)
	scanned := all
	if len(scanned) > recordingsDisplayCap {
		scanned = scanned[:recordingsDisplayCap]
	}

	// Read the per-clip fragility log once: the per-file history feeds each clip's
	// drill-down, and the same records derive the per-target verdict on each group.
	// "Said it wrong" clips are excluded from the verdict (misspoken audio) but stay
	// in the list, dimmed — the recordings rows already carry their disposition, so
	// no extra corpus read here.
	fragRecs := h.loadClipFragility()
	fragByFile := clipFragilityByFile(fragRecs)
	// Latest bias score per clip: the strength-sweep flip threshold,
	// shown beside the fragility outcome on each clip row. The same log folds
	// into the corpus-level strength recommendation for the header.
	biasRecords := h.loadClipBiasScore()
	biasByFile := calibration.LatestBiasPerFile(biasRecords)
	biasRec := calibration.RecommendBias(biasRecords, nil)
	excluded := map[string]bool{}
	for _, e := range scanned {
		if e.Disposition == calibration.DispositionMisspoke {
			excluded[e.File] = true
		}
	}
	verdictByTarget := fragilityByTargetExcluding(fragRecs, excluded)

	rows := make([]views.RecordingView, 0, len(scanned))
	for _, e := range scanned {
		rv := views.RecordingView{
			Word:        e.Target,
			Heard:       e.Heard,
			Matched:     e.Matched,
			When:        recordingWhen(e.Ts),
			File:        e.File,
			AudioURL:    "plugins/voice/data?file=" + url.QueryEscape(calibration.DataRelPath(e.File)),
			Labeled:     e.Target != "",
			Disposition: e.Disposition,
		}
		attachClipFragility(&rv, fragByFile[e.File])
		if b, ok := biasByFile[e.File]; ok {
			rv.BiasScore = b.Label()
		}
		// A fragile/fails-clean clip that hasn't been reviewed is the one worth
		// listening to: is it a genuine hard example (said right) or misspoken
		// audio (said wrong)? Flag it so the UI can draw the eye.
		rv.NeedsReview = rv.Labeled && rv.Disposition == "" &&
			(rv.FragOutcome == calibration.ClipFragile || rv.FragOutcome == calibration.ClipFailsClean)
		rows = append(rows, rv)
	}
	// rows arrive newest-first (the platform's scan sorts by ts desc), so they are
	// already the flat "most recent" list; grouping reorders into per-target buckets.
	return groupRecordingsByTarget(rows, verdictByTarget), rows, total, biasRec
}

// clipFragilityByFile buckets per-clip readings by clip file, newest-first within
// each file — so byFile[f][0] is the latest probe of that clip and the slice is
// the clip's re-probe history. ListLog's order isn't guaranteed, so we sort.
func clipFragilityByFile(records []calibration.ClipFragilityRecord) map[string][]calibration.ClipFragilityRecord {
	byFile := map[string][]calibration.ClipFragilityRecord{}
	for _, r := range records {
		byFile[r.File] = append(byFile[r.File], r)
	}
	for _, recs := range byFile {
		sort.Slice(recs, func(i, j int) bool { return recs[i].Ts > recs[j].Ts })
	}
	return byFile
}

// attachClipFragility folds a clip's re-probe history (newest first) onto its
// view row: the latest reading drives the row's outcome label + per-word detail,
// and the full history drives the drill-down's "re-probes of THIS clip" list.
// No-op when the clip was never probed.
func attachClipFragility(rv *views.RecordingView, history []calibration.ClipFragilityRecord) {
	if len(history) == 0 {
		return
	}
	latest := history[0]
	rv.FragOutcome = latest.Outcome
	rv.FragLabel = clipOutcomeLabel(latest)
	for _, w := range latest.Words {
		rv.Words = append(rv.Words, views.ClipWordView{
			Word:     w.Word,
			Held:     w.Confuser == "",
			Confuser: w.Confuser,
		})
	}
	for _, h := range history {
		rv.History = append(rv.History, views.ClipHistoryView{
			When:    recordingWhen(h.Ts),
			Model:   modelLabel(h.Model),
			Summary: clipHistorySummary(h),
		})
	}
}

// groupRecordingsByTarget folds the flat clip rows into per-target groups. Rows
// arrive newest-first, so a target's group is ordered by its most-recent clip —
// keeping the actively-recorded target near the top. The per-target fragility
// verdict is the derived rollup; an unprobed target (no rollup) shows no badge.
// Unlabeled clips (no corpus target) collapse into a single Target:"" group.
func groupRecordingsByTarget(rows []views.RecordingView, verdictByTarget map[string]calibration.WordFragility) []views.RecordingGroup {
	idx := map[string]int{}
	var groups []views.RecordingGroup
	for _, r := range rows {
		key := strings.ToLower(r.Word)
		gi, ok := idx[key]
		if !ok {
			g := views.RecordingGroup{Target: r.Word}
			if w, ok := verdictByTarget[key]; ok {
				g.Verdict = fragilityStatus(w)
				g.VerdictDetail = fragilityDetail(w)
				g.VerdictRatio = verdictRatio(w)
			}
			gi = len(groups)
			idx[key] = gi
			groups = append(groups, g)
		}
		groups[gi].Clips = append(groups[gi].Clips, r)
	}
	for i := range groups {
		groups[i].ClipCount = len(groups[i].Clips)
	}
	return groups
}

// modelLabel renders a stored model-version hash for display. The probe stamps a
// 16-hex content hash of model.onnx; the fragility probe is Sherpa-NeMo-only (it
// errors on any other engine), so a "nemo@" family prefix + short hash reads as a
// distinguishable model id. "" → "unknown model".
func modelLabel(hash string) string {
	if hash == "" {
		return "unknown model"
	}
	if len(hash) > 8 {
		hash = hash[:8]
	}
	return "nemo@" + hash
}

// clipOutcomeLabel renders a per-clip reading as a one-line verdict for the clip
// row + the overall line in its drill-down.
func clipOutcomeLabel(r calibration.ClipFragilityRecord) string {
	switch r.Outcome {
	case calibration.ClipHeld:
		return "held"
	case calibration.ClipFragile:
		if r.FallSNRdB != nil {
			return fmt.Sprintf("falls @ %.0f dB", *r.FallSNRdB)
		}
		return "fragile"
	case calibration.ClipFailsClean:
		if r.Heard != "" {
			return "fails clean → " + strings.ToUpper(r.Heard)
		}
		return "fails clean"
	}
	return ""
}

// clipHistorySummary is clipOutcomeLabel enriched for a history row: a fragile
// reading also names the constituent that slipped (e.g. "BAT→BACK · falls @ 9 dB")
// so the history reads as the per-clip story across model versions.
func clipHistorySummary(r calibration.ClipFragilityRecord) string {
	base := clipOutcomeLabel(r)
	if r.Outcome == calibration.ClipFragile {
		for _, w := range r.Words {
			if w.Confuser != "" {
				return fmt.Sprintf("%s→%s · %s", strings.ToUpper(w.Word), strings.ToUpper(w.Confuser), base)
			}
		}
	}
	return base
}

// recordingWhen formats a clip's RFC3339 timestamp as an absolute LOCAL datetime
// ("2026-06-22 14:32"). Absolute (not "today · …") because the capture ts is UTC
// and a relative "today" bucketed by elapsed hours misreads a clip recorded <24h
// ago but on the previous calendar day; .Local() also corrects the wall-clock time
// for the user's timezone. Used on the clip rows + the per-clip history rows.
func recordingWhen(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, ts); err != nil {
			return ""
		}
	}
	return t.Local().Format("2006-01-02 15:04")
}

// enumerateCommandsForPractice queries commands.enumerate and returns the
// full command set as calibration.EnumeratedCommand rows — the source of
// truth the command-practice queue filters to Tier 1. Returns nil on RPC
// failure or when the plugin handle isn't wired (tests, early boot).
func (h *Host) enumerateCommandsForPractice() []calibration.EnumeratedCommand {
	cmds, err := h.plugin.CommandsEnumerate()
	if err != nil {
		branchkit.Logf("voice", "calibration: commands.enumerate failed: %v", err)
		return nil
	}
	// calibration.EnumeratedCommand is the practice queue's own shape, a
	// subset of the platform's. Convert rather than alias: the queue does
	// not want binding/category/dynamic and should not grow them by
	// accident.
	out := make([]calibration.EnumeratedCommand, 0, len(cmds))
	for _, c := range cmds {
		// EVERY field the practice queue reads, not just the obvious ones.
		// The first version of this conversion set five and let Go zero the
		// rest; SetsTags (which marks a mode entry and drives the combo
		// tree), Dynamic, Category and Description all silently became
		// empty. The decode this replaced had populated them from the wire,
		// and nothing failed — `just check-deadfields` caught it.
		e := calibration.EnumeratedCommand{
			ID:           c.ID,
			Owner:        c.OwnerPlugin,
			Pattern:      c.Pattern,
			Action:       c.Action,
			RequiresTags: c.RequiresTags,
			SetsTags:     c.SetsTags,
			Dynamic:      c.Dynamic,
		}
		if c.Category != nil {
			e.Category = *c.Category
		}
		if c.Description != nil {
			e.Description = *c.Description
		}
		out = append(out, e)
	}
	return out
}

// languageRows is the dictation language menu, with the configured
// language selected.
func languageRows(selected string) []views.LanguageRowView {
	allLangs := []struct{ Label, Code string }{
		{"English", "en"}, {"Chinese", "zh"}, {"German", "de"}, {"Spanish", "es"},
		{"Russian", "ru"}, {"Korean", "ko"}, {"French", "fr"}, {"Japanese", "ja"},
		{"Portuguese", "pt"}, {"Turkish", "tr"}, {"Polish", "pl"}, {"Catalan", "ca"},
		{"Dutch", "nl"}, {"Arabic", "ar"}, {"Swedish", "sv"}, {"Italian", "it"},
		{"Indonesian", "id"}, {"Hindi", "hi"}, {"Finnish", "fi"}, {"Vietnamese", "vi"},
		{"Hebrew", "he"}, {"Ukrainian", "uk"}, {"Greek", "el"}, {"Malay", "ms"},
		{"Czech", "cs"}, {"Romanian", "ro"}, {"Danish", "da"}, {"Hungarian", "hu"},
		{"Tamil", "ta"}, {"Norwegian", "no"}, {"Thai", "th"}, {"Urdu", "ur"},
		{"Croatian", "hr"}, {"Bulgarian", "bg"}, {"Lithuanian", "lt"}, {"Latin", "la"},
		{"Maori", "mi"}, {"Malayalam", "ml"}, {"Welsh", "cy"}, {"Slovak", "sk"},
		{"Telugu", "te"}, {"Persian", "fa"}, {"Latvian", "lv"}, {"Bengali", "bn"},
		{"Serbian", "sr"}, {"Azerbaijani", "az"}, {"Slovenian", "sl"}, {"Kannada", "kn"},
		{"Estonian", "et"}, {"Macedonian", "mk"}, {"Breton", "br"}, {"Basque", "eu"},
		{"Icelandic", "is"}, {"Armenian", "hy"}, {"Nepali", "ne"}, {"Mongolian", "mn"},
		{"Bosnian", "bs"}, {"Kazakh", "kk"}, {"Albanian", "sq"}, {"Swahili", "sw"},
		{"Galician", "gl"}, {"Marathi", "mr"}, {"Punjabi", "pa"}, {"Sinhala", "si"},
		{"Khmer", "km"}, {"Shona", "sn"}, {"Yoruba", "yo"}, {"Somali", "so"},
		{"Afrikaans", "af"}, {"Occitan", "oc"}, {"Georgian", "ka"}, {"Belarusian", "be"},
		{"Tajik", "tg"}, {"Sindhi", "sd"}, {"Gujarati", "gu"}, {"Amharic", "am"},
		{"Yiddish", "yi"}, {"Lao", "lo"}, {"Uzbek", "uz"}, {"Faroese", "fo"},
		{"Haitian Creole", "ht"}, {"Pashto", "ps"}, {"Turkmen", "tk"}, {"Maltese", "mt"},
		{"Sanskrit", "sa"}, {"Luxembourgish", "lb"}, {"Myanmar", "my"}, {"Tibetan", "bo"},
		{"Tagalog", "tl"}, {"Malagasy", "mg"}, {"Assamese", "as"}, {"Tatar", "tt"},
		{"Hawaiian", "haw"}, {"Lingala", "ln"}, {"Hausa", "ha"}, {"Bashkir", "ba"},
		{"Javanese", "jw"}, {"Sundanese", "su"}, {"Cantonese", "yue"},
		{"Auto-detect (not recommended)", ""},
	}
	var langs []views.LanguageRowView
	for _, l := range allLangs {
		langs = append(langs, views.LanguageRowView{
			Label: l.Label, Code: l.Code, Selected: l.Code == selected,
		})
	}
	return langs
}

func (h *Host) renderDictation(search string, preferredLanguage string, corrections []CorrectionRowData, active json.RawMessage, dl *ModelDownloadData) string {
	var corrRows []views.CorrectionRowView
	for _, c := range corrections {
		corrRows = append(corrRows, views.CorrectionRowView{
			ID: c.ID, MatchPattern: c.MatchPattern, Replacement: c.Replacement,
			IsRegex: c.IsRegex, Enabled: c.Enabled,
			Shipped: c.Source == speech.SourceSeeded,
		})
	}

	var words []views.WordRowView
	for _, r := range speech.WordRows(h.speech.Get(), h.speech.Vocabulary()) {
		row := views.WordRowView{Word: r.Word, HasPattern: r.HasPattern}
		for _, f := range r.Fixes {
			row.Fixes = append(row.Fixes, views.WordFixView{ID: f.ID, Heard: f.Heard})
		}
		words = append(words, row)
	}

	dictate := activeKey(active, "voice.dictation")
	if dictate == "" {
		dictate = "opt+t"
	}
	cfg := h.config.Get()
	return views.Render(views.Dictation(views.DictationViewData{
		DictateKeys:       keyCaps(dictate, runtime.GOOS),
		Sounds:            soundsChoice(h.config.Get().DictationSounds),
		MicLabel:          micLabel(h.micMenuView(cfg.InputDevice, cfg.InputDeviceName)),
		Words:             words,
		RecentHTML:        h.renderDictationHistoryHTML(),
		FixAllCaps:        h.config.Get().FixAllCaps,
		SpokenPunctuation: cfg.SpokenPunctuation,
		StylesReady:       styleModelReady(),
		StyleRef:          h.modelRef(styleModel),
		StyleRows:         h.appStyleRows(),
		AddApps:           h.addAppOptions(),
		DefaultStyle:      h.defaultStyle(),
		Refine:            cfg.RefineDictation,
		RefineReady:       refineModelReady(),
		RefineRef:         h.modelRef(refineModel),
		RefineDownload:    downloadOf(dl, h.modelRef(refineModel)),
		StyleDownload:     downloadOf(dl, h.modelRef(styleModel)),
		Corrections:       corrRows,
	}))
}

// appProfileRows builds the per-app profile table: the shipped rows, the user's
// overrides, and the apps focused most recently, unioned.
//
// Recently-focused apps are rows in their own right rather than entries behind
// an "add" button. The button would be the natural design and it cannot work
// here — the app you want is never the focused one while you are reading this
// page — so the ring IS the affordance, and an app you have used simply
// appears, already showing what it currently resolves to.
func (h *Host) appProfileRows() []views.AppProfileRowView {
	h.loadAppNames()

	recent := h.recentlyFocusedApps()
	rank := make(map[string]int, len(recent))
	for i, id := range recent {
		rank[id] = i
	}

	rows := make(map[string]views.AppProfileRowView)
	for _, r := range h.knownAppProfiles() {
		// A shipped row for an app this machine does not have is a row nobody
		// can use — it renders as a bare bundle identifier, because the `apps`
		// collection has no name for something that is not installed. Eight of
		// the fourteen shipped rows were exactly that on the first render.
		//
		// Kept anyway when the user has touched it, or when they were just in
		// it: an override is a deliberate statement about an app, and the ring
		// only ever contains apps that genuinely ran.
		if r.Shipped && !r.Overridden && !h.appIsInstalled(r.BundleID) {
			if _, recent := rank[r.BundleID]; !recent {
				continue
			}
		}
		rows[r.BundleID] = views.AppProfileRowView{
			BundleID: r.BundleID, Profile: r.Profile,
			Shipped: r.Shipped, Overridden: r.Overridden, Via: r.Via,
		}
	}
	for _, id := range recent {
		r, ok := rows[id]
		if !ok {
			// Resolves to prose; the row exists so it can be changed, not
			// because anything has an opinion about it yet.
			r = views.AppProfileRowView{BundleID: id, Profile: h.appProfile(id)}
		}
		r.Recent = true
		rows[id] = r
	}

	out := make([]views.AppProfileRowView, 0, len(rows))
	for _, r := range rows {
		r.Name = h.appDisplayName(r.BundleID)
		out = append(out, r)
	}
	// Recent first, in ring order — the app you just came from is the one you
	// are here to fix — then everything else by display name.
	sort.SliceStable(out, func(i, j int) bool {
		ri, iOK := rank[out[i].BundleID]
		rj, jOK := rank[out[j].BundleID]
		if iOK != jOK {
			return iOK
		}
		if iOK && jOK && ri != rj {
			return ri < rj
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// --- Appearance settings ---

func renderHudSettings(conf hud.HudConfig) string {
	data := views.HudSettingsData{
		IsSmall:          conf.FontSize == "small",
		IsDefault:        conf.FontSize == "default",
		IsLarge:          conf.FontSize == "large",
		ShowBadges:       conf.ShowBadges,
		CompactMode:      conf.CompactMode,
		Opacity:          int(conf.Opacity),
		BlurAmount:       conf.BlurAmount,
		CornerRadius:     conf.CornerRadius,
		MainEnabled:      conf.IsChannelEnabled("main"),
		DiscoveryEnabled: conf.IsChannelEnabled("discovery"),
	}
	return views.Render(views.HudSettings(data))
}

func (h *Host) handleSetFontSize(req *struct {
	FontSize string `json:"font_size"`
}) error {
	switch req.FontSize {
	case "small", "default", "large":
	default:
		return fmt.Errorf("invalid font_size")
	}
	_, err := h.hud.SetUserField("font_size", req.FontSize)
	if err != nil {
		return fmt.Errorf("hud config relay: %w", err)
	}
	// The tab redraws through the re-render stream — the proxy answers 204
	// and discards whatever a method returns.
	return nil
}

func (h *Host) handleSetShowBadges(req *struct {
	Enabled bool `json:"enabled"`
}) error {
	_, err := h.hud.SetUserField("show_badges", req.Enabled)
	if err != nil {
		return fmt.Errorf("hud config relay: %w", err)
	}
	// The tab redraws through the re-render stream — the proxy answers 204
	// and discards whatever a method returns.
	return nil
}

func (h *Host) handleSetCompactMode(req *struct {
	Enabled bool `json:"enabled"`
}) error {
	_, err := h.hud.SetUserField("compact_mode", req.Enabled)
	if err != nil {
		return fmt.Errorf("hud config relay: %w", err)
	}
	// The tab redraws through the re-render stream — the proxy answers 204
	// and discards whatever a method returns.
	return nil
}

func (h *Host) handleSetHudOpacity(req *struct {
	Value int `json:"value"`
}) error {
	v := req.Value
	if v < 50 {
		v = 50
	} else if v > 100 {
		v = 100
	}
	_, err := h.hud.SetUserField("opacity", float64(v))
	if err != nil {
		return fmt.Errorf("hud config relay: %w", err)
	}
	// The tab redraws through the re-render stream — the proxy answers 204
	// and discards whatever a method returns.
	return nil
}

func (h *Host) handleSetHudBlur(req *struct {
	Value int `json:"value"`
}) error {
	v := req.Value
	if v < 0 {
		v = 0
	} else if v > 60 {
		v = 60
	}
	_, err := h.hud.SetUserField("blur_amount", v)
	if err != nil {
		return fmt.Errorf("hud config relay: %w", err)
	}
	// The tab redraws through the re-render stream — the proxy answers 204
	// and discards whatever a method returns.
	return nil
}

func (h *Host) handleSetHudCornerRadius(req *struct {
	Value int `json:"value"`
}) error {
	v := req.Value
	if v < 0 {
		v = 0
	} else if v > 30 {
		v = 30
	}
	_, err := h.hud.SetUserField("corner_radius", v)
	if err != nil {
		return fmt.Errorf("hud config relay: %w", err)
	}
	// The tab redraws through the re-render stream — the proxy answers 204
	// and discards whatever a method returns.
	return nil
}

func (h *Host) handleSetHudChannel(req *struct {
	Channel string `json:"channel"`
	Enabled bool   `json:"enabled"`
}) error {
	switch req.Channel {
	case hud.ChannelMain, hud.ChannelDiscovery:
	default:
		return fmt.Errorf("unknown channel: %s", req.Channel)
	}
	// Whole-map relay: the patch replaces the disabled_channels field.
	disabled := map[string]bool{}
	for ch, off := range h.hud.Config().DisabledChannels {
		disabled[ch] = off
	}
	if req.Enabled {
		delete(disabled, req.Channel)
	} else {
		disabled[req.Channel] = true
	}
	_, err := h.hud.SetUserField("disabled_channels", disabled)
	if err != nil {
		return fmt.Errorf("hud config relay: %w", err)
	}
	if !req.Enabled {
		h.plugin.HUDHide(branchkit.HUDHideRequest{Channel: req.Channel})
		if req.Channel == hud.ChannelMain {
			h.hud.SetHudOpen(false)
		}
	}
	// The tab redraws through the re-render stream — the proxy answers 204
	// and discards whatever a method returns.
	return nil
}

// --- Calibration stats ---

// aggregatePhoneticStats returns pass/total counts and misheard-as
// words per codeword for the Phonetics settings page.
//
// The calibration rework replaced the old
// `calibration-history.jsonl` read path — nothing writes that file
// anymore. The same per-codeword aggregate now derives from
// `verification_attempts.jsonl` (via `loadVerificationAttempts`)
// which the calibration finish path writes one entry per word into.
func (h *Host) aggregatePhoneticStats() (map[string][2]int, map[string][]string) {
	stats := make(map[string][2]int)
	misheardSets := make(map[string]map[string]bool)
	attempts := h.loadVerificationAttempts()
	for _, a := range attempts {
		if a.Target == "" {
			continue
		}
		e := stats[a.Target]
		e[1]++
		if a.Matched {
			e[0]++
		}
		stats[a.Target] = e

		// Collect every distinct misheard value: the final transcript
		// AND the first-heard partial (useful when the recognizer corrected
		// itself mid-utterance — both forms are diagnostic).
		for _, heard := range []string{a.FirstHeard, a.Heard} {
			if heard == "" || heard == a.Target {
				continue
			}
			if misheardSets[a.Target] == nil {
				misheardSets[a.Target] = make(map[string]bool)
			}
			misheardSets[a.Target][heard] = true
		}
	}

	misheard := make(map[string][]string, len(misheardSets))
	for word, set := range misheardSets {
		for h := range set {
			misheard[word] = append(misheard[word], h)
		}
		sort.Strings(misheard[word])
	}
	return stats, misheard
}

// toViewTallies maps the calibration occurrence-counted mishear trail into the
// view shape (keeps the views package free of a calibration import).
func toViewTallies(t []calibration.HeardTally) []views.CalHeardTally {
	if len(t) == 0 {
		return nil
	}
	out := make([]views.CalHeardTally, len(t))
	for i, h := range t {
		out[i] = views.CalHeardTally{Text: h.Text, Count: h.Count}
	}
	return out
}

// frictionBlurb formats a word's observed friction for the Reports word
// diagnostics line. Returns "" for words that land cleanly (nothing to show);
// otherwise "<per-attempt>/try · N mishears[, M silent]".
func frictionBlurb(fs calibration.FrictionStat) string {
	if fs.Mishears == 0 && fs.SilentHolds == 0 {
		return ""
	}
	mishearWord := "mishears"
	if fs.Mishears == 1 {
		mishearWord = "mishear"
	}
	s := fmt.Sprintf("%.1f/try · %d %s", fs.PerAttempt(), fs.Mishears, mishearWord)
	if fs.SilentHolds > 0 {
		s += fmt.Sprintf(", %d silent", fs.SilentHolds)
	}
	return s
}

// attemptsReadLimit bounds how many recent verification attempts are pulled in
// one read — now a UI window, not a survival measure.
//
// It used to be the latter, and the history is worth keeping: the log is
// append-only and unbounded, and reading it whole returned one JSON-RPC
// response that, past ~1MB, overflowed the SDK's stdin line buffer and killed
// the plugin. 250 was picked to stay "comfortably under" that cap assuming
// ~2.5KB per attempt.
//
// Both halves of that stopped holding. The SDK no longer has a line cap
// (plugin-sdk-go 3740f31 — an oversized frame is dispatched, not fatal), and
// the per-attempt estimate drifted as the grammar grew: a quarter of records
// carry the full union snapshot, up to 676 words, so 250 records measured
// 1,054,035 bytes on 2026-08-17 rather than the ~625KB the estimate implied.
//
// What actually makes this read heavy is `grammar_snapshot` on every attempt —
// 1,038,999 of those bytes, 88% of the payload, and a quarter of records carry
// the full union (up to 676 words). It belongs once per verification SESSION,
// alongside the other per-session context, not copied onto every attempt.
const attemptsReadLimit = 250

// loadVerificationAttempts reads the most recent verification attempts (bounded
// by attemptsReadLimit, newest-first) from the collection system.
func (h *Host) loadVerificationAttempts() []calibration.VerificationAttempt {
	entries, err := h.plugin.ListLog("plugin.voice.verification_attempts", branchkit.NewLogListOpts().Limit(attemptsReadLimit).Build())
	if err != nil {
		return nil
	}
	attempts := make([]calibration.VerificationAttempt, 0, len(entries))
	for _, e := range entries {
		var a calibration.VerificationAttempt
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			continue
		}
		attempts = append(attempts, a)
	}
	return attempts
}

// loadVerificationSessions reads the most recent verification session records
// (bounded, newest-first) — same unbounded-log-over-stdio hazard as the attempts
// read, so it's capped to keep the response under the SDK's line limit.
func (h *Host) loadVerificationSessions() []calibration.VerificationSession {
	entries, err := h.plugin.ListLog("plugin.voice.verification_sessions", branchkit.NewLogListOpts().Limit(attemptsReadLimit).Build())
	if err != nil {
		return nil
	}
	sessions := make([]calibration.VerificationSession, 0, len(entries))
	for _, e := range entries {
		var s calibration.VerificationSession
		if err := json.Unmarshal(e.Payload, &s); err != nil {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions
}

// clipFragilityReadLimit bounds the per-clip history read. The log grows as
// clips × probe-runs; an unbounded read would overflow the SDK's stdin line
// buffer (the crash that bit the verification logs). Each record is tiny (~200B),
// so this window stays well under the cap while covering the recent history the
// surfaces need.
const clipFragilityReadLimit = 1500

// loadClipFragility reads the recent per-clip fragility history (bounded,
// newest-first) from the append-only log.
func (h *Host) loadClipFragility() []calibration.ClipFragilityRecord {
	entries, err := h.plugin.ListLog(calibration.ClipFragilityCollection, branchkit.NewLogListOpts().Limit(clipFragilityReadLimit).Build())
	if err != nil {
		return nil
	}
	recs := make([]calibration.ClipFragilityRecord, 0, len(entries))
	for _, e := range entries {
		var r calibration.ClipFragilityRecord
		if err := json.Unmarshal(e.Payload, &r); err != nil {
			continue
		}
		recs = append(recs, r)
	}
	return recs
}

// loadClipBiasScore reads the append-only bias-score log (one entry per
// clip × scoring-run). Mirrors loadClipFragility.
func (h *Host) loadClipBiasScore() []calibration.ClipBiasRecord {
	entries, err := h.plugin.ListLog(calibration.BiasScoreCollection, branchkit.NewLogListOpts().Limit(clipFragilityReadLimit).Build())
	if err != nil {
		return nil
	}
	recs := make([]calibration.ClipBiasRecord, 0, len(entries))
	for _, e := range entries {
		var r calibration.ClipBiasRecord
		if err := json.Unmarshal(e.Payload, &r); err != nil {
			continue
		}
		recs = append(recs, r)
	}
	return recs
}

// misspokeFiles returns the set of clip files the user marked "said it wrong" —
// read from the corpus, where dispositions live. Those clips are excluded from the
// per-target fragility verdict (their reading is misspoken audio, not the engine).
// Empty on read failure: fail open, judge on what we have.
func misspokeFiles() map[string]bool {
	out := map[string]bool{}
	for _, r := range calibration.ReadCorpus() {
		if r.Disposition == calibration.DispositionMisspoke {
			out[r.File] = true
		}
	}
	return out
}

// latestFragilityByTarget returns the per-target fragility verdict, DERIVED from
// the latest per-clip readings — the per-clip log is the measured truth, the
// target verdict is a denoised rollup over it. Keyed by lowercased target.
func (h *Host) latestFragilityByTarget() map[string]calibration.WordFragility {
	return fragilityByTargetExcluding(h.loadClipFragility(), misspokeFiles())
}

// fragilityByTarget is latestFragilityByTarget over already-loaded records, so a
// caller that needs both the per-target rollup AND the per-clip detail reads the
// append-only log once.
func fragilityByTarget(records []calibration.ClipFragilityRecord) map[string]calibration.WordFragility {
	return fragilityByTargetExcluding(records, nil)
}

// fragilityByTargetExcluding rolls per-clip readings up to a per-target verdict,
// dropping clips in `excluded` — those the user marked "my mistake". Their reading
// measures misspoken audio, not the engine, so they must not skew the word's
// verdict (even though the clip itself stays visible, dimmed, in the list).
func fragilityByTargetExcluding(records []calibration.ClipFragilityRecord, excluded map[string]bool) map[string]calibration.WordFragility {
	if len(excluded) > 0 {
		kept := make([]calibration.ClipFragilityRecord, 0, len(records))
		for _, r := range records {
			if !excluded[r.File] {
				kept = append(kept, r)
			}
		}
		records = kept
	}
	latest := calibration.LatestClipPerFile(records)
	if len(latest) == 0 {
		return map[string]calibration.WordFragility{}
	}
	byTarget := map[string][]calibration.ClipFragilityRecord{}
	for _, r := range latest {
		if r.Target == "" {
			continue
		}
		key := strings.ToLower(r.Target)
		byTarget[key] = append(byTarget[key], r)
	}
	out := make(map[string]calibration.WordFragility, len(byTarget))
	for key, clips := range byTarget {
		out[key] = calibration.RollupClips(clips[0].Target, clips)
	}
	return out
}

// fragilityConfuserWords returns just the word(s) this word slips to (the
// nearest confusers), e.g. "FIRST" or "BACK / BRANCH". Empty when nothing
// confuses it (a robust word).
func fragilityConfuserWords(w calibration.WordFragility) string {
	var parts []string
	for _, c := range w.Constituents {
		if c.NearestConfuser != "" {
			parts = append(parts, c.NearestConfuser)
		}
	}
	return strings.Join(parts, " / ")
}

// wordLifetimeAccuracy returns matched/total per target across all attempts,
// keyed by lowercased target.
func wordLifetimeAccuracy(attempts []calibration.VerificationAttempt) map[string][2]int {
	out := map[string][2]int{}
	for _, a := range attempts {
		key := strings.ToLower(a.Target)
		v := out[key]
		v[1]++
		if a.Matched {
			v[0]++
		}
		out[key] = v
	}
	return out
}

// buildFragilityWords folds the fragility log into the Word Fragility panel rows,
// keeping the most recent readout per target and ranking most-fragile first.
func (h *Host) buildFragilityWords() []views.FragilityWordView {
	byWord := h.latestFragilityByTarget()
	if len(byWord) == 0 {
		return nil
	}
	words := make([]calibration.WordFragility, 0, len(byWord))
	for _, w := range byWord {
		words = append(words, w)
	}

	rows := make([]views.FragilityWordView, 0, len(words))
	for _, w := range calibration.RankFragility(words) {
		rows = append(rows, views.FragilityWordView{
			Target:    w.Target,
			Confusers: fragilityConfuserText(w),
			Status:    fragilityStatus(w),
			Detail:    fragilityDetail(w),
		})
	}
	return rows
}

// fragilityStatus is the per-target verdict, worst-first: a word that mishears
// even clean is "broken"; one that breaks to varying words is "unstable" (the
// scatter signal, formerly "mushy"); one that breaks to a steady confuser is
// "fragile"; one that survives the whole noise ladder is "robust". Checked
// broken-before-unstable because an all-fail-clean word also trips the
// no-steady-confuser test.
func fragilityStatus(w calibration.WordFragility) string {
	switch {
	case w.Held:
		return "robust"
	case w.Broken:
		return "broken"
	case w.Mushy:
		return "unstable"
	default:
		return "fragile"
	}
}

func fragilityDetail(w calibration.WordFragility) string {
	switch {
	case w.Held:
		return fmt.Sprintf("held the full noise ladder (%d/%d)", w.Reps, w.Reps)
	case w.Broken:
		return fmt.Sprintf("mishears even at zero noise (0/%d held)", w.Reps)
	case w.Mushy:
		return fmt.Sprintf("breaks to varying words · %d/%d held", w.HeldReps, w.Reps)
	case !math.IsNaN(w.FallSNRdB):
		return fmt.Sprintf("breaks at ~%.0f dB SNR · %d/%d held", w.FallSNRdB, w.HeldReps, w.Reps)
	default:
		return fmt.Sprintf("%d/%d held", w.HeldReps, w.Reps)
	}
}

// verdictRatio is the held/total magnitude shown next to the badge — how many of
// the target's clips survived the full noise test ("3/5" = 3 solid, 2 broke).
func verdictRatio(w calibration.WordFragility) string {
	return fmt.Sprintf("%d/%d", w.HeldReps, w.Reps)
}

// fragilityConfuserText joins the per-constituent nearest confusers a reader can
// act on, with how consistent each slip was, e.g. "BAT→BACK (3/3), QUENCH→BRANCH
// (2/3)". The (n/m) is: of the m reps where that word slipped, n slipped to the
// SAME confuser — a high ratio is a reliable warning. Skips constituents that held.
func fragilityConfuserText(w calibration.WordFragility) string {
	var parts []string
	for _, c := range w.Constituents {
		if c.NearestConfuser == "" {
			continue
		}
		p := c.Word + "→" + c.NearestConfuser
		if c.RepsDiverged > 0 {
			n := int(math.Round(c.Consistency * float64(c.RepsDiverged)))
			p += fmt.Sprintf(" (%d/%d)", n, c.RepsDiverged)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// buildReportsSessions loads all sessions + attempts and folds them
// into the template-friendly shape for the Reports surface. Reverse-
// chronological. Skipped (returns nil) when not reviewing — the data
// is meaningful only on that view.
func (h *Host) buildReportsSessions(reviewing bool) []views.ReportsSessionView {
	if !reviewing {
		return nil
	}
	sessions := h.loadVerificationSessions()
	attempts := h.loadVerificationAttempts()
	if len(sessions) == 0 {
		return nil
	}

	// Bucket attempts by session_id once so per-session lookup is O(1).
	attemptsBySession := make(map[string][]calibration.VerificationAttempt, len(sessions))
	for _, a := range attempts {
		attemptsBySession[a.SessionID] = append(attemptsBySession[a.SessionID], a)
	}

	// Word-level diagnostics joined onto each row: lifetime accuracy (always
	// available) + the fragility confuser (present once the word's audio was
	// captured + analyzed). Both keyed by lowercased target.
	fragByWord := h.latestFragilityByTarget()
	accByWord := wordLifetimeAccuracy(attempts)
	frictionByWord := calibration.ComputeFriction(attempts)

	// Reverse-chronological: parse timestamps once, sort desc.
	type indexed struct {
		idx int
		ts  time.Time
	}
	order := make([]indexed, 0, len(sessions))
	for i, s := range sessions {
		t, _ := time.Parse(time.RFC3339, s.Timestamp)
		order = append(order, indexed{idx: i, ts: t})
	}
	sort.Slice(order, func(i, j int) bool {
		return order[i].ts.After(order[j].ts)
	})

	rows := make([]views.ReportsSessionView, 0, len(sessions))
	for _, o := range order {
		s := sessions[o.idx]
		dateStr := ""
		dateRel := ""
		if !o.ts.IsZero() {
			dateStr = o.ts.Format("2006-01-02 15:04")
			days := int(time.Since(o.ts).Hours() / 24)
			switch {
			case days == 0:
				dateRel = "today"
			case days == 1:
				dateRel = "yesterday"
			default:
				dateRel = fmt.Sprintf("%d days ago", days)
			}
		}
		score := "—"
		if s.WordCount > 0 {
			score = fmt.Sprintf("%.0f%%", s.ScorePct)
		}
		engine := s.Engine
		if s.ModelID != "" {
			engine = fmt.Sprintf("%s · %s", s.Engine, s.ModelID)
		}
		group := s.Group
		if group == "" {
			group = s.Source
		}

		// Build attempts for this session in their natural order
		// (the JSONL is append-ordered = recognition-order).
		sessionAttempts := attemptsBySession[s.ID]
		attemptViews := make([]views.ReportsAttemptView, 0, len(sessionAttempts))
		for _, a := range sessionAttempts {
			dur := "—"
			if a.DurationMs > 0 {
				dur = fmt.Sprintf("%dms", a.DurationMs)
			}
			av := views.ReportsAttemptView{
				Target:          a.Target,
				Heard:           a.Heard,
				Matched:         a.Matched,
				MatcherRejected: a.MatcherRejected,
				Duration:        dur,
				HeardTallies:    toViewTallies(a.HeardTallies),
				SilentHolds:     a.SilentHolds,
			}
			key := strings.ToLower(a.Target)
			if f, ok := fragByWord[key]; ok {
				av.Confuser = fragilityConfuserWords(f)
				av.FragilityStatus = fragilityStatus(f)
			}
			if acc := accByWord[key]; acc[1] > 1 {
				av.Accuracy = fmt.Sprintf("%.0f%%", float64(acc[0])/float64(acc[1])*100)
				av.AccuracyRuns = acc[1]
			}
			if fs, ok := frictionByWord[key]; ok {
				av.Friction = frictionBlurb(fs)
			}
			if a.MatcherDecision != nil {
				av.HasMatcherDecision = true
				av.MatcherWinner = a.MatcherDecision.Winner
				av.MatcherGates = a.MatcherDecision.ActiveGates
				av.MatcherHasCompletions = a.MatcherDecision.HasCompletions
				av.MatcherNextWords = a.MatcherDecision.NextWords
			}
			attemptViews = append(attemptViews, av)
		}

		duration := "—"
		if s.ElapsedSecs > 0 {
			duration = fmt.Sprintf("%.1fs", s.ElapsedSecs)
		}

		rows = append(rows, views.ReportsSessionView{
			Date:       dateStr,
			DateRel:    dateRel,
			Group:      group,
			Score:      score,
			WordCount:  s.WordCount,
			MatchCount: s.MatchCount,
			Duration:   duration,
			Engine:     engine,
			Mode:       s.Mode,
			Context:    s.Context,
			Attempts:   attemptViews,
		})
	}
	return rows
}

// buildReportsRollups aggregates attempts across all sessions, grouped
// by context, to surface systemic patterns at the top of Reports.
// Identity for a context is:
//   - dynamic sessions: session.Context (the friendly name set at trial-spec build time)
//   - static sessions: session.Group (falls back to session.Source)
//
// Empty-string contexts are skipped — a corrupt or pre-feature record
// shouldn't anchor a rollup row. Sort order is recency-of-last-session
// desc: the most recently exercised context sits first so the rollup
// reads like "what you've been practicing."
func (h *Host) buildReportsRollups(reviewing bool) []views.ReportsContextRollupView {
	if !reviewing {
		return nil
	}
	sessions := h.loadVerificationSessions()
	if len(sessions) == 0 {
		return nil
	}
	attempts := h.loadVerificationAttempts()

	type bucket struct {
		mode     string
		sessions map[string]bool
		latestTs time.Time
		attempts int
		matched  int
		filter   int
		miss     int
	}
	buckets := make(map[string]*bucket)

	contextKey := func(s calibration.VerificationSession) string {
		if s.Mode == "dynamic" && s.Context != "" {
			return s.Context
		}
		if s.Group != "" {
			return s.Group
		}
		return s.Source
	}

	// Index session_id → context so attempts can find their bucket
	// in one pass without re-deriving the key on each attempt.
	sessionContext := make(map[string]string, len(sessions))
	sessionMode := make(map[string]string, len(sessions))
	for _, s := range sessions {
		ctx := contextKey(s)
		if ctx == "" {
			continue
		}
		sessionContext[s.ID] = ctx
		sessionMode[s.ID] = s.Mode
		b, ok := buckets[ctx]
		if !ok {
			b = &bucket{sessions: make(map[string]bool)}
			buckets[ctx] = b
		}
		// Dynamic mode wins if any session under this context was dynamic
		// — happens for browser.hints where every session is dynamic and
		// the badge should always read "realistic".
		if s.Mode == "dynamic" {
			b.mode = "dynamic"
		} else if b.mode == "" {
			b.mode = s.Mode
		}
		b.sessions[s.ID] = true
		if ts, err := time.Parse(time.RFC3339, s.Timestamp); err == nil && ts.After(b.latestTs) {
			b.latestTs = ts
		}
	}

	for _, a := range attempts {
		ctx := sessionContext[a.SessionID]
		if ctx == "" {
			continue
		}
		b := buckets[ctx]
		b.attempts++
		switch {
		case a.Matched:
			b.matched++
		case a.MatcherRejected:
			b.filter++
		default:
			b.miss++
		}
	}

	rows := make([]views.ReportsContextRollupView, 0, len(buckets))
	for ctx, b := range buckets {
		acc := "—"
		if b.attempts > 0 {
			acc = fmt.Sprintf("%.0f%%", 100.0*float64(b.matched)/float64(b.attempts))
		}
		latestRel := ""
		if !b.latestTs.IsZero() {
			days := int(time.Since(b.latestTs).Hours() / 24)
			switch {
			case days == 0:
				latestRel = "today"
			case days == 1:
				latestRel = "yesterday"
			default:
				latestRel = fmt.Sprintf("%d days ago", days)
			}
		}
		rows = append(rows, views.ReportsContextRollupView{
			Context:           ctx,
			Mode:              b.mode,
			SessionCount:      len(b.sessions),
			AttemptCount:      b.attempts,
			MatchCount:        b.matched,
			AcousticMissCount: b.miss,
			FilterRejectCount: b.filter,
			AccuracyPct:       acc,
			LatestRel:         latestRel,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		// Recency-first; ties broken by AttemptCount then Context name
		// so the order is stable across renders without timestamps.
		ri, rj := rows[i], rows[j]
		bi := buckets[ri.Context].latestTs
		bj := buckets[rj.Context].latestTs
		if !bi.Equal(bj) {
			return bi.After(bj)
		}
		if ri.AttemptCount != rj.AttemptCount {
			return ri.AttemptCount > rj.AttemptCount
		}
		return ri.Context < rj.Context
	})
	return rows
}
