package settings

// Library domain: dependency scanning and phrase parsing helpers.
// List UI and overrides are handled by the platform Collections tab.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// EntryDependency is the output shape of EntryDependencies, which has no
// production caller yet — it answers "what breaks if this list entry goes",
// the question a list editor asks before letting you delete a row. All three
// fields are its answer, so none is separable from it.
type EntryDependency struct {
	//deadfield:ignore output of EntryDependencies, which nothing calls in
	// production yet. Staged for the list/phonetics editing work; the fields
	// ARE the return contract, so they cannot be trimmed without gutting it.
	// Delete the function and this type together, or wire it — the choice
	// belongs to whoever owns that UI.
	CommandPhrase string // e.g. "press [modifier]* [key]"
	//deadfield:ignore see CommandPhrase above — same unconsumed return shape.
	DisplayPhrase string // e.g. "press arch → a"
	//deadfield:ignore see CommandPhrase above — same unconsumed return shape.
	IsCovered bool // another list in the union also has this spoken form
}

// --- Dependency scanner ---

// CaptureTokenRe matches capture tokens like <keys>, <keys|alphabet>, <modifiers>*
var CaptureTokenRe = regexp.MustCompile(`<([a-z_]+(?:\|[a-z_]+)*)>[+*]?`)

// commandForDeps is the minimal command structure needed for dependency scanning.
type commandForDeps struct {
	Phrase   json.RawMessage `json:"phrase"`
	Category string          `json:"category"`
}

// loadCommandsForDeps reads and parses commands.json once.
func LoadCommandsForDeps(pluginDir string) []commandForDeps {
	data, err := os.ReadFile(filepath.Join(pluginDir, "commands.json"))
	if err != nil {
		return nil
	}
	var commands []commandForDeps
	if err := json.Unmarshal(data, &commands); err != nil {
		return nil
	}
	return commands
}

// parsedCommand holds the pre-parsed phrase info for dependency scanning.
type parsedCommand struct {
	PhraseDisplay string   // human-readable: "press [modifier]* [key]"
	Captures      []string // inner capture groups: ["modifiers", "keys|alphabet"]
}

// parseCommandsForDeps pre-parses all commands, returning display strings and captures.
func ParseCommandsForDeps(commands []commandForDeps) []parsedCommand {
	result := make([]parsedCommand, 0, len(commands))
	for _, cmd := range commands {
		display, captures := ParsePhraseForDeps(cmd.Phrase)
		result = append(result, parsedCommand{PhraseDisplay: display, Captures: captures})
	}
	return result
}

// entryDependencies returns, for each list and spoken form, the commands that depend on it.
// Returns: map[listName]map[spokenForm][]EntryDependency
func EntryDependencies(parsed []parsedCommand, allLists map[string]map[string]string) map[string]map[string][]EntryDependency {
	result := make(map[string]map[string][]EntryDependency)

	for _, pc := range parsed {
		if len(pc.Captures) == 0 {
			continue
		}

		for _, capture := range pc.Captures {
			listNames := strings.Split(capture, "|")

			for _, ln := range listNames {
				entries, ok := allLists[ln]
				if !ok {
					continue
				}
				if result[ln] == nil {
					result[ln] = make(map[string][]EntryDependency)
				}
				for spoken, value := range entries {
					// Check if another list in the union also has this spoken form
					isCovered := false
					for _, otherLn := range listNames {
						if otherLn == ln {
							continue
						}
						if otherEntries, ok := allLists[otherLn]; ok {
							if _, has := otherEntries[spoken]; has {
								isCovered = true
								break
							}
						}
					}

					result[ln][spoken] = append(result[ln][spoken], EntryDependency{
						CommandPhrase: pc.PhraseDisplay,
						DisplayPhrase: FormatImpact(pc.PhraseDisplay, capture, spoken, value),
						IsCovered:     isCovered,
					})
				}
			}
		}
	}

	return result
}

// captureDisplayName converts a raw capture inner group to its display bracket form.
// e.g. "keys|alphabet" → "[key]", "modifiers" → "[modifier]"
// CollectionDisplayNames maps collection names to their singular display forms.
var CollectionDisplayNames = map[string]string{
	"keys":      "key",
	"modifiers": "modifier",
}

func CaptureDisplayName(inner string) string {
	name := strings.Split(inner, "|")[0]
	if display, ok := CollectionDisplayNames[name]; ok {
		name = display
	}
	return "[" + name + "]"
}

// formatImpact produces a clean display string for an entry's impact on a command.
// Instead of showing raw capture syntax, it substitutes the spoken form for the
// relevant capture and renders other captures as descriptive placeholders.
func FormatImpact(phraseDisplay string, targetCapture string, spoken string, value string) string {
	// phraseDisplay uses formatted names like [key], targetCapture is raw like "keys|alphabet"
	bracket := CaptureDisplayName(targetCapture)
	display := strings.Replace(phraseDisplay, bracket, spoken, 1)
	return display + " \u2192 " + value
}

// listUsedByCommands returns a map of list name → command display phrases that reference it.
func ListUsedByCommands(parsed []parsedCommand) map[string][]string {
	result := make(map[string][]string)

	for _, pc := range parsed {
		for _, capture := range pc.Captures {
			for _, ln := range strings.Split(capture, "|") {
				found := false
				for _, existing := range result[ln] {
					if existing == pc.PhraseDisplay {
						found = true
						break
					}
				}
				if !found {
					result[ln] = append(result[ln], pc.PhraseDisplay)
				}
			}
		}
	}
	return result
}

// parsePhraseForDeps returns a human-readable phrase string (with captures shown as
// [name] placeholders) and the list of capture group names found.
func ParsePhraseForDeps(raw json.RawMessage) (string, []string) {
	var phrase []json.RawMessage
	if err := json.Unmarshal(raw, &phrase); err != nil {
		// Single string phrase
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil
		}
		return s, nil
	}

	var parts []string
	var captures []string
	for _, elem := range phrase {
		// Try as string first
		var s string
		if err := json.Unmarshal(elem, &s); err == nil {
			part, capture := FormatPhrasePart(s)
			parts = append(parts, part)
			if capture != "" {
				captures = append(captures, capture)
			}
			continue
		}
		// Try as array of alternatives
		var alts []string
		if err := json.Unmarshal(elem, &alts); err == nil && len(alts) > 0 {
			part, capture := FormatPhrasePart(alts[0])
			parts = append(parts, part)
			if capture != "" {
				captures = append(captures, capture)
			}
		}
	}
	return strings.Join(parts, " "), captures
}

// formatPhrasePart converts a raw phrase element to a display string and extracts
// any capture group name. Captures like "<keys|alphabet>+" become "[key]+"
// for display, with the inner group "keys|alphabet" returned separately.
func FormatPhrasePart(s string) (display string, capture string) {
	m := CaptureTokenRe.FindStringSubmatchIndex(s)
	if m == nil {
		return s, ""
	}
	// m[0]:m[1] is full match (<...>+/*), m[2]:m[3] is inner group
	inner := s[m[2]:m[3]]
	suffix := s[m[1]:] // anything after the full match (shouldn't happen, but safe)
	// Quantifier is between > and end of full match
	quantifier := ""
	if m[1] > m[0] {
		afterClose := strings.IndexByte(s[m[0]:m[1]], '>')
		if afterClose >= 0 {
			quantifier = s[m[0]+afterClose+1 : m[1]]
		}
	}

	return CaptureDisplayName(inner) + quantifier + suffix, inner
}
