package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/config"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
	"github.com/mhersson/contextmatrix-architect/internal/webhook"
)

const shutdownTimeout = 10 * time.Second

func newServeCmd() *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the architect backend",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("config: %w", err)
			}

			return runServe(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVar(&path, "config", "config.yaml", "config file path")

	return cmd
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level

	_ = lvl.UnmarshalText([]byte(level))

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// buildServer wires store, pack, controller and the webhook server. The
// runner is injected so tests can replace the model. The returned cleanup
// stops the replay janitor and the controller's runs, then closes the store.
func buildServer(ctx context.Context, cfg *config.Config, logger *slog.Logger, runner blueprint.Runner) (*webhook.Server, func(context.Context) error, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("state dir: %w", err)
	}

	st, err := store.Open(ctx, filepath.Join(cfg.StateDir, "architect.db"))
	if err != nil {
		return nil, nil, err
	}

	var pk *pack.Pack

	if cfg.Pack == "" {
		pk, err = pack.LoadEmbedded()
	} else {
		pk, err = pack.LoadDir(cfg.Pack)
	}

	if err != nil {
		_ = st.Close()

		return nil, nil, err
	}

	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)

	// One instance: its per-repository and per-blueprint locks only hold
	// within it, so the code stages and the sweep share it.
	workspace := code.NewWorkspace(filepath.Join(cfg.StateDir, "workspaces"), time.Now)

	ctl := blueprint.New(blueprint.Options{
		Store:          st,
		Pack:           pk,
		Runner:         runner,
		Logger:         logger,
		MaxRunCostUSD:  cfg.MaxRunCostUSD,
		MaxTurns:       cfg.MaxTurns,
		Models:         cfg.Models,
		Sink:           eventSink(hub),
		HarnessLine:    harnessForwarder(hub),
		Workspace:      workspace,
		ExportDir:      filepath.Join(cfg.StateDir, "exports"),
		Projects:       cmProjects(cfg.ContextMatrixURL),
		CMTools:        cmReadTools(cfg.ContextMatrixURL),
		ConnectCM:      cmConnect(cfg.ContextMatrixURL),
		CallerIdentity: cfg.CallerIdentity,
		WatchInterval:  cfg.WatchInterval,
		Credentials: func(gitToken string) code.Credentials {
			return code.NewCMCredentials(cfg.ContextMatrixURL, gitToken)
		},
	})

	// Removes trees not written for workspace_max_age_days, at startup and hourly.

	sweepCtx, stopSweep := context.WithCancel(context.Background())

	var sweepDone sync.WaitGroup

	sweepDone.Go(func() {
		sweepWorkspaces(sweepCtx, workspace, time.Duration(cfg.WorkspaceMaxAgeDays)*24*time.Hour, sweepInterval, logger)
	})

	// Never set: the controller's Shutdown refuses new starts and runs itself,
	// so the drain gate is wired but stays open.
	var draining atomic.Bool

	replay := webhookcore.NewReplayCache(cfg.ReplaySkew, cfg.ReplayCacheSize)
	stopJanitor := replay.StartJanitor()

	srv := webhook.NewServer(webhook.Config{
		APIKey:     cfg.APIKey,
		Skew:       cfg.ReplaySkew,
		Replay:     replay,
		Dedup:      webhookcore.NewDedupCache(cfg.MessageDedupTTL, cfg.MessageDedupCacheSize),
		Draining:   &draining,
		Hub:        hub,
		Logger:     logger,
		Controller: ctl,
	})

	// Stops the runs before closing the store, so no run writes to a closed DB.
	cleanup := func(ctx context.Context) error {
		stopJanitor()
		stopSweep()
		sweepDone.Wait()

		err := ctl.Shutdown(ctx)
		if closeErr := st.Close(); err == nil {
			err = closeErr
		}

		return err
	}

	return srv, cleanup, nil
}

// cmProjects lists CM's projects over one MCP session per call, keyed with
// the session's MCP key.
func cmProjects(baseURL string) func(ctx context.Context, mcpAPIKey string) ([]cm.Project, error) {
	return func(ctx context.Context, mcpAPIKey string) ([]cm.Project, error) {
		client, err := cm.Connect(ctx, baseURL, mcpAPIKey)
		if err != nil {
			return nil, err
		}

		defer func() { _ = client.Close() }()

		return client.ListProjects(ctx)
	}
}

// cmConnect opens one MCP session to ContextMatrix with the session's key.
func cmConnect(baseURL string) func(ctx context.Context, mcpAPIKey string) (blueprint.CMClient, error) {
	return func(ctx context.Context, mcpAPIKey string) (blueprint.CMClient, error) {
		client, err := cm.Connect(ctx, baseURL, mcpAPIKey)
		if err != nil {
			// A nil *cm.Client in the interface would not compare equal to nil.
			return nil, err
		}

		return client, nil
	}
}

// cmReadTools opens one MCP session per stage run with the session's MCP key
// and offers the run CM's read tools.
func cmReadTools(baseURL string) func(ctx context.Context, mcpAPIKey, tag string) ([]tools.Tool, func(), error) {
	return func(ctx context.Context, mcpAPIKey, tag string) ([]tools.Tool, func(), error) {
		client, err := cm.Connect(ctx, baseURL, mcpAPIKey)
		if err != nil {
			return nil, nil, err
		}

		ts, err := client.ReadTools(ctx, cm.StageReads(), tag)
		if err != nil {
			_ = client.Close()

			return nil, nil, err
		}

		return ts, func() { _ = client.Close() }, nil
	}
}

func runServe(ctx context.Context, cfg *config.Config) error {
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	srv, cleanup, err := buildServer(ctx, cfg, logger, stage.NewRunner())
	if err != nil {
		return err
	}

	stopped := false

	defer func() {
		if !stopped {
			_ = stopRuns(cleanup)
		}
	}()

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	httpServer.RegisterOnShutdown(srv.CloseSSE)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)

	go func() {
		logger.Info("architect listening", "addr", httpServer.Addr, "pack", cfg.Pack)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	logger.Info("architect shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Lingering connections must not hold the process.
		_ = httpServer.Close()

		return fmt.Errorf("shutdown: %w", err)
	}

	stopped = true

	return stopRuns(cleanup)
}

// stopRuns runs cleanup bounded by shutdownTimeout.
func stopRuns(cleanup func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := cleanup(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}
