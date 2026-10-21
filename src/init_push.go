package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/branchkit/plugin-sdk-go"
)

func (h *Host) postAlphabetStore(phonetics []PhoneticEntry) {
	type entry struct {
		Letter   string `json:"letter"`
		Codeword string `json:"codeword"`
	}
	entries := make([]entry, 0, len(phonetics))
	for _, e := range phonetics {
		key := "?"
		if e.Letter != nil {
			key = string(*e.Letter)
		}
		entries = append(entries, entry{Letter: key, Codeword: e.Word})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Letter < entries[j].Letter })
	// `alphabet` declares id_strategy by_field "letter": the LETTER is each
	// record's identity, the codeword is its editable value. A user's edit
	// ("g is gap") is therefore keyed to the letter and survives this plugin
	// retuning the default codeword, which it has done seven times.
	// The backend stamps the
	// id field from this ID, so it must be the letter.
	records := make([]branchkit.CollectionPutEntry, 0, len(entries))
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			branchkit.Logf("voice", "alphabet: marshal %q: %v", e.Codeword, err)
			return
		}
		records = append(records, branchkit.CollectionPutEntry{ID: e.Letter, Payload: raw})
	}
	if _, err := h.plugin.Replace("alphabet", records, branchkit.ScopeCollection()); err != nil {
		branchkit.Logf("voice", "Failed to write alphabet collection: %v", err)
	}
}

// pushDefaultLists loads JSON files from the entities/ directory and pushes each
// as an array of objects to the platform.
//
// `number` and `direction` declare id_strategy by_field "key": the value
// ("7", "up") is each record's identity and the spoken word is editable, so
// a user's retune of a spoken form survives this plugin changing its
// defaults.
func (h *Host) pushDefaultLists() {
	entitiesDir := filepath.Join(branchkit.PluginDir(), "entities")

	// Push voice-owned lists (alphabet pushed separately by initAlphabet)
	for _, name := range []string{"number", "direction"} {
		data, err := os.ReadFile(filepath.Join(entitiesDir, name+".json"))
		if err != nil {
			branchkit.Logf("voice", "Failed to read entities/%s.json: %v", name, err)
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			branchkit.Logf("voice", "Failed to parse entities/%s.json: %v", name, err)
			continue
		}

		records := make([]branchkit.CollectionPutEntry, 0, len(arr))
		for _, raw := range arr {
			var idCarrier struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(raw, &idCarrier); err != nil || idCarrier.Key == "" {
				branchkit.Logf("voice", "skipping entity in %s.json: missing or invalid `key` field: %v", name, err)
				continue
			}
			records = append(records, branchkit.CollectionPutEntry{ID: idCarrier.Key, Payload: raw})
		}

		if _, err := h.plugin.Replace(name, records, branchkit.ScopeCollection()); err != nil {
			branchkit.Logf("voice", "Failed to push list '%s': %v", name, err)
			continue
		}
		branchkit.Logf("voice", "Pushed %d entries to '%s' collection", len(records), name)
	}
}

// initAlphabet loads the default alphabet, pushes it as array-of-objects,
// builds phonetics from it, and posts the alphabet store.
func (h *Host) initAlphabet() {
	data, err := os.ReadFile(filepath.Join(branchkit.PluginDir(), "entities", "alphabet.json"))
	if err != nil {
		branchkit.Logf("voice", "Failed to read entities/alphabet.json: %v", err)
		return
	}
	var defaults map[string]string // word → letter
	if err := json.Unmarshal(data, &defaults); err != nil {
		branchkit.Logf("voice", "Failed to parse entities/alphabet.json: %v", err)
		return
	}

	// Convert to PhoneticEntry list and push as array-of-objects
	var phonetics []PhoneticEntry
	for word, letterStr := range defaults {
		if len(letterStr) == 1 && letterStr[0] >= 'a' && letterStr[0] <= 'z' {
			letter := letterStr[0]
			phonetics = append(phonetics, PhoneticEntry{Word: word, Letter: &letter})
		}
	}

	sort.Slice(phonetics, func(i, j int) bool {
		if phonetics[i].Letter == nil {
			return true
		}
		if phonetics[j].Letter == nil {
			return false
		}
		return *phonetics[i].Letter < *phonetics[j].Letter
	})

	h.postAlphabetStore(phonetics)

	// Cache phonetics in session state
	h.sess.SetPhonetics(phonetics)

	branchkit.Logf("voice", "Initialized alphabet: %d entries", len(phonetics))
}
