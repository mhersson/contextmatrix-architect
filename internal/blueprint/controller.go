// Package blueprint is the controller: one blueprint per CM session, a state
// machine over stages, questions that block the run, and the event stream.
package blueprint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// State is a blueprint's lifecycle state.
type State string

// States.
const (
	StateNew     State = "new"
	StateRunning State = "running"
	StateWaiting State = "waiting"
	StateIdle    State = "idle"
	StateFailed  State = "failed"
)

// needStage is the pseudo-stage the user's first message is stored under.
const needStage = "need"

// ErrNotFound is returned for an unknown blueprint or a detached session.
var ErrNotFound = errors.New("blueprint: not found")

// StartInput is what a CM session start carries.
type StartInput struct {
	ID         string
	Projects   []string
	Mode       string
	CostCapUSD float64
	Model      string
	Endpoint   protocol.LLMEndpoint
}

// Runner runs one stage. internal/stage implements it; tests fake it.
type Runner interface {
	Run(ctx context.Context, in stage.Input) (stage.Output, error)
}

// Options wires a Controller.
type Options struct {
	Store         *store.Store
	Pack          *pack.Pack
	Runner        Runner
	Logger        *slog.Logger
	MaxRunCostUSD float64
	MaxTurns      int
	// Models overrides the model per persona; the session model is the default.
	Models map[string]string
	// Sink receives every controller event after it is stored. nil discards.
	Sink func(Event)
	// HarnessLine receives every raw harness JSONL line. nil discards.
	HarnessLine func(blueprintID string, line []byte)
	Now         func() time.Time
}

// session is an attached CM session: the provisioned endpoint and model, and
// the audit writer that masks the session's secrets.
type session struct {
	endpoint protocol.LLMEndpoint
	model    string
	audit    audit
}

// run holds a blueprint's run slot: a run in flight, or a caller deciding
// whether to start one. Only the holder may start a run, so a run, a crash
// recovery and the need check never overlap. Slots outlive sessions, so Wait
// still blocks after End.
type run struct {
	cancel context.CancelFunc
	done   chan struct{}
	// answer is the open question's reply channel; nil when none is open.
	answer chan string
}

// Controller owns every attached blueprint.
type Controller struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*session
	runs     map[string]*run
}

// New builds a Controller.
func New(opts Options) *Controller {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Controller{opts: opts, sessions: map[string]*session{}, runs: map[string]*run{}}
}

// Start attaches a session to the blueprint with in.ID, creating it on first
// sight. A stage left running by a crash is marked interrupted and re-run.
// Start on an attached session only logs the reopen.
func (c *Controller) Start(ctx context.Context, in StartInput) error {
	sess := &session{endpoint: in.Endpoint, model: in.Model, audit: c.newAudit(in.Endpoint.APIKey)}

	bp, err := c.openBlueprint(ctx, in, sess.audit)
	if err != nil {
		return err
	}

	c.mu.Lock()

	if _, attached := c.sessions[in.ID]; attached {
		c.mu.Unlock()

		return nil
	}

	c.sessions[in.ID] = sess
	r, runCtx, claimed := c.claimLocked(in.ID)
	c.mu.Unlock()

	if !claimed {
		// A run of an ended session is still finishing; it records its own end.
		return nil
	}

	stageID, task, err := c.interruptedStage(ctx, bp.ID)
	if err != nil {
		c.release(in.ID, r)
		c.detach(in.ID, sess)

		return err
	}

	if stageID == "" {
		c.release(in.ID, r)

		return nil
	}

	c.launch(runCtx, r, sess, bp, stageID, task)

	return nil
}

// openBlueprint loads the blueprint, creating it on first sight.
func (c *Controller) openBlueprint(ctx context.Context, in StartInput, au audit) (store.Blueprint, error) {
	bp, err := c.opts.Store.GetBlueprint(ctx, in.ID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		now := c.opts.Now().UTC()
		bp = store.Blueprint{
			ID: in.ID, Projects: in.Projects, Mode: in.Mode, CostCapUSD: in.CostCapUSD,
			Model: in.Model, State: string(StateNew), CreatedAt: now, UpdatedAt: now,
		}

		if err := c.opts.Store.CreateBlueprint(ctx, bp); err != nil {
			return store.Blueprint{}, fmt.Errorf("blueprint: start: %w", err)
		}

		au.event(ctx, Event{Kind: EventCreated, BlueprintID: in.ID, Payload: map[string]any{
			"projects": in.Projects, "mode": in.Mode, "cost_cap_usd": in.CostCapUSD, "model": in.Model,
		}})
	case err != nil:
		return store.Blueprint{}, fmt.Errorf("blueprint: start: %w", err)
	default:
		au.event(ctx, Event{Kind: EventReopened, BlueprintID: in.ID, Payload: map[string]any{"state": bp.State}})
	}

	return bp, nil
}

// interruptedStage marks a run a crash left running as interrupted and
// returns its stage and the stored need to re-run it from. An empty stage
// means nothing was interrupted. The caller holds the run slot.
func (c *Controller) interruptedStage(ctx context.Context, id string) (string, string, error) {
	interrupted, err := c.markInterrupted(ctx, id)
	if err != nil || interrupted == nil {
		return "", "", err
	}

	need, err := c.opts.Store.LatestArtifact(ctx, id, needStage)
	if err != nil {
		return "", "", fmt.Errorf("blueprint: start: interrupted %s without a need: %w", interrupted.Stage, err)
	}

	return interrupted.Stage, need.Body, nil
}

// markInterrupted flips a run left in "running" by a crash to interrupted
// and returns it, or nil when none exists.
func (c *Controller) markInterrupted(ctx context.Context, id string) (*store.Run, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list runs: %w", err)
	}

	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Status != store.RunRunning {
			continue
		}

		if err := c.opts.Store.EndRun(ctx, runs[i].ID, store.RunInterrupted, "process restarted", 0, c.opts.Now().UTC()); err != nil {
			return nil, fmt.Errorf("blueprint: mark interrupted: %w", err)
		}

		return &runs[i], nil
	}

	return nil, nil
}

// Message handles one user message: the abort command, the answer to an
// open question, the need on a new blueprint, or a note.
func (c *Controller) Message(ctx context.Context, id, text, msgID string) error {
	trimmed := strings.TrimSpace(text)

	c.mu.Lock()

	sess, ok := c.sessions[id]
	if !ok {
		c.mu.Unlock()

		return ErrNotFound
	}

	r := c.runs[id]

	// Abort comes first: during an open question it ends the run, it is not
	// the answer. Cancelling under c.mu means Ask cannot open a new question
	// on a run that is already aborted.
	if strings.EqualFold(trimmed, "abort") {
		if r != nil {
			r.answer = nil
			r.cancel()
		}

		c.mu.Unlock()
		sess.audit.event(ctx, Event{Kind: EventAborted, BlueprintID: id, Payload: map[string]any{"message_id": msgID}})

		return nil
	}

	if r != nil {
		answer := r.answer
		r.answer = nil
		c.mu.Unlock()

		if answer == nil {
			sess.audit.event(ctx, noteEvent(id, trimmed, msgID))

			return nil
		}

		// Logged before it is delivered, so the answer precedes what the run
		// does with it. The channel is buffered: the send never blocks.
		sess.audit.event(ctx, Event{Kind: EventAnswer, BlueprintID: id, Payload: map[string]any{"text": trimmed, "message_id": msgID}})

		answer <- trimmed

		return nil
	}

	r, runCtx, _ := c.claimLocked(id)
	c.mu.Unlock()

	return c.needOrNote(ctx, runCtx, r, sess, id, trimmed, msgID)
}

// needOrNote stores the message as the need and starts the first stage on a
// new blueprint, or logs it as a note. The caller holds the run slot, so no
// other message can take the need meanwhile.
func (c *Controller) needOrNote(ctx, runCtx context.Context, r *run, sess *session, id, text, msgID string) error {
	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		c.release(id, r)

		return fmt.Errorf("blueprint: message: %w", err)
	}

	if bp.State != string(StateNew) {
		c.release(id, r)
		sess.audit.event(ctx, noteEvent(id, text, msgID))

		return nil
	}

	need := sess.audit.text(text)
	if _, err := sess.audit.artifact(ctx, id, needStage, need, nil); err != nil {
		c.release(id, r)

		return err
	}

	c.launch(runCtx, r, sess, bp, c.opts.Pack.Stages[0].ID, need)

	return nil
}

func noteEvent(id, text, msgID string) Event {
	return Event{Kind: EventNote, BlueprintID: id, Payload: map[string]any{"text": text, "message_id": msgID}}
}

// End detaches the session, cancelling any run in flight. The blueprint
// persists and a later Start reopens it.
func (c *Controller) End(ctx context.Context, id string) error {
	if _, err := c.opts.Store.GetBlueprint(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}

		return fmt.Errorf("blueprint: end: %w", err)
	}

	c.mu.Lock()
	delete(c.sessions, id)

	if r := c.runs[id]; r != nil {
		r.answer = nil
		r.cancel()
	}

	c.mu.Unlock()

	return nil
}

// Wait blocks until no run is in flight for id, including a run whose
// session has ended. For tests and shutdown.
func (c *Controller) Wait(id string) {
	for {
		c.mu.Lock()
		r := c.runs[id]
		c.mu.Unlock()

		if r == nil {
			return
		}

		<-r.done
	}
}

// claimLocked takes id's run slot; it fails while the slot is held. c.mu
// must be held.
func (c *Controller) claimLocked(id string) (*run, context.Context, bool) {
	if _, held := c.runs[id]; held {
		return nil, nil, false
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, done: make(chan struct{})}
	c.runs[id] = r

	return r, ctx, true
}

// release frees id's run slot and wakes Wait.
func (c *Controller) release(id string, r *run) {
	c.mu.Lock()

	if c.runs[id] == r {
		delete(c.runs, id)
	}

	r.answer = nil
	c.mu.Unlock()

	r.cancel()
	close(r.done)
}

// detach removes sess unless another session replaced it.
func (c *Controller) detach(id string, sess *session) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sessions[id] == sess {
		delete(c.sessions, id)
	}
}
