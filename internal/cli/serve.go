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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
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
// closes the store and stops the replay janitor.
func buildServer(ctx context.Context, cfg *config.Config, logger *slog.Logger, runner blueprint.Runner) (*webhook.Server, func(), error) {
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
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub, SurfaceRunState: true})

	ctl := blueprint.New(blueprint.Options{
		Store:         st,
		Pack:          pk,
		Runner:        runner,
		Logger:        logger,
		MaxRunCostUSD: cfg.MaxRunCostUSD,
		MaxTurns:      cfg.MaxTurns,
		Models:        cfg.Models,
		Sink: func(e blueprint.Event) {
			if entry, ok := logEntryFor(e); ok {
				hub.Publish(entry)
			}
		},
		HarnessLine: func(id string, line []byte) {
			bridge.BridgeLine(logbridge.Key{SessionID: id}, line, false)
		},
	})

	// Never set yet: the drain gate matters once a run can outlive a shutdown
	// window, which a later slice handles by waiting on every attached blueprint.
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

	cleanup := func() {
		stopJanitor()

		_ = st.Close()
	}

	return srv, cleanup, nil
}

func runServe(ctx context.Context, cfg *config.Config) error {
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	srv, cleanup, err := buildServer(ctx, cfg, logger, stage.NewRunner())
	if err != nil {
		return err
	}

	defer cleanup()

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
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
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}
