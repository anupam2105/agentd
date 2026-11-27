// Command agentd is the agent runtime server and its operator CLI.
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
	"time"

	"github.com/anupam2105/agentd/internal/agentcfg"
	"github.com/anupam2105/agentd/internal/cli"
	"github.com/anupam2105/agentd/internal/config"
	"github.com/anupam2105/agentd/internal/engine"
	"github.com/anupam2105/agentd/internal/httpserver"
	"github.com/anupam2105/agentd/internal/llm"
	"github.com/anupam2105/agentd/internal/logging"
	"github.com/anupam2105/agentd/internal/notify"
	"github.com/anupam2105/agentd/internal/observability"
	"github.com/anupam2105/agentd/internal/policy"
	"github.com/anupam2105/agentd/internal/sandbox"
	"github.com/anupam2105/agentd/internal/store"
	"github.com/anupam2105/agentd/internal/tracing"
)

// buildVersion is populated at build time via -ldflags.
var buildVersion = "dev"

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch {
	case cmd == "serve":
		return serve()
	case cmd == "version" || cmd == "--version":
		fmt.Println(buildVersion)
		return nil
	case cmd == "help" || cmd == "-h" || cmd == "--help":
		fmt.Print(cli.Usage)
		return nil
	case cli.IsCommand(cmd):
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return cli.FromEnv(os.Stdout).Run(ctx, cmd, args)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, cli.Usage)
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := logging.New(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	file, err := agentcfg.Load(cfg.ConfigPath)
	if err != nil {
		return err
	}

	metrics := observability.NewMetrics()
	metrics.BuildInfo.WithLabelValues(buildVersion).Set(1)

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	var providers []llm.Provider
	if cfg.AnthropicAPIKey != "" {
		providers = append(providers, llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL, cfg.LLMTimeout))
	}
	if cfg.OpenAIAPIKey != "" {
		providers = append(providers, llm.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.LLMTimeout))
	}
	gateway := llm.NewGateway(file.Pricing, providers...)

	var runner sandbox.Runner
	if len(file.Tools) > 0 {
		d, err := sandbox.NewDocker(cfg.DockerHost)
		if err != nil {
			return fmt.Errorf("docker sandbox: %w", err)
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := d.Ping(pctx); err != nil {
			logger.Warn("docker is not reachable; tool steps will fail (retryable) until it is", "docker_host", cfg.DockerHost, "error", err)
		}
		cancel()
		runner = d
	}
	tools, err := sandbox.NewRegistry(file.Tools, runner)
	if err != nil {
		return err
	}
	pol, err := policy.New(file.Policy)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}

	var tracer *tracing.Tracer
	if cfg.TracingEnabled {
		tracer, err = tracing.New(ctx, buildVersion)
		if err != nil {
			return err
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tracer.Shutdown(sctx)
		}()
	}

	var notifier notify.Notifier
	if cfg.SlackWebhookURL != "" {
		notifier = notify.NewSlack(cfg.SlackWebhookURL, cfg.PublicURL)
	}

	eng := engine.New(&engine.Options{
		Store: st, LLM: gateway, Tools: tools, Policy: pol, Metrics: metrics, Tracer: tracer,
		Notifier: notifier, Logger: logger, LLMTimeout: cfg.LLMTimeout, Lease: cfg.StepLease,
	})

	if cfg.APIToken == "" {
		logger.Warn("AGENTD_API_TOKEN not set: /v1 API is unauthenticated")
	}
	toolNames := make([]string, 0, len(tools.List()))
	for _, t := range tools.List() {
		toolNames = append(toolNames, t.Name)
	}

	srv := httpserver.New(httpserver.Options{
		Addr: cfg.HTTPAddr, Version: buildVersion, Logger: logger, Metrics: metrics, Engine: eng, APIToken: cfg.APIToken,
	})

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting server", "addr", cfg.HTTPAddr, "version", buildVersion, "db", cfg.DBPath,
			"providers", gateway.Providers(), "tools", toolNames, "tracing", cfg.TracingEnabled)
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
