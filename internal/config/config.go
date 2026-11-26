// Package config loads and validates process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds all process configuration derived from the environment.
type Config struct {
	HTTPAddr        string
	LogLevel        string
	LogFormat       string
	ShutdownTimeout time.Duration

	// DBPath is the SQLite file holding the run/step journal (used from M1).
	DBPath string

	// APIToken protects /v1/* when set. Health and metrics stay open so
	// probes and scrapers work without credentials.
	APIToken string
}

// Load reads configuration from environment variables and validates it.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getEnv("AGENTD_HTTP_ADDR", ":7700"),
		LogLevel:        strings.ToLower(getEnv("AGENTD_LOG_LEVEL", "info")),
		LogFormat:       strings.ToLower(getEnv("AGENTD_LOG_FORMAT", "json")),
		ShutdownTimeout: parseDurationOr("AGENTD_SHUTDOWN_TIMEOUT", 15*time.Second),
		DBPath:          getEnv("AGENTD_DB_PATH", "agentd.db"),
		APIToken:        getEnv("AGENTD_API_TOKEN", ""),
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("invalid AGENTD_LOG_LEVEL: %q (want debug|info|warn|error)", cfg.LogLevel)
	}

	switch cfg.LogFormat {
	case "json", "text":
	default:
		return Config{}, fmt.Errorf("invalid AGENTD_LOG_FORMAT: %q (want json|text)", cfg.LogFormat)
	}

	if cfg.DBPath == "" {
		return Config{}, errors.New("AGENTD_DB_PATH must not be empty")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseDurationOr(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
