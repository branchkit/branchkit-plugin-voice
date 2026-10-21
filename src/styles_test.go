package main

import (
	"encoding/json"
	branchkit "github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/views"
	"strings"
	"sync"
	"testing"
	"time"
)

// The model's answers below are Qwen3-4B's on 2026-10-06, plus
// the failures the facts check exists for.
func TestFinishStyle(t *testing.T) {
	for _, tt := range []struct {
		name, style, said, answer, want string
		ok                              bool
	}{
		{"casual gives names back their casing and drops the final stop", styleCasual,
			"The meeting with Dr. Okafor moved to 3 p.m. on Wednesday, the room is the same.",
			"the meeting with dr. okafor moved to 3 p.m. on wednesday, the room is the same.",
			"the meeting with Dr. Okafor moved to 3 p.m. on Wednesday, the room is the same", true},
		{"casual keeps mixed-case words", styleCasual,
			"Please review the BranchKit dictation changes and tell me if the refine step feels too slow.",
			"please review the branchkit dictation changes and tell me if the refine step feels too slow",
			"please review the BranchKit dictation changes and tell me if the refine step feels too slow", true},
		{"casual keeps the pronoun I capitalised", styleCasual,
			"I think we should, um, move the launch to March and give the iOS team two more weeks.",
			"i think we should move the launch to march and give the ios team two more weeks. i'm sure.",
			"I think we should move the launch to March and give the iOS team two more weeks. I'm sure", true},
		{"a correction may drop the abandoned fact", styleFormal,
			"Tell Marcus the invoice for 2,400 dollars went out on the 14th, sorry, the 15th.",
			"Tell Marcus the invoice for $2,400 went out on the 15th.",
			"Tell Marcus the invoice for $2,400 went out on the 15th.", true},
		{"a formal rewrite that keeps every fact", styleFormal,
			"Hey so I'm gonna be like ten minutes late, the train's stuck outside Paddington.",
			"I will be ten minutes late; the train is stuck outside Paddington.",
			"I will be ten minutes late; the train is stuck outside Paddington.", true},
		{"email frames the formal body", styleEmail,
			"Can we push our one on one to Thursday at 2, I have a dentist appointment.",
			"Can we push our one-on-one to Thursday at 2? I have a dentist appointment.",
			"Hi,\n\nCan we push our one-on-one to Thursday at 2? I have a dentist appointment.\n\nThanks,", true},
		{"a reasoning block is removed", styleFormal,
			"Thanks for the quick turnaround, the client was happy.",
			"<think>\n\n</think>\n\nThanks for the quick turnaround; the client was happy.",
			"Thanks for the quick turnaround; the client was happy.", true},
		{"a lost name is refused", styleFormal,
			"Can you send Priya the Q3 numbers before Thursday.",
			"Can you send the numbers before Thursday?", "", false},
		{"a lost number is refused", styleCasual,
			"The bug only happens on Windows 11 when the user has two monitors.",
			"the bug only happens on windows when the user has two monitors", "", false},
		{"an answer instead of a rewrite is refused", styleCasual,
			"Thanks for the quick turnaround, really appreciate it, the client was happy.",
			"no problem, glad to help, client was cool", "", false},
		// Correct: Qwen3-4B's answers on 2026-10-06, grammar fixed and tone kept.
		{"correct fixes agreement and keeps the speaker's words", styleCorrect,
			"Me and him was going to look at the logs but the server were down.",
			"He and I were going to look at the logs, but the server was down.",
			"He and I were going to look at the logs, but the server was down.", true},
		{"correct fixes verb forms and keeps the name", styleCorrect,
			"I seen the email from Marcus this morning and I already replied him.",
			"I saw the email from Marcus this morning and I already replied to him.",
			"I saw the email from Marcus this morning and I already replied to him.", true},
		{"correct keeps a casual tone", styleCorrect,
			"Hey so I'm gonna be like ten minutes late, the train's stuck outside Paddington.",
			"Hey, I'm gonna be ten minutes late, the train's stuck outside Paddington.",
			"Hey, I'm gonna be ten minutes late, the train's stuck outside Paddington.", true},
		{"correct keeps the correction and drops the abandoned half", styleCorrect,
			"Can you send Priya the Q3 numbers before the standup tomorrow, no, wait, before Thursday.",
			"Can you send Priya the Q3 numbers before Thursday?",
			"Can you send Priya the Q3 numbers before Thursday?", true},
		{"a formal rewrite is not a correction", styleCorrect,
			"Thanks so much, the demo went great, really appreciate everyone jumping in.",
			"Thank you all for your contributions; the demonstration was a tremendous success and is greatly appreciated.",
			"", false},
		{"a correction that grows is refused", styleCorrect,
			"Can we push it to Thursday at 2.",
			"Could we perhaps push the meeting to Thursday at 2 if that works for everyone?",
			"", false},
		{"a rewrite far longer than what was said is refused", styleFormal,
			"I can't make it tonight.",
			"I regret to inform you that I will be unable to attend this evening's event, as unforeseen circumstances have arisen.",
			"", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, why := finishStyle(tt.said, tt.answer, tt.style)
			if ok != tt.ok {
				t.Fatalf("ok = %v (%s), want %v; got %q", ok, why, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if !ok && got != tt.said {
				t.Errorf("a refused rewrite must hand back what was said, got %q", got)
			}
		})
	}
}

func TestStyleRequestBodyTurnsOffReasoning(t *testing.T) {
	st, _ := styleByID(styleEmail)
	if st.ID != styleFormal {
		t.Fatalf("email should rewrite as formal, got %q", st.ID)
	}
	body := string(styleRequestBody("Call Sam at five.", st))
	if !strings.Contains(body, "Call Sam at five. /no_think") || !strings.Contains(body, `"role":"assistant"`) {
		t.Errorf("body: %s", body)
	}
}

func TestAppStylesRefuseUnknownNames(t *testing.T) {
	h := newTestHost()
	if err := h.setAppStyle("com.apple.mail", "shouty"); err == nil {
		t.Error("an unknown style must be refused")
	}
	if styleLabel(styleEmail) != "Email" || styleLabel(styleCasual) != "Casual" || styleLabel(styleCorrect) != "Correct" {
		t.Error("labels")
	}
}

func TestDictationCatalogExcludesTheStyleModel(t *testing.T) {
	for _, m := range dictationCatalog() {
		if m.Name == styleModel {
			t.Fatal("the style model appears in the dictation model picker")
		}
	}
}

// The table lists the apps that have a style, by name; an app only used
// recently is not a row. The Add row offers every other app, the recently
// used ones first (most recent first, marked Recent), then the rest by name,
// never an app that already has a style.
func TestAppStyleRowsAndTheAddList(t *testing.T) {
	h := newTestHost()
	h.styles.by = map[string]string{"com.apple.mail": styleEmail, "com.tinyspeck.slackmacgap": styleCasual}
	h.appNamesByID = map[string]string{
		"com.apple.mail": "Mail", "com.tinyspeck.slackmacgap": "Slack", "com.apple.Notes": "Notes",
		"org.mozilla.firefox": "Firefox", "com.apple.Safari": "Safari",
	}
	h.noteFocusedApp("com.apple.Notes")
	h.noteFocusedApp("com.tinyspeck.slackmacgap")
	h.noteFocusedApp("org.mozilla.firefox")
	var rows []string
	for _, r := range h.appStyleRows() {
		rows = append(rows, r.Name+"="+r.Style)
	}
	if want := "Mail=email,Slack=casual"; strings.Join(rows, ",") != want {
		t.Errorf("rows %v, want %s", rows, want)
	}
	var adds []string
	for _, a := range h.addAppOptions() {
		n := a.Name
		if a.Recent {
			n += "*"
		}
		adds = append(adds, n)
	}
	if want := "Firefox*,Notes*,Firefox,Notes,Safari"; strings.Join(adds, ",") == want {
		t.Errorf("a recent app must not be offered twice: %v", adds)
	}
	if want := "Firefox*,Notes*,Safari"; strings.Join(adds, ",") != want {
		t.Errorf("add list %v, want %s", adds, want)
	}
}

// A recent dictation's picker sets the app's exception: it marks the
// exception (Default when the app follows the default), never the resolved
// style, so a marked button is never a hidden pin. A speak session or an
// unknown app offers nothing. Without the style model nothing is marked and
// nothing posts.
func TestStyleControlOnARecentDictation(t *testing.T) {
	h := newTestHost()
	if styleModelReady() {
		t.Skip("the style model is installed here; this covers the missing case")
	}
	h.styles.def = styleFormal
	h.styles.by = map[string]string{"com.example.x": styleEmail}
	out := h.styleControlHTML(dictationEntry{App: "com.example.x"})
	if strings.Contains(out, "app_styles") || strings.Count(out, `aria-disabled="true"`) != 6 {
		t.Errorf("without the model every choice must be disabled and post nothing: %s", out)
	}
	if strings.Contains(out, `aria-pressed="true"`) {
		t.Errorf("without the model nothing is marked: %s", out)
	}
	if !strings.Contains(out, ">Default (Formal)<") {
		t.Errorf("the Default choice names the default: %s", out)
	}
	if h.styleControlHTML(dictationEntry{App: "com.example.x", Speak: true}) != "" || h.styleControlHTML(dictationEntry{}) != "" {
		t.Error("no control for a speak session or an unknown app")
	}
}

// Clean-up and styles share one card, because they are one decision: an app
// with a style uses it instead of clean-up. The card says so, and holds the
// clean-up switch first, then the styles and their per-app control.
func TestDictationPageShowsCleanUpAndStylesTogether(t *testing.T) {
	h := newTestHost()
	h.noteFocusedApp("com.apple.mail")
	html := h.renderDictation("", "", nil, nil, nil)
	for _, want := range []string{`label="Clean-up and styles"`, "instead of clean-up", "Correct", "Casual", "Formal", "Email"} {
		if !strings.Contains(html, want) {
			t.Errorf("Dictation page lacks %q", want)
		}
	}
	card := strings.Index(html, `label="Clean-up and styles"`)
	cleanUp := strings.Index(html, `>Clean-up</span>`)
	styles := strings.Index(html, `>Styles</span>`)
	if !(card < cleanUp && cleanUp < styles) {
		t.Errorf("want the card, then its Clean-up section, then Styles (at %d, %d, %d)", card, cleanUp, styles)
	}
	if strings.Contains(html, `label="Styles"`) || strings.Contains(html, `label="Clean-up"`) {
		t.Error("clean-up and styles must not also have cards of their own")
	}
}

func TestDecideRewrite(t *testing.T) {
	formal := func(core string) (string, bool, string) {
		return "I will be ten minutes late.", true, "written as Formal"
	}
	refuse := func(string) (string, bool, string) {
		return "", false, "style refused: it changed a number (10)"
	}
	cases := []struct {
		name, style   string
		ready         bool
		sel           string
		rewrite       func(string) (string, bool, string)
		notice, typed string
	}{
		{"rewritten, whitespace kept", "formal", true, "  gonna be 10 min late\n", formal, "", "  I will be ten minutes late.\n"},
		{"unknown style", "pirate", true, "hello there", formal, "There is no style called pirate.", ""},
		{"no model", "formal", false, "hello there", formal, "Download the style model first, on the Dictation tab in Settings.", ""},
		{"nothing selected", "formal", true, " \n", formal, "Select the text to rewrite first.", ""},
		{"refused", "casual", true, "I will be 10 minutes late", refuse, "Kept as it was: it changed a number (10).", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notice, typed := decideRewrite(c.style, c.ready, c.sel, c.rewrite)
			if notice != c.notice || typed != c.typed {
				t.Errorf("got (%q, %q), want (%q, %q)", notice, typed, c.notice, c.typed)
			}
		})
	}
	long := strings.Repeat("word ", rewriteMaxWords+1)
	if notice, _ := decideRewrite("formal", true, long, formal); !strings.Contains(notice, "a rewrite takes up to") {
		t.Errorf("a selection over the limit was not refused: %q", notice)
	}
}

// While the platform fetches the style model, the Styles card's header shows
// its progress in place of the Download button, so a click visibly did
// something; a download of another model leaves the button as it is. Each
// card that needs a model carries its own status, so the page holds two
// Download buttons while neither model is here (Clean-up's and Styles').
func TestDictationPageShowsTheStyleModelDownloading(t *testing.T) {
	h := newTestHost()
	if styleModelReady() || refineModelReady() {
		t.Skip("a model is installed here, so its Download button is already gone")
	}
	other := h.renderDictation("", "", nil, nil, &ModelDownloadData{Downloading: true, Progress: 0.42, Model: "voice/something-else"})
	if strings.Contains(other, "Downloading model") {
		t.Error("another model's download must not show on these cards")
	}
	if n := strings.Count(other, ">Download</button>"); n != 2 {
		t.Fatalf("want the clean-up and style models' Download buttons, found %d", n)
	}
	html := h.renderDictation("", "", nil, nil, &ModelDownloadData{Downloading: true, Progress: 0.42, Model: h.modelRef(styleModel)})
	if !strings.Contains(html, "Downloading model · 42%") || !strings.Contains(html, `aria-valuenow="42"`) {
		t.Errorf("the style model's download must show its progress:\n%s", html)
	}
	if n := strings.Count(html, ">Download</button>"); n != 1 {
		t.Errorf("only the clean-up model's Download button may remain while the style model downloads, found %d", n)
	}
}

// Once a model is on disk its card's header says so with a chip, and the
// download offer is gone, so the button's absence does not read as
// something missing.
func TestDictationPageSaysAModelIsInstalled(t *testing.T) {
	h := newTestHost()
	html := h.renderDictation("", "", nil, nil, nil)
	installed := 0
	for _, ready := range []bool{refineModelReady(), styleModelReady()} {
		if ready {
			installed++
		}
	}
	if n := strings.Count(html, ">Model downloaded<"); n != installed {
		t.Errorf("%d models installed, %d chips say so", installed, n)
	}
	if n := strings.Count(html, ">Download</button>"); n != 2-installed {
		t.Errorf("%d models missing, %d Download buttons", 2-installed, n)
	}
}

// A section whose model is missing is greyed and inert, and says why: the
// clean-up switch is disabled and off, every style button is aria-disabled,
// shows None and posts nothing. The Download in each sub-heading stays usable.
func TestDictationPageDisablesWhatNeedsAMissingModel(t *testing.T) {
	h := newTestHost()
	if styleModelReady() || refineModelReady() {
		t.Skip("a model is installed here; this covers the missing case")
	}
	h.noteFocusedApp("com.apple.mail")
	h.styles.by = map[string]string{"com.apple.mail": styleEmail}
	html := h.renderDictation("", "", nil, nil, nil)
	if n := strings.Count(html, "dict-needs-model"); n != 2 {
		t.Errorf("want both sections greyed, found %d", n)
	}
	for _, want := range []string{"dict-cleanup-needs-model", "dict-styles-needs-model", "Download the clean-up model to turn clean-up on", "Download the style model to choose a style"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(html, "app_styles/set") {
		t.Error("no style can be chosen without the model")
	}
	if !strings.Contains(html, " disabled") {
		t.Error("the clean-up switch must be disabled")
	}
	if n := strings.Count(html, ">Download</button>"); n != 2 {
		t.Errorf("both Download buttons must stay usable, found %d", n)
	}
}

// Voice refuses what the page will not offer: a style, or clean-up turned on,
// without its model. Clearing a style and turning clean-up off always pass.
func TestSettingsNeedingAMissingModelAreRefused(t *testing.T) {
	h := newTestHost()
	if styleModelReady() || refineModelReady() {
		t.Skip("a model is installed here; this covers the missing case")
	}
	err := h.HandleAppStyleSet(&struct {
		BundleID string `json:"bundle_id"`
		Style    string `json:"style"`
	}{BundleID: "com.apple.mail", Style: styleCorrect})
	if err != errStyleModelMissing {
		t.Errorf("a style without its model: got %v", err)
	}
	on := map[string]json.RawMessage{"refine_dictation": json.RawMessage("true")}
	if err := h.handleConfigUpdate(&on); err != errRefineModelMissing {
		t.Errorf("clean-up on without its model: got %v", err)
	}
	off := map[string]json.RawMessage{"refine_dictation": json.RawMessage("false")}
	if err := h.handleConfigUpdate(&off); err != nil {
		t.Errorf("clean-up off must always pass: %v", err)
	}
}

// Each exception row offers None and the four styles and a remove (back to
// the default); the Add row adds an exception. Without the style model all
// of it is present and inert.
func TestStylesTableRowsAndAddRow(t *testing.T) {
	h := newTestHost()
	h.styles.by = map[string]string{"com.apple.mail": styleEmail}
	h.appNamesByID = map[string]string{"com.apple.mail": "Mail", "com.apple.Notes": "Notes"}
	html := h.renderDictation("", "", nil, nil, nil)
	for _, want := range []string{`aria-label="Mail uses the default again"`, `aria-label="App to add as an exception"`, `<option value="com.apple.Notes">Notes</option>`, `aria-label="Style for the app"`, "Default for every app", "Exceptions"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	group := html[strings.Index(html, `aria-label="Style in Mail"`):]
	group = group[:strings.Index(group, "</div>")]
	if !strings.Contains(group, ">None<") || strings.Count(group, "<button") != 5 {
		t.Errorf("an exception offers None and the four styles: %s", group)
	}
	if !styleModelReady() {
		if strings.Contains(html, "app_styles/") {
			t.Error("without the model nothing may post a style or a default")
		}
		if strings.Count(html, `aria-label="App to add as an exception" disabled`) != 1 {
			t.Error("without the model the Add row is disabled")
		}
	}
}

// An app's style is its exception if it has one, else the default; an
// exception of None means no style whatever the default.
func TestEffectiveStyleIsTheExceptionElseTheDefault(t *testing.T) {
	for _, c := range []struct{ exception, def, want string }{
		{"", "", ""},
		{"", styleCorrect, styleCorrect},
		{styleFormal, styleCorrect, styleFormal},
		{styleNone, styleCorrect, ""},
		{styleNone, "", ""},
	} {
		if got := resolveStyle(c.exception, c.def); got != c.want {
			t.Errorf("exception %q over default %q = %q, want %q", c.exception, c.def, got, c.want)
		}
	}
	h := newTestHost()
	h.styles.def = styleCorrect
	h.styles.by = map[string]string{"com.mitchellh.ghostty": styleNone, "com.apple.mail": styleEmail}
	for app, want := range map[string]string{"com.mitchellh.ghostty": "", "com.apple.mail": styleEmail, "org.mozilla.firefox": styleCorrect} {
		if got := h.effectiveStyle(app); got != want {
			t.Errorf("%s gets %q, want %q", app, got, want)
		}
	}
}

// A default style needs its model, like an exception; None is always allowed.
func TestDefaultStyleNeedsTheModel(t *testing.T) {
	h := newTestHost()
	if styleModelReady() {
		t.Skip("the style model is installed here; this covers the missing case")
	}
	if err := h.HandleAppStyleDefault(&struct {
		Style string `json:"style"`
	}{Style: styleCorrect}); err != errStyleModelMissing {
		t.Errorf("a default style without its model: got %v", err)
	}
}

// With the model, the default picker marks the default and posts the box
// clicked; None is a choice like the styles.
func TestDefaultPickerMarksTheDefault(t *testing.T) {
	html := views.Render(views.Dictation(views.DictationViewData{StylesReady: true, DefaultStyle: styleFormal}))
	if strings.Count(html, "dict-style-box") != 5 {
		t.Fatalf("want five default choices: %s", html)
	}
	if strings.Count(html, `dict-style-box is-active" aria-pressed="true"`) != 1 || !strings.Contains(html, `is-active" aria-pressed="true" data-on:click="@post(&#39;/v1/plugins/unknown/methods/app_styles/default&#39;, {payload: {&#34;style&#34;:&#34;formal&#34;}})"`) {
		t.Errorf("Formal must be the one marked default")
	}
	if !strings.Contains(html, `{&#34;style&#34;:&#34;&#34;}`) {
		t.Error("None must post an empty default")
	}
}

// A row a person added to the apps registry ("browser" for Firefox) carries no
// display name; whatever order the records come in, the registry's named
// record wins, so the app reads "Firefox". An app with no named record shows
// its first spoken name.
func TestAppNamesPreferTheRegistrysDisplayName(t *testing.T) {
	rec := func(payload string) branchkit.CollectionRecord {
		return branchkit.CollectionRecord{Payload: json.RawMessage(payload)}
	}
	index := appNameIndex([]branchkit.CollectionRecord{
		rec(`{"spoken":"browser","app_id":"org.mozilla.firefox"}`),
		rec(`{"spoken":"firefox","app_id":"org.mozilla.firefox","name":"Firefox"}`),
		rec(`{"spoken":"notes","app_id":"com.apple.Notes"}`),
		rec(`{"spoken":"my notes","app_id":"com.apple.Notes"}`),
	})
	if index["org.mozilla.firefox"] != "Firefox" {
		t.Errorf("Firefox labelled %q", index["org.mozilla.firefox"])
	}
	if index["com.apple.Notes"] != "notes" {
		t.Errorf("an app with no named record: %q", index["com.apple.Notes"])
	}
}

// A default and an exception written at the same time both reach the stored
// record: each write is built from the other's committed value, never a
// stale copy.
func TestStyleWritesDoNotLoseEachOther(t *testing.T) {
	h := newTestHost()
	h.styles.by = map[string]string{}
	var mu sync.Mutex
	var last map[string]any
	h.putAppStyles = func(record map[string]any) error {
		time.Sleep(5 * time.Millisecond) // widen the window a race needs
		mu.Lock()
		last = record
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = h.setDefaultStyle(styleFormal) }()
	go func() { defer wg.Done(); _ = h.setAppStyle("com.apple.mail", styleNone) }()
	wg.Wait()
	styles, _ := last["styles"].(map[string]string)
	if last["default"] != styleFormal || styles["com.apple.mail"] != styleNone {
		t.Errorf("the last stored record lost a write: %v", last)
	}
}
