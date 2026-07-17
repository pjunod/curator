package config

import (
	"os"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := load(env(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("port = %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("data dir = %q, want %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "text" {
		t.Errorf("logging defaults = %q/%q, want info/text", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.Addr() != ":7676" {
		t.Errorf("addr = %q, want :7676", cfg.Addr())
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"MONARR_HOST":       "127.0.0.1",
		"MONARR_PORT":       "9000",
		"MONARR_DATA_DIR":   "/tmp/monarr",
		"MONARR_LOG_LEVEL":  "debug",
		"MONARR_LOG_FORMAT": "json",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Addr() != "127.0.0.1:9000" {
		t.Errorf("addr = %q", cfg.Addr())
	}
	if cfg.DataDir != "/tmp/monarr" || cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Errorf("unexpected cfg: %+v", cfg)
	}
}

func TestFileOverlayAndEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"port": 8000, "logLevel": "warn"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// File overlays defaults…
	cfg, err := load(env(map[string]string{"MONARR_CONFIG": path}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != 8000 || cfg.LogLevel != "warn" {
		t.Errorf("file overlay not applied: %+v", cfg)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("file must not clobber unset keys: %+v", cfg)
	}

	// …and env beats file.
	cfg, err = load(env(map[string]string{"MONARR_CONFIG": path, "MONARR_PORT": "9001"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != 9001 {
		t.Errorf("env should beat file, got port %d", cfg.Port)
	}
}

func TestValidation(t *testing.T) {
	cases := []map[string]string{
		{"MONARR_PORT": "0"},
		{"MONARR_PORT": "70000"},
		{"MONARR_PORT": "not-a-number"},
		{"MONARR_LOG_LEVEL": "verbose"},
		{"MONARR_LOG_FORMAT": "xml"},
		{"MONARR_DATA_DIR": ""},
	}
	for _, c := range cases {
		// An explicitly empty MONARR_DATA_DIR is indistinguishable from unset
		// through getenv, so force the empty-dir case via the file path.
		if _, ok := c["MONARR_DATA_DIR"]; ok {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			os.WriteFile(path, []byte(`{"dataDir": ""}`), 0o644)
			c = map[string]string{"MONARR_CONFIG": path}
		}
		if _, err := load(env(c)); err == nil {
			t.Errorf("load(%v) should have failed", c)
		}
	}
}

func TestMissingConfigFileErrors(t *testing.T) {
	if _, err := load(env(map[string]string{"MONARR_CONFIG": "/nonexistent/nope.json"})); err == nil {
		t.Error("missing explicit config file should be an error")
	}
}
