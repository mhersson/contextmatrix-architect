// Package webhook is the CM-facing side: the chat-backend protocol on
// backendkit's webhookcore, with the controller behind it.
package webhook

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/metrics"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

// Controller is what the handlers drive. internal/blueprint implements it.
type Controller interface {
	Start(ctx context.Context, in blueprint.StartInput) error
	Message(ctx context.Context, id, text, msgID string) error
	Abort(ctx context.Context, id, msgID string) error
	Export(ctx context.Context, id, msgID string) error
	SetPolicy(ctx context.Context, id, stageID, policy, msgID string) error
	End(ctx context.Context, id string) error
}

// Config wires a Server. Replay, Dedup and Draining default when nil.
type Config struct {
	APIKey       string
	MetricsToken string
	Skew         time.Duration
	Replay       *webhookcore.ReplayCache
	Dedup        *webhookcore.DedupCache
	Draining     *atomic.Bool
	Hub          *logbridge.Hub
	Logger       *slog.Logger
	Metrics      *metrics.Metrics
	Controller   Controller
}

// Server mounts the protocol routes on the shared transport core.
type Server struct {
	*webhookcore.Core

	ctl    Controller
	dedup  *webhookcore.DedupCache
	logger *slog.Logger
}

// NewServer builds a Server.
func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dedup := cfg.Dedup
	if dedup == nil {
		dedup = webhookcore.NewDedupCache(10*time.Minute, 4096)
	}

	core := webhookcore.NewCore(webhookcore.CoreConfig{
		APIKey:          cfg.APIKey,
		MetricsToken:    cfg.MetricsToken,
		Skew:            cfg.Skew,
		Replay:          cfg.Replay,
		Draining:        cfg.Draining,
		Metrics:         cfg.Metrics,
		Logger:          logger,
		Hub:             cfg.Hub,
		LogsFilterParam: "session_id",
		LogsFilterAttr:  "session_id",
	})

	return &Server{Core: core, ctl: cfg.Controller, dedup: dedup, logger: logger}
}

// Routes mounts every route. /chat/start and /message sit behind the drain
// gate; /chat/end, /logs, /health and /readyz stay reachable while draining.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /chat/start", s.RecordMetrics(s.Auth(s.DrainGate(s.handleStart))))
	mux.HandleFunc("POST /chat/end", s.RecordMetrics(s.Auth(s.handleEnd)))
	mux.HandleFunc("POST /message", s.RecordMetrics(s.Auth(s.DrainGate(s.handleMessage))))
	mux.HandleFunc("GET /logs", s.RecordMetrics(s.Auth(s.HandleLogs)))
	mux.HandleFunc("GET /health", s.RecordMetrics(s.HandleHealth))
	mux.HandleFunc("GET /readyz", s.RecordMetrics(s.HandleReadyz))

	return mux
}
