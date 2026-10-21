package speech

// StopPhrases and CancelPhrases are the canonical speak-dictation close
// phrases plus the recognizer's common misrecognitions of them. SINGLE
// SOURCE — consumed by the sherpa-side detector (speakPhraseIndex,
// actions.go), the text-domain backstop (StripSpeakPhrases in corrections.go),
// and pinned to the gated command patterns in commands.json by
// TestSpeakPhraseTablesMatchCommandPatterns. A desync between detector and
// grammar silently broke voice-stop for 25 days once.
// A detected stop cuts the AUDIO at the phrase
// onset so whisper usually never sees these
// words; StripSpeakPhrases removes them in text as a backstop for the paths
// the cut can't cover (mis-heard/split phrase, nil-cutoff escape, Linux
// whisper-stt, which ignores cutoff_ms).
var StopPhrases = [][]string{
	{"speak", "over"},
	{"speech", "over"},
	{"speed", "over"},
	{"branch", "over"},
	{"branch", "stop"},
}

var CancelPhrases = [][]string{
	{"speak", "cancel"},
	{"speech", "cancel"},
	{"branch", "cancel"},
}
