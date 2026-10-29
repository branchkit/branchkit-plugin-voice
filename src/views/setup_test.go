package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The Try it steps name the user's real keys. templ only reads `@` as a
// component call at the start of a line; written mid-sentence it printed
// "@keys(data.DictateKeys)" on the page.
func TestSetupTryRendersTheKeysNotTheTemplate(t *testing.T) {
	var b bytes.Buffer
	data := SetupTryViewData{DictateKeys: []string{"⌥", "T"}, CommandKeys: []string{"⌥", "G"}, CommandsOnDisk: true}
	if err := SetupTry(data).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if strings.Contains(html, "@keys") || strings.Contains(html, "data.") {
		t.Fatalf("template source leaked into the page: %s", html)
	}
	for _, want := range []string{"<kbd>⌥</kbd><kbd>T</kbd>", "<kbd>⌥</kbd><kbd>G</kbd>", "hold "} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q in %s", want, html)
		}
	}
}

// Text beside keycaps keeps its spaces: templ trims literal text at an
// element's edge, which printed "⌥Gand".
func TestSetupTryKeepsTheSpacesBesideKeys(t *testing.T) {
	var b bytes.Buffer
	data := SetupTryViewData{DictateKeys: []string{"⌥", "T"}, CommandKeys: []string{"⌥", "G"}, Phrase: "show help", CommandsOnDisk: true}
	if err := SetupTry(data).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{"hold <kbd>⌥</kbd>", "<kbd>G</kbd> and say <q>show help</q>", "Microphone", "Dictation", "Commands", ">Check</button>"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q in %s", want, html)
		}
	}
}

func TestSetupTryShowsEachCheckState(t *testing.T) {
	render := func(d SetupTryViewData) string {
		var b bytes.Buffer
		d.Phrase = "show help"
		if err := SetupTry(d).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	html := render(SetupTryViewData{CommandsOnDisk: true, Listening: true, Heard: "scroll down"})
	if !strings.Contains(html, "Listening.") || !strings.Contains(html, "<q>scroll down</q>") || strings.Contains(html, ">Check</button>") {
		t.Fatalf("listening: %s", html)
	}
	html = render(SetupTryViewData{})
	if !strings.Contains(html, "Download the command model") {
		t.Fatalf("no command model: %s", html)
	}
	html = render(SetupTryViewData{Mic: "fail", MicNote: "Your last hold was silent"})
	if !strings.Contains(html, "check-mark fail") || !strings.Contains(html, "Your last hold was silent") {
		t.Fatalf("silent mic: %s", html)
	}
	html = render(SetupTryViewData{Mic: "pass", Dictation: "pass", Commands: "pass"})
	if strings.Count(html, "check-mark pass") != 3 || !strings.Contains(html, "Everything works") {
		t.Fatalf("all done: %s", html)
	}
}

// Get started holds, in order, the microphone, the models and the checks;
// a chosen microphone that is unplugged stays chosen and says so.
func TestGetStartedHoldsMicModelsAndChecks(t *testing.T) {
	var b strings.Builder
	data := GetStartedViewData{
		Mic: MicMenuView{
			Mics:        []MicOption{{UID: "BuiltIn", Name: "MacBook Air Microphone"}},
			UID:         "USB-1",
			Name:        "Desk Mic",
			DefaultName: "MacBook Air Microphone",
		},
		Models: SetupModelsViewData{
			Languages: []LanguageRowView{{Label: "English", Code: "en", Selected: true}},
			Models: []SetupModelView{
				{Name: "large", Size: "Large", Bytes: "1.6 GB", Chosen: true, InUse: true, Downloaded: true, Ref: "r1"},
				{Name: "small", Size: "Small", Bytes: "70 MB", Downloaded: true, Ref: "r2"},
			},
		},
		Try: SetupTryViewData{DictateKeys: []string{"⌥", "T"}, CommandKeys: []string{"⌥", "G"}},
	}
	if err := GetStarted(data).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	mic, models, checks := strings.Index(html, `label="Microphone"`), strings.Index(html, `label="Speech models"`), strings.Index(html, `label="Check it works"`)
	if mic < 0 || models < mic || checks < models {
		t.Fatalf("sections out of order or missing: mic %d, models %d, checks %d", mic, models, checks)
	}
	for _, want := range []string{
		"System default (MacBook Air Microphone)",
		"Desk Mic (not connected)",
		`for="dict-lang">Language<`,
		"Delete the Small dictation model",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "Delete the Large") {
		t.Error("the model in use offered for deletion")
	}
}

// renderSetupModels renders the Speech models card.
func renderSetupModels(t *testing.T, data SetupModelsViewData) string {
	t.Helper()
	var b strings.Builder
	if err := SetupModels(data).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// rowOf is the HTML of the one row whose radio names size, from its box to
// the next row's (or the end).
func rowOf(t *testing.T, html, size string) string {
	t.Helper()
	at := strings.Index(html, `<span class="model-title">`+size+`</span>`)
	if at < 0 {
		t.Fatalf("no %s row in %s", size, html)
	}
	start := strings.LastIndex(html[:at], `<div class="setup-model`)
	end := strings.Index(html[at:], `<div class="setup-model`)
	if end < 0 {
		return html[start:]
	}
	return html[start : at+end]
}

// The card is two pick-one lists, Dictation and Commands, each a labelled
// radio group with exactly the chosen row checked. There are no Choose or Use
// buttons: picking the row is the action.
func TestSetupModelsAreOnePickOneListPerKind(t *testing.T) {
	html := renderSetupModels(t, SetupModelsViewData{
		Models: []SetupModelView{
			{Name: "large", Size: "Large", Bytes: "1.51 GB", Languages: "Many languages", Note: "most accurate", Chosen: true, InUse: true, Downloaded: true, Ref: "voice/large"},
			{Name: "small", Size: "Small", Bytes: "466 MB", Languages: "English", Downloaded: true, Ref: "voice/small"},
			{Name: "base", Size: "Base", Bytes: "139 MB", Languages: "English", Ref: "voice/base"},
		},
		Commands: []SetupModelView{
			{Name: "std", Size: "Standard", Bytes: "153 MB", Languages: "English", Chosen: true, InUse: true, Downloaded: true, Ref: "voice/std"},
			{Name: "light", Size: "Light", Bytes: "70 MB", Languages: "English", Ref: "voice/light"},
		},
	})
	for _, want := range []string{
		`role="radiogroup" aria-labelledby="setup-dictation-head"`,
		`id="setup-dictation-head">Dictation<`,
		`role="radiogroup" aria-labelledby="setup-commands-head"`,
		`id="setup-commands-head">Commands<`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if got := strings.Count(html, `role="radio"`); got != 5 {
		t.Errorf("want a radio per row (5), got %d", got)
	}
	if got := strings.Count(html, `aria-checked="true"`); got != 2 {
		t.Errorf("want one checked row per list (2), got %d", got)
	}
	for _, gone := range []string{">Choose<", ">Use<", "model/set"} {
		if strings.Contains(html, gone) {
			t.Errorf("the old two-button row is back: %q in %s", gone, html)
		}
	}

	large := rowOf(t, html, "Large")
	if !strings.Contains(large, `aria-checked="true"`) || !strings.Contains(large, ">In use<") || strings.Contains(large, "@post") {
		t.Errorf("the model in use is checked, says so, and posts nothing: %s", large)
	}
	if !strings.Contains(large, " · most accurate · Many languages · 1.51 GB") {
		t.Errorf("facts read what it is for, its languages, its size: %s", large)
	}
	small := rowOf(t, html, "Small")
	if !strings.Contains(small, ">On this computer<") || !strings.Contains(small, "methods/model/pick") || !strings.Contains(small, "&#34;name&#34;:&#34;small&#34;") {
		t.Errorf("a downloaded model says so and picks by name: %s", small)
	}
	if !strings.Contains(small, `aria-label="Delete the Small dictation model"`) {
		t.Errorf("a downloaded model nothing loads can be deleted: %s", small)
	}
	if strings.Contains(large, "Delete the") {
		t.Errorf("the model in use offered for deletion: %s", large)
	}
	base := rowOf(t, html, "Base")
	if !strings.Contains(base, ">139 MB download<") || !strings.Contains(base, "methods/model/pick") || strings.Contains(base, "aria-disabled") {
		t.Errorf("a model not downloaded states its download and picks: %s", base)
	}
	light := rowOf(t, html, "Light")
	if !strings.Contains(light, "methods/command_model/pick") || !strings.Contains(light, ">70 MB download<") {
		t.Errorf("a command row picks a command model: %s", light)
	}
}

// The chosen model not on this computer, with nothing downloading (a fresh
// install's default, or a download that failed), says so and offers its
// Download; pressing its row again does the same.
func TestSetupModelsChosenButMissingOffersDownload(t *testing.T) {
	html := renderSetupModels(t, SetupModelsViewData{
		Models:        []SetupModelView{{Name: "large", Size: "Large", Bytes: "1.51 GB", Chosen: true, Ref: "voice/large"}},
		DownloadError: "connection reset",
	})
	row := rowOf(t, html, "Large")
	for _, want := range []string{`aria-checked="true"`, ">Not downloaded yet<", "methods/model/download", "&#34;ref&#34;:&#34;voice/large&#34;", "methods/model/pick"} {
		if !strings.Contains(row, want) {
			t.Errorf("missing %q: %s", want, row)
		}
	}
	if !strings.Contains(html, `role="alert">Download failed: connection reset<`) {
		t.Errorf("a failed download says why: %s", html)
	}
}

// While one of voice's models downloads, its row shows the progress; the
// other models not on this computer wait, inert and pointing at the reason;
// a model already here can still be picked, since that downloads nothing.
func TestSetupModelsWhileVoiceDownloads(t *testing.T) {
	html := renderSetupModels(t, SetupModelsViewData{
		Models: []SetupModelView{
			{Name: "large", Size: "Large", Bytes: "1.51 GB", Chosen: true, Ref: "voice/large", Downloading: true, Pct: 54},
			{Name: "small", Size: "Small", Bytes: "466 MB", Downloaded: true, Ref: "voice/small"},
			{Name: "base", Size: "Base", Bytes: "139 MB", Ref: "voice/base"},
		},
		Commands:    []SetupModelView{{Name: "std", Size: "Standard", Bytes: "153 MB", Chosen: true, Ref: "voice/std"}},
		Downloading: true,
	})
	large := rowOf(t, html, "Large")
	for _, want := range []string{">Downloading · 54%<", `role="progressbar"`, `aria-valuenow="54"`, `aria-label="Downloading the Large dictation model"`, "--progress: 54%;"} {
		if !strings.Contains(large, want) {
			t.Errorf("missing %q: %s", want, large)
		}
	}
	if strings.Contains(large, "@post") {
		t.Errorf("the chosen row on its way posts nothing: %s", large)
	}
	base := rowOf(t, html, "Base")
	if !strings.Contains(base, `aria-disabled="true" aria-describedby="setup-models-wait"`) || strings.Contains(base, "@post") || !strings.Contains(base, "is-waiting") {
		t.Errorf("a missing model waits while another downloads: %s", base)
	}
	std := rowOf(t, html, "Standard")
	if !strings.Contains(std, `disabled aria-describedby="setup-models-wait">Download<`) || strings.Contains(std, "@post") {
		t.Errorf("the chosen command model waits for its download too: %s", std)
	}
	if small := rowOf(t, html, "Small"); !strings.Contains(small, "methods/model/pick") {
		t.Errorf("a downloaded model stays pickable: %s", small)
	}
	if !strings.Contains(html, `id="setup-models-wait">Downloads run one at a time, so a model that is not on this computer yet can be picked when this one finishes.<`) {
		t.Errorf("missing the reason: %s", html)
	}

	// Before the first progress line the bar sweeps instead of sitting at 0.
	html = renderSetupModels(t, SetupModelsViewData{
		Models:      []SetupModelView{{Name: "large", Size: "Large", Chosen: true, Ref: "voice/large", Downloading: true}},
		Downloading: true,
	})
	if !strings.Contains(html, ">Starting download…<") || !strings.Contains(html, "bk-progress-indeterminate") || strings.Contains(html, "aria-valuenow") {
		t.Errorf("starting: %s", html)
	}
}

// While another plugin downloads, the card says so instead of showing its
// bar as voice's, and every model voice would have to download waits.
func TestSetupModelsSaysAnotherPluginIsDownloading(t *testing.T) {
	html := renderSetupModels(t, SetupModelsViewData{
		Models: []SetupModelView{
			{Name: "large", Size: "Large", Chosen: true, Ref: "voice/large"},
			{Name: "small", Size: "Small", Downloaded: true, Ref: "voice/small"},
		},
		Commands:      []SetupModelView{{Name: "cmd", Size: "Standard", Chosen: true, Ref: "voice/cmd"}},
		OtherDownload: "Another plugin (announcements) is downloading kokoro",
	})
	if strings.Contains(html, "progressbar") || strings.Contains(html, "Downloading ·") {
		t.Fatalf("another plugin's download shown as voice's progress: %s", html)
	}
	if !strings.Contains(html, `role="status" id="setup-models-wait">Another plugin (announcements) is downloading kokoro. Downloads run one at a time`) {
		t.Fatalf("missing the reason: %s", html)
	}
	if got := strings.Count(html, `disabled aria-describedby="setup-models-wait">Download<`); got != 2 {
		t.Fatalf("want both Download buttons disabled and described, got %d: %s", got, html)
	}
	if strings.Contains(html, "model/download") || strings.Contains(html, "&#34;name&#34;:&#34;large&#34;") || strings.Contains(html, "&#34;name&#34;:&#34;cmd&#34;") {
		t.Fatalf("a model that would download still posts: %s", html)
	}
	if small := rowOf(t, html, "Small"); !strings.Contains(small, "methods/model/pick") {
		t.Errorf("a downloaded model stays pickable: %s", small)
	}
}

// Picking Light while Standard is in use: Light is checked and on its way,
// Standard keeps running and says so, cannot be deleted, and can be picked
// back.
func TestSetupCommandModelStandsInWhileTheChosenOneDownloads(t *testing.T) {
	html := renderSetupModels(t, SetupModelsViewData{
		Commands: []SetupModelView{
			{Name: "std", Size: "Standard", Note: "most accurate", InUse: true, Downloaded: true, Ref: "voice/std"},
			{Name: "light", Size: "Light", Note: "smaller and faster", Chosen: true, Ref: "voice/light", Downloading: true, Pct: 10},
		},
		Downloading: true,
	})
	std := rowOf(t, html, "Standard")
	if !strings.Contains(std, `aria-checked="false"`) || !strings.Contains(std, ">In use<") || !strings.Contains(std, "methods/command_model/pick") {
		t.Errorf("the stand-in is in use, not chosen, and can be picked back: %s", std)
	}
	if strings.Contains(std, "Delete the") {
		t.Errorf("the command model in use offered for deletion: %s", std)
	}
	light := rowOf(t, html, "Light")
	if !strings.Contains(light, `aria-checked="true"`) || !strings.Contains(light, ">Downloading · 10%<") || !strings.Contains(light, "Downloading the Light command model") {
		t.Errorf("the chosen one is on its way: %s", light)
	}
}

func TestSetupModelFactsAndState(t *testing.T) {
	m := SetupModelView{Size: "Large", Bytes: "1.51 GB", Languages: "Many languages", Note: "most accurate"}
	if got := m.Facts(); got != "most accurate · Many languages · 1.51 GB" {
		t.Errorf("Facts = %q", got)
	}
	if got := (SetupModelView{Bytes: "70 MB", Languages: "English"}).Facts(); got != "English · 70 MB" {
		t.Errorf("Facts without a note = %q", got)
	}
	cases := []struct {
		m    SetupModelView
		want string
	}{
		{SetupModelView{Bytes: "70 MB"}, "70 MB download"},
		{SetupModelView{}, "Download"},
		{SetupModelView{Chosen: true, Bytes: "70 MB"}, "Not downloaded yet"},
		{SetupModelView{Downloaded: true}, "On this computer"},
		{SetupModelView{Downloaded: true, InUse: true, Chosen: true}, "In use"},
		{SetupModelView{Downloading: true, Chosen: true}, "Starting download…"},
		{SetupModelView{Downloading: true, Pct: 7}, "Downloading · 7%"},
	}
	for _, c := range cases {
		if got := c.m.State(); got != c.want {
			t.Errorf("State(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}

func TestSetupModelsWaitNote(t *testing.T) {
	if got := (SetupModelsViewData{}).WaitNote(); got != "" {
		t.Errorf("nothing downloading: %q", got)
	}
	own := SetupModelsViewData{Downloading: true}
	if got := own.WaitNote(); !strings.HasPrefix(got, "Voice is downloading another of its models.") {
		t.Errorf("voice downloading a model not on the card: %q", got)
	}
	own.Commands = []SetupModelView{{Downloading: true}}
	if got := own.WaitNote(); !strings.HasSuffix(got, "when this one finishes.") {
		t.Errorf("voice downloading a model on the card: %q", got)
	}
}
