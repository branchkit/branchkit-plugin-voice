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
				{Name: "large", Size: "Large", Bytes: "1.6 GB", Chosen: true, Downloaded: true, Ref: "r1"},
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
		"Dictation language",
		"Delete the Small model",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "Delete the Large model") {
		t.Error("the model in use offered for deletion")
	}
}

// While another plugin downloads, the models section says so instead of
// showing its bar as voice's, and voice's Download buttons stay, disabled,
// pointing at the reason.
func TestSetupModelsSaysAnotherPluginIsDownloading(t *testing.T) {
	var b strings.Builder
	data := SetupModelsViewData{
		Models:        []SetupModelView{{Name: "large", Size: "Large", Chosen: true, Ref: "voice/large"}},
		Commands:      []SetupModelView{{Name: "cmd", Size: "Standard", Chosen: true, Ref: "voice/cmd"}},
		OtherDownload: "Another plugin (announcements) is downloading kokoro",
	}
	if err := SetupModels(data).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if strings.Contains(html, "Downloading…") || strings.Contains(html, "model-progress-fill") {
		t.Fatalf("another plugin's download shown as voice's progress: %s", html)
	}
	if !strings.Contains(html, `id="setup-other-download"`) || !strings.Contains(html, "Another plugin (announcements) is downloading kokoro. Downloads run one at a time") {
		t.Fatalf("missing the reason: %s", html)
	}
	if got := strings.Count(html, `disabled aria-describedby="setup-other-download"`); got != 2 {
		t.Fatalf("want both Download buttons disabled and described, got %d: %s", got, html)
	}
	if strings.Contains(html, "model/download") {
		t.Fatalf("a Download button still posts: %s", html)
	}
}

// The command rows: the model in use shows a badge and no delete; a chosen
// model still to download offers Download; another downloaded model offers Use
// (posting command_model/set) and delete; one not downloaded offers Choose.
func TestSetupCommandModelRows(t *testing.T) {
	render := func(cmds []SetupModelView) string {
		var b strings.Builder
		if err := SetupModels(SetupModelsViewData{Commands: cmds}).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	html := render([]SetupModelView{
		{Name: "std", Size: "Standard", Note: "most accurate", Chosen: true, InUse: true, Downloaded: true, Ref: "voice/std"},
		{Name: "light", Size: "Light", Note: "smaller and faster", Downloaded: true, Ref: "voice/light"},
	})
	for _, want := range []string{"Commands · Standard", "Commands · Light", "In use", "command_model/set", "Delete the Light command model"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "Delete the Standard command model") {
		t.Error("the command model in use offered for deletion")
	}
	// Light chosen and downloading: Standard still runs, Light offers Download.
	html = render([]SetupModelView{
		{Name: "std", Size: "Standard", InUse: true, Downloaded: true, Ref: "voice/std"},
		{Name: "light", Size: "Light", Chosen: true, Ref: "voice/light"},
	})
	if !strings.Contains(html, "In use") || !strings.Contains(html, "voice/light") || !strings.Contains(html, "model/download") {
		t.Errorf("chosen-but-missing row should offer Download while the other stays in use: %s", html)
	}
	html = render([]SetupModelView{{Name: "light", Size: "Light", Ref: "voice/light"}})
	if !strings.Contains(html, ">Choose<") {
		t.Errorf("a model neither chosen nor downloaded should offer Choose: %s", html)
	}
}
