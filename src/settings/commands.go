package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// LoadCommandDetails reads commands.json and builds a lookup from raw canonical phrase to detail.
func LoadCommandDetails(pluginDir string) map[string]CommandDetail {
	data, err := os.ReadFile(filepath.Join(pluginDir, "commands.json"))
	if err != nil {
		return nil
	}
	var commands []CommandDetail
	if err := json.Unmarshal(data, &commands); err != nil {
		return nil
	}

	result := make(map[string]CommandDetail, len(commands)*2)
	for _, cmd := range commands {
		canonical := RawPhraseCanonical(cmd.Phrase)
		result[canonical] = cmd
		// Also index by display phrase (e.g. "scroll [direction]") for editor lookups
		display, _ := ParsePhraseForDeps(cmd.Phrase)
		if display != canonical {
			result[display] = cmd
		}
	}
	return result
}

// RawPhraseCanonical returns the first-alternative join of a phrase without display
// formatting. This matches the platform's display_phrase/canonical_name output.
func RawPhraseCanonical(raw json.RawMessage) string {
	var phrase []json.RawMessage
	if err := json.Unmarshal(raw, &phrase); err != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}

	var parts []string
	for _, elem := range phrase {
		var s string
		if err := json.Unmarshal(elem, &s); err == nil {
			parts = append(parts, s)
			continue
		}
		var alts []string
		if err := json.Unmarshal(elem, &alts); err == nil && len(alts) > 0 {
			parts = append(parts, alts[0])
		}
	}
	return strings.Join(parts, " ")
}

// RawPhraseFull is like RawPhraseCanonical but preserves alternatives instead
// of collapsing to the first option, so editing an alternatives command doesn't
// silently drop the other options on save. RawPhraseCanonical stays the stable
// *identity* form (first-alt) used for command lookup/dedup; only the editable
// phrase needs full fidelity.
//
// Multi-option alternatives are wrapped in parentheses ("(new line, return)")
// so options containing spaces survive the whitespace-delimited phrase string —
// the platform's parse_pattern_slots reads the group back into alternatives.
func RawPhraseFull(raw json.RawMessage) string {
	var phrase []json.RawMessage
	if err := json.Unmarshal(raw, &phrase); err != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}

	var parts []string
	for _, elem := range phrase {
		var s string
		if err := json.Unmarshal(elem, &s); err == nil {
			parts = append(parts, s)
			continue
		}
		var alts []string
		if err := json.Unmarshal(elem, &alts); err == nil && len(alts) > 0 {
			if len(alts) == 1 {
				parts = append(parts, alts[0])
			} else {
				// Parens group the options so multi-word alternatives keep
				// their spaces through the phrase string.
				parts = append(parts, "("+strings.Join(alts, ", ")+")")
			}
		}
	}
	return strings.Join(parts, " ")
}
