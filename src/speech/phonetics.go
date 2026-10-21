package speech

import (
	"strings"
)

// PhoneticEntry represents a single phonetic codeword.
type PhoneticEntry struct {
	Word   string `json:"word"`
	Letter *byte  `json:"letter,omitempty"`
}

// PhoneticToLetter resolves a spoken phrase to a single letter.
func PhoneticToLetter(cleanPhrase string, phonetics []PhoneticEntry) *byte {
	for _, entry := range phonetics {
		if cleanPhrase == strings.ToLower(entry.Word) {
			return entry.Letter
		}
	}
	return nil
}
