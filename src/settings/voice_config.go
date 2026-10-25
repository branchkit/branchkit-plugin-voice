package settings

import (
	"encoding/json"

	"github.com/branchkit/plugin-sdk-go"
)

// VoiceConfig holds all voice/dictation settings in one place.
//
// Storage moved to the `settings` preset (plugin.json declares every field
// with its shipped default): the platform
// materializes defaults + the user's sparse changes, this plugin only READS
// the composed view through a typed mirror. The load/save/mutex/cache
// machinery that used to live here is gone; user edits from the voice tab
// relay into the user band via overrides.apply (tenant `_user`).
type VoiceConfig struct {
	Model string `json:"model"`
	// CommandModel is the artifact the command stage loads (the voice
	// commands model); the choices are commandModels in the plugin.
	CommandModel    string `json:"command_model"`
	Language        string `json:"language"`
	CommandLanguage string `json:"command_language"`
	// FixAllCaps rewrites an entirely upper-case transcript into sentence
	// case. Declared since the settings tab existed and INERT until
	// 2026-08-15: the WhisperKit stage did the transformation in Swift,
	// unconditionally, so turning this off changed nothing. The behaviour
	// moved into `speech.FixAllCaps` with the rest of the stage's text policy
	// and this flag finally gates it.
	FixAllCaps bool `json:"fix_all_caps"`
	// SpokenPunctuation turns punctuation said as a word ("comma", "question
	// mark") into the mark (speech.SpokenPunctuation). Off by default: it
	// changes text that has not changed before, so the user opts in.
	SpokenPunctuation bool `json:"spoken_punctuation"`
	// RefineDictation turns on the local-model clean-up step (refine.go):
	// off by default, because it adds latency and a large download.
	RefineDictation bool `json:"refine_dictation"`

	// DictationSounds plays a short sound as dictation starts and stops, for
	// anyone not watching the pill. Off by default: sound is a
	// secondary cue, never the primary one (meetings, muted Macs).
	DictationSounds bool `json:"dictation_sounds"`
	// InputDevice is the microphone BranchKit listens through, as the
	// platform's device id (`native.audio_devices` uid); empty follows the
	// system default. InputDeviceName is its name, for showing it while it
	// is disconnected and for OSes that match devices by name.
	InputDevice     string `json:"input_device"`
	InputDeviceName string `json:"input_device_name"`
	// DictationEnabled and VoiceCommandsEnabled are the two feature switches
	// that replace any notion of disabling the engines at the plugin level.
	// Not running a pipeline is already this plugin's decision, stages spawn on
	// demand, and an unspawned stage holds no hardware grant — so a switch here
	// is finer-grained than plugin enable/disable and names what it actually
	// does (plugin boundaries are for code
	// ownership and confinement; feature toggles are settings).
	DictationEnabled     bool `json:"dictation_enabled"`
	VoiceCommandsEnabled bool `json:"voice_commands_enabled"`
	// CalibrationCapture, when true, captures each calibration trial's audio
	// (dump-segments) so the fragility ladder can re-decode it. ON by default —
	// it's the most useful diagnostic;
	// the toggle is an opt-OUT for users who don't want their calibration audio
	// recorded. Capture rewrites the command_recognition record only while a
	// calibration game is running.
	CalibrationCapture bool `json:"calibration_capture"`
	// CaptureRetention decides how long captured calibration audio is kept on
	// disk (independent of whether it's ever shared): "delete_after_analysis"
	// (privacy-first), "keep_bounded" (default; small pruned set), or "keep_all"
	// (full personal corpus).
	CaptureRetention string `json:"capture_retention"`
	// CalibrationAutoAdvance, when true, advances the calibration walk to the
	// next target automatically as soon as the current one is heard correctly —
	// no "Next" click. Opt-in (off by default); a preference, not a per-walk
	// mode. The walk still respects an active rail filter on manual nav, but the
	// auto-advance from speech moves to the literal next target.
	CalibrationAutoAdvance bool `json:"calibration_auto_advance"`
	// CommandConfidenceGate decides what a command whose least certain word
	// falls below CommandConfidenceFloor does: "off", "shadow" (runs, marked
	// in History and the log: the evidence-gathering default) or "enforce"
	// (dropped). The floor is a per-model number measured with
	// `just margin-gate`.
	CommandConfidenceGate  string  `json:"command_confidence_gate"`
	CommandConfidenceFloor float64 `json:"command_confidence_floor"`
}

const configCollection = "plugin.voice.config"

// Config is the typed voice-config mirror. Build it with New before
// plugin.Run() so the initial fetch lands; until then Get answers the
// manifest defaults.
type Config struct {
	mirror *branchkit.SettingsMirror[VoiceConfig]
}

// New wires the typed settings mirror. Must run before plugin.Run().
//
// It used to push every change to the shell as `shell:voice-config:`, which
// neither host acted on (the macOS shell only logged it), and which the
// platform now refuses from plugins.
func New(p *branchkit.Plugin) *Config {
	return &Config{mirror: branchkit.Settings[VoiceConfig](p, configCollection)}
}

// DefaultVoiceConfig mirrors the manifest's declared defaults. This is the
// pre-Ready fallback only (the mirror's first fetch lands at on_ready) and
// the diff base for sparse relays; plugin.json is the authoritative copy —
// keep the two in sync when a default changes.
func DefaultVoiceConfig() VoiceConfig {
	return VoiceConfig{
		Model:                  "openai_whisper-large-v3-v20240930",
		CommandModel:           "sherpa-offline-nemo",
		Language:               "en",
		CommandLanguage:        "en-US",
		FixAllCaps:             true,
		DictationEnabled:       true,
		VoiceCommandsEnabled:   true,
		CalibrationCapture:     true,
		CaptureRetention:       "keep_bounded",
		CommandConfidenceGate:  "shadow",
		CommandConfidenceFloor: -6,
	}
}

// Get returns the current composed settings (defaults until the mirror's
// first fetch).
func (c *Config) Get() VoiceConfig {
	if c.mirror.Ready() {
		return c.mirror.Get()
	}
	return DefaultVoiceConfig()
}

// OnChange registers fn to run after the first fetch and after every change
// (the mirror's OnChange). Must be called before plugin.Run().
func (c *Config) OnChange(fn func(VoiceConfig)) {
	c.mirror.OnChange(fn)
}

// CaptureRetentionResolved returns the configured capture-retention policy and
// whether that value came from the user's REAL config rather than the pre-Ready
// defaults.
//
// The distinction exists because one of the policies deletes recordings.
// Get conflates "the user chose keep_bounded" with "the mirror has
// not fetched yet", and the declared default is keep_bounded — so a finished
// calibration game landing in the window before the first fetch would prune the
// user's curated corpus while believing it was following their setting. That is
// not hypothetical: it deleted nine confirmed clips on 2026-08-16. Callers that
// can DESTROY data must branch on `resolved`, never on the value alone.
func (c *Config) CaptureRetentionResolved() (policy string, resolved bool) {
	if c.mirror.Ready() {
		return c.mirror.Get().CaptureRetention, true
	}
	return DefaultVoiceConfig().CaptureRetention, false
}

// SetUserField relays ONE user gesture (a toggle on the voice tab) into the
// user band. D3: this plugin never writes settings itself — the platform
// stores the user's choice and emits collection.updated.
func (c *Config) SetUserField(key string, value any) error {
	return c.relayUserPatch(map[string]any{key: value})
}

func (c *Config) relayUserPatch(fields map[string]any) error {
	return c.mirror.SetUserFields(fields)
}

// --- RPC handlers ---

// HandleUpdateConfig relays a partial update verbatim — every present field
// reaches the user band (the old copy hand-listed 4 of 7 fields and silently
// dropped the rest; passing the patch through kills that bug class).
func (c *Config) HandleUpdateConfig(req *map[string]json.RawMessage) error {
	fields := map[string]any{}
	for k, v := range *req {
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			branchkit.Logf("voice", "config patch field %q unreadable: %v", k, err)
			continue
		}
		fields[k] = val
	}
	if len(fields) > 0 {
		if err := c.relayUserPatch(fields); err != nil {
			branchkit.Logf("voice", "config relay failed: %v", err)
		}
	}
	return nil
}
