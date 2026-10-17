// Package blueprint is the controller: one blueprint per CM session, a state
// machine over stages, questions that block the run, and the event stream.
package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/export"
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
	// StateEmitting runs from an approve until the cards exist and, in yolo
	// mode, the run start was attempted. A restart resumes an emission it
	// finds emitting; one a stop left idle or emitted waits for approve.
	StateEmitting State = "emitting"
	StateEmitted  State = "emitted"
	StateDone     State = "done"
)

// needStage is the pseudo-stage the user's first message is stored under.
const needStage = "need"

// ErrNotFound is returned for an unknown blueprint or a detached session.
var ErrNotFound = errors.New("blueprint: not found")

// ErrShuttingDown is returned when a call would launch a run after Shutdown.
var ErrShuttingDown = errors.New("blueprint: shutting down")

// StartInput is what a CM session start carries.
type StartInput struct {
	ID         string
	Projects   []string
	Mode       string
	CostCapUSD float64
	Model      string
	Endpoint   protocol.LLMEndpoint
	// Price is the session model's resolved price from CM; nil, or all four
	// rates zero, means CM has none, and then a capped blueprint cannot run.
	Price *protocol.ModelPrice
	// MCPAPIKey lists CM's projects for the code stages.
	MCPAPIKey string
	// GitCredentialsToken is the bearer CM mints repository credentials for.
	GitCredentialsToken string
}

// Runner runs one stage. internal/stage implements it; tests fake it.
type Runner interface {
	Run(ctx context.Context, in stage.Input) (stage.Output, error)
}

// Workspace is the code checkout the stages with code tools read.
// internal/code implements it; tests fake it.
type Workspace interface {
	Checkout(ctx context.Context, blueprintID, name, repoURL string, creds code.Credentials) (code.Checkout, error)
	TreeRoot(blueprintID string) string
	// RemoteHead reads a repository's default branch and its sha without
	// fetching; preflight compares it with what grounding recorded.
	RemoteHead(ctx context.Context, repoURL string, creds code.Credentials) (string, string, error)
	// Remove deletes a blueprint's trees; emission calls it once the cards exist.
	Remove(ctx context.Context, blueprintID string) error
}

// CMClient is the ContextMatrix surface emission, its preflight, the run
// start and the watch use. internal/cm's Client implements it; tests fake it.
type CMClient interface {
	ListProjects(ctx context.Context) ([]cm.Project, error)
	ListPlaybooks(ctx context.Context) ([]cm.PlaybookSummary, error)
	GetPlaybook(ctx context.Context, id string) (cm.Playbook, error)
	GetCard(ctx context.Context, project, id string) (cm.Card, error)
	CreateCard(ctx context.Context, in cm.CardInput) (cm.CreatedCard, error)
	CreatePlaybook(ctx context.Context, in cm.PlaybookInput) (string, error)
	RunCard(ctx context.Context, project, id, agentID string) (cm.Card, error)
	RunPlaybook(ctx context.Context, id, agentID string) error
	Close() error
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
	// Workspace holds the read-only trees code stages read. nil disables
	// code stages: a stage with code tools then fails with a clear error.
	Workspace Workspace
	// Projects lists CM's projects with the session's MCP key; code stages
	// need it for repository urls and verify commands. nil disables them.
	Projects func(ctx context.Context, mcpAPIKey string) ([]cm.Project, error)
	// Credentials builds the git credential source for a session's token.
	// nil means repositories are fetched without auth.
	Credentials func(gitToken string) code.Credentials
	// CMTools opens one MCP session with the session's key for a run of a
	// stage with the cm tools and returns them, each result tagged with tag,
	// and the closer the run calls when it ends; the closer may be nil. nil
	// disables cm stages: such a stage then fails with a clear error before
	// any checkout.
	CMTools func(ctx context.Context, mcpAPIKey, tag string) ([]tools.Tool, func(), error)
	// ExportDir is where export bundles are written, one directory per
	// blueprint and time; empty refuses export.
	ExportDir string
	// ConnectCM opens a ContextMatrix MCP session with the session's key for
	// emission, its preflight, the run start and the watch. nil refuses
	// emission with a clear error.
	ConnectCM func(ctx context.Context, mcpAPIKey string) (CMClient, error)
	// CallerIdentity is the agent_id every ContextMatrix write passes. It is
	// a human: id, because run_card and run_playbook are human-gated; stage
	// tools never get it.
	CallerIdentity string
	// WatchInterval is how often an emitted blueprint's watch polls
	// ContextMatrix; 0 means one minute.
	WatchInterval time.Duration
}

// session is an attached CM session: the provisioned endpoint, model and
// keys, and the audit writer that masks the session's secrets.
type session struct {
	endpoint protocol.LLMEndpoint
	model    string
	price    *protocol.ModelPrice
	mcpKey   string
	gitToken string
	audit    audit
}

// secrets are the session's secrets, masked in every record and tool result.
func (s *session) secrets() []string {
	return []string{s.endpoint.APIKey, s.mcpKey, s.gitToken}
}

// run holds a blueprint's run slot: a run in flight, or a caller deciding
// whether to start one. Only the holder may start a run, so a run, a crash
// recovery and the need check never overlap. Slots outlive sessions, so Wait
// still blocks after End.
type run struct {
	cancel context.CancelFunc
	done   chan struct{}
	// shutdown is set under Controller.mu by Shutdown: the run ends without
	// touching the store.
	shutdown bool
	// answer is the open question's reply channel; nil when none is open.
	answer chan string
}

// Controller owns every attached blueprint.
type Controller struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*session
	runs     map[string]*run
	watches  map[string]*watcher
	closing  bool
}

// New builds a Controller.
func New(opts Options) *Controller {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.WatchInterval <= 0 {
		opts.WatchInterval = time.Minute
	}

	return &Controller{opts: opts, sessions: map[string]*session{}, runs: map[string]*run{}, watches: map[string]*watcher{}}
}

// effectivePrice maps an all-zero price to nil. CM sends zero rates for a
// model it cannot price; treating that as priced would make a cost cap that
// can never bind.
func effectivePrice(p *protocol.ModelPrice) *protocol.ModelPrice {
	if p == nil || *p == (protocol.ModelPrice{}) {
		return nil
	}

	return p
}

// Start attaches a session to the blueprint with in.ID, creating it on first
// sight. A stage left running by a crash is marked interrupted and re-run, a
// stage whose inputs were stored but which never started runs, and a
// human-sourced stage that waits shows its prompt again. At a ready end an
// open sign-off is shown again or, never shown, runs; an emission a restart
// cut short resumes, a stopped one is reminded of approve, and an emitted
// blueprint's watch restarts. Start on a session already attached does
// nothing.
func (c *Controller) Start(ctx context.Context, in StartInput) error {
	in.Price = effectivePrice(in.Price)

	// CM re-sends the start on every log reconnect; a session already held
	// changes nothing and leaves no trace.
	c.mu.Lock()
	_, held := c.sessions[in.ID]
	c.mu.Unlock()

	if held {
		return nil
	}

	sess := &session{
		endpoint: in.Endpoint, model: in.Model, price: in.Price,
		mcpKey: in.MCPAPIKey, gitToken: in.GitCredentialsToken,
	}
	sess.audit = c.newAudit(sess.secrets()...)

	bp, err := c.openBlueprint(ctx, in, sess.audit)
	if err != nil {
		return err
	}

	// A reopen whose start lost the price (CM's lookup failed) keeps pricing
	// at the rate the blueprint was created with, but only on the model that
	// rate belongs to.
	if sess.price == nil && bp.Price != nil && in.Model == bp.Model {
		sess.price = bp.Price
	}

	c.mu.Lock()

	if c.closing {
		c.mu.Unlock()

		return ErrShuttingDown
	}

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

	next, err := c.resumePoint(ctx, sess.audit, bp)
	if err != nil {
		c.release(in.ID, r)
		c.detach(in.ID, sess)

		return err
	}

	if next.end {
		c.resumeEnd(runCtx, r, sess, bp, next)

		return nil
	}

	// The prompt is shown again: the panel lost it with the ended session,
	// and the next message becomes the stage's artifact.
	if next.human {
		c.await(ctx, sess.audit, bp.ID, next.stage)
	}

	if next.stage == "" || next.human {
		c.release(in.ID, r)

		return nil
	}

	c.launch(runCtx, r, sess, bp, next.stage)

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
			Model: in.Model, Price: in.Price, State: string(StateNew), CreatedAt: now, UpdatedAt: now,
		}

		if err := c.opts.Store.CreateBlueprint(ctx, bp); err != nil {
			return store.Blueprint{}, fmt.Errorf("blueprint: start: %w", err)
		}

		au.event(ctx, Event{Kind: EventCreated, BlueprintID: in.ID, Payload: map[string]any{
			"projects": in.Projects, "mode": in.Mode, "cost_cap_usd": in.CostCapUSD, "model": in.Model, "priced": in.Price != nil,
		}})
	case err != nil:
		return store.Blueprint{}, fmt.Errorf("blueprint: start: %w", err)
	default:
		au.event(ctx, Event{Kind: EventReopened, BlueprintID: in.ID, Payload: map[string]any{"state": bp.State}})
	}

	return bp, nil
}

// interruptedStage marks every run a crash left running as interrupted and
// returns the stage of the latest such stage run, to re-run. A responder run
// left running sat inside its stage's run, so the stage run alone is re-run.
// Empty means no stage run was interrupted. The caller holds the run slot.
func (c *Controller) interruptedStage(ctx context.Context, bp store.Blueprint) (string, error) {
	left, err := c.leftRunning(ctx, bp.ID)
	if err != nil || len(left) == 0 {
		return "", err
	}

	stageID := ""

	for _, r := range left {
		if r.Kind == store.RunKindStage {
			stageID = r.Stage
		}
	}

	// Stage runs are closed last. A partial close must never leave the stage
	// run interrupted with a run inside it still running: the next start would
	// see only the inner run and re-run nothing.
	sort.SliceStable(left, func(i, j int) bool {
		return left[i].Kind != store.RunKindStage && left[j].Kind == store.RunKindStage
	})

	// Checked before the runs are marked: a run marked interrupted after a
	// transient error, with nothing relaunched, is never recovered. With no
	// need stored there is nothing to re-run from; the runs are closed and
	// the next message becomes the need.
	_, err = c.opts.Store.LatestArtifact(ctx, bp.ID, needStage)

	noNeed := errors.Is(err, store.ErrNotFound)
	if err != nil && !noNeed {
		return "", fmt.Errorf("blueprint: start: load the need for interrupted run %d: %w", left[0].ID, err)
	}

	for _, r := range left {
		if err := c.opts.Store.EndRun(ctx, r.ID, store.RunInterrupted, "process restarted", store.RunUsage{}, c.opts.Now().UTC()); err != nil {
			return "", fmt.Errorf("blueprint: mark interrupted: %w", err)
		}
	}

	if noNeed {
		return "", nil
	}

	return stageID, nil
}

// leftRunning returns every run a crash left in "running", oldest first.
func (c *Controller) leftRunning(ctx context.Context, id string) ([]store.Run, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list runs: %w", err)
	}

	var out []store.Run

	for _, r := range runs {
		if r.Status == store.RunRunning {
			out = append(out, r)
		}
	}

	return out, nil
}

// Message handles one user message: the answer to an open question, the
// need while the first stage has no artifact, the artifact of a
// human-sourced stage that waits for it, the sign-off decision (approve or
// reject <feedback>) at a ready end, or a note. The abort and export
// commands are parsed by the webhook and reach Abort and Export instead.
func (c *Controller) Message(ctx context.Context, id, text, msgID string) error {
	trimmed := strings.TrimSpace(text)

	c.mu.Lock()

	sess, ok := c.sessions[id]
	if !ok {
		c.mu.Unlock()

		return ErrNotFound
	}

	r := c.runs[id]

	if r != nil {
		answer := r.answer
		r.answer = nil
		c.mu.Unlock()

		if answer == nil {
			sess.audit.event(ctx, noteEvent(id, trimmed, msgID, true))

			return nil
		}

		// Logged before it is delivered, so the answer precedes what the run
		// does with it. The channel is buffered: the send never blocks.
		sess.audit.event(ctx, Event{Kind: EventAnswer, BlueprintID: id, Payload: map[string]any{"text": trimmed, "message_id": msgID}})

		answer <- trimmed

		return nil
	}

	if c.closing {
		c.mu.Unlock()

		return ErrShuttingDown
	}

	r, runCtx, _ := c.claimLocked(id)
	c.mu.Unlock()

	return c.input(ctx, runCtx, r, sess, id, trimmed, msgID)
}

// Abort ends the run in flight, if any. During an open question it is the
// abort, not the answer. The blueprint persists; the next message retries
// the stage that was cut short.
func (c *Controller) Abort(ctx context.Context, id, msgID string) error {
	c.mu.Lock()

	sess, ok := c.sessions[id]
	if !ok {
		c.mu.Unlock()

		return ErrNotFound
	}

	// Cancelled under c.mu, so Ask cannot open a new question on a run that
	// is already aborted.
	if r := c.runs[id]; r != nil {
		r.answer = nil
		r.cancel()
	}

	c.mu.Unlock()
	sess.audit.event(ctx, Event{Kind: EventAborted, BlueprintID: id, Payload: map[string]any{"message_id": msgID}})

	return nil
}

// safeID reports whether id is usable as one path element. Ids come from
// CM, so the export never trusts one to stay inside ExportDir.
func safeID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`) && filepath.IsLocal(id)
}

// Export renders the blueprint's record under ExportDir/<id>/<time> and
// tells the panel where. It reads the store only, whose records were masked
// when written, so a running stage is not disturbed and no secret is in the
// bundle. A session that is not attached is not found, as for a message, so
// ContextMatrix starts it again and re-sends the export: only an attached
// session's panel hears of the bundle.
func (c *Controller) Export(ctx context.Context, id, msgID string) error {
	if !safeID(id) {
		return ErrNotFound
	}

	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}

	if err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	if c.opts.ExportDir == "" {
		return errors.New("blueprint: export: no export directory configured")
	}

	bundle := export.Bundle{Blueprint: bp, Transcripts: map[int64]json.RawMessage{}}

	if bundle.Runs, err = c.opts.Store.ListRuns(ctx, id); err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	if bundle.Artifacts, err = c.opts.Store.ListArtifacts(ctx, id); err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	if bundle.Events, err = c.opts.Store.ListEvents(ctx, id); err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	if bundle.Sources, err = c.opts.Store.ListSources(ctx, id); err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	if bundle.Gates, err = c.opts.Store.ListGates(ctx, id, signoffStage); err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	if bundle.Emissions, err = c.opts.Store.ListEmissions(ctx, id); err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	for _, r := range bundle.Runs {
		t, err := c.opts.Store.GetTranscript(ctx, r.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}

		if err != nil {
			return fmt.Errorf("blueprint: export: %w", err)
		}

		bundle.Transcripts[r.ID] = t
	}

	dir := filepath.Join(c.opts.ExportDir, id, c.opts.Now().UTC().Format("20060102T150405Z"))

	files, err := export.Write(dir, bundle)
	if err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	c.mu.Lock()
	_, running := c.runs[id]
	c.mu.Unlock()

	// during_run tells the panel whether a stage keeps working after the
	// export; when none does, the panel must be told it is idle.
	sess.audit.event(ctx, Event{Kind: EventExported, BlueprintID: id, Payload: map[string]any{
		"path": dir, "files": len(files), "message_id": msgID, "during_run": running,
	}})

	return nil
}

// runModel is the model a run uses and its price; price is nil when the run
// is unpriced. override marks a per-persona override of the session model.
type runModel struct {
	name     string
	price    *protocol.ModelPrice
	override bool
}

// modelFor resolves the model a run of persona uses. CM prices only the
// session model, so an override naming another model runs unpriced.
func (c *Controller) modelFor(sess *session, persona string) runModel {
	if o := c.opts.Models[persona]; o != "" && o != sess.model {
		return runModel{name: o, override: true}
	}

	return runModel{name: sess.model, price: sess.price}
}

// capAllows reports whether a run of stageID on model m may start under the
// blueprint's cost cap, and the cap the run must stay under: the configured
// per-run cap, or what is left of the blueprint cap when that is smaller; 0
// means no cap (a run cap of 0 on an uncapped blueprint). A
// capped blueprint whose run would be unpriced is refused, because the cap
// could not bind. A refusal is reported to the panel, with refused merged
// into its event payload, and leaves the blueprint idle.
func (c *Controller) capAllows(ctx context.Context, au audit, bp store.Blueprint, stageID string, m runModel, refused map[string]any) (float64, bool) {
	runCap := c.opts.MaxRunCostUSD

	if bp.CostCapUSD <= 0 {
		return runCap, true
	}

	refuse := func(msg string) (float64, bool) {
		payload := map[string]any{"stage": stageID, "message": msg}
		maps.Copy(payload, refused)

		au.event(ctx, Event{Kind: EventError, BlueprintID: bp.ID, Payload: payload})
		c.setState(ctx, bp.ID, StateIdle)

		return 0, false
	}

	if m.price == nil {
		if m.override {
			return refuse(fmt.Sprintf("cost cap %.2f USD is set but model %q, the configured override for stage %s, has no price (CM prices only the session model), so the cap cannot bind; drop the override, or start a new blueprint without a cap", bp.CostCapUSD, m.name, stageID))
		}

		return refuse(fmt.Sprintf("cost cap %.2f USD is set but model %q has no price, so the cap cannot bind; pick a priced model, or start a new blueprint without a cap", bp.CostCapUSD, m.name))
	}

	spent, err := c.opts.Store.BlueprintCost(ctx, bp.ID)
	if err != nil {
		c.fail(ctx, au, bp.ID, 0, "cost lookup failed: "+err.Error())

		return 0, false
	}

	// A run cap of zero means no per-run cap; the blueprint cap alone
	// bounds the run by what is left of it.
	if remaining := bp.CostCapUSD - spent; runCap <= 0 || remaining < runCap {
		runCap = remaining
	}

	if runCap <= 0 {
		return refuse(fmt.Sprintf("cost cap %.2f USD would be breached (spent %.2f)", bp.CostCapUSD, spent))
	}

	return runCap, true
}

// noteEvent records a message that is neither the need nor an answer.
// duringRun tells the panel the run goes on working after it.
func noteEvent(id, text, msgID string, duringRun bool) Event {
	return Event{Kind: EventNote, BlueprintID: id, Payload: map[string]any{
		"text": text, "message_id": msgID, "during_run": duringRun,
	}}
}

// End detaches the session, cancelling any run in flight and stopping the
// watch, which it waits for. The blueprint persists and a later Start reopens it.
func (c *Controller) End(ctx context.Context, id string) error {
	if _, err := c.opts.Store.GetBlueprint(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}

		return fmt.Errorf("blueprint: end: %w", err)
	}

	c.mu.Lock()
	sess := c.sessions[id]
	delete(c.sessions, id)

	if r := c.runs[id]; r != nil {
		r.answer = nil
		r.cancel()
	}

	// The watcher leaves in the same section as the session, so a Start that
	// reopens the blueprint right after always sees no watcher and can start
	// its own.
	w := c.watches[id]
	delete(c.watches, id)

	c.mu.Unlock()

	if w != nil {
		w.cancel()
		<-w.done
	}

	au := c.newAudit()
	if sess != nil {
		au = sess.audit
	}

	au.event(ctx, Event{Kind: EventEnded, BlueprintID: id})

	return nil
}

// Shutdown stops accepting runs, cancels every run in flight and every watch
// and waits for those goroutines to exit. A run the cancel cut short writes no results
// (only its harness lines), so the next Start re-runs its stage; a run that
// completed anyway writes its results first. The store can be closed
// afterwards.
func (c *Controller) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	c.closing = true

	dones := make([]chan struct{}, 0, len(c.runs))

	for _, r := range c.runs {
		r.shutdown = true
		r.answer = nil
		r.cancel()

		dones = append(dones, r.done)
	}

	for _, w := range c.watches {
		w.cancel()

		dones = append(dones, w.done)
	}

	c.mu.Unlock()

	for _, d := range dones {
		select {
		case <-d:
		case <-ctx.Done():
			return fmt.Errorf("blueprint: shutdown: %w", ctx.Err())
		}
	}

	return nil
}

// shuttingDown reports whether r was cancelled by Shutdown.
func (c *Controller) shuttingDown(r *run) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return r.shutdown
}

// closingDown reports whether Shutdown has begun. A sign-off, emission or
// run start it cuts short is interrupted, not stopped: it reports nothing
// and keeps the state, so the restart resumes it.
func (c *Controller) closingDown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closing
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
