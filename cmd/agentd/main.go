// Command agentd is the agent runtime server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/anupam2105/agentd/internal/config"
	"github.com/anupam2105/agentd/internal/httpserver"
	"github.com/anupam2105/agentd/internal/logging"
	"github.com/anupam2105/agentd/internal/observability"
)

// buildVersion is populated at build time via -ldflags.
var buildVersion = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := logging.New(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics := observability.NewMetrics()
	metrics.BuildInfo.WithLabelValues(buildVersion).Set(1)

	if cfg.APIToken == "" {
		logger.Warn("AGENTD_API_TOKEN not set: /v1 API is unauthenticated")
	}

	srv := httpserver.New(cfg.HTTPAddr, buildVersion, logger, metrics)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting server", "addr", cfg.HTTPAddr, "version", buildVersion)
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}
	logger.Info("shutdown complete")
	return nil
}
