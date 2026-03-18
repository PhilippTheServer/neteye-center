// Package config loads and validates neteye-center configuration from YAML and environment variables.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for neteye-center.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Retention RetentionConfig `yaml:"retention"`
}

// ServerConfig holds listen addresses and timing for the two HTTP servers.
type ServerConfig struct {
	// AgentAddr is the WebSocket listen address for agents.
	AgentAddr string `yaml:"agent_addr"`
	// FrontendAddr is the HTTP/WebSocket listen address for the frontend.
	FrontendAddr string `yaml:"frontend_addr"`
	// OfflineTimeout is how long after the last heartbeat a device is marked offline.
	OfflineTimeout time.Duration `yaml:"offline_timeout"`
}

// DatabaseConfig holds the PostgreSQL connection settings.
type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
	// MaxConns is the pgx connection pool size.
	MaxConns int32 `yaml:"max_conns"`
}

// RetentionConfig controls how long each tier of metrics data is kept.
type RetentionConfig struct {
	// RawInterval is the cadence at which agents send updates.
	RawInterval time.Duration `yaml:"raw_interval"`
	// RawKeep is how long raw metric rows are retained.
	RawKeep time.Duration `yaml:"raw_keep"`
	// MinKeep is how long 1-minute aggregates are retained.
	MinKeep time.Duration `yaml:"min_keep"`
	// HourKeep is how long 1-hour aggregates are retained.
	HourKeep time.Duration `yaml:"hour_keep"`
	// DailyKeep is how long daily aggregates are retained (0 = forever).
	DailyKeep time.Duration `yaml:"daily_keep"`
	// PurgeInterval is how often the retention job runs.
	PurgeInterval time.Duration `yaml:"purge_interval"`
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			AgentAddr:      ":9090",
			FrontendAddr:   ":8080",
			OfflineTimeout: 30 * time.Second,
		},
		Database: DatabaseConfig{
			DSN:      "postgres://neteye:neteye@localhost:5432/neteye?sslmode=disable",
			MaxConns: 20,
		},
		Retention: RetentionConfig{
			RawInterval:   5 * time.Second,
			RawKeep:       1 * time.Hour,
			MinKeep:       24 * time.Hour,
			HourKeep:      30 * 24 * time.Hour,
			DailyKeep:     365 * 24 * time.Hour,
			PurgeInterval: 5 * time.Minute,
		},
	}
}

// Load reads a YAML config file and overlays environment variables.
// If path is empty the defaults are used.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open config %s: %w", path, err)
		}
		defer f.Close() //nolint:errcheck // read-only, close error not actionable
		if err := yaml.NewDecoder(f).Decode(cfg); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}

	// Environment variable overrides
	if v := os.Getenv("NETEYE_DB_DSN"); v != "" {
		cfg.Database.DSN = v
	}
	if v := os.Getenv("NETEYE_AGENT_ADDR"); v != "" {
		cfg.Server.AgentAddr = v
	}
	if v := os.Getenv("NETEYE_FRONTEND_ADDR"); v != "" {
		cfg.Server.FrontendAddr = v
	}

	return cfg, nil
}
