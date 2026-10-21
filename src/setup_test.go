package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func catalogForTest() []declaredModel {
	return []declaredModel{
		{Name: "openai_whisper-large-v3-v20240930", Description: "Dictation — best quality, slowest", SizeBytes: 1620000000},
		{Name: "openai_whisper-small.en", Description: "Dictation — balanced quality and speed", SizeBytes: 489000000},
	}
}

func TestModelsStepFollowsTheDownloadAndTheModelOnDisk(t *testing.T) {
	cat := catalogForTest()
	large := cat[0].Name

	state, summary := modelsStep(true, false, large, nil, cat)
	if state != setupStateInProgress || summary == "" {
		t.Fatalf("downloading: got %q %q", state, summary)
	}

	state, _ = modelsStep(false, false, large, map[string]string{"openai_whisper-small.en": "466 MB"}, cat)
	if state != setupStateNeedsYou {
		t.Fatalf("the model that loads is not on disk: got %q", state)
	}

	state, summary = modelsStep(false, false, large, map[string]string{large: "1.5 GB", defaultCommandModel: "153 MB"}, cat)
	if state != setupStateDone || summary != "Large · 1.5 GB · commands on" {
		t.Fatalf("done: got %q %q", state, summary)
	}

	_, summary = modelsStep(false, false, large, map[string]string{large: "1.5 GB"}, cat)
	if summary != "Large · 1.5 GB · commands not downloaded" {
		t.Fatalf("no command model: got %q", summary)
	}

	// Another plugin's download is not voice's progress: the step waits on
	// the person, and says what holds the Download button.
	state, summary = modelsStep(false, true, large, nil, cat)
	if state != setupStateNeedsYou || summary != "Another plugin is downloading; download yours when it finishes" {
		t.Fatalf("another plugin downloading: got %q %q", state, summary)
	}
	state, _ = modelsStep(false, true, large, map[string]string{large: "1.5 GB"}, cat)
	if state != setupStateDone {
		t.Fatalf("another plugin downloading, voice's model on disk: got %q", state)
	}
}

func TestOnlyVoicesDownloadIsVoicesProgress(t *testing.T) {
	cases := []struct {
		name  string
		dl    *ModelDownloadData
		own   bool
		other string
	}{
		{"no record", nil, false, ""},
		{"nothing running", &ModelDownloadData{Model: "announcements/kokoro"}, false, ""},
		{"voice's download", &ModelDownloadData{Downloading: true, Model: "voice/openai_whisper-small.en"}, true, ""},
		{"another plugin's download", &ModelDownloadData{Downloading: true, Model: "announcements/kokoro"}, false, "Another plugin (announcements) is downloading kokoro"},
		{"a plugin whose id shares the prefix", &ModelDownloadData{Downloading: true, Model: "voicex/m"}, false, "Another plugin (voicex) is downloading m"},
		{"a record without a model ref", &ModelDownloadData{Downloading: true}, false, "Another plugin is downloading a model"},
	}
	for _, c := range cases {
		if got := ownDownloadRunning(c.dl, "voice/"); got != c.own {
			t.Errorf("%s: own = %v, want %v", c.name, got, c.own)
		}
		if got := otherPluginDownload(c.dl, "voice/"); got != c.other {
			t.Errorf("%s: other = %q, want %q", c.name, got, c.other)
		}
	}
}

func TestFactsArePlain(t *testing.T) {
	f := factsFor(catalogForTest()[1], "")
	want := modelFacts{Size: "Small", Bytes: "466 MB", Languages: "English", Note: "balanced quality and speed"}
	if f != want {
		t.Fatalf("got %+v, want %+v", f, want)
	}
	if f := factsFor(catalogForTest()[0], ""); f.Languages != "Many languages" || f.Size != "Large" {
		t.Fatalf("large: %+v", f)
	}
}

func TestKeyCapsUseThisOSWords(t *testing.T) {
	cases := []struct {
		chord, goos string
		want        []string
	}{
		{"opt+t", "darwin", []string{"⌥", "T"}},
		{"opt+t", "linux", []string{"Alt", "T"}},
		{"cmd+shift+g", "windows", []string{"Shift", "Win", "G"}},
		{"ctrl+space", "linux", []string{"Ctrl", "Space"}},
	}
	for _, c := range cases {
		if got := keyCaps(c.chord, c.goos); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s on %s: got %v, want %v", c.chord, c.goos, got, c.want)
		}
	}
}

func TestActiveKeyReadsTheBindingTable(t *testing.T) {
	table := json.RawMessage(`{"entries":[
		{"source":"keyboard","trigger":"ctrl+d DOWN","action":"voice.dictation"},
		{"source":"keyboard","trigger":"ctrl+d UP","action":"voice.dictation"},
		{"source":"xppen","trigger":"button1","action":"voice.command"}
	]}`)
	if got := activeKey(table, "voice.dictation"); got != "ctrl+d" {
		t.Fatalf("dictation: got %q", got)
	}
	if got := activeKey(table, "voice.command"); got != "" {
		t.Fatalf("a device button is not a keyboard chord: got %q", got)
	}
	wrapped := json.RawMessage(`{"singleton":` + string(table) + `}`)
	if got := activeKey(wrapped, "voice.dictation"); got != "ctrl+d" {
		t.Fatalf("wrapped: got %q", got)
	}
	if got := activeKey(nil, "voice.dictation"); got != "" {
		t.Fatalf("nothing: got %q", got)
	}
}

func TestParseModelDownload(t *testing.T) {
	if md := parseModelDownload(json.RawMessage(`{"downloading":true,"progress":0.4}`)); !md.Downloading || md.Progress != 0.4 {
		t.Fatalf("direct: %+v", md)
	}
	if md := parseModelDownload(json.RawMessage(`{"singleton":{"downloading":true,"progress":0.5}}`)); !md.Downloading {
		t.Fatalf("by id: %+v", md)
	}
	if md := parseModelDownload(nil); md.Downloading {
		t.Fatalf("nothing: %+v", md)
	}
}

func TestSetupDownloadErrorOnlyVoicesStandingFailure(t *testing.T) {
	small, large := "openai_whisper-small.en", "openai_whisper-large-v3-v20240930"
	fail := func(ref string) *ModelDownloadData {
		return &ModelDownloadData{Model: ref, Error: "checksum mismatch"}
	}
	cases := []struct {
		name   string
		dl     *ModelDownloadData
		chosen string
		onDisk map[string]string
		want   string
	}{
		{"no record", nil, large, nil, ""},
		{"no error", &ModelDownloadData{Model: "voice/" + large}, large, nil, ""},
		{"voice's chosen model failed", fail("voice/" + large), large, nil, "checksum mismatch"},
		{"another plugin's model failed", fail("announcements/kokoro"), large, nil, ""},
		{"record without a model ref", fail(""), large, nil, ""},
		{"a plugin whose id shares the prefix", fail("voicex/" + large), large, nil, ""},
		{"switched to a model already on disk", fail("voice/" + large), small, map[string]string{small: "466 MB"}, ""},
		{"failed model since downloaded", fail("voice/" + large), large, map[string]string{large: "1.5 GB"}, ""},
		{"chosen model also missing", fail("voice/" + large), small, nil, "checksum mismatch"},
		{"command model failed, dictation on disk", fail("voice/" + defaultCommandModel), small, map[string]string{small: "466 MB"}, "checksum mismatch"},
		{"command model since downloaded", fail("voice/" + defaultCommandModel), small, map[string]string{defaultCommandModel: "153 MB"}, ""},
	}
	for _, c := range cases {
		if got := setupDownloadError(c.dl, "voice/", c.chosen, c.onDisk); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestReporterSendsOnlyChangesAndRetriesAFailure(t *testing.T) {
	var sent []string
	fail := false
	r := &setupReporter{send: func(step, state, summary string) error {
		sent = append(sent, step+"="+state+":"+summary)
		if fail {
			return errors.New("down")
		}
		return nil
	}}
	r.report("models", setupStateNeedsYou, "x")
	r.report("models", setupStateNeedsYou, "x")
	r.report("models", setupStateDone, "y")
	if len(sent) != 2 {
		t.Fatalf("a repeat is not sent: %v", sent)
	}
	fail = true
	r.report("try", setupStateDone, "z")
	fail = false
	r.report("try", setupStateDone, "z")
	if len(sent) != 4 {
		t.Fatalf("a failed send is retried on the next check: %v", sent)
	}
}

func TestStartStepFoldsModelsThenChecks(t *testing.T) {
	cases := []struct {
		mState, tState string
		tried          bool
		want           string
	}{
		{setupStateNeedsYou, setupStateDone, true, setupStateNeedsYou},         // no model: that first
		{setupStateInProgress, setupStateNeedsYou, true, setupStateInProgress}, // downloading beats a silent mic
		{setupStateDone, setupStateNeedsYou, true, setupStateNeedsYou},         // a silent mic reopens it
		{setupStateDone, setupStateInProgress, true, setupStateDone},           // checks half done: optional
		{setupStateDone, "", false, setupStateDone},                            // never tried
	}
	for _, c := range cases {
		if got, _ := startStep(c.mState, "m", c.tState, "t", c.tried); got != c.want {
			t.Errorf("models %s, checks %s (tried %v): got %s, want %s", c.mState, c.tState, c.tried, got, c.want)
		}
	}
}
