package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level dread.sh server configuration.
type Config struct {
	Server ServerConfig `yaml:"server"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Addr    string `yaml:"addr"`
	DB      string `yaml:"db"`
	BaseURL string `yaml:"base_url"`
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{
		Server: ServerConfig{
			Addr:    ":8080",
			DB:      "dread.db",
			BaseURL: "http://localhost:8080",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	applyEnv(cfg)
	return cfg, nil
}

// LoadFromEnv creates a config purely from environment variables.
func LoadFromEnv() *Config {
	cfg := &Config{
		Server: ServerConfig{
			Addr:    ":8080",
			DB:      "dread.db",
			BaseURL: "http://localhost:8080",
		},
	}
	applyEnv(cfg)
	return cfg
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("DREAD_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("DREAD_DB"); v != "" {
		cfg.Server.DB = v
	}
	if v := os.Getenv("DREAD_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	}
}
