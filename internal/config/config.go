// Package config loads client (agent) and server configuration. Files are plain
// JSON to keep the skeleton dependency-free; env vars override file values so a
// systemd unit or a quick shell test can tweak settings without editing files.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Client is the per-machine agent configuration.
type Client struct {
	DataDir        string   `json:"data_dir"`        // dir holding the local SQLite replica
	ServerURL      string   `json:"server_url"`      // central yas-server, e.g. https://yas.lan
	Token          string   `json:"token"`           // bearer token for the sync API
	Hostname       string   `json:"hostname"`        // overrides os.Hostname() when set
	IgnorePatterns []string `json:"ignore_patterns"` // commands matching these are never recorded
}

// Server is the homelab server configuration.
type Server struct {
	Addr        string `json:"addr"`         // listen address, e.g. :8732
	DatabaseURL string `json:"database_url"` // Postgres DSN
	Token       string `json:"token"`        // bearer token clients must present
}

// DBPath is the absolute path to the local SQLite replica.
func (c Client) DBPath() string { return filepath.Join(c.DataDir, "history.db") }

// DefaultClientPath returns ~/.config/yas/config.json (honoring XDG_CONFIG_HOME).
func DefaultClientPath() string { return filepath.Join(configHome(), "yas", "config.json") }

// LoadClient reads the client config from path (empty = default path), fills in
// defaults, and applies env overrides. A missing file is fine — defaults + env
// are enough to run.
func LoadClient(path string) (Client, error) {
	if path == "" {
		path = DefaultClientPath()
	}
	var c Client
	if err := readJSON(path, &c); err != nil {
		return Client{}, err
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(dataHome(), "yas")
	}
	envOverride(&c.ServerURL, "YAS_SERVER_URL")
	envOverride(&c.Token, "YAS_TOKEN")
	envOverride(&c.DataDir, "YAS_DATA_DIR")
	envOverride(&c.Hostname, "YAS_HOSTNAME")
	if c.Hostname == "" {
		c.Hostname, _ = os.Hostname()
	}
	return c, nil
}

// LoadServer reads the server config, applies env overrides, and validates.
func LoadServer(path string) (Server, error) {
	var s Server
	if err := readJSON(path, &s); err != nil {
		return Server{}, err
	}
	if s.Addr == "" {
		s.Addr = ":8732"
	}
	envOverride(&s.Addr, "YAS_ADDR")
	envOverride(&s.DatabaseURL, "YAS_DATABASE_URL")
	envOverride(&s.Token, "YAS_TOKEN")
	if s.DatabaseURL == "" {
		return Server{}, errors.New("config: database_url is required (set YAS_DATABASE_URL)")
	}
	return s, nil
}

// readJSON decodes path into v. A non-existent path is not an error.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func envOverride(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func configHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

func dataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}
