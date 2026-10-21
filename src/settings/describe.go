package settings

// Natural language description generation for voice rules.
// Produces human-readable sentences from command structure.
// Used by both the Rules view (rule card descriptions) and
// the Library view (impact descriptions).

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/branchkit/branchkit-plugin-voice/views"
)

// actionForDescribe holds the fields from a command's action JSON.
type actionForDescribe struct {
	Type      string `json:"type"`
	Command   string `json:"command"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	Direction string `json:"direction"`
	Action    string `json:"action"` // for clipboard
	AppID     string `json:"app_id"`
	Target    string `json:"target"` // for hud.show
}

// describeRule generates a natural language description from a CommandDetail.
func DescribeRule(cmd CommandDetail) views.RuleDescription {
	return views.RuleDescription{
		Trigger:    DescribeTrigger(cmd.Phrase),
		Effect:     DescribeEffect(cmd.Action),
		Condition:  DescribeConditions(cmd.RequiresTags),
		SideEffect: DescribeSideEffects(cmd.SetsTags, cmd.ClearsTags),
	}
}

// describeTrigger generates "Say "press" + modifiers + key" from phrase slots.
func DescribeTrigger(raw json.RawMessage) string {
	var phrase []json.RawMessage
	if err := json.Unmarshal(raw, &phrase); err != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return fmt.Sprintf("Say \"%s\"", s)
	}

	var parts []string
	for _, elem := range phrase {
		var s string
		if err := json.Unmarshal(elem, &s); err == nil {
			if m := CaptureTokenRe.FindStringSubmatch(s); len(m) > 1 {
				parts = append(parts, DescribeSlot(m[1], m[0]))
			} else {
				parts = append(parts, fmt.Sprintf("\"%s\"", s))
			}
			continue
		}
		var alts []string
		if err := json.Unmarshal(elem, &alts); err == nil && len(alts) > 0 {
			if m := CaptureTokenRe.FindStringSubmatch(alts[0]); len(m) > 1 {
				parts = append(parts, DescribeSlot(m[1], m[0]))
			} else {
				parts = append(parts, fmt.Sprintf("\"%s\"", alts[0]))
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return "Say " + strings.Join(parts, " + ")
}

// describeSlot converts a capture token to a readable slot description.
func DescribeSlot(inner string, full string) string {
	name := SlotDisplayName(inner)

	if strings.HasSuffix(full, "+") {
		return "one or more " + name + "s"
	}
	if strings.HasSuffix(full, "*") {
		return "optional " + name + "s"
	}
	return name
}

// slotDisplayName converts a capture inner group to a readable name.
// e.g. "keys|alphabet" → "key", "modifiers" → "modifier"
func SlotDisplayName(inner string) string {
	name := strings.Split(inner, "|")[0]
	if display, ok := CollectionDisplayNames[name]; ok {
		return display
	}
	name = strings.ReplaceAll(name, "_", " ")
	return name
}

// describeEffect generates a verb phrase from the action JSON.
func DescribeEffect(raw json.RawMessage) string {
	var a actionForDescribe
	if err := json.Unmarshal(raw, &a); err != nil {
		return ""
	}
	// An action carries its values inline or, as commands.json writes it,
	// under "params"; read both, params winning.
	var nested struct {
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &nested) == nil && len(nested.Params) > 0 {
		typ := a.Type
		if err := json.Unmarshal(nested.Params, &a); err == nil {
			a.Type = typ
		}
	}

	switch a.Type {
	case "input.type":
		if a.Text != "" && !strings.Contains(a.Text, "{") {
			return fmt.Sprintf("type \"%s\"", a.Text)
		}
		return "type that text"
	case "input.key":
		return "press that key"
	case "input.key_by_name":
		if a.Name != "" && !strings.Contains(a.Name, "{") {
			return fmt.Sprintf("press %s", a.Name)
		}
		return "press that key"
	case "input.shortcut":
		return "press that shortcut"
	case "input.shortcut_by_name":
		return "press key with modifiers"
	case "input.scroll":
		if a.Direction != "" && !strings.Contains(a.Direction, "{") {
			return fmt.Sprintf("scroll %s", a.Direction)
		}
		return "scroll in that direction"
	case "input.click":
		return "click"
	case "apps.launch":
		if a.AppID != "" && !strings.Contains(a.AppID, "{") {
			// The last part of a macOS bundle id ("com.apple.Safari") is
			// the app's name; a Linux or Windows id is its name already.
			name := a.AppID[strings.LastIndex(a.AppID, ".")+1:]
			return fmt.Sprintf("switch to %s", name)
		}
		return "switch to that app"
	case "input.clipboard":
		switch a.Action {
		case "copy":
			return "copy"
		case "paste":
			return "paste"
		case "cut":
			return "cut"
		}
		return a.Action
	case "hud.show":
		if a.Target != "" {
			return fmt.Sprintf("show %s", a.Target)
		}
		return "show HUD"
	case "hud.hide":
		return "hide HUD"
	case "sequence":
		return "run a sequence of actions"
	default:
		return a.Type
	}
}

// Known tag descriptions for conditions and side effects.
var TagDescriptions = map[string]string{
	"plugin.voice.mode.spelling": "spelling mode",
	"plugin.voice.speaking":      "dictation mode",
	"plugin.wm.tiled":            "window is tiled",
	"plugin.wm.resize_mode":      "resize mode",
}

// describeConditions generates the "while ..." clause from requires_tags.
func DescribeConditions(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	var parts []string
	for _, tag := range tags {
		if desc, ok := TagDescriptions[tag]; ok {
			parts = append(parts, desc)
		} else {
			// Fallback: strip prefix, replace dots/underscores
			short := tag
			if i := strings.LastIndex(tag, "."); i >= 0 {
				short = tag[i+1:]
			}
			short = strings.ReplaceAll(short, "_", " ")
			parts = append(parts, short)
		}
	}
	return "while in " + strings.Join(parts, " and ")
}

// describeSideEffects generates the "then enters/exits ..." clause.
func DescribeSideEffects(sets []string, clears []string) string {
	var parts []string
	for _, tag := range sets {
		if desc, ok := TagDescriptions[tag]; ok {
			parts = append(parts, "enters "+desc)
		} else {
			short := tag
			if i := strings.LastIndex(tag, "."); i >= 0 {
				short = tag[i+1:]
			}
			parts = append(parts, "activates "+strings.ReplaceAll(short, "_", " "))
		}
	}
	for _, tag := range clears {
		if desc, ok := TagDescriptions[tag]; ok {
			parts = append(parts, "exits "+desc)
		} else {
			short := tag
			if i := strings.LastIndex(tag, "."); i >= 0 {
				short = tag[i+1:]
			}
			parts = append(parts, "deactivates "+strings.ReplaceAll(short, "_", " "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "then " + strings.Join(parts, ", ")
}
