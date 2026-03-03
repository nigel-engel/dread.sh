package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Channel holds a channel ID and its display name.
type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AlertRule defines a threshold alert that fires when a pattern
// matches more than Threshold events within WindowMinutes.
type AlertRule struct {
	Pattern       string `json:"pattern"`
	Threshold     int    `json:"threshold"`
	WindowMinutes int    `json:"window_minutes"`
}

// UserConfig holds the user's local dread configuration.
type UserConfig struct {
	Token       string    `json:"token"`
	Channels    []Channel `json:"channels"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Follows     []string  `json:"follows,omitempty"`
	Sound       string    `json:"sound,omitempty"`
	Muted       []string  `json:"muted,omitempty"`
	SlackURL    string    `json:"slack_url,omitempty"`
	DiscordURL  string    `json:"discord_url,omitempty"`
	Alerts      []AlertRule `json:"alerts,omitempty"`
	path        string
}

// ChannelIDs returns just the IDs for passing to the server.
func (c *UserConfig) ChannelIDs() []string {
	ids := make([]string, len(c.Channels))
	for i, ch := range c.Channels {
		ids[i] = ch.ID
	}
	return ids
}

// ChannelName returns the display name for a channel ID, or the ID if not found.
func (c *UserConfig) ChannelName(id string) string {
	for _, ch := range c.Channels {
		if ch.ID == id {
			return ch.Name
		}
	}
	return id
}

// GenerateWorkspace creates a new workspace ID like "ws_" + 12 random hex chars.
func GenerateWorkspace() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "ws_" + hex.EncodeToString(b)
}

// GenerateToken creates a new user token like "dk_" + 20 random hex chars.
func GenerateToken() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "dk_" + hex.EncodeToString(b)
}

// GenerateChannel creates a new channel ID like "ch_name_" + 12 random hex chars.
func GenerateChannel(name string) string {
	b := make([]byte, 6)
	rand.Read(b)
	suffix := hex.EncodeToString(b)
	if name == "" {
		return "ch_" + suffix
	}
	slug := strings.ToLower(name)
	slug = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, slug)
	slug = strings.Trim(slug, "-")
	if len(slug) > 20 {
		slug = slug[:20]
	}
	return "ch_" + slug + "_" + suffix
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "dread"), nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the user config from disk. Returns a new default config if none exists.
func Load() (*UserConfig, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := &UserConfig{
			Token: GenerateToken(),
			path:  path,
		}
		if err := cfg.Save(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg UserConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.path = path

	if cfg.Token == "" {
		cfg.Token = GenerateToken()
		if err := cfg.Save(); err != nil {
			return nil, err
		}
	}

	return &cfg, nil
}

// Save writes the config to disk.
func (c *UserConfig) Save() error {
	if c.path == "" {
		path, err := configPath()
		if err != nil {
			return err
		}
		c.path = path
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0600)
}

// AddChannel adds a channel if not already present.
func (c *UserConfig) AddChannel(id, name string) bool {
	for _, existing := range c.Channels {
		if existing.ID == id {
			return false
		}
	}
	c.Channels = append(c.Channels, Channel{ID: id, Name: name})
	return true
}

// RemoveChannel removes a channel from the config.
func (c *UserConfig) RemoveChannel(id string) bool {
	for i, existing := range c.Channels {
		if existing.ID == id {
			c.Channels = append(c.Channels[:i], c.Channels[i+1:]...)
			return true
		}
	}
	return false
}

// HasChannel checks if a channel is in the config.
func (c *UserConfig) HasChannel(id string) bool {
	for _, existing := range c.Channels {
		if existing.ID == id {
			return true
		}
	}
	return false
}

// IsMuted returns true if the given channel ID is muted.
func (c *UserConfig) IsMuted(id string) bool {
	for _, m := range c.Muted {
		if m == id {
			return true
		}
	}
	return false
}

// Mute adds a channel ID to the muted list. Returns false if already muted.
func (c *UserConfig) Mute(id string) bool {
	if c.IsMuted(id) {
		return false
	}
	c.Muted = append(c.Muted, id)
	return true
}

// Unmute removes a channel ID from the muted list. Returns false if not muted.
func (c *UserConfig) Unmute(id string) bool {
	for i, m := range c.Muted {
		if m == id {
			c.Muted = append(c.Muted[:i], c.Muted[i+1:]...)
			return true
		}
	}
	return false
}
