// Package config loads Monarr's server configuration.
//
// Precedence (highest wins): environment variables > optional JSON config
// file (MONARR_CONFIG=/path/to/config.json) > built-in defaults.
//
// Only the ops layer lives here (bind address, port, data dir, logging).
// Everything the user manages — indexers, download clients, profiles — lives
// in the database and is edited in the UI, per the *arr operational model
// (blueprint §7).
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
)

// Defaults.
const (
	DefaultPort    = 7676
	DefaultDataDir = "data"
)

// Config is the fully resolved server configuration.
type Config struct {
	// Host is the bind address; empty means all interfaces.
	Host string `json:"host"`
	// Port is the HTTP port shared by the UI, /api/v1, and (later) the
	// compat personalities.
	Port int `json:"port"`
	// DataDir holds the SQLite database and runtime state.
	DataDir string `json:"dataDir"`
	// LogLevel is one of debug|info|warn|error.
	LogLevel string `json:"logLevel"`
	// LogFormat is text or json.
	LogFormat string `json:"logFormat"`
}

// fileConfig mirrors Config with pointers so we can tell "absent" from "zero"
// when overlaying the optional JSON file.
type fileConfig struct {
	Host      *string `json:"host"`
	Port      *int    `json:"port"`
	DataDir   *string `json:"dataDir"`
	LogLevel  *string `json:"logLevel"`
	LogFormat *string `json:"logFormat"`
}

// Load resolves configuration from defaults, the optional file named by
// MONARR_CONFIG, and MONARR_* environment variables, then validates it.
func Load() (Config, error) {
	return load(os.Getenv)
}

// load is the testable core; getenv abstracts the environment.
func load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Host:      "",
		Port:      DefaultPort,
		DataDir:   DefaultDataDir,
		LogLevel:  "info",
		LogFormat: "text",
	}

	if path := getenv("MONARR_CONFIG"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: reading %s: %w", path, err)
		}
		var fc fileConfig
		if err := json.Unmarshal(raw, &fc); err != nil {
			return Config{}, fmt.Errorf("config: parsing %s: %w", path, err)
		}
		overlay(&cfg, fc)
	}

	if v := getenv("MONARR_HOST"); v != "" {
		cfg.Host = v
	}
	if v := getenv("MONARR_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: MONARR_PORT %q is not a number", v)
		}
		cfg.Port = p
	}
	if v := getenv("MONARR_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := getenv("MONARR_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := getenv("MONARR_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func overlay(cfg *Config, fc fileConfig) {
	if fc.Host != nil {
		cfg.Host = *fc.Host
	}
	if fc.Port != nil {
		cfg.Port = *fc.Port
	}
	if fc.DataDir != nil {
		cfg.DataDir = *fc.DataDir
	}
	if fc.LogLevel != nil {
		cfg.LogLevel = *fc.LogLevel
	}
	if fc.LogFormat != nil {
		cfg.LogFormat = *fc.LogFormat
	}
}

func (c Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("config: port %d out of range", c.Port)
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: data dir must not be empty")
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("config: log format %q (want text|json)", c.LogFormat)
	}
	return nil
}

// Addr returns the host:port to bind.
func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// SlogLevel maps LogLevel to a slog.Level.
func (c Config) SlogLevel() (slog.Level, error) {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: log level %q (want debug|info|warn|error)", c.LogLevel)
	}
}
