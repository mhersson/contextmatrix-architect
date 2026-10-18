// Package blueprint is the controller: one blueprint per CM session, a state
// machine over stages, stored question rounds, and the event stream.
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
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/export"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/research"
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

// errSessionEnded is the cancel cause End gives the run in flight: a stage
// it cuts short is recorded session_ended and runs again on reopen.
var errSessionEnded = errors.New("blueprint: the session ended")

// reasonSessionEnded is the reason a run End cut short ends with.
const reasonSessionEnded = "session_ended"

// endedRunWait bounds how long a reopen waits for the ended session's run to
// leave the slot. End's cancel winds a run down within moments; past this
// the run is left to record its own end and the stage waits for the next
// message.
const endedRunWait = 10 * time.Second

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
	// SearchProvider and SearchAPIKey are the session's research settings;
	// an empty provider means research is off.
	SearchProvider string
	SearchAPIKey   string
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
	// fetching; preflight compares it with the sha the checkout recorded.
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
	// Search builds a session's search provider from its research settings,
	// and Fetch reads the pages web_fetch is given. Either nil leaves
	// research off for every session.
	Search func(provider, apiKey string) (research.Searcher, error)
	Fetch  research.PageFetcher
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
	endpoint  protocol.LLMEndpoint
	model     string
	price     *protocol.ModelPrice
	mcpKey    string
	gitToken  string
	searchKey string
	// searcher is the session's search provider: nil when research is off,
	// and researchOff then says why.
	searcher    research.Searcher
	researchOff string
	// offNoted is set once the session has said research is off.
	offNoted atomic.Bool
	audit    audit
}

// secrets are the session's secrets, masked in every record and tool result.
func (s *session) secrets() []string {
	return []string{s.endpoint.APIKey, s.mcpKey, s.gitToken, s.searchKey}
}

// run holds a blueprint's run slot: a run in flight, or a caller deciding
// whether to start one. Only the holder may start a run, so a run, a crash
// recovery and the need check never overlap. Slots outlive sessions, so Wait
// still blocks after End.
type run struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
	// shutdown is set under Controller.mu by Shutdown: the run ends without
	// touching the store.
	shutdown bool
	// ended is set under Controller.mu by End: a reopen that finds the run
	// still in the slot waits for it to leave.
	ended bool
	// aborted is set under Controller.mu by Abort: a reopen waiting for the
	// run to leave resumes nothing, as after any abort.
	aborted bool
	// stage is the stage the run in flight runs, or the sign-off while it and
	// its emission hold the slot; set under Controller.mu.
	stage string
	// recheck is set under Controller.mu, and under the round lock, when a
	// reply or a chat message changed a round while the slot was held. The
	// holder's read of the round spends it, so one left at release is a
	// change the holder never saw, and the release acts on the round again.
	recheck bool
}

// Controller owns every attached blueprint.
type Controller struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*session
	runs     map[string]*run
	watches  map[string]*watcher
	// flights are each blueprint's runs in flight, outermost first, for the view.
	flights map[string][]*flight
	// live streams each blueprint's view and drafts to its subscribers.
	live *liveHub
	// roundLocks serialize each blueprint's round changes: a post, a reply
	// and a close. Per blueprint, not per session: a reopen attaches a new
	// session while the ended one's run may still post.
	roundLocks map[string]*sync.Mutex
	closing    bool
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

	c := &Controller{
		opts: opts, sessions: map[string]*session{}, runs: map[string]*run{}, watches: map[string]*watcher{},
		flights: map[string][]*flight{}, roundLocks: map[string]*sync.Mutex{},
	}
	c.live = newLiveHub(c.View, opts.Logger)

	return c
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

// Start attaches a session to the blueprint with in.ID, creating it on
// first sight. A question round the pipeline stands at resumes: an open one
// is shown again and waits, an answered one closes and its stage continues
// from the stored conversation, and one whose continuation failed or was
// aborted waits for the next message; an open round the pipeline went past
// is collapsed. A stage left running by a crash is marked interrupted and
// re-run, and a stage whose inputs were stored but which never started
// runs. An open gate is shown again, or, never shown, opened under the
// policy now in effect once its pending review or builder round has run. At
// a ready end an open sign-off is shown again or, never shown, runs; an
// emission a restart cut short resumes, a stopped one is reminded of
// approve, and an emitted blueprint's watch restarts. A reopen that finds
// the ended session's run still finishing waits for it to leave the slot
// first. Start on a session already attached does nothing.
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
		mcpKey: in.MCPAPIKey, gitToken: in.GitCredentialsToken, searchKey: in.SearchAPIKey,
	}
	sess.audit = c.newAudit(sess.secrets()...)
	sess.searcher, sess.researchOff = c.sessionResearch(in, sess.audit)

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
	prev := c.runs[in.ID]
	ended := !claimed && prev.ended
	c.mu.Unlock()

	if !claimed {
		if !ended {
			return nil
		}

		// The ended session's run records its own end before it leaves the
		// slot, so the record then says what the reopen resumes.
		if r, runCtx, claimed = c.claimAfterEnd(ctx, in.ID, sess, prev); !claimed {
			return nil
		}
	}

	next, err := c.resumePoint(ctx, sess.audit, bp)
	if err != nil {
		c.release(in.ID, r)
		c.detach(in.ID, sess)

		return err
	}

	if next.qround != nil {
		c.resumeRound(runCtx, r, sess, bp, *next.qround)

		return nil
	}

	if next.end {
		c.resumeEnd(runCtx, r, sess, bp, next)

		return nil
	}

	if next.gate {
		c.resumeGate(runCtx, r, sess, bp, next)

		return nil
	}

	if next.stage == "" {
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
// A builder round is not returned: the critique runs that round again, in
// its attempt. Empty means no stage is re-run. The caller holds the run slot.
func (c *Controller) interruptedStage(ctx context.Context, bp store.Blueprint) (string, error) {
	left, err := c.leftRunning(ctx, bp.ID)
	if err != nil || len(left) == 0 {
		return "", err
	}

	var latest store.Run

	for _, r := range left {
		if r.Kind == store.RunKindStage {
			latest = r
		}
	}

	stageID := latest.Stage
	if latest.Round > 1 {
		stageID = ""
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

// Message handles one user message: chat about the round a human answers
// while it waits, the need while the first stage has no artifact, the
// sign-off decision (approve or reject <feedback>) at a ready end, or a
// note. The abort and export
// commands are parsed by the webhook and reach Abort and Export instead.
func (c *Controller) Message(ctx context.Context, id, text, msgID string) error {
	trimmed := strings.TrimSpace(text)

	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	// A message while a human's round waits is chat about it, whether or not
	// a chat run holds the slot.
	chatted, err := c.chatAbout(ctx, sess, id, trimmed, msgID)
	if err != nil {
		return fmt.Errorf("blueprint: message: %w", err)
	}

	if chatted {
		return nil
	}

	c.mu.Lock()

	if sess, ok = c.sessions[id]; !ok {
		c.mu.Unlock()

		return ErrNotFound
	}

	r := c.runs[id]

	if r != nil {
		open := r.stage
		c.mu.Unlock()

		sess.audit.event(ctx, noteEvent(id, trimmed, msgID, open, true))

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

// Abort ends the run in flight, if any; an open round stays open. The
// blueprint persists; the next message retries the stage that was cut short.
func (c *Controller) Abort(ctx context.Context, id, msgID string) error {
	c.mu.Lock()

	sess, ok := c.sessions[id]
	if !ok {
		c.mu.Unlock()

		return ErrNotFound
	}

	if r := c.runs[id]; r != nil {
		r.aborted = true
		r.cancel(nil)
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

	if bundle.Questions, err = c.opts.Store.ListQuestions(ctx, id); err != nil {
		return fmt.Errorf("blueprint: export: %w", err)
	}

	if bundle.Gates, err = c.opts.Store.AllGates(ctx, id); err != nil {
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

// minRunFloorUSD is the least a run needs left of the blueprint cap to
// start, however little its kind's earlier runs cost.
const minRunFloorUSD = 0.10

// capBlueprint is a cap stop's "cap" in its error event payload.
const capBlueprint = "blueprint"

// runBudget is what the blueprint cap allows a run: the cap it must stay
// under (0 means none), or refused, why it may not start. bound says capUSD
// is what the blueprint cap has left, being below the per-run cap or there
// being none, so the run reaching it is a cap stop. blueprintCapUSD is the
// blueprint cap it was read under.
type runBudget struct {
	capUSD          float64
	bound           bool
	refused         *capStop
	blueprintCapUSD float64
}

// capStop is a stage the blueprint cost cap stopped: a run of it refused, or
// cut short at what the blueprint had left. floor is set when what is left
// is below the floor the next run needs; unpriced is the refusal of a model
// without a price, which keeps its own text.
type capStop struct {
	stage         string
	capUSD, spent float64
	floor         float64
	unpriced      string
}

// Error is the stop's message, which says how to go on.
func (s *capStop) Error() string {
	if s.unpriced != "" {
		return s.unpriced
	}

	return "The " + s.shortfall() + "; " + pack.StageName(s.stage) + " stopped. Raise the cap by clicking the cost in the header (or reply `cap <usd>`) and it continues."
}

// reason is why a responder the blueprint cap refused did not run: the
// asking stage goes on, so it names no stop.
func (s *capStop) reason() string {
	if s.unpriced != "" {
		return s.unpriced
	}

	return "the " + s.shortfall()
}

func (s *capStop) shortfall() string {
	if s.floor == 0 {
		return fmt.Sprintf("blueprint's $%.2f cap is used up ($%.2f spent)", s.capUSD, s.spent)
	}

	return fmt.Sprintf("blueprint's $%.2f cap leaves $%.2f ($%.2f spent), less than the $%.2f the next run needs",
		s.capUSD, s.capUSD-s.spent, s.spent, s.floor)
}

// payload is the stop's error event payload.
func (s *capStop) payload() map[string]any {
	return map[string]any{
		"stage": s.stage, "message": s.Error(),
		"cap": capBlueprint, "cap_usd": s.capUSD, "blueprint_spent_usd": s.spent,
	}
}

// capStopOf reports whether e is a cap stop, and the stage it stopped.
func capStopOf(e store.Event) (string, bool) {
	if e.Kind != EventError {
		return "", false
	}

	var p struct {
		Cap   string `json:"cap"`
		Stage string `json:"stage"`
	}

	if json.Unmarshal(e.Payload, &p) != nil || p.Cap != capBlueprint {
		return "", false
	}

	return p.Stage, true
}

// runCap is what the blueprint cap allows a run of kind on stageID on model
// m: the configured per-run cap, or what is left of the blueprint cap when
// that is smaller. It refuses a capped blueprint whose run would be
// unpriced, since the cap could not bind, a spent cap, and less left than
// the run's floor. It reports nothing.
func (c *Controller) runCap(ctx context.Context, bp store.Blueprint, kind, stageID string, m runModel) (runBudget, error) {
	// The stored cap, not bp's: a chain of runs holds bp from its start, and
	// a cap command applies from the next run.
	cur, err := c.opts.Store.GetBlueprint(ctx, bp.ID)
	if err != nil {
		return runBudget{}, fmt.Errorf("cost lookup failed: %w", err)
	}

	capUSD := cur.CostCapUSD
	b := runBudget{capUSD: c.opts.MaxRunCostUSD, blueprintCapUSD: capUSD}

	if capUSD <= 0 {
		return b, nil
	}

	spent, err := c.opts.Store.BlueprintCost(ctx, bp.ID)
	if err != nil {
		return runBudget{}, fmt.Errorf("cost lookup failed: %w", err)
	}

	stop := &capStop{stage: stageID, capUSD: capUSD, spent: spent}

	if m.price == nil {
		stop.unpriced = fmt.Sprintf("cost cap %.2f USD is set but model %q has no price, so the cap cannot bind; pick a priced model, or start a new blueprint without a cap", capUSD, m.name)
		if m.override {
			stop.unpriced = fmt.Sprintf("cost cap %.2f USD is set but model %q, the configured override for stage %s, has no price (CM prices only the session model), so the cap cannot bind; drop the override, or start a new blueprint without a cap", capUSD, m.name, pack.StageName(stageID))
		}

		return runBudget{refused: stop}, nil
	}

	left := capUSD - spent
	if left <= 0 {
		return runBudget{refused: stop}, nil
	}

	floor, err := c.runFloor(ctx, bp.ID, kind, stageID)
	if err != nil {
		return runBudget{}, err
	}

	if left < floor {
		stop.floor = floor

		return runBudget{refused: stop}, nil
	}

	// A run cap of zero means no per-run cap; the blueprint cap alone
	// bounds the run by what is left of it.
	if b.capUSD <= 0 || left < b.capUSD {
		b.capUSD, b.bound = left, true
	}

	return b, nil
}

// runFloor is what a run of kind on stageID needs left of the blueprint cap
// to start: the median cost of the blueprint's done runs of that kind and
// stage, never below minRunFloorUSD.
func (c *Controller) runFloor(ctx context.Context, id, kind, stageID string) (float64, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("cost lookup failed: %w", err)
	}

	var costs []float64

	for _, r := range runs {
		if r.Status == store.RunDone && r.Kind == kind && r.Stage == stageID {
			costs = append(costs, r.Usage.CostUSD)
		}
	}

	if len(costs) == 0 {
		return minRunFloorUSD, nil
	}

	sort.Float64s(costs)

	mid := len(costs) / 2
	median := costs[mid]

	if len(costs)%2 == 0 {
		median = (costs[mid-1] + costs[mid]) / 2
	}

	return max(minRunFloorUSD, median), nil
}

// capStopAfter is the cap stop of a run of kind on stageID cut short at what
// the blueprint cap capUSD it started under had left, as the ledger stands
// once the run has ended.
func (c *Controller) capStopAfter(ctx context.Context, id string, capUSD float64, kind, stageID string) (*capStop, error) {
	spent, err := c.opts.Store.BlueprintCost(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("cost lookup failed: %w", err)
	}

	stop := &capStop{stage: stageID, capUSD: capUSD, spent: spent}

	if spent < capUSD {
		if stop.floor, err = c.runFloor(ctx, id, kind, stageID); err != nil {
			return nil, err
		}
	}

	return stop, nil
}

// capAllows reports whether a stage run of stageID on model m may start
// under the blueprint's cost cap, and what it is allowed (see runCap). A
// refusal is a cap stop reported to the panel, with refused merged into its
// event payload, and leaves the blueprint idle.
func (c *Controller) capAllows(ctx context.Context, au audit, bp store.Blueprint, stageID string, m runModel, refused map[string]any) (runBudget, bool) {
	b, err := c.runCap(ctx, bp, store.RunKindStage, stageID, m)
	if err != nil {
		c.fail(ctx, au, bp.ID, 0, err.Error())

		return runBudget{}, false
	}

	if b.refused != nil {
		payload := b.refused.payload()
		maps.Copy(payload, refused)

		au.event(ctx, Event{Kind: EventError, BlueprintID: bp.ID, Payload: payload})
		c.setState(ctx, bp.ID, StateIdle)

		return runBudget{}, false
	}

	return b, true
}

// noteEvent records a message that is neither the need nor an answer.
// stage is the stage open when it came, "" when none; duringRun tells the
// panel a run goes on working after it.
func noteEvent(id, text, msgID, stage string, duringRun bool) Event {
	return Event{Kind: EventNote, BlueprintID: id, Payload: map[string]any{
		"text": text, "message_id": msgID, "stage": stage, "during_run": duringRun,
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
		r.ended = true
		r.cancel(errSessionEnded)
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
		r.cancel(nil)

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

	ctx, cancel := context.WithCancelCause(context.Background())
	r := &run{cancel: cancel, done: make(chan struct{})}
	c.runs[id] = r

	return r, ctx, true
}

// claimAfterEnd takes id's run slot for sess once prev, the ended session's
// run, has left it, waiting at most endedRunWait. It fails when the wait runs
// out, or when by then an abort has reached prev, Shutdown has begun, sess is
// no longer attached or another caller took the slot; c.mu is not held while
// it waits. prev's cancel cause stays End's, so only the flag tells an abort
// during the wait.
func (c *Controller) claimAfterEnd(ctx context.Context, id string, sess *session, prev *run) (*run, context.Context, bool) {
	wait, cancel := context.WithTimeout(ctx, endedRunWait)
	defer cancel()

	select {
	case <-prev.done:
	case <-wait.Done():
		c.opts.Logger.Warn("blueprint: the ended session's run still holds the slot", "blueprint", id, "error", wait.Err())

		return nil, nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if prev.aborted || c.closing || c.sessions[id] != sess {
		return nil, nil, false
	}

	return c.claimLocked(id)
}

// release frees id's run slot and wakes Wait. When a round changed while r
// held the slot and r never read the change, the slot is taken again at once
// to act on it, unless r was aborted, which leaves the round to the next
// message, or ended, which leaves it to the reopen.
func (c *Controller) release(id string, r *run) {
	c.mu.Lock()

	if c.runs[id] == r {
		delete(c.runs, id)
	}

	var (
		next    *run
		nextCtx context.Context
	)

	// Claimed before r's done closes, so Wait never sees the slot free while
	// a round still has to be acted on.
	if r.recheck && !r.aborted && !r.ended && !c.closing && c.sessions[id] != nil {
		next, nextCtx, _ = c.claimLocked(id)
	}

	c.mu.Unlock()

	r.cancel(nil)
	close(r.done)

	if next != nil {
		c.settleOn(nextCtx, next, id)
	}
}

// roundLock is blueprint id's round lock.
func (c *Controller) roundLock(id string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()

	l := c.roundLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		c.roundLocks[id] = l
	}

	return l
}

// detach removes sess unless another session replaced it.
func (c *Controller) detach(id string, sess *session) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sessions[id] == sess {
		delete(c.sessions, id)
	}
}
