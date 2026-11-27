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

	// DBPath is the SQLite file holding the run/step journal.
	DBPath string

	// APIToken protects /v1/* when set. Health and metrics stay open so
	// probes and scrapers work without credentials.
	APIToken string

	// ConfigPath is the YAML file with pricing, tools and policy (optional).
	ConfigPath string

	AnthropicAPIKey  string
	AnthropicBaseURL string
	OpenAIAPIKey     string
	OpenAIBaseURL    string
	LLMTimeout       time.Duration

	// StepLease is how long an executing step is owned before it may be reclaimed.
	StepLease time.Duration

	// DockerHost is the Docker Engine endpoint for the tool sandbox.
	DockerHost string

	SlackWebhookURL string
	// PublicURL is agentd's externally reachable base URL (used in notifications).
	PublicURL string

	// TracingEnabled is true when an OTLP endpoint is configured.
	TracingEnabled bool
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
		ConfigPath:      getEnv("AGENTD_CONFIG", ""),

		AnthropicAPIKey:  getEnv("AGENTD_ANTHROPIC_API_KEY", os.Getenv("ANTHROPIC_API_KEY")),
		AnthropicBaseURL: getEnv("AGENTD_ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		OpenAIAPIKey:     getEnv("AGENTD_OPENAI_API_KEY", os.Getenv("OPENAI_API_KEY")),
		OpenAIBaseURL:    getEnv("AGENTD_OPENAI_BASE_URL", "https://api.openai.com/v1"),
		LLMTimeout:       parseDurationOr("AGENTD_LLM_TIMEOUT", 2*time.Minute),
		StepLease:        parseDurationOr("AGENTD_STEP_LEASE", 10*time.Minute),
		DockerHost:       getEnv("AGENTD_DOCKER_HOST", getEnv("DOCKER_HOST", "unix:///var/run/docker.sock")),
		SlackWebhookURL:  getEnv("AGENTD_SLACK_WEBHOOK_URL", ""),
		PublicURL:        strings.TrimRight(getEnv("AGENTD_PUBLIC_URL", ""), "/"),
		TracingEnabled:   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "",
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
