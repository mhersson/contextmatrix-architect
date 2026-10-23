// Package webhook is the CM-facing side: the chat-backend protocol on
// backendkit's webhookcore, with the controller behind it.
package webhook

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/metrics"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

// Controller is what the handlers drive. internal/blueprint implements it.
type Controller interface {
	Start(ctx context.Context, in blueprint.StartInput) error
	Message(ctx context.Context, id, text, msgID string) error
	Reply(ctx context.Context, id string, rr protocol.RoundReply, msgID string) error
	Mockup(ctx context.Context, id string, mr protocol.MockupReply, content, msgID string) error
	Abort(ctx context.Context, id, msgID string) error
	Export(ctx context.Context, id, msgID string) error
	SetPolicy(ctx context.Context, id, stageID, policy, msgID string) error
	SetCap(ctx context.Context, id, value, msgID string) error
	SetMode(ctx context.Context, id, mode, msgID string) error
	Resume(ctx context.Context, id, msgID string) error
	Retrospective(ctx context.Context, id, msgID string) error
	Lessons(ctx context.Context, id, msgID string) error
	Retire(ctx context.Context, id, ref, msgID string) error
	End(ctx context.Context, id string) error

	// View, Artifact and Subscribe serve CM's blueprint pane; the blueprint
	// id is the session id.
	View(ctx context.Context, id string) (protocol.BlueprintView, error)
	Artifact(ctx context.Context, id, stage, slice string, version int) (protocol.BlueprintArtifact, error)
	Subscribe(ctx context.Context, id string) (<-chan protocol.LiveFrame, error)
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
	// Build is the build every /health answer reports.
	Build      protocol.BuildInfo
	Controller Controller
}

// Server mounts the protocol routes on the shared transport core.
type Server struct {
	*webhookcore.Core

	ctl    Controller
	dedup  *webhookcore.DedupCache
	logger *slog.Logger
	// keepalive spaces the live stream's heartbeat comments.
	keepalive time.Duration
	// liveDone closes at drain and ends every live stream.
	liveDone chan struct{}
	liveOnce sync.Once
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
		Build:           cfg.Build,
	})

	return &Server{Core: core, ctl: cfg.Controller, dedup: dedup, logger: logger, keepalive: liveKeepalive, liveDone: make(chan struct{})}
}

// Routes mounts every route. /chat/start and /message sit behind the drain
// gate; /chat/end, /logs, the blueprint reads and their live stream, /health
// and /readyz stay reachable while draining.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /chat/start", s.RecordMetrics(s.Auth(s.DrainGate(s.handleStart))))
	mux.HandleFunc("POST /chat/end", s.RecordMetrics(s.Auth(s.handleEnd)))
	mux.HandleFunc("POST /message", s.RecordMetrics(s.Auth(s.DrainGate(s.handleMessage))))
	mux.HandleFunc("GET /logs", s.RecordMetrics(s.Auth(s.HandleLogs)))
	mux.HandleFunc("GET /blueprints/{id}", s.RecordMetrics(s.Auth(s.handleView)))
	mux.HandleFunc("GET /blueprints/{id}/artifacts/{stage}/{version}", s.RecordMetrics(s.Auth(s.handleArtifact)))
	mux.HandleFunc("GET /blueprints/{id}/live", s.RecordMetrics(s.Auth(s.handleLive)))
	mux.HandleFunc("GET /health", s.RecordMetrics(s.HandleHealth))
	mux.HandleFunc("GET /readyz", s.RecordMetrics(s.HandleReadyz))

	return mux
}

// CloseSSE ends every /logs stream and every blueprint live stream, for the
// drain: an SSE stream never idles, so http.Server.Shutdown would otherwise
// wait out its timeout. The browser reconnects and starts with a snapshot.
func (s *Server) CloseSSE() {
	s.Core.CloseSSE()
	s.liveOnce.Do(func() { close(s.liveDone) })
}
