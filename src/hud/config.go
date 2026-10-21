package hud

import ()

// HudConfig rides the settings preset: plugin.json declares every field
// with its shipped default, the platform materializes the composed view,
// this package only READS it through a typed mirror. The applyDefaults
// zero-value hack is gone with the storage model that required it: an
// untouched field always reads the CURRENT shipped default, because only user
// changes are stored.
type HudConfig struct {
	FontSize         string          `json:"font_size"`
	ShowBadges       bool            `json:"show_badges"`
	CompactMode      bool            `json:"compact_mode"`
	Opacity          float64         `json:"opacity"`
	BlurAmount       int             `json:"blur_amount"`
	CornerRadius     int             `json:"corner_radius"`
	DisabledChannels map[string]bool `json:"disabled_channels,omitempty"`
}

const (
	ChannelMain      = "main"
	ChannelDiscovery = "discovery"
)

const configCollection = "plugin.voice.hud_config"

// IsChannelEnabled returns whether a HUD channel is enabled.
func (c HudConfig) IsChannelEnabled(channel string) bool {
	if c.DisabledChannels == nil {
		return true
	}
	return !c.DisabledChannels[channel]
}

// DefaultHudConfig mirrors the manifest defaults — the pre-Ready fallback
// only; plugin.json is the authoritative copy.
func DefaultHudConfig() HudConfig {
	return HudConfig{
		FontSize:     "default",
		ShowBadges:   true,
		CompactMode:  false,
		Opacity:      98,
		BlurAmount:   40,
		CornerRadius: 20,
	}
}

// LoadCurrentHudConfig returns the current composed HUD config (defaults
// until the mirror's first fetch).
func (h *HUD) Config() HudConfig {
	if h.config.Ready() {
		return h.config.Get()
	}
	return DefaultHudConfig()
}

// SetUserField relays ONE user gesture into the user band (plugins never
// write settings; writers: platform_only). SetUser refreshes the mirror in
// the same operation, so the returned config — and any re-render — already
// reflects the write.
func (h *HUD) SetUserField(key string, value any) (HudConfig, error) {
	err := h.config.SetUser(key, value)
	return h.Config(), err
}
