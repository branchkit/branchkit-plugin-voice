package views

import (
	"fmt"
	"strings"

	"github.com/a-h/templ"
	"github.com/branchkit/plugin-sdk-go"
)

// Render is this plugin's error policy over the SDK's RenderComponent: a
// component that fails to render logs and draws nothing. Settings tabs and
// HUD fragments share it, so a HUD render never propagates an error into a
// push.
func Render(c templ.Component) string {
	html, err := branchkit.RenderComponent(c)
	if err != nil {
		branchkit.Logf("voice", "templ render error: %v", err)
	}
	return html
}

// --- Template helpers ---

// dispoToggle returns the disposition to send when a clip's mark button is
// clicked: clicking the already-active verdict clears it (""), otherwise it
// sets the clicked one.
func dispoToggle(current, value string) string {
	if current == value {
		return ""
	}
	return value
}

// slugify converts a string to a safe HTML id: lowercase, non-alphanumeric -> hyphens, collapse runs.
func slugify(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			lastHyphen = false
		} else if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// --- HUD Settings ---

type HudSettingsData struct {
	IsSmall          bool
	IsDefault        bool
	IsLarge          bool
	ShowBadges       bool
	CompactMode      bool
	Opacity          int
	BlurAmount       int
	CornerRadius     int
	MainEnabled      bool
	DiscoveryEnabled bool
}

// --- Calibration ---

type CalibrationViewData struct {
	IsActive bool
	// IsReviewing is true when the Reports surface is active (Phase ==
	// Reviewing). The template swaps the catalog body for the reports body when
	// this is set. Reports renders ReportsSessions as a chronological list of
	// past sessions with inline detail.
	IsReviewing bool
	// ReportsSessions is the full list of past calibration sessions,
	// reverse-chronological, with attempts attached inline for
	// expandable detail. Populated only when IsReviewing is true.
	ReportsSessions []ReportsSessionView
	// ReportsRollups aggregates attempts by context (the friendly
	// dynamic-mode name, or the static group/source). One row per
	// context, sorted by recency of last session. Populated only
	// when IsReviewing is true.
	ReportsRollups []ReportsContextRollupView
	HasPhonetics   bool
	CurrentWord    string
	CurrentIndex   int
	TotalWords     int
	MatchedCount   int
	ElapsedSecs    string
	HeardBuffer    string
	MatchStatus    bool
	// CommandKey is the command hold key in this OS's words ("⌥G", "Alt+G").
	CommandKey   string
	FrameCount   int
	TrialElapsed string
	CanGoBack    bool

	// ShowBanner is the post-stop "a sitting just closed" state: the catalog is
	// shown with a "Tested N commands this session · View report" banner.
	// BannerTested is N (distinct commands/codewords with a real attempt).
	ShowBanner   bool
	BannerTested int

	// CaptureSupported is true when the active engine can capture trial audio
	// (Sherpa offline); CaptureEnabled mirrors the user's opt-in toggle. Drive the
	// "Word Fragility Capture" control on the calibration tab.
	CaptureSupported bool
	CaptureEnabled   bool
	// CaptureRetention is the current retention policy
	// ("delete_after_analysis"|"keep_bounded"|"keep_all").
	CaptureRetention string
	// RecordingGroups is the kept-clip list shown under the capture controls,
	// grouped by target (each group carries the derived fragility verdict), built
	// only on the idle surface so it doesn't re-render mid-walk. RecordingsShown
	// is the number of clips actually rendered; RecordingsTotal is the full kept
	// count — if it exceeds RecordingsShown the list was capped for display (shown
	// explicitly, never silently truncated).
	RecordingGroups []RecordingGroup
	RecordingsShown int
	RecordingsTotal int
	// RecordingsFlat is the same clips ungrouped, newest-first across all targets —
	// rendered instead of the groups when RecordingsView is "recent". RecordingsView
	// is "grouped" (default) or "recent"; it drives which body shows + the toggle's
	// active state.
	RecordingsFlat []RecordingView
	RecordingsView string
	// ExportStatus is the result line from the most recent clip export ("Exported
	// N clips → path" or an error), shown under the recordings controls.
	ExportStatus string

	// BiasRecommendLabel is the corpus-level bias strength recommendation
	// readout ("recommend 8 — mishears flip by 6, correct commands hold to 16");
	// "" when no bias scores exist yet. BiasRecommendStrength carries the
	// recommended strength for the Apply payload; the button renders only when
	// BiasRecommendOK. BiasApplyStatus is transient feedback from the last apply
	// attempt, and BiasApplyNeedsConfirm re-labels the button "Replace manual
	// setting" after an apply was refused over a manually-set value.
	BiasRecommendLabel    string
	BiasRecommendStrength string
	BiasRecommendOK       bool
	BiasApplyStatus       string
	BiasApplyNeedsConfirm bool

	// The bias setting itself (read via recognition.bias.get) — the manual
	// control moved here from the platform commands page 2026-09-11:
	// the knob lives with the evidence
	// that recommends values. BiasProvenance is the human line ("manually
	// set 2026-07-05" / "set by calibration" / "default").
	BiasEnabled    bool
	BiasStrength   string
	BiasProvenance string

	// AutoAdvance mirrors the calibration auto-advance preference: when on, a
	// target heard correctly advances to the next on its own (no "Next" click),
	// chaining command-to-command. Drives the toggle in the catalog header + the
	// active-walk controls.
	AutoAdvance bool

	// Catalog is the always-on command sidebar — every command grouped by owner,
	// each row carrying a glanceable health summary. Selecting a row activates
	// its trial directly. Empty when no commands are enumerable.
	Catalog []CatalogGroup
	// ActiveCmd is the command id the live walk's cursor sits on ("" when no walk
	// is active). Drives the sidebar's active-row highlight.
	ActiveCmd string
	// ActiveDetail is the right-pane health readout for ActiveCmd, shown above the
	// live trial card. nil when no walk is active (or a bare-codeword target).
	ActiveDetail *CommandDetail

	// DockedClips is the working set: kept clips whose target matches the
	// selected command, rendered under the stage so the test→clip loop never
	// leaves the tab. DockedFor names the command they belong to. The global
	// library lives on the Recordings tab.
	DockedClips []RecordingGroup
	DockedFor   string

	// GroupTargets is the second column shown while a multi-target command is
	// active: every target inside the current command's expansion group (the hint
	// codeword cross product, a list slot's entries) with its done/current/
	// upcoming status + two-signal result. Empty for a single-target (base /
	// mode-gated) command, which has no group to list.
	GroupTargets []CalGroupTarget
	// CurrentTier is the current target's tier ("base"|"mode_gated"|"dynamic"|
	// "slotted") for the live trial card's context badge.
	CurrentTier string
	// CurrentRetuneKey is the current target's bound vocabulary surface form (a
	// codeword, a number/direction/app word) when it's retunable, "" otherwise.
	// Non-empty gates the in-walk retune affordance — edit the failing word and
	// retest it on the spot — and pre-fills the edit box.
	CurrentRetuneKey string
	// CurrentGroup is the cleaned label of the current target's expansion group
	// ("focus apps") when it's part of one, "" otherwise; CurrentGroupPos /
	// CurrentGroupTotal place it within that group (e.g. 3 of 100). Drives the
	// "you're in a contained group, skip it as a unit" affordance.
	CurrentGroup      string
	CurrentGroupPos   int
	CurrentGroupTotal int

	// Bias is the live Level-1 never-standalone counterfactual for the current
	// attempt, or nil when there's none to show (no attempt yet, capture off, a
	// non-Sherpa engine, or the bias didn't change the decode). When set, the
	// trial card shows "bias: <off> → <on>".
	Bias *BiasCounterfactual
}

// BiasCounterfactual is the live Level-1 never-standalone readout on the trial
// card: Off is what the just-captured clip decodes to with the penalty disabled,
// On is the live biased result the pipeline produced. Built only when the bias
// changed the decode (Off != On), so its mere presence means "the penalty flipped
// this one".
type BiasCounterfactual struct {
	Off string
	On  string
}

// RecordingView is one kept calibration clip in the Recordings list: the word
// you were asked to say, what the engine decoded for that clip (with a match
// flag), and when it was recorded.
type RecordingView struct {
	Word    string
	Heard   string
	Matched bool
	When    string // human-readable date/time
	// File is the capture-root-relative path (e.g. "<game>/seg_7.wav"), used as
	// the set_disposition key. AudioURL is the same path wired into the iframe
	// audio route for the <audio> element. Labeled is true when the clip has a
	// corpus row (a ground-truth target) — only then can it be scored/marked.
	File        string
	AudioURL    string
	Labeled     bool
	Disposition string // ""|confirmed|misspoke (the human review verdict)
	// NeedsReview is true for a fragile/fails-clean clip with no review verdict yet
	// — the clip worth listening to and judging (said right vs. said wrong).
	NeedsReview bool

	// Per-clip fragility (the probe re-decodes each clip across a noise ladder).
	// FragOutcome is the latest probe verdict for THIS clip: "held"|"fragile"|
	// "fails_clean"|"" (unprobed). FragLabel is the one-line readout ("held",
	// "falls @ 9 dB", "fails clean → BACK QUENCH").
	FragOutcome string
	FragLabel   string
	// BiasScore is the latest bias strength-sweep readout for THIS clip ("no bias
	// needed" / "flips at 6 (four→page)" / "never (four)"), or "" when unscored.
	BiasScore string
	// Words are the per-constituent readings on the latest probe of this clip
	// (laddered clips only; a mishear carries none).
	Words []ClipWordView
	// History is this clip's re-probe history (one row per probe-run), newest
	// first — each stamped with the model it ran on, so a re-probe on a new model
	// is distinguishable.
	History []ClipHistoryView
}

// RecordingGroup is one target's section in the grouped Recordings list: the
// frozen prompt label + a fragility verdict DERIVED from that target's clips
// (robust/mushy/fragile), with the clips collapsed beneath it. The verdict is a
// denoised rollup over the per-clip readings, never a stored per-target row.
type RecordingGroup struct {
	Target        string // frozen prompt; "" for unlabeled clips (rendered "Unlabeled")
	Verdict       string // "robust"|"fragile"|"unstable"|"broken"|"" (unprobed)
	VerdictDetail string // qualifier shown as title text, e.g. "breaks at ~9 dB SNR · 1/3 held"
	VerdictRatio  string // held/total magnitude shown beside the badge, e.g. "3/5"
	ClipCount     int
	Clips         []RecordingView
}

// ClipWordView is one constituent's reading within a clip's latest probe: the
// word and whether it held under noise (Confuser is what it slipped to when it
// didn't).
type ClipWordView struct {
	Word     string
	Held     bool
	Confuser string // what it slipped to; "" when held
}

// ClipHistoryView is one past probe-run of a clip (newest first), stamped with
// the model version it ran on so a re-probe on a new model is distinguishable.
type ClipHistoryView struct {
	When    string // formatted date/time
	Model   string // model label, e.g. "nemo@a1b2c3d4"
	Summary string // outcome one-liner ("held", "BAT→BACK · falls @ 9 dB", "fails clean → BACK QUENCH")
}

// CalGroupTarget is one row of the active command's group rail (the second
// column): an expanded target with its status and inline two-signal result.
type CalGroupTarget struct {
	Prompt string
	Status string // "done" | "current" | "upcoming"
	Index  int    // walk index; a click jumps the cursor here (goto)
	// Heard / Resolves are the inline two-signal result for a done target:
	// "pass" | "fail" | "skip" | "" (Resolves is "" for codeword/dynamic targets
	// that have no functional signal).
	Heard    string
	Resolves string
}

// CatalogGroup is one owner's section of the calibration catalog.
type CatalogGroup struct {
	Owner  string
	Rows   []CatalogRow
	Rollup string // group accuracy rollup, e.g. "88%" or "—"
	Tested int
	Total  int
}

// CatalogRow is one command in the catalog, with a glanceable health summary.
type CatalogRow struct {
	ID       string // command id "<owner>:<pattern>"
	Pattern  string // display pattern
	Tier     string // "base"|"mode_gated"|"dynamic"|"slotted"
	Accuracy string // "92%" or "—" (never tested)
	Warn     bool   // accuracy below the attention threshold (glance flag)
	// Description is the command's one-line "what saying this does", from the
	// same declaration the Command Editor shows. The move-list row renders it
	// under the pattern.
	Description string
	// Children nests the commands this row's sets_tags gate — the combo tree
	// "snap" opens left/right/full/…, each rendered with its one-breath
	// form ("snap left"). Only mode entries carry children; one level deep.
	Children []CatalogRow
}

// RecordingsViewData drives the Recordings tab — the voice library as its own
// concept: consent and keep policy lead,
// the full clip library follows. Calibration keeps only the working set (the
// selected command's clips, docked under the stage).
type RecordingsViewData struct {
	CaptureSupported bool
	CaptureEnabled   bool
	CaptureRetention string
	RecordingGroups  []RecordingGroup
	RecordingsFlat   []RecordingView
	RecordingsView   string
	RecordingsShown  int
	RecordingsTotal  int
	ExportStatus     string

	BiasRecommendLabel    string
	BiasRecommendStrength string
	BiasRecommendOK       bool
	BiasApplyStatus       string
	BiasApplyNeedsConfirm bool

	// The bias setting itself (read via recognition.bias.get) — the manual
	// control moved here from the platform commands page 2026-09-11:
	// the knob lives with the evidence
	// that recommends values. BiasProvenance is the human line ("manually
	// set 2026-07-05" / "set by calibration" / "default").
	BiasEnabled    bool
	BiasStrength   string
	BiasProvenance string
}

// CommandDetail is the right-pane readout for the selected catalog command.
type CommandDetail struct {
	Pattern         string
	Owner           string
	Tier            string
	ContextLabel    string // why it needs setup: "base context" / "needs mode tags" / "hints fixture" / "slotted"
	Accuracy        string // "92% (24 runs)" / "never tested"
	Friction        string // "low" / "high" / "" (unknown)
	Fragility       string // "robust" / "mushy" / "fragile" / "" (unprobed)
	FragilityDetail string // "falls at ~9 dB SNR · held 1/3 reps" / "" — extra readout detail
	Confuser        string // nearest/observed confuser (with consistency), or ""
	LastTested      string // "today" / "3 days ago" / "never"
	// The miss anatomy: the newest failed
	// attempt's heard text, when it happened, and whether it was a filter
	// rejection (heard right, matcher refused) rather than an acoustic miss.
	// Empty when no failed attempt is on record.
	LastMissHeard    string
	LastMissWhen     string
	LastMissFiltered bool
	// Description is the command's "what it does / use case" text (empty when the
	// command definition has none); Category is its grouping label.
	Category string
}

// --- Reusable calibration components ---

type CalStatItem struct {
	Label string
	Value string
	Color string // optional CSS color override
}

// FragilityWordView is one row in the Word Fragility panel — a per-word
// perturbation-ladder readout from the Sherpa re-decode probe. Confusers
// pre-formats the per-constituent nearest confusers (e.g. "BAT→BACK,
// QUENCH→BRANCH"); Status is the headline ("robust" | "mushy" | "fragile");
// Detail is a short qualifier ("held the ladder", "falls early").
type FragilityWordView struct {
	Target    string
	Confusers string
	Status    string
	Detail    string
}

// ReportsSessionView is one row in the Reports session list. Carries
// summary stats + an inline collection of attempt details so the
// template can render an expandable <details> block per session
// without a server round-trip to load the inner attempts.
type ReportsSessionView struct {
	Date       string // pre-formatted ISO date for sorting/display
	DateRel    string // "today", "yesterday", "3 days ago"
	Group      string // pretty group label for display
	Score      string // "92%" or "—" if no attempts
	WordCount  int
	MatchCount int
	Duration   string // formatted elapsed time
	Engine     string // recognizer engine + model id
	Mode       string // "static" / "dynamic" (or legacy "sandbox" for pre-2026-05-27 records)
	Context    string // friendly target name when realistic-mode; empty otherwise
	Attempts   []ReportsAttemptView
}

// ReportsContextRollupView is one row in the by-context rollup at the
// top of the Reports surface. Aggregates every attempt belonging to a
// single context (static group or dynamic context name) across the
// full session history. The three-bucket count split (matched /
// filter-rejected / acoustic-miss) mirrors the per-attempt outcome
// indicators in the detail rows, so a user scanning the rollup can
// jump to a session expecting the same vocabulary of symbols.
type ReportsContextRollupView struct {
	Context           string // friendly name (e.g. "browser.hints" or "alphabet")
	Mode              string // "static" / "dynamic" — controls the realistic badge
	SessionCount      int
	AttemptCount      int
	MatchCount        int    // ✓ — passed
	AcousticMissCount int    // ✗ — heard != target
	FilterRejectCount int    // ⚠ — heard correctly but matcher rejected (dynamic only)
	AccuracyPct       string // "92%"
	LatestRel         string // "today" / "3 days ago"
}

// ReportsAttemptView is one attempt inside a session detail, built from a
// persisted VerificationAttempt rather than an in-memory Trial.
type ReportsAttemptView struct {
	Target  string
	Heard   string
	Matched bool
	// MatcherRejected distinguishes a filter-rejection failure (the
	// engine heard the codeword correctly but the matcher's active_tags
	// filter refused to resolve it — fixture didn't apply, tag state
	// wrong, etc.) from an acoustic failure (heard != target).
	// Populated from VerificationAttempt.MatcherRejected. The Reports
	// row distinguishes ✗ acoustic vs ⚠ filter-rejected.
	MatcherRejected bool
	// HasMatcherDecision is true when the attempt carries a structured
	// matcher decision (dynamic-mode trials with a successful resolve
	// RPC). Static-mode and resolve-failed trials leave it false and
	// the detail panel hides the decision section.
	HasMatcherDecision    bool
	MatcherWinner         string   // "gated_scoped" | "gated_unscoped" | "ungated" | "no_match"
	MatcherGates          []string // active scoped tags at decision time
	MatcherHasCompletions bool
	MatcherNextWords      []string
	Duration              string
	// HeardTallies is the occurrence-counted mishear trail — distinct heards in
	// first-seen order with repeat counts (last = settled). Rendered as the
	// sequence with "×N" on repeats. SilentHolds is the number of holds that
	// decoded to nothing, shown as a trailing "(no recognition) ×N".
	HeardTallies []CalHeardTally
	SilentHolds  int
	// Word-level diagnostics joined by target word (shown as a sub-line):
	// Confuser is what this word tends to slip to (from the fragility probe,
	// only present once captured); FragilityStatus is robust/fragile/mushy;
	// Accuracy is the word's lifetime pass rate ("90%") over AccuracyRuns runs.
	Confuser        string
	FragilityStatus string
	Accuracy        string
	AccuracyRuns    int
	// Friction is the observed per-person difficulty blurb for this word
	// ("3.0/try · 6 mishears, 2 silent"), shown when the word has been fought
	// with. Empty when the word lands cleanly. See ComputeFriction.
	Friction string
}

// CalHeardTally is a view row for the occurrence-counted mishear trail: a
// recognized value and how many times it came back across a trial.
type CalHeardTally struct {
	Text  string
	Count int
}

// --- Dictation ---

// MicOption is one connected input device in the Microphone menu.
type MicOption struct {
	UID  string
	Name string
}

type DictationViewData struct {
	// DictateKeys is the dictation chord as keycaps (⌥ T), so the page names
	// the key the user actually has bound.
	DictateKeys []string
	// Sounds is the start and stop sounds setting: "auto", "on" or "off".
	Sounds string
	// MicLabel names the microphone in use, for the one-line pointer to Get
	// started, where it is chosen.
	MicLabel string
	// Words is everything dictation is taught to spell: the vocabulary list
	// and the words correction rules write, one row each, with the
	// mishearings fix-a-word rules repair listed under their word.
	Words []WordRowView
	// RecentHTML is the recent-dictations list, rendered by the plugin.
	//
	// The same panel appears on the Option+B HUD, and the duplication is
	// deliberate rather than sloppy: the two answer different questions. The
	// HUD is for RECOVERY mid-work — "where did my text go" — and wants to be
	// one keystroke from wherever you are. This is for the deliberate pass:
	// a word fixed here appears under "Words it should know" above.
	RecentHTML string

	// SpokenPunctuation is the "Say punctuation" setting.
	SpokenPunctuation bool

	// Advanced: the machinery, folded away.
	FixAllCaps bool
	// StylesReady is whether the style model is downloaded, StyleRef the ref
	// that downloads it, and StyleRows the apps a style can be set for: the
	// recently used ones and every one that has a style.
	StylesReady bool
	StyleRef    string
	StyleRows   []AppStyleRowView
	// AddApps are the apps the Add row offers (addAppOptions), recent first.
	AddApps []AppOptionView
	// DefaultStyle is the style every app gets unless it is an exception, ""
	// for none.
	DefaultStyle string
	// Refine is whether the local-model clean-up step is on; RefineReady
	// whether its model is downloaded, and RefineRef the ref that downloads it.
	Refine      bool
	RefineReady bool
	RefineRef   string
	// RefineDownload and StyleDownload are each model's download while one
	// is running (Active), so the page shows its progress instead of a
	// Download button that seems to do nothing.
	RefineDownload ModelDownloadView
	StyleDownload  ModelDownloadView

	Corrections []CorrectionRowView
}

// WordRowView is one word under "Words it should know".
type WordRowView struct {
	Word       string
	Fixes      []WordFixView
	HasPattern bool
}

// WordFixView is one fix-a-word rule, shown under the word it writes.
type WordFixView struct {
	ID    string
	Heard string
}

// AppProfileRowView is one app's standing dictation profile.
type AppProfileRowView struct {
	BundleID string
	// Name is the human-readable app name, falling back to the bundle id.
	Name string
	// Profile is the RESOLVED value — override if set, else shipped, else prose.
	Profile string
	// Shipped means BranchKit ships an opinion for this app.
	Shipped bool
	// Via names the app trait the profile came from ("terminal"), so a
	// surprising row can explain itself. Empty when the user set the row, or
	// when nothing has an opinion and it resolves to prose.
	Via string
	// Overridden means the user has set this row, so it can be reset.
	Overridden bool
	// Recent means the app was focused recently; those sort first, because the
	// app you just came from is the one you are here to fix.
	Recent bool
}

type LanguageRowView struct {
	Label    string
	Code     string
	Selected bool
}

type CorrectionRowView struct {
	ID           string
	MatchPattern string
	Replacement  string
	IsRegex      bool
	Enabled      bool
	// Shipped marks a rule that came with the plugin rather than from the
	// user. Rendered so shipped POLICY is legible as policy — the
	// hallucination filter in particular, which deletes a transcript and had
	// previously been indistinguishable from something you typed and forgot.
	Shipped bool
}

// --- Phonetics ---

type PhoneticRowView struct {
	Letter        string
	Word          string
	IsOverride    bool
	ScoreClass    string
	ScoreText     string
	MisheardHint  string
	BadgeClass    string
	TierText      string
	ModifiedClass string
}

// --- Codeword candidates (codeword-lab harness reports + ledger) ---

type CandidateSlotView struct {
	Slot    string
	Letter  string // "B" for alphabet slots, "" otherwise
	Current string // the slot's current codeword, when known
	When    string // report generation time, display form
	Rows    []CandidateRowView
}

type CandidateRowView struct {
	Slot         string
	Word         string
	Marks        string // per-rendition pass/fail, e.g. "YYxY YYYY"
	Verdict      string // pass | pass* (stress misses) | fail
	VerdictClass string
	Nearest      string // nearest union words (phoneme distance)
	Confusers    string // wrong decodes observed for this word
	Status       string // ledger: adopted | rejected | retired | ""
	Reason       string
	HistoryOnly  bool // ledger row not present in the latest report
}

// --- Rules ---

type RulesViewData struct {
	// Listening says whether always-listening is on, and awake or asleep.
	Listening  string
	Groups     []RuleCategoryGroup
	Tabs       []PluginTab
	TestResult TestResultView
	KnownTags  []string
	Editor     EditorView
	// Selected is the inspector's command: the catalog row the user
	// clicked, with SelectedCanonical for the row highlight. nil = the
	// inspector shows its hint.
	Selected          *RuleCardView
	SelectedCanonical string
	// Locked is the selected command rendered as the form, read-only —
	// used when the editor is closed and something is selected.
	Locked    EditorView
	HasLocked bool
}

type PluginTab struct {
	Name   string
	Active bool
}

type RuleCategoryGroup struct {
	Name  string
	Count int
	Rules []RuleCardView
}

type RuleCardView struct {
	Canonical     string
	DisplayPhrase string // formatted
	PluginName    string
	IsUser        bool
	ModifiedClass string
	Description   string
	NL            RuleDescription
	HasNL         bool
	IsEditable    bool
	IsBeingEdited bool
	// BlockedReasons says why the platform has this command switched off
	// (its action is gone or changed, or this computer can't do it); empty
	// when it is live.
	BlockedReasons []string
}

type RuleDescription struct {
	Trigger    string
	Effect     string
	Condition  string
	SideEffect string
}

// --- Test Result ---

type TestResultView struct {
	Phrase        string
	Matched       bool
	ActionDisplay string
	ConsumedWords string
	Args          []TestArgView
	HasArgs       bool
	SetsTags      []string
	ClearsTags    []string
	RequiresTags  []string
	HasSetsTags   bool
	HasClearsTags bool
	HasRequires   bool
	// The WHY lines (the inspector's test box): WhyLine
	// explains a match's context in one sentence ("no mode required" / which
	// gates were active); GatedEntry/GatedChild explain a miss that would
	// match inside a mode — the entry word that opens it and the one-breath
	// form the chain grammar already accepts.
	WhyLine     string
	GatedChild  string
	GatedEntry  string
	HasGatedWhy bool
	OwnerPlugin string
	HasPartial  bool
	NextList    string
	HasNextList bool
	Empty       bool
	// Chain: the words after the first match, resolved under the tags that
	// match sets — "snap left" reads as snap (enters snap mode), then left.
	Chain    []TestChainStep
	HasChain bool
}

// TestChainStep is one later hop of a chained test phrase.
type TestChainStep struct {
	Words       string
	Effect      string
	OwnerPlugin string
}

type TestArgView struct {
	// Phase B: captures are keyed by binding name (was positional Index).
	Name  string
	Value string
}

// --- Editor ---

// ConfusableWarning is one author-time acoustic-confusability note: the phrase
// being built may be misheard as an existing command that competes in its
// context.
type ConfusableWarning struct {
	Message string
}

// EditorPreview is the live conflict signal under the phrase field: whether the
// phrase being authored already matches ("conflict") or extends ("info") an
// existing command, or is free.
type EditorPreview struct {
	Show    bool
	Kind    string // "conflict" (blocks) | "shadow" | "info" | "free"
	Message string
	// Existing: the canonical of the command this exact phrase already
	// belongs to — the first of the three fixes ("Customize it instead").
	Existing string
}

type EditorView struct {
	Open          bool
	IsNew         bool
	IsOverride    bool
	EditCanonical string
	Phrase        string
	Category      string
	Description   string
	ActionType    string
	ActionVal     string
	ActionJSON    string
	ShowRaw       bool
	// Discovery affordance: opt the command's bare prefix into the Discovery
	// HUD. ShowDiscovery gates the control to phrases that end in a capture
	// (the only shape it applies to); the platform is the authority on the full
	// shape rule and rejects a bad one loudly at save.
	Discovery        string
	ShowDiscovery    bool
	DiscoveryOptions []SelectOption
	// App scope: the "Only in app" dropdown. AppScope is the current
	// `app.<bundle_id>` requires tag (or "" = any app); AppOptions lists
	// installed apps. A friendly front-end over a requires tag.
	AppScope   string
	AppOptions []SelectOption
	// Preview: live author-time signal shown under the phrase — does the phrase
	// being built collide with (or continue) an existing command?
	Preview EditorPreview
	// Confusables: acoustic near-misses — existing commands this phrase may be
	// misheard as (and that compete in its context).
	Confusables []ConfusableWarning
	// ShowNewList: an inline "＋ New list…" create form is open for a capture slot.
	ShowNewList bool
	// Dropdown options
	Categories  []SelectOption
	ActionTypes []SelectOption
	// Simple action
	SimpleLabel string
	SimplePh    string
	ShowSimple  bool
	ShowRecord  bool
	Recording   bool
	// Template fields
	ShowTemplate bool
	TmplFields   []TmplFieldView
	// Sequence steps
	ShowSequence bool
	SeqSteps     []SeqStepEditView
	SeqCanAdd    bool
	// Phrase builder
	PhraseSlots   []PhraseSlotView
	ShowRawPhrase bool
	// Available lists for phrase help
	// Tag picker fields
	TagFields []TagFieldView
	// SaveError, when non-empty, is shown above the buttons (e.g. a
	// terminal-command-rule rejection from the platform). The editor stays open.
	SaveError string

	// The three questions. Each optional piece of "Only when" is shown when it holds a
	// value or the user pressed its "+"; Details folds the rest.
	ShowDetails   bool
	ShowApp       bool
	ShowMode      bool
	ShowAfter     bool
	OnlyWhenEmpty bool
	// Picker: what "It does" offers — the everyday shapes, then every
	// plugin's declared actions under the plugin's name.
	Picker ActionPickerView
	// A launch action picks the app from the installed list instead of
	// asking for a bundle id.
	ShowLaunchPicker bool
	LaunchOptions    []SelectOption
	// CanAddStep: the current single action can become step 1 of a sequence.
	CanAddStep bool

	// Locked: the inspector's read-only rendering of a selected command —
	// the same three boxes with the controls printed as text, and
	// Customize / Duplicate where Save would be. PluginName and IsUser feed
	// its header; ValueText and AppLabel are what the controls would show.
	Locked     bool
	PluginName string
	IsUser     bool
	ValueText  string
	AppLabel   string
	// ShowStart: a new command's "Start from…" cards — a copy of the
	// selected command, one of the everyday shapes, or blank.
	ShowStart   bool
	StartCopyOf string
	StartShapes []ActionPickEntry
	// Block: something refuses the save (an exact-phrase collision). Save
	// stays visible and says what is left; warnings never block.
	Block bool
	// SeqTypeGroups: what a step can do — the everyday shapes, then every
	// declared type under its group — as optgroups for the per-step select.
	SeqTypeGroups []SelectGroup
}

// SelectGroup is one <optgroup>.
type SelectGroup struct {
	Label   string
	Options []SelectOption
}

// ActionPickerView is the "It does" picker: the current choice as the
// button's face, the everyday shapes, and the per-plugin groups.
type ActionPickerView struct {
	Current    string // the face of the closed picker
	CurrentSub string // plugin name or step count, dim
	Everyday   []ActionPickEntry
	Groups     []ActionPickGroup
	Total      int
}

type ActionPickGroup struct {
	Plugin  string
	Entries []ActionPickEntry
}

type ActionPickEntry struct {
	Value    string
	Label    string
	Example  string
	Match    string // lowercased search text
	Selected bool
}

type PhraseSlotView struct {
	Index          int
	Value          string
	Repeat         string
	IsLiteral      bool
	IsList         bool
	IsAlternatives bool
	// IsRaw: a piece the builder cannot draw — a named or dependent blank
	// such as <name:audio_outputs> — kept as one locked chip. Details → Raw
	// phrase is where it changes.
	IsRaw         bool
	Display       string // the blank as written, all its lists
	TypeOptions   []SelectOption
	ListOptions   []SelectOption
	RepeatOptions []SelectOption
}

type TagChipView struct {
	Name  string
	Label string
}

type TagGroupView struct {
	Plugin string
	Tags   []TagPickerEntry
}

type TagPickerEntry struct {
	Name        string
	Label       string
	Description string
	Match       string // lowercased label, for the client-side search filter
}

type TagFieldView struct {
	FieldName    string
	FieldLabel   string
	HelpText     string
	Chips        []TagChipView
	Groups       []TagGroupView
	Signal       string
	SearchSignal string // signal backing the dropdown's filter input
	ShowSearch   bool   // render the filter input (long lists only)
}

type SelectOption struct {
	Value    string
	Label    string
	Selected bool
}

type TmplFieldView struct {
	Index          int
	Label          string
	Placeholder    string
	Mode           string
	Val            string
	ShowInput      bool
	HideModeSelect bool
	ModeOptions    []SelectOption
	ModeLabel      string
	// IsRawBinding: the field takes a transform the builder cannot draw
	// ({alphabet:join("")}); shown verbatim as one locked chip.
	IsRawBinding bool
	ShowRecord   bool
	Recording    bool
}

type SeqStepEditView struct {
	Index       int
	TypeOptions []SelectOption
	Val         string
	TypeLabel   string
	Placeholder string
	IsFirst     bool
	IsLast      bool
}

// --- Sequence step view (used by rules display) ---

type SeqStepView struct {
	Type string
	Val  string
}

// --- Setup (the platform's Setup page frames these two tabs) ---

// SetupModelView is one dictation size in the setup picker, as plain facts.
type SetupModelView struct {
	Name       string
	Size       string // "Large"
	Bytes      string // "1.51 GB"
	Languages  string // "English" / "Many languages"
	Note       string // "best quality, slowest"
	Chosen     bool   // the model the settings name
	InUse      bool   // a command model: the one the stage loads now
	Downloaded bool
	Ref        string
}

// GetStartedViewData is the Get started tab: the microphone menu, the model
// picker and the three checks.
type GetStartedViewData struct {
	Mic    MicMenuView
	Models SetupModelsViewData
	Try    SetupTryViewData
}

// MicMenuView is the Microphone menu: the connected input devices, the
// chosen one (UID empty: the system default, named DefaultName), and whether
// the chosen one is plugged in now.
type MicMenuView struct {
	Mics        []MicOption
	UID         string
	Name        string
	Connected   bool
	DefaultName string
}

type SetupModelsViewData struct {
	// Languages is the dictation language menu.
	Languages []LanguageRowView
	Models    []SetupModelView
	// Commands are the command model rows (Standard, Light).
	Commands []SetupModelView
	// Downloading is true while one of this plugin's models downloads.
	Downloading bool
	// OtherDownload says another plugin's download is running ("Another
	// plugin (x) is downloading y"), or is "". The platform runs one
	// download at a time, so the Download buttons stay off meanwhile and
	// the page says why.
	OtherDownload string
	ProgressPct   int
	// DownloadError is why the last download stopped short, when it did.
	DownloadError string
}

// ProgressStyle is the bar's width as the custom property the models CSS
// reads (see .model-progress-fill).
// ModelDownloadView is one model's download as a Download button shows it.
type ModelDownloadView struct {
	Active bool
	Pct    int
	// Failed is why the last download of this model stopped, when it did.
	Failed string
}

// ProgressStyle sets the bar's fill.
func (d ModelDownloadView) ProgressStyle() string {
	return fmt.Sprintf("--progress: %d%%;", d.Pct)
}

func (d SetupModelsViewData) ProgressStyle() string {
	return fmt.Sprintf("--progress: %d%%;", d.ProgressPct)
}

type SetupTryViewData struct {
	DictateKeys []string
	CommandKeys []string
	// Each check's last result: "pass", "fail" or "" (not tried yet).
	Mic       string
	MicNote   string
	Dictation string
	Commands  string
	// The command check is listening, and what it last heard.
	Listening bool
	Heard     string
	// The phrase the command check asks for.
	Phrase string
	// The command model is downloaded; without it no command is heard.
	CommandsOnDisk bool
	Linux          bool
}

// Done is every check passed.
func (d SetupTryViewData) Done() bool {
	return d.Mic == "pass" && d.Dictation == "pass" && d.Commands == "pass"
}

// AppStyleRowView is one app in the dictation styles table.
type AppStyleRowView struct {
	BundleID string
	Name     string
	// Style is the app's style id, "" for none.
	Style string
}

// AppOptionView is one app the styles Add row offers. Recent marks the apps
// used lately, which the menu lists first under their own heading.
type AppOptionView struct {
	BundleID string
	Name     string
	Recent   bool
}

// RowStyleChoices are an exception's choices: None (no style in this app,
// whatever the default) and every style. Removing the exception, so the app
// follows the default again, is the row's remove button.
var RowStyleChoices = append([]StyleChoice{{"none", "None"}}, StyleChoices...)

// StyleChoice is one button of an app's style control.
type StyleChoice struct {
	ID, Label string
}

// StyleChoices are the buttons of every app's style control, in order.
var StyleChoices = []StyleChoice{{"correct", "Correct"}, {"casual", "Casual"}, {"formal", "Formal"}, {"email", "Email"}}

// StyleBtnClass marks the current choice in a style control.
func StyleBtnClass(current, id string) string {
	if current == id {
		return "segmented-btn is-active"
	}
	return "segmented-btn"
}

// cleanUpDescription is the clean-up switch's description.
const cleanUpDescription = "Removes false starts and self-corrections (“Tuesday, no, Wednesday” becomes “Wednesday”) with a language model on this computer. Adds a second or two. It may only remove words."

// needsModelClass greys a section whose model is not downloaded: still
// readable, so a person sees what the download would give them, but nothing
// in it can be used until the model is there.
func needsModelClass(ready bool) string {
	if ready {
		return "dict-model-section"
	}
	return "dict-model-section dict-needs-model"
}

// downloadClass is a model's Download button: .primary when it is the only
// Download on the page (the other model is installed), else the ordinary
// button, since the stylesheet allows one .primary per view.
func downloadClass(otherReady bool) string {
	if otherReady {
		return "small primary"
	}
	return "small"
}

// hasRecent reports whether any offered app was used lately.
func hasRecent(apps []AppOptionView) bool {
	for _, a := range apps {
		if a.Recent {
			return true
		}
	}
	return false
}

// addStyleStart is the Add row's first style: None when there is a default
// (an exception is then most often "not here"), else Correct.
func addStyleStart(def string) string {
	if def != "" {
		return "none"
	}
	return "correct"
}

// busyAttrs marks a button busy (the platform's aria-busy look: a spinner,
// clicks ignored) for as long as its request is in flight. The signal is
// named for the ref, so one Download button spinning never spins the other.
func busyAttrs(ref string) templ.Attributes {
	var b strings.Builder
	b.WriteString("busy")
	for _, r := range strings.ToLower(ref) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	sig := b.String()
	return templ.Attributes{"data-indicator:" + sig: true, "data-attr:aria-busy": "$" + sig}
}

// --- Models tab ---

// ModelsViewData is the Models tab: the command models with their licenses,
// and the comparison on the user's own voice.
type ModelsViewData struct {
	Cards []ModelCard
	// Dictation are the dictation models for this OS, Helpers the dictation
	// clean-up and style models: listed with their licenses, not compared.
	Dictation []ModelCard
	Helpers   []ModelCard
	// DictationCompare is Read a sentence and the dictation comparison.
	DictationCompare DictationCompareView
	// The comparison: running with its progress, or its last result.
	Running  bool
	Progress string
	Error    string
	Compared bool // a comparison has finished in this session
	Clips    int  // confirmed recordings it considered
	Skipped  int  // of those, the ones no model could decode under this grammar
	// Columns are the compared models' labels, in Cards order, heading the
	// disagreement rows.
	Columns       []string
	Disagreements []DisagreementRow
	// SayNow is Say a phrase, below the comparison.
	SayNow SayNowView
	// Downloads, as on Get started: off while any download runs.
	Downloading   bool
	OtherDownload string
}

// ModelCard is one model's card. Only command models are scored.
type ModelCard struct {
	Name, Label, Title, License, Source, Bytes, Ref string
	Downloaded, InUse                               bool
	// Chosen is a dictation model the settings name, downloaded or not.
	Chosen bool
	// Scored when the comparison covered it; Best marks the one that did
	// best on this voice (most right, then fastest), when two were compared.
	Scored   bool
	Best     bool
	Right    int
	Total    int
	MedianMs string // "11 ms"
	// WordsPct scores a dictation model ("96%" of words right) in place of
	// Right of Total.
	WordsPct string
}

// DisagreementRow is one recording the compared models heard differently.
type DisagreementRow struct {
	Said     string // what was said: the confirmed target
	AudioURL string
	Cells    []HeardCell // one per Columns entry
}

// HeardCell is what one model heard on one recording.
type HeardCell struct {
	Heard string
	Right bool
}

// SayNowView is the Models tab's Say a phrase: listening, or phrases said
// just now waiting for "which was right?".
type SayNowView struct {
	Armed   bool
	Key     string // the command key, as text ("opt+g")
	Error   string
	Columns []string // the downloaded command models' labels
	Rows    []SayNowRow
}

// SayNowRow is one phrase said just now.
type SayNowRow struct {
	File     string
	AudioURL string
	Heard    []string // per Columns entry, "" when that model heard nothing
	Choices  []string // the distinct non-empty hearings, offered as answers
	Signal   string   // the Datastar signal of its "something else" box
}

// DictationCompareView is the Models tab's dictation comparison: Read a
// sentence, the recordings waiting to be kept, and the comparison.
type DictationCompareView struct {
	Recorded int // kept recordings
	// Read a sentence.
	Armed     bool
	Sentence  string
	Key       string // the dictation key, as text ("opt+t")
	ReadError string
	Pending   []ReadClipRow
	// The comparison: running, or its last result.
	Running  bool
	Progress string
	Error    string
	Compared bool
	Columns  []string // the compared models' labels, in card order
	Rows     []DictationRow
}

// ReadClipRow is a recording waiting to be kept or discarded.
type ReadClipRow struct {
	File, AudioURL, Sentence, Heard string
}

// DictationRow is one recording the models heard differently.
type DictationRow struct {
	Sentence string
	AudioURL string
	Cells    []DictationCell // one per Columns entry
}

// DictationCell is what one model heard on one recording, and its word
// errors.
type DictationCell struct {
	Heard  string
	Errors int
}

// soundsChoices are the start and stop sounds setting's values, as the
// Dictation tab offers them.
var soundsChoices = []struct{ Value, Label string }{
	{"auto", "Automatic"},
	{"on", "On"},
	{"off", "Off"},
}
