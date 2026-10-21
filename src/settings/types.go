package settings

import "encoding/json"

// CommandDetail holds the parsed structure of a command from commands.json.
type CommandDetail struct {
	Phrase       json.RawMessage `json:"phrase"`
	Action       json.RawMessage `json:"action"`
	Category     string          `json:"category"`
	Description  string          `json:"description"`
	RequiresTags []string        `json:"requires_tags"`
	SetsTags     []string        `json:"sets_tags"`
	ClearsTags   []string        `json:"clears_tags"`
}
