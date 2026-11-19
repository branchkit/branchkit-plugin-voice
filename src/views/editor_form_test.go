package views

import (
	"context"
	"strings"
	"testing"
)

// While Record is on, the field that takes the keys asks for focus, has a
// name and says how to cancel. Focus used to stay on the button that had
// just become Stop, so the pressed keys never reached the recorder (and
// Enter or Space pressed Stop), and the field had only a placeholder.
func TestRecordingFieldsTakeFocusAndAreNamed(t *testing.T) {
	render := func(v EditorView) string {
		t.Helper()
		var b strings.Builder
		if err := itDoes(v).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	// The recorder field of the rendered form: the input drawn while recording.
	field := func(html string) string {
		t.Helper()
		at := strings.Index(html, `class="recording-pulse`)
		if at < 0 {
			t.Fatalf("no recording field in %s", html)
		}
		start := strings.LastIndex(html[:at], "<input")
		end := strings.Index(html[at:], ">")
		return html[start : at+end+1]
	}

	simple := render(EditorView{ShowSimple: true, ShowRecord: true, Recording: true})
	f := field(simple)
	for _, want := range []string{
		`data-init="el.focus()"`,
		`aria-label="Press the keys for this command"`,
		`aria-describedby="newRuleRecordHint"`,
	} {
		if !strings.Contains(f, want) {
			t.Errorf("the recording field lacks %s: %s", want, f)
		}
	}
	if !strings.Contains(simple, `<span id="newRuleRecordHint" hidden>Recording. Esc cancels.</span>`) {
		t.Errorf("no hint for the field to be described by: %s", simple)
	}

	tmpl := render(EditorView{ShowTemplate: true, TmplFields: []TmplFieldView{
		{Index: 2, ShowInput: true, ShowRecord: true, Recording: true},
	}})
	f = field(tmpl)
	for _, want := range []string{
		`data-init="el.focus()"`,
		`aria-label="Press the keys for this command"`,
		`aria-describedby="tmplf2rechint"`,
	} {
		if !strings.Contains(f, want) {
			t.Errorf("the template recording field lacks %s: %s", want, f)
		}
	}
	if !strings.Contains(tmpl, `<span id="tmplf2rechint" hidden>Recording. Esc cancels.</span>`) {
		t.Errorf("no hint for the template field: %s", tmpl)
	}

	// Not recording: nothing asks for focus.
	idle := render(EditorView{ShowSimple: true, ShowRecord: true})
	if strings.Contains(idle, `data-init="el.focus()"`) || strings.Contains(idle, "newRuleRecordHint") {
		t.Errorf("an idle form asks for focus: %s", idle)
	}
}
