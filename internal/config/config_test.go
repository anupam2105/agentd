package config_test

import (
	"testing"
	"time"

	"github.com/anupam2105/agentd/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"AGENTD_HTTP_ADDR", "AGENTD_LOG_LEVEL", "AGENTD_LOG_FORMAT", "AGENTD_SHUTDOWN_TIMEOUT", "AGENTD_DB_PATH", "AGENTD_API_TOKEN"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":7700" || cfg.LogLevel != "info" || cfg.LogFormat != "json" ||
		cfg.ShutdownTimeout != 15*time.Second || cfg.DBPath != "agentd.db" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"AGENTD_LOG_LEVEL":  "verbose",
		"AGENTD_LOG_FORMAT": "xml",
	}
	for key, val := range cases {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, val)
			if _, err := config.Load(); err == nil {
				t.Fatalf("Load accepted %s=%q", key, val)
			}
		})
	}
}
