package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/branchkit-plugin-voice/speech"
)

// PhoneticEntry is an alias for speech.PhoneticEntry.
type PhoneticEntry = speech.PhoneticEntry

// CommandSnapshot received from platform's build-command-registry hook.
type CommandSnapshot struct {
	Canonical string  `json:"canonical"`
	Word      string  `json:"word"`
	Category  *string `json:"category"`
	Keycode   *uint16 `json:"keycode"`
}

// buildPhonetics builds phonetic entries from the alphabet collection.
// The keys list is the single source of truth for codeword→letter mappings,
// populated by the voice plugin at startup from alphabet.json.
func (h *Host) buildPhonetics() []PhoneticEntry {
	lr, err := h.plugin.CollectionGet(branchkit.CollectionGetRequest{Name: "alphabet"})
	if err != nil {
		branchkit.Logf("voice", "Failed to fetch alphabet collection for phonetics: %v", err)
		return nil
	}

	// `entries` is map[string]json.RawMessage on the wire — the flat
	// key→value projection carries whatever the value field holds, so the
	// local mirror's map[string]string was an assumption that happened to
	// hold for the alphabet. Decode per entry and skip anything that is not
	// a bare string rather than silently treating it as one.
	var result []PhoneticEntry
	for codeword, raw := range lr.Entries {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			continue
		}
		if len(value) == 1 && value[0] >= 'a' && value[0] <= 'z' {
			letter := value[0]
			result = append(result, PhoneticEntry{
				Word:   codeword,
				Letter: &letter,
			})
		}
	}
	return result
}

// --- Phonetic override handlers ---

// currentCodewordForLetter returns the codeword presently mapped to a letter,
// reading the override-applied alphabet collection (authoritative). The empty
// string + false when the letter has no codeword.
func (h *Host) currentCodewordForLetter(letter string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(letter))
	for _, p := range h.buildPhonetics() {
		if p.Letter != nil && string(*p.Letter) == want {
			return p.Word, true
		}
	}
	return "", false
}

// renameVocabEntry changes the surface form (collection key) of a
// named_entities entry from oldKey to newKey via the platform's `rename`
// override action — the generic "retune" primitive. The entry is keyed by its
// own surface form, so it can't be patched in place; rename suppresses the old
// key and re-adds the record (value + aliases preserved) under the new one. Then
// re-arms the grammar: the alphabet refreshes the phonetics cache (which
// commits), other lists commit the vocabulary directly. Generic over the
// collection so it serves both the Phonetics-tab rename and any list retune.
func (h *Host) renameVocabEntry(collection, oldKey, newKey string) error {
	// The user's retune gesture, transported by this plugin — it lands in
	// the user band ("rename" is a user-band act; a plugin's own overlay
	// carries patches only).
	userBand := "_user"
	if _, err := h.plugin.OverridesApply(branchkit.OverridesApplyRequest{
		Action:     "rename",
		Collection: collection,
		ID:         &oldKey,
		NewID:      &newKey,
		Tenant:     &userBand,
	}); err != nil {
		branchkit.Logf("voice", "settings: rename %s %q→%q failed: %v", collection, oldKey, newKey, err)
		return fmt.Errorf("failed to save override")
	}
	if collection == "alphabet" {
		h.rebuildAndPostPhonetics()
	} else {
		if err := h.plugin.VocabularyCommit(); err != nil {
			branchkit.Logf("voice", "settings: vocab commit after rename %s failed: %v", collection, err)
		}
	}
	return nil
}

// patchCodeword sets the codeword for one letter — the Phonetics-tab path.
// `alphabet` is keyed by LETTER (id_strategy by_field "letter"), so the edit
// is a patch of the record's editable value, not a rename of its key: the
// user's choice is stored as "g's codeword is gap" and survives this plugin
// retuning its default.
func (h *Host) patchCodeword(letter, newWord string) error {
	// The user's retune gesture, transported by this plugin — user band.
	userBand := "_user"
	fields, _ := json.Marshal(map[string]string{"codeword": newWord})
	if _, err := h.plugin.OverridesApply(branchkit.OverridesApplyRequest{
		Action:     "patch",
		Collection: "alphabet",
		Fields:     fields,
		ID:         &letter,
		Tenant:     &userBand,
	}); err != nil {
		branchkit.Logf("voice", "settings: patch alphabet %q→%q failed: %v", letter, newWord, err)
		return fmt.Errorf("failed to save override")
	}
	h.rebuildAndPostPhonetics()
	return nil
}

// applyPhoneticOverride persists a codeword change for one letter and re-pushes
// phonetics. Shared by the Phonetics-tab/results-screen rename endpoint and the
// in-walk calibration retune so both go through one code path. The letter is
// resolved to its current codeword (the override key) before renaming.
func (h *Host) applyPhoneticOverride(letter, word string) error {
	letterStr := strings.ToLower(strings.TrimSpace(letter))
	newWord := strings.ToLower(strings.TrimSpace(word))
	if newWord == "" {
		return fmt.Errorf("word required")
	}
	if letterStr == "" {
		return fmt.Errorf("letter required")
	}

	current, ok := h.currentCodewordForLetter(letterStr)
	if !ok {
		return fmt.Errorf("no codeword for letter %q", letterStr)
	}
	if current == newWord {
		return nil // no-op
	}
	return h.patchCodeword(letterStr, newWord)
}

// PhoneticOverrideRequest is the typed payload for the Phonetics-tab rename.
type PhoneticOverrideRequest struct {
	Letter string `json:"letter"`
	Word   string `json:"word"`
}

func (h *Host) handlePhoneticOverride(req *PhoneticOverrideRequest) error {
	if err := h.applyPhoneticOverride(req.Letter, req.Word); err != nil {
		return err
	}
	return nil
}

// alphabetDefaults returns the plugin's seeded codeword for each letter
// (letter → codeword), read from entities/alphabet.json. The source of truth for
// "is this codeword a user override?" and for reverting a rename.
func alphabetDefaults() map[string]string {
	out := make(map[string]string, 26)
	data, err := os.ReadFile(filepath.Join(branchkit.PluginDir(), "entities", "alphabet.json"))
	if err != nil {
		branchkit.Logf("voice", "settings: failed to read alphabet defaults: %v", err)
		return out
	}
	var defaults map[string]string // word → letter
	if err := json.Unmarshal(data, &defaults); err != nil {
		return out
	}
	for word, l := range defaults {
		out[strings.ToLower(l)] = word
	}
	return out
}

// revertVocabEntry resets a named_entities entry to its plugin default via the
// platform's `revert` override action — clearing every override on the entry's
// meaning so the merged collection falls back to the base the plugin pushed. The
// caller need not know the default; the platform returns the default key it
// restored. Re-arms the grammar (the alphabet refreshes the phonetics cache).
// Generic over the collection, so it serves both the Phonetics-tab reset and the
// in-walk revert.
func (h *Host) revertVocabEntry(collection, currentKey string) (string, error) {
	// The user's reset gesture — user band, same as rename above.
	userBand := "_user"
	resp, err := h.plugin.OverridesApply(branchkit.OverridesApplyRequest{
		Action:     "revert",
		Collection: collection,
		ID:         &currentKey,
		Tenant:     &userBand,
	})
	if err != nil {
		branchkit.Logf("voice", "settings: revert %s %q failed: %v", collection, currentKey, err)
		return "", fmt.Errorf("failed to reset override")
	}
	if collection == "alphabet" {
		h.rebuildAndPostPhonetics()
	} else {
		if err := h.plugin.VocabularyCommit(); err != nil {
			branchkit.Logf("voice", "settings: vocab commit after revert %s failed: %v", collection, err)
		}
	}
	if resp.Key == nil {
		return "", nil
	}
	return *resp.Key, nil
}

// PhoneticResetRequest is the typed payload for the Phonetics-tab reset.
type PhoneticResetRequest struct {
	Letter string `json:"letter"`
}

func (h *Host) handlePhoneticReset(req *PhoneticResetRequest) error {
	if req.Letter == "" {
		return fmt.Errorf("letter required")
	}
	letter := strings.ToLower(strings.TrimSpace(req.Letter))
	if _, ok := h.currentCodewordForLetter(letter); !ok {
		return fmt.Errorf("no codeword for letter %q", req.Letter)
	}
	// Keyed by letter now: revert addresses the record by its id.
	if _, err := h.revertVocabEntry("alphabet", letter); err != nil {
		return err
	}
	return nil
}

// CalibrationRetuneRequest is the typed payload for the in-walk retune: edit a
// failing vocabulary entry and retest it on the spot. `word` is the replacement
// surface form; the entry to rename is the current target's binding.
type CalibrationRetuneRequest struct {
	Word string `json:"word"`
}

// handleCalibrationRetune renames the current target's vocabulary entry and
// retests it under the same grammar.
func (h *Host) handleCalibrationRetune(req *CalibrationRetuneRequest) error {
	if err := h.calib.RetuneTarget(req.Word); err != nil {
		return err
	}
	h.calib.FlushRefresh()
	return nil
}

// handleCalibrationRevert resets the current target's vocabulary entry to its
// plugin default and re-points the walk prompt at it.
func (h *Host) handleCalibrationRevert() (any, error) {
	if err := h.calib.RevertTarget(); err != nil {
		return nil, err
	}
	h.calib.FlushRefresh()
	return map[string]string{"status": "ok"}, nil
}

// rebuildAndPostPhonetics refreshes the voice plugin's phonetics cache from the
// (override-applied) alphabet collection and re-arms the recognition grammar.
//
// It deliberately does NOT re-push the whole alphabet: a user codeword edit
// lives in the platform's override layer (overrides.apply rename/restore), and
// re-pushing the override-applied view via collection.replace would bake the
// edit into the base collection — defeating the override layer and leaving
// residue that reset can't clear. The base is established once at boot
// (initAlphabet); overrides compose on top at read time.
func (h *Host) rebuildAndPostPhonetics() {
	newPhonetics := h.buildPhonetics()
	h.sess.SetPhonetics(newPhonetics)
	// Make the changed codeword hearable (previously a side effect of the
	// alphabet re-push). nil plugin in tests.
	if err := h.plugin.VocabularyCommit(); err != nil {
		branchkit.Logf("voice", "phonetics: vocabulary commit failed: %v", err)
	}
}
