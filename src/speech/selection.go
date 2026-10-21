package speech

import (
	"strings"
	"unicode/utf8"
)

// PhoneticMatchSelection resolves a spoken phrase to a selection index via phonetic matching.
func PhoneticMatchSelection(cleanPhrase string, phonetics []PhoneticEntry, selectionTags []*string) (int, bool) {
	letter := PhoneticToLetter(cleanPhrase, phonetics)
	if letter == nil {
		return 0, false
	}

	for idx, tag := range selectionTags {
		if tag == nil {
			continue
		}
		trimmed := strings.TrimSpace(*tag)
		if len(trimmed) == 0 {
			continue
		}
		firstChar, _ := utf8.DecodeRuneInString(trimmed)
		if firstChar >= 'A' && firstChar <= 'Z' {
			firstChar += 'a' - 'A'
		}
		if firstChar >= 'a' && firstChar <= 'z' && byte(firstChar) == *letter {
			return idx, true
		}
	}
	return 0, false
}
