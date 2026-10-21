package calibration

import (
	"sort"
	"strings"
)

// CalibrateRequest is the RPC payload for calibration actions. A walk is
// selected by one of: an explicit command-id list (`targets`), a named
// `selection` ("all" / "alphabet" / "drill" / "category:<owner>"), or a literal
// `words` list (an ad-hoc acoustic walk). The old per-game `source` enum is
// gone — context and scoring are derived per target, not picked.
type CalibrateRequest struct {
	Action string `json:"action"`
	// Selection is a named walk scope. Empty defaults to the alphabet codewords.
	Selection string `json:"selection,omitempty"`
	// Targets is an explicit list of command ids ("<owner>:<pattern>") to walk.
	Targets []string `json:"targets,omitempty"`
	// Words is a literal phrase list walked acoustic-only (custom sets, a single
	// drill word). Also the preview word list.
	Words []string `json:"words,omitempty"`
	// Prefix labels a preview group / completions prefix.
	Prefix []string `json:"prefix,omitempty"`
	// Command is the catalog row id for the "select" action (detail pane).
	Command string `json:"command,omitempty"`
	// Variant tunes how a dynamic command expands: "single" walks each codeword
	// alone (26) instead of the full pair cross product (676).
	Variant string `json:"variant,omitempty"`
	// Index is the target index to jump to for the "goto" action (a click in the
	// active command's group rail — the second column listing its expanded targets).
	Index *int `json:"index,omitempty"`
	// Filter is the live catalog search term. When set, advance/skip/back move
	// through only the targets whose prompt contains it, and command-level
	// chaining picks the next matching catalog command.
	Filter string `json:"filter,omitempty"`
}

// CalibrateResponse is the RPC response for calibration actions.
type CalibrateResponse struct {
	Active bool `json:"calibration_active"`
}

// HandleCalibrateRPC handles the "calibrate" RPC method.
func (s *State) HandleCalibrateRPC(req *CalibrateRequest) (any, error) {
	switch req.Action {
	case "start":
		targets, letters, source := s.resolveSelection(req)
		active, redirect := s.StartWalk(targets, letters, source)
		if redirect && s.SettingsRedirect != nil {
			s.SettingsRedirect("calibration")
		}
		return CalibrateResponse{Active: active}, nil

	case "activate":
		// A sidebar row click: start (or jump to) this command's trial directly.
		// Selecting records nothing until the user holds Option+G; it just arms
		// the next hold and, with auto-advance, the walk chains from here.
		active, redirect := s.ActivateCommand(req.Command, req.Filter, req.Variant)
		if redirect && s.SettingsRedirect != nil {
			s.SettingsRedirect("calibration")
		}
		return CalibrateResponse{Active: active}, nil

	case "goto":
		// A click in the active command's group rail (the second column): jump the
		// walk cursor to that target index.
		if req.Index != nil {
			s.GotoTarget(*req.Index)
		}
		return CalibrateResponse{Active: s.IsActive()}, nil

	case "ping":
		// Heartbeat from the open calibration page; keeps the walk alive against
		// the liveness backstop. No re-render.
		s.RecordSeen()
		return CalibrateResponse{Active: s.IsActive()}, nil

	default:
		active, _ := s.HandleCalibrate(req.Action, req.Filter)
		return CalibrateResponse{Active: active}, nil
	}
}

// resolveSelection turns a start request into a concrete walk queue: targets,
// the parallel alphabet-letter list (nil for command walks), and the selection
// provenance label.
func (s *State) resolveSelection(req *CalibrateRequest) (targets []Target, letters []string, source string) {
	var enum []EnumeratedCommand
	if s.EnumerateCommands != nil {
		enum = s.EnumerateCommands()
	}

	switch {
	case len(req.Targets) > 0:
		targets = TargetsFromIDs(enum, req.Targets)
		if len(req.Targets) == 1 {
			source = req.Targets[0]
		} else {
			source = "selection"
		}

	case len(req.Words) > 0:
		words := append([]string(nil), req.Words...)
		sort.Strings(words)
		for _, w := range words {
			targets = append(targets, CodewordTarget(w))
		}
		source = "custom"
		if len(req.Prefix) > 0 {
			source = req.Prefix[0]
		}

	case req.Selection == SelectionAll || req.Selection == SelectionDrill || strings.HasPrefix(req.Selection, categoryPrefix):
		var attempts []VerificationAttempt
		if req.Selection == SelectionDrill && s.LoadAttempts != nil {
			attempts = s.LoadAttempts()
		}
		targets = BuildCommandTargets(enum, req.Selection, attempts)
		source = req.Selection

	default: // alphabet (also the empty-selection default)
		targets, letters = s.alphabetTargets()
		source = SelectionAlphabet
	}

	// Dynamic (all-slot) commands carry no literal words: expand each into
	// concrete codeword prompts under its hint context — either the full pair
	// cross product (676) or, in "single" mode, each codeword alone (26) to test
	// individual codeword acoustics under the heavy grammar.
	targets = s.expandDynamicTargets(targets, req.Variant == "single")
	// Expand list-backed slotted commands over their entries so every list word
	// is walked + retunable (codewords/dynamic handled above).
	targets = s.expandListSlotTargets(targets)
	return
}

// sortedCodewords returns the voice codeword set (alphabet), sorted.
func (s *State) sortedCodewords() []string {
	if s.GetPhonetics == nil {
		return nil
	}
	ph := s.GetPhonetics()
	words := make([]string, 0, len(ph))
	for _, p := range ph {
		words = append(words, p.Word)
	}
	sort.Strings(words)
	return words
}

// alphabetTargets builds the voice codeword set as acoustic-only targets, with
// the parallel letter list, sorted by codeword.
func (s *State) alphabetTargets() (targets []Target, letters []string) {
	if s.GetPhonetics == nil {
		return nil, nil
	}
	ph := s.GetPhonetics()
	sort.Slice(ph, func(i, j int) bool { return ph[i].Word < ph[j].Word })
	for _, p := range ph {
		targets = append(targets, CodewordTarget(p.Word))
		l := ""
		if p.Letter != nil {
			l = string(*p.Letter)
		}
		letters = append(letters, l)
	}
	return targets, letters
}
