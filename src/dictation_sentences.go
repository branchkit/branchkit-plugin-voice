package main

// The sentences "Read a sentence" asks for on the Models tab: a fixed set, in
// line with the calibration tool, never drawn from what the user dictates day
// to day. Every dictation model is scored
// on the same sentences, so a comparison is fair and can be run again after a
// new model arrives.
//
// They are written to cover what breaks dictation. Numbers are written the
// way Whisper models type them (digits, "3:30"), since the score compares
// words and a model that writes "three thirty" for "3:30" is not wrong in a
// way the user would mind; every model meets the same sentences either way.

// dictationSentence is one sentence and what it tests.
type dictationSentence struct {
	Text  string
	Tests string
}

var dictationSentences = []dictationSentence{
	{"Please send the quarterly report to Priya and Tomasz by Friday.", "names"},
	{"Their team said they're going to put the boxes over there.", "homophones"},
	{"The meeting moved from 3:30 to 4:15 on March 12.", "times and dates"},
	{"Call Dr. Nakamura about the results before noon.", "a title and a name"},
	{"Its battery lasts longer than it's ever lasted before.", "its and it's"},
	{"Remind me to buy eggs, bread, and a dozen lemons.", "a list"},
	{"The weather in Reykjavik was colder than we expected.", "a place name"},
	{"We shipped version 2 of the app on the 21st.", "numbers in a sentence"},
	{"Could you read the paragraph aloud before we decide?", "a question"},
	{"Whether or not it rains, we'll meet by the old oak tree.", "whether and weather"},
	{"I'd like two coffees, one with oat milk and one black.", "two, to and too"},
	{"After the long weekend, everyone came back rested, cheerful, and ready to work on the new design.", "a long sentence"},
}
