package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSlotDisplayName(t *testing.T) {
	tests := []struct {
		inner string
		want  string
	}{
		{"keys", "key"},
		{"alphabet", "alphabet"},
		{"modifiers", "modifier"},
		{"keys|alphabet", "key"},
		{"direction", "direction"},
		{"number", "number"},
	}
	for _, tt := range tests {
		got := SlotDisplayName(tt.inner)
		if got != tt.want {
			t.Errorf("SlotDisplayName(%q) = %q, want %q", tt.inner, got, tt.want)
		}
	}
}

func TestDescribeSlot(t *testing.T) {
	tests := []struct {
		inner string
		full  string
		want  string
	}{
		{"direction", "<direction>", "direction"},
		{"alphabet", "<alphabet>+", "one or more alphabets"},
		{"modifiers", "<modifiers>*", "optional modifiers"},
		{"keys|alphabet", "<keys|alphabet>", "key"},
	}
	for _, tt := range tests {
		got := DescribeSlot(tt.inner, tt.full)
		if got != tt.want {
			t.Errorf("DescribeSlot(%q, %q) = %q, want %q", tt.inner, tt.full, got, tt.want)
		}
	}
}

func TestDescribeTrigger_Simple(t *testing.T) {
	raw := json.RawMessage(`["undo"]`)
	got := DescribeTrigger(raw)
	if got != `Say "undo"` {
		t.Errorf("got %q, want %q", got, `Say "undo"`)
	}
}

func TestDescribeTrigger_Alternatives(t *testing.T) {
	raw := json.RawMessage(`[["new line", "return"]]`)
	got := DescribeTrigger(raw)
	if got != `Say "new line"` {
		t.Errorf("got %q, want %q", got, `Say "new line"`)
	}
}

func TestDescribeTrigger_WithCaptures(t *testing.T) {
	raw := json.RawMessage(`["press", "<modifiers>*", "<keys|alphabet>"]`)
	got := DescribeTrigger(raw)
	want := `Say "press" + optional modifiers + key`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDescribeTrigger_SpellPlus(t *testing.T) {
	raw := json.RawMessage(`["spell", "<alphabet>+"]`)
	got := DescribeTrigger(raw)
	want := `Say "spell" + one or more alphabets`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDescribeEffect_Type(t *testing.T) {
	raw := json.RawMessage(`{"type": "input.type", "text": "hello"}`)
	got := DescribeEffect(raw)
	if got != `type "hello"` {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_TypeTemplate(t *testing.T) {
	raw := json.RawMessage(`{"type": "input.type", "text": "{text}"}`)
	got := DescribeEffect(raw)
	if got != "type that text" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_KeyByName(t *testing.T) {
	raw := json.RawMessage(`{"type": "input.key_by_name", "name": "escape"}`)
	got := DescribeEffect(raw)
	if got != "press escape" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_ShortcutByName(t *testing.T) {
	raw := json.RawMessage(`{"type": "input.shortcut_by_name", "name": "{keys}", "modifiers": ["{modifiers}"]}`)
	got := DescribeEffect(raw)
	if got != "press key with modifiers" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_Scroll(t *testing.T) {
	raw := json.RawMessage(`{"type": "input.scroll", "direction": "up", "amount": 300}`)
	got := DescribeEffect(raw)
	if got != "scroll up" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_ScrollTemplate(t *testing.T) {
	raw := json.RawMessage(`{"type": "input.scroll", "direction": "{direction}", "amount": 300}`)
	got := DescribeEffect(raw)
	if got != "scroll in that direction" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_Launch(t *testing.T) {
	for _, raw := range []string{
		`{"type": "apps.launch", "app_id": "com.apple.Safari"}`,
		`{"type": "apps.launch", "params": {"app_id": "com.apple.Safari"}}`,
		`{"type": "apps.launch", "params": {"app_id": "safari"}}`,
	} {
		got := DescribeEffect(json.RawMessage(raw))
		if !strings.EqualFold(got, "switch to Safari") {
			t.Errorf("%s: got %q", raw, got)
		}
	}
	if got := DescribeEffect(json.RawMessage(`{"type": "apps.launch", "params": {"app_id": "{apps}"}}`)); got != "switch to that app" {
		t.Errorf("capture: got %q", got)
	}
}

func TestDescribeEffect_Clipboard(t *testing.T) {
	for _, action := range []string{"copy", "paste", "cut"} {
		raw := json.RawMessage(`{"type": "input.clipboard", "action": "` + action + `"}`)
		got := DescribeEffect(raw)
		if got != action {
			t.Errorf("clipboard %s: got %q", action, got)
		}
	}
}

func TestDescribeEffect_HudShow(t *testing.T) {
	raw := json.RawMessage(`{"type": "hud.show", "target": "commands"}`)
	got := DescribeEffect(raw)
	if got != "show commands" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_HudHide(t *testing.T) {
	raw := json.RawMessage(`{"type": "hud.hide"}`)
	got := DescribeEffect(raw)
	if got != "hide HUD" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeEffect_Sequence(t *testing.T) {
	raw := json.RawMessage(`{"type": "sequence", "actions": [{}, {}]}`)
	got := DescribeEffect(raw)
	if got != "run a sequence of actions" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeConditions_Empty(t *testing.T) {
	got := DescribeConditions(nil)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDescribeConditions_KnownTag(t *testing.T) {
	got := DescribeConditions([]string{"plugin.voice.mode.spelling"})
	if got != "while in spelling mode" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeConditions_UnknownTag(t *testing.T) {
	got := DescribeConditions([]string{"plugin.foo.bar_baz"})
	if got != "while in bar baz" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeConditions_Multiple(t *testing.T) {
	got := DescribeConditions([]string{"plugin.voice.mode.spelling", "plugin.voice.speaking"})
	if got != "while in spelling mode and dictation mode" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeSideEffects_Empty(t *testing.T) {
	got := DescribeSideEffects(nil, nil)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDescribeSideEffects_Sets(t *testing.T) {
	got := DescribeSideEffects([]string{"plugin.voice.mode.spelling"}, nil)
	if got != "then enters spelling mode" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeSideEffects_Clears(t *testing.T) {
	got := DescribeSideEffects(nil, []string{"plugin.voice.mode.spelling"})
	if got != "then exits spelling mode" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeSideEffects_Both(t *testing.T) {
	got := DescribeSideEffects(
		[]string{"plugin.voice.speaking"},
		[]string{"plugin.voice.mode.spelling"},
	)
	if got != "then enters dictation mode, exits spelling mode" {
		t.Errorf("got %q", got)
	}
}

func TestDescribeRule_FullCommand(t *testing.T) {
	cmd := CommandDetail{
		Phrase:   json.RawMessage(`["spell", "<alphabet>+"]`),
		Action:   json.RawMessage(`{"type": "input.type", "text": "{alphabet:join(\"\")}"}`),
		SetsTags: []string{"plugin.voice.mode.spelling"},
	}
	desc := DescribeRule(cmd)
	if desc.Trigger != `Say "spell" + one or more alphabets` {
		t.Errorf("Trigger = %q", desc.Trigger)
	}
	if desc.Effect != "type that text" {
		t.Errorf("Effect = %q", desc.Effect)
	}
	if desc.Condition != "" {
		t.Errorf("Condition = %q, want empty", desc.Condition)
	}
	if desc.SideEffect != "then enters spelling mode" {
		t.Errorf("SideEffect = %q", desc.SideEffect)
	}
}
