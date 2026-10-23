package blueprint

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// emissionStage names the board, the pseudo-artifact emission produces.
const emissionStage = "emission"

// What the view calls a pipeline step's state.
const (
	stepPending    = "pending"
	stepRunning    = "running"
	stepReviewing  = "reviewing"
	stepRevising   = "revising"
	stepWaiting    = "waiting"
	stepDone       = "done"
	stepFailed     = "failed"
	stepFolded     = "folded"
	stepSkipped    = "skipped"
	stepStopped    = "stopped"
	stepNotReached = "not_reached"
)

// viewEnded is the view's state for a pipeline that stopped before the
// sign-off; the blueprint's row stays idle.
const viewEnded = "ended"

// What the view calls an artifact version's state.
const (
	artCurrent    = "current"
	artReviewing  = "reviewing"
	artAtGate     = "at_gate"
	artApproved   = "approved"
	artRejected   = "rejected"
	artSuperseded = "superseded"
	artFolded     = "folded"
)

// Who decided a gate, as the view says it.
const (
	byYou       = "you"
	byAutomatic = "automatic"
)

// Open prompt kinds, as the prompts themselves name them; a mockup has no
// transcript prompt, only the open one.
const (
	promptGate   = "gate"
	promptRound  = "round"
	promptMockup = "mockup"
)

// record is what one view is built from: the blueprint's stored record,
// where its pipeline stands, and what is in flight.
type record struct {
	bp       store.Blueprint
	seq      int64
	runs     []store.Run
	arts     map[string][]store.Artifact // by stage, oldest version first
	gates    map[string][]store.Gate     // by stage, oldest first
	shown    map[string]map[int]bool     // stage gates shown, by stage and version
	signoffs []store.Event               // sign-offs recorded, oldest first
	emitted  *store.Event
	// resume is the newest stopped emission or run start that waits for
	// approve, and resumed the id of the newest emission or run start.
	resume   *store.Event
	resumed  int64
	unpriced bool
	pos      outcome
	last     store.Run
	flights  []flightView
	// round is the open round's number and roundStage its stage; 0 and ""
	// when none waits. mockup marks a mockup round, and unprompted one no
	// prompt waits on: the auto responder steers it (steered), a designer run
	// is writing its next revision or reply, or a use or skip decided it.
	round      int
	roundStage string
	mockup     bool
	unprompted bool
	steered    bool
	// handedRound is the builder round an open round handed to the
	// responder was asked in; 0 when there is none.
	handedRound int
	// offer is set while the mockup offer waits at the step the pipeline
	// stands at.
	offer bool
	// policies is each stage's gate policy, its question policy when it has no gate.
	policies map[string]string
	// steps is the blueprint's pipeline and pieces the slices it repeats
	// per-slice stages for.
	steps  []pipelineStep
	pieces []piece
}

// View is where blueprint id stands: the pipeline's steps, the run in
// flight, the prompt open, the cost and every artifact version. It reads
// the store and memory only, so an ended session's blueprint has a view
// while the architect is up. ErrNotFound for an unknown id.
func (c *Controller) View(ctx context.Context, id string) (protocol.BlueprintView, error) {
	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return protocol.BlueprintView{}, ErrNotFound
	}

	if err != nil {
		return protocol.BlueprintView{}, fmt.Errorf("blueprint: view: %w", err)
	}

	rec, err := c.record(ctx, bp)
	if err != nil {
		return protocol.BlueprintView{}, fmt.Errorf("blueprint: view %s: %w", id, err)
	}

	endReason, endDetail := c.endReason(rec), ""
	if bp.PlainLanguage && endReason != "" {
		endReason, endDetail = plainEndReason, endReason
	}

	return protocol.BlueprintView{
		ID: bp.ID, Mode: bp.Mode, PlainLanguage: bp.PlainLanguage, State: rec.state(), EndReason: endReason, EndDetail: endDetail,
		Seq:      uint64(rec.seq), //nolint:gosec // event ids are positive
		Pipeline: c.pipeline(rec), Current: rec.current(), Open: rec.open(),
		Cost: rec.cost(), Artifacts: c.artifactRefs(rec), Slices: rec.sliceList(),
	}, nil
}

// sliceList is the blueprint's slices for the view, those shownPieces
// gives: [] before slicing stores a version, for a record from before
// slices, and on a pack with no per-slice stage, whose steps never repeat.
func (rec record) sliceList() []protocol.BlueprintSlice {
	shown := shownPieces(rec.steps, rec.pieces)
	out := make([]protocol.BlueprintSlice, 0, len(shown))

	for _, p := range shown {
		out = append(out, protocol.BlueprintSlice{ID: p.id, Title: p.title})
	}

	return out
}

// record loads what a view of bp is built from. The sequence is read
// first, so a change that lands meanwhile raises the next view's.
func (c *Controller) record(ctx context.Context, bp store.Blueprint) (record, error) {
	rec := record{
		bp: bp, arts: map[string][]store.Artifact{}, gates: map[string][]store.Gate{},
		shown: map[string]map[int]bool{}, policies: map[string]string{},
	}

	var err error

	if rec.seq, err = c.opts.Store.LastEventID(ctx, bp.ID); err != nil {
		return record{}, err
	}

	// A flight lands only after its row ends, so reading flights first
	// counts an ending run once.
	rec.flights = c.inFlight(bp.ID)

	if rec.runs, err = c.opts.Store.ListRuns(ctx, bp.ID); err != nil {
		return record{}, err
	}

	arts, err := c.opts.Store.ListArtifacts(ctx, bp.ID)
	if err != nil {
		return record{}, err
	}

	for _, a := range arts {
		rec.arts[a.Stage] = append(rec.arts[a.Stage], a)
	}

	var slicing *store.Artifact

	if arts := rec.arts[c.opts.Pack.SlicingStage()]; len(arts) > 0 {
		slicing = &arts[len(arts)-1]
	}

	noMockup, err := c.mockupCollapsed(ctx, bp.ID)
	if err != nil {
		return record{}, err
	}

	rec.steps, rec.pieces = blueprintSteps(c.opts.Pack, rec.runs, slicing, noMockup)

	gates, err := c.opts.Store.AllGates(ctx, bp.ID)
	if err != nil {
		return record{}, err
	}

	for _, g := range gates {
		rec.gates[g.Stage] = append(rec.gates[g.Stage], g)
	}

	shown, err := c.opts.Store.ListEventsOfKind(ctx, bp.ID, EventGate)
	if err != nil {
		return record{}, err
	}

	for _, e := range shown {
		var p struct {
			Stage   string `json:"stage"`
			Version int    `json:"version"`
		}

		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}

		if rec.shown[p.Stage] == nil {
			rec.shown[p.Stage] = map[int]bool{}
		}

		rec.shown[p.Stage][p.Version] = true
	}

	if rec.signoffs, err = c.signoffEvents(ctx, bp.ID); err != nil {
		return record{}, err
	}

	if rec.emitted, err = c.latestEvent(ctx, bp.ID, EventEmitted); err != nil {
		return record{}, err
	}

	if err := c.readResume(ctx, &rec); err != nil {
		return record{}, err
	}

	if rec.unpriced, err = c.anyUnpriced(ctx, bp.ID); err != nil {
		return record{}, err
	}

	replies, err := c.opts.Store.ListEventsOfKind(ctx, bp.ID, EventPolicy)
	if err != nil {
		return record{}, fmt.Errorf("blueprint: read the gate replies: %w", err)
	}

	for _, st := range c.opts.Pack.Stages {
		if rec.policies[st.ID], err = c.resolvePolicy(bp.Mode, bp.PlainLanguage, replies, st.ID, true); err != nil {
			return record{}, err
		}
	}

	// A record the pack cannot place still has a view: every step pending,
	// the artifacts and the cost as stored.
	if rec.pos, rec.last, err = c.position(ctx, bp); err != nil {
		c.opts.Logger.Warn("blueprint: the view cannot place the pipeline", "blueprint", bp.ID, "error", err)

		rec.pos, rec.last = outcome{}, store.Run{}
	}

	if rec.offer, err = c.offerAt(ctx, bp.ID, rec.pos); err != nil {
		return record{}, err
	}

	place, found, err := c.roundAt(ctx, bp.ID)
	if err != nil {
		return record{}, err
	}

	// A handed-over mockup reads as one the auto responder steers.
	switch {
	case !found || place.state.Frame.Closed:
	case place.state.Mockup != nil:
		rec.round, rec.roundStage, rec.mockup = place.state.Number, place.state.Frame.Stage, true
		_, writing := rec.flightOf(place.state.Frame.Stage)
		rec.unprompted = place.state.Frame.Responder || writing || place.state.Mockup.Action != ""
		rec.steered = place.state.Frame.Responder
	case place.state.HandedOver != nil:
		// A handed-over round waits for nobody: the responder answers it.
		rec.handedRound = place.state.BuilderRound
	default:
		rec.round, rec.roundStage = place.state.Number, place.state.Frame.Stage
	}

	return rec, nil
}

// latestEvent is blueprint id's newest event of kind; nil when there is none.
func (c *Controller) latestEvent(ctx context.Context, id, kind string) (*store.Event, error) {
	e, err := c.opts.Store.LatestEvent(ctx, id, kind)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("blueprint: read the latest %s: %w", kind, err)
	}

	return &e, nil
}

// signoffEvents are the sign-offs recorded for blueprint id, shown or
// approved automatically, oldest first.
func (c *Controller) signoffEvents(ctx context.Context, id string) ([]store.Event, error) {
	var out []store.Event

	for _, kind := range []string{EventSignOff, EventSignOffAuto} {
		events, err := c.opts.Store.ListEventsOfKind(ctx, id, kind)
		if err != nil {
			return nil, fmt.Errorf("blueprint: read the sign-offs: %w", err)
		}

		out = append(out, events...)
	}

	slices.SortFunc(out, func(a, b store.Event) int { return cmp.Compare(a.ID, b.ID) })

	return out, nil
}

// readResume finds the newest stopped emission or run start, an await or
// error event whose payload names what approve resumes, and the newest
// emission or run start that may have followed it.
func (c *Controller) readResume(ctx context.Context, rec *record) error {
	for _, kind := range []string{EventAwaitInput, EventError} {
		events, err := c.opts.Store.ListEventsOfKind(ctx, rec.bp.ID, kind)
		if err != nil {
			return fmt.Errorf("blueprint: read the %s events: %w", kind, err)
		}

		for i := range events {
			var p struct {
				Resume string `json:"resume"`
			}

			if json.Unmarshal(events[i].Payload, &p) == nil && p.Resume != "" && (rec.resume == nil || events[i].ID > rec.resume.ID) {
				rec.resume = &events[i]
			}
		}
	}

	if rec.emitted != nil {
		rec.resumed = rec.emitted.ID
	}

	started, err := c.latestEvent(ctx, rec.bp.ID, EventRunStarted)
	if err != nil {
		return err
	}

	if started != nil && started.ID > rec.resumed {
		rec.resumed = started.ID
	}

	return nil
}

// anyUnpriced reports whether a run of blueprint id ended unpriced.
func (c *Controller) anyUnpriced(ctx context.Context, id string) (bool, error) {
	ended, err := c.opts.Store.ListEventsOfKind(ctx, id, EventStageEnded)
	if err != nil {
		return false, fmt.Errorf("blueprint: read the run ends: %w", err)
	}

	for _, e := range ended {
		var p struct {
			Priced *bool `json:"priced"`
		}

		if json.Unmarshal(e.Payload, &p) == nil && p.Priced != nil && !*p.Priced {
			return true, nil
		}
	}

	return false, nil
}

// signoffVersion is the version of the artifact a sign-off event judged.
func signoffVersion(e store.Event) int {
	var p struct {
		Version int `json:"version"`
	}

	_ = json.Unmarshal(e.Payload, &p)

	return p.Version
}

// payloadInt reads a payload number, typed or JSON-decoded.
func payloadInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}

// live is the runs in flight whose rows still run, outermost first: a run
// whose end is recorded counts in the ledger, not twice.
func (rec record) live() []flightView {
	running := map[int64]bool{}

	for _, r := range rec.runs {
		if r.Status == store.RunRunning {
			running[r.ID] = true
		}
	}

	var out []flightView

	for _, f := range rec.flights {
		if running[f.runID] {
			out = append(out, f)
		}
	}

	return out
}

// flightOf is the outermost live run of stageID.
func (rec record) flightOf(stageID string) (flightView, bool) {
	for _, f := range rec.live() {
		if f.stage == stageID {
			return f, true
		}
	}

	return flightView{}, false
}

// latest is stageID's newest artifact version; 0 when it has none.
func (rec record) latest(stageID string) int {
	arts := rec.arts[stageID]
	if len(arts) == 0 {
		return 0
	}

	return arts[len(arts)-1].Version
}

// decision is the latest decision on stageID's version, superseded rows
// aside, as the gate reads it.
func (rec record) decision(stageID string, version int) (store.Gate, bool) {
	var (
		g     store.Gate
		found bool
	)

	for _, x := range rec.gates[stageID] {
		if x.Version == version && x.Decision != store.GateSuperseded {
			g, found = x, true
		}
	}

	return g, found
}

// failed reports whether the run that placed the pipeline failed, or the
// blueprint did.
func (rec record) failed() bool {
	return rec.last.Status == store.RunFailed || rec.bp.State == string(StateFailed)
}

// ended reports a pipeline that stopped before the sign-off: slicing that
// holds no slices, cards the contract stopped, or, on a pack with no stage
// that checks cards, readiness's rounds spent. It is read from the record,
// so a blueprint that ended before the view said so reads ended too.
func (rec record) ended() bool { return rec.pos.end && !rec.pos.ready }

// state is the view's lifecycle state: ended for an ended pipeline, whose
// row stays idle, else the row's.
func (rec record) state() string {
	if rec.ended() {
		return viewEnded
	}

	return rec.bp.State
}

// plainEndReason is what a person reads of an ended pipeline; the technical
// reason is folded into the view's end detail.
const plainEndReason = "The architect could not finish the plan on its own. A technical helper can read why below; start a new blueprint to try again."

// endReason says in one line why an ended pipeline stopped, by the step
// whose artifact its end carries; "" when it has not ended.
func (c *Controller) endReason(rec record) string {
	if !rec.ended() {
		return ""
	}

	st, ok := c.stepAt(rec.pos.art.Stage)
	if !ok {
		return ""
	}

	name := strings.ReplaceAll(st.Stage.ID, "_", " ")

	switch {
	case st.LoopsTo != "":
		return fmt.Sprintf("%s still found problems after %d rounds", name, rec.pos.round)
	case slices.Contains(st.Checks, pack.CheckCards):
		return fmt.Sprintf("the cards still failed their contract checks after %d submits", maxContractRounds)
	case slices.Contains(st.Checks, pack.CheckSlices):
		return name + " holds no slices, so nothing after it can run"
	default:
		return ""
	}
}

// unreached is the state of a step after the one that ended the pipeline:
// skipped when its collapse is recorded, done or folded when it holds a
// version from before the end (a loop's chain the end cut short), and
// not_reached when nothing of it was stored for this slicing or a reject
// superseded its version and it never ran again.
func (rec record) unreached(st pipelineStep) string {
	switch {
	case st.Collapsed && rec.collapseRecorded(st.ID):
		return stepSkipped
	case st.Collapsed || rec.latest(st.ID) == 0 || rec.supersededByGate(st.ID, rec.latest(st.ID)):
		return stepNotReached
	case rec.folded(st.ID):
		return stepFolded
	default:
		return stepDone
	}
}

// collapseRecorded reports a fold row at version 0 on key: the record of a
// shared stage that collapsed.
func (rec record) collapseRecorded(key string) bool {
	return slices.ContainsFunc(rec.gates[key], func(g store.Gate) bool { return g.Version == 0 && g.Decision == store.GateFold })
}

// supersededByGate reports whether a gates row records stageID's version
// superseded, as a rejection of a stage folded into its gate does.
func (rec record) supersededByGate(stageID string, version int) bool {
	return slices.ContainsFunc(rec.gates[stageID], func(g store.Gate) bool {
		return g.Version == version && g.Decision == store.GateSuperseded
	})
}

// folded reports whether stageID's newest version folded into a later gate.
func (rec record) folded(stageID string) bool {
	v := rec.latest(stageID)

	for _, g := range rec.gates[stageID] {
		if g.Version == v && g.Decision == store.GateFold {
			return true
		}
	}

	return false
}

// attempt is the attempt of stageID's newest stage run; 0 before its first.
func (rec record) attempt(stageID string) int {
	n := 0

	for _, r := range rec.runs {
		if r.Kind == store.RunKindStage && r.Stage == stageID {
			n = r.Attempt
		}
	}

	return n
}

// stageCost is what stageID's runs of every kind cost: the ledger and the
// live runs.
func (rec record) stageCost(stageID string) float64 {
	sum := 0.0

	for _, r := range rec.runs {
		if r.Stage == stageID {
			sum += r.Usage.CostUSD
		}
	}

	for _, f := range rec.live() {
		if f.stage == stageID {
			sum += f.spent
		}
	}

	return sum
}

// signoffRecorded reports whether a sign-off of version was shown or
// recorded as approved automatically.
func (rec record) signoffRecorded(version int) bool {
	return slices.ContainsFunc(rec.signoffs, func(e store.Event) bool { return signoffVersion(e) == version })
}

// pipeline is one step per pipeline step, a per-slice stage once per slice,
// and, when the pack checks cards, the sign-off and the board.
func (c *Controller) pipeline(rec record) []protocol.PipelineStep {
	steps := make([]protocol.PipelineStep, 0, len(rec.steps)+2)
	current := rec.currentIndex()

	for i, st := range rec.steps {
		step := protocol.PipelineStep{
			Stage: st.Stage.ID, Slice: st.Slice, Persona: st.Persona, Gate: st.Gate, Policy: rec.policies[st.Stage.ID],
			Status: rec.stageStatus(st, i, current), Attempt: rec.attempt(st.ID),
			LatestVersion: rec.latest(st.ID), CostUSD: rec.stageCost(st.ID),
		}

		if st.Fold != nil {
			step.FoldsInto, _ = pack.SplitStep(foldTarget(rec.steps, st.ID))
		}

		if st.LoopsTo != "" {
			step.Round, step.MaxRounds = rec.loopRound(st), st.MaxRounds
		}

		steps = append(steps, step)
	}

	if c.cardsStage() != "" {
		steps = append(steps, rec.signoffStep(), rec.emissionStep())
	}

	return steps
}

// currentIndex is the index of the step the pipeline stands at: on an
// ended pipeline the step that stopped it, the number of steps once it
// ended ready, and -1 when the record cannot be placed.
func (rec record) currentIndex() int {
	switch {
	case rec.ended():
		return stepIndex(rec.steps, rec.pos.art.Stage)
	case rec.pos.end:
		return len(rec.steps)
	default:
		return stepIndex(rec.steps, rec.pos.stage)
	}
}

// stageStatus is the state of step st at index i: done or folded behind the
// current step, pending after it, and at it what its live run, open round,
// gate or last run says. A collapsed shared stage is skipped, and while a
// decomposition chain runs the per-slice steps after it that it does not
// run are done. On an ended pipeline the step that stopped it is stopped and
// the steps after it read as unreached says.
func (rec record) stageStatus(st pipelineStep, i, current int) string {
	chained, _ := pack.SplitStep(rec.pos.stage)

	switch {
	case rec.ended() && current >= 0 && i > current:
		return rec.unreached(st)
	case st.Collapsed:
		// A collapsed shared step has a fold row at version 0, which is no fold; the mockup has none.
		return stepSkipped
	case rec.ended() && i == current:
		return stepStopped
	case current >= 0 && i > current && rec.pos.chain && st.Slice != "" && st.Stage.ID != chained:
		// A decomposition chain runs only the later slices' cards.
		return stepDone
	case current < 0 || i > current:
		return stepPending
	case i < current:
		if rec.folded(st.ID) {
			return stepFolded
		}

		return stepDone
	}

	if f, ok := rec.flightOf(st.ID); ok {
		switch {
		case f.kind == store.RunKindReviewer:
			return stepReviewing
		// A responder run outside its stage's run answers a handed-over
		// round, and reads as the run that asked it would.
		case f.kind == store.RunKindStage && f.round > 1, f.kind == store.RunKindResponder && rec.handedRound > 1:
			return stepRevising
		default:
			return stepRunning
		}
	}

	// A mockup the auto responder steers waits for nobody, so a failure there
	// is the step's: CM reads waiting with no prompt as the responder at work.
	if (rec.round > 0 && rec.roundStage == st.ID && (!rec.steered || !rec.failed())) || rec.offer {
		return stepWaiting
	}

	switch {
	case rec.pos.gate && !rec.pos.critique:
		return stepWaiting
	case rec.failed():
		return stepFailed
	case rec.pos.gate && rec.pos.critique:
		// A review or a builder round comes next.
		return stepReviewing
	default:
		return stepPending
	}
}

// loopRound is a loop stage's round since the sign-off's latest rejection,
// counting a run of it in flight.
func (rec record) loopRound(st pipelineStep) int {
	latest, base := rec.latest(st.ID), 0

	for _, g := range rec.gates[signoffStage] {
		if g.Decision == store.GateReject && g.Version <= latest && g.Version > base {
			base = g.Version
		}
	}

	round := latest - base

	if f, ok := rec.flightOf(st.ID); ok && f.kind == store.RunKindStage {
		round++
	}

	return round
}

// signoffStep is the sign-off: pending until the pipeline ends ready,
// not reached when it ends otherwise, done when one was shown before that
// end, then running until shown, waiting while shown and undecided, and
// done once approved.
func (rec record) signoffStep() protocol.PipelineStep {
	step := protocol.PipelineStep{Stage: signoffStage, Gate: true, Policy: pack.PolicyHuman, Status: stepPending}

	if emitsOnItsOwn(rec.bp.Mode) {
		step.Policy = pack.PolicyAuto
	}

	if n := len(rec.signoffs); n > 0 {
		step.LatestVersion = signoffVersion(rec.signoffs[n-1])
	}

	switch {
	case !rec.pos.end:
	case !rec.pos.ready && len(rec.signoffs) > 0:
		// A sign-off shown before the end holds its versions.
		step.Status = stepDone
	case !rec.pos.ready:
		step.Status = stepNotReached
	default:
		v := rec.pos.art.Version

		g, decided := rec.decision(signoffStage, v)

		switch {
		case decided && g.Decision == store.GateApprove:
			step.Status = stepDone
		case rec.signoffRecorded(v):
			step.Status = stepWaiting
		case rec.bp.State == string(StateFailed):
			step.Status = stepFailed
		default:
			step.Status = stepRunning
		}
	}

	return step
}

// emissionStep is the board: done once the cards exist, running while they
// are created, waiting when an approved emission stopped, not reached when the
// pipeline ended before the sign-off.
func (rec record) emissionStep() protocol.PipelineStep {
	step := protocol.PipelineStep{Stage: emissionStage, Status: stepPending}

	approved := false

	if rec.pos.end && rec.pos.ready {
		g, decided := rec.decision(signoffStage, rec.pos.art.Version)
		approved = decided && g.Decision == store.GateApprove
	}

	switch {
	case rec.emitted != nil:
		step.Status, step.LatestVersion = stepDone, 1
	case rec.pos.end && !rec.pos.ready:
		step.Status = stepNotReached
	case rec.bp.State == string(StateEmitting):
		step.Status = stepRunning
	case approved && rec.bp.State == string(StateFailed):
		step.Status = stepFailed
	case approved:
		step.Status = stepWaiting
	}

	return step
}

// current is the innermost live run: a responder inside the stage that
// asked, else the run itself. A retro run is none: the view's current run
// names a pipeline stage, and a pass runs alone on the slot.
func (rec record) current() *protocol.CurrentRun {
	live := slices.DeleteFunc(rec.live(), func(f flightView) bool { return f.kind == store.RunKindRetro })
	if len(live) == 0 {
		return nil
	}

	f := live[len(live)-1]
	stage, slice := pack.SplitStep(f.stage)

	return &protocol.CurrentRun{
		Stage: stage, Slice: slice, Kind: f.kind, Persona: f.persona, Model: f.model, StartedAt: f.startedAt,
		Turns: f.turns, ToolCalls: f.toolCalls, Tokens: f.tokens,
	}
}

// open is the prompt that waits for the operator: the open round, the
// mockup waiting for the person, the mockup offer, a shown gate or the
// shown sign-off, undecided, or a stopped emission or run start, which
// approve resumes.
func (rec record) open() *protocol.OpenPrompt {
	if rec.waitsToResume() {
		return &protocol.OpenPrompt{PromptID: fmt.Sprintf("emission:v%d", signoffVersion(*rec.resume)), Kind: promptGate}
	}

	switch {
	case rec.unprompted:
		return nil
	case rec.mockup:
		return &protocol.OpenPrompt{PromptID: mockupPromptID(rec.round), Kind: promptMockup}
	case rec.round > 0:
		return &protocol.OpenPrompt{PromptID: roundPromptID(rec.round), Kind: promptRound}
	case rec.offer:
		return &protocol.OpenPrompt{PromptID: offerPromptID, Kind: promptGate}
	case rec.pos.gate && !rec.pos.critique && rec.shown[rec.pos.stage][rec.pos.art.Version]:
		return &protocol.OpenPrompt{PromptID: GatePromptID(rec.pos.stage, rec.pos.art.Version), Kind: promptGate}
	case rec.pos.end && rec.pos.ready && rec.signoffRecorded(rec.pos.art.Version):
		if _, decided := rec.decision(signoffStage, rec.pos.art.Version); !decided {
			return &protocol.OpenPrompt{PromptID: fmt.Sprintf("gate:signoff:v%d", rec.pos.art.Version), Kind: promptGate}
		}
	}

	return nil
}

// waitsToResume reports a stopped emission or run start still open: no
// emission or run start came after it, and nothing is emitting or running
// on an approve already given.
func (rec record) waitsToResume() bool {
	return rec.resume != nil && rec.resume.ID > rec.resumed &&
		rec.bp.State != string(StateEmitting) && rec.bp.State != string(StateRunning)
}

// cost is the ledger plus the live runs. The run figures are the outermost
// live run's: its cap, and what it and the runs inside it spent against it.
func (rec record) cost() protocol.BlueprintCost {
	out := protocol.BlueprintCost{CapUSD: rec.bp.CostCapUSD, Priced: !rec.unpriced}

	for _, r := range rec.runs {
		out.SpentUSD += r.Usage.CostUSD
	}

	live := rec.live()

	for i, f := range live {
		out.SpentUSD += f.spent
		out.RunSpentUSD += f.spent

		if i == 0 {
			out.RunSpentUSD += f.charged
			out.RunCapUSD = f.capUSD
		}

		if !f.priced {
			out.Priced = false
		}
	}

	return out
}

// artifactRefs lists every version of every step's artifact in pipeline
// order, then those of slices the newest slicing no longer has, then the
// sign-offs shown and the board. A collapsed stage's versions, and a
// dropped slice's, are superseded.
func (c *Controller) artifactRefs(rec record) []protocol.ArtifactRef {
	refs := make([]protocol.ArtifactRef, 0)
	listed := map[string]bool{}

	for _, st := range rec.steps {
		listed[st.ID] = true
		refs = append(refs, rec.stepRefs(st, st.Collapsed)...)
	}

	for _, key := range rec.droppedKeys(c.opts.Pack, listed) {
		if st, ok := c.stepAt(key); ok {
			refs = append(refs, rec.stepRefs(st, true)...)
		}
	}

	refs = append(refs, rec.signoffRefs()...)

	if rec.emitted != nil {
		refs = append(refs, protocol.ArtifactRef{Stage: emissionStage, Version: 1, CreatedAt: rec.emitted.Time, Status: artCurrent})
	}

	return refs
}

// stepRefs lists every version of st's artifact, oldest first; superseded
// marks them all superseded.
func (rec record) stepRefs(st pipelineStep, superseded bool) []protocol.ArtifactRef {
	arts := rec.arts[st.ID]
	out := make([]protocol.ArtifactRef, 0, len(arts))

	for i, a := range arts {
		ref := protocol.ArtifactRef{Stage: st.Stage.ID, Slice: st.Slice, Version: a.Version, CreatedAt: a.CreatedAt}
		rec.decide(&ref, st.ID, i == len(arts)-1)

		if superseded {
			ref.Status = artSuperseded
		}

		ref.Review = rec.reviewOf(st, a.Version)
		out = append(out, ref)
	}

	return out
}

// droppedKeys lists the per-slice step keys the record holds artifacts for
// that are not listed: slices an older slicing had. In pack order, then
// slice order.
func (rec record) droppedKeys(pk *pack.Pack, listed map[string]bool) []string {
	var out []string

	for _, st := range pk.Stages {
		if !st.PerSlice {
			continue
		}

		var keys []string

		for key := range rec.arts {
			// A reviewer's verdicts are keyed <key>.review and travel with their step.
			id, slice := pack.SplitStep(key)
			if id == st.ID && slice != "" && !strings.Contains(slice, ".") && !listed[key] {
				keys = append(keys, key)
			}
		}

		slices.SortFunc(keys, func(a, b string) int {
			return cmp.Or(cmp.Compare(sliceNumber(a), sliceNumber(b)), strings.Compare(a, b))
		})
		out = append(out, keys...)
	}

	return out
}

// sliceNumber is n of a step key's slice SL<n>; 0 when it has none.
func sliceNumber(key string) int {
	_, slice := pack.SplitStep(key)
	n, _ := strconv.Atoi(strings.TrimPrefix(slice, "SL"))

	return n
}

// decide sets the status of a version of key's artifact and who decided
// its gate: a rejection stays rejected, an older version or one a gates row
// records superseded is superseded, and the newest is approved, folded, at
// its gate, being reviewed or current. A failed review or builder round
// leaves it current: the version stands unjudged.
func (rec record) decide(ref *protocol.ArtifactRef, key string, newest bool) {
	g, found := rec.decision(key, ref.Version)
	if found && (g.Decision == store.GateApprove || g.Decision == store.GateReject) {
		at := g.DecidedAt
		ref.DecidedAt, ref.DecidedBy = &at, decider(g)
	}

	gated := rec.pos.gate && rec.pos.stage == key

	switch {
	case found && g.Decision == store.GateReject:
		ref.Status = artRejected
	case !newest || rec.supersededByGate(key, ref.Version):
		ref.Status = artSuperseded
	case found && g.Decision == store.GateApprove:
		ref.Status = artApproved
	case found && g.Decision == store.GateFold:
		ref.Status = artFolded
	case gated && rec.pos.critique && !rec.failed():
		ref.Status = artReviewing
	case gated && !rec.pos.critique && rec.pos.art.Version == ref.Version:
		ref.Status = artAtGate
	default:
		ref.Status = artCurrent
	}
}

// decider names who decided g: you, or automatically, for a stage gate the
// auto policy approved on its reviewer's verdict and for a sign-off an
// emitting mode approved.
func decider(g store.Gate) string {
	if g.Responder == store.ResponderHuman {
		return byYou
	}

	return byAutomatic
}

// sentBack reports whether v sent its version of st back for a revision
// in the same attempt. Artifacts carry no attempt, but an attempt ends only
// at a gate, so a later version is a revision exactly when no gate opened
// or decided on this one; without one, a verdict that does not settle is
// about to be answered.
func (rec record) sentBack(st pipelineStep, v review) bool {
	if rec.gated(st.ID, v.Version) {
		return false
	}

	return rec.latest(st.ID) > v.Version || !settled(st.Stage, v)
}

// gated reports whether key's version reached a gate: shown, or decided.
func (rec record) gated(key string, version int) bool {
	return rec.shown[key][version] || slices.ContainsFunc(rec.gates[key], func(g store.Gate) bool { return g.Version == version })
}

// reviewOf is the reviewer's verdict on st's version: each finding still
// open sent to the next version when the verdict sent the builder back (see
// sentBack), and addressed in it, with its note, only when that version
// listed its id. Each review lists every finding its round judged; an earlier
// version's review is never rewritten.
func (rec record) reviewOf(st pipelineStep, version int) *protocol.ArtifactReview {
	if st.Reviewer == nil {
		return nil
	}

	var (
		v     review
		found bool
	)

	for _, a := range rec.arts[reviewStage(st.ID)] {
		var r review
		if json.Unmarshal(a.Data, &r) == nil && r.Version == version {
			v, found = r, true
		}
	}

	if !found {
		return nil
	}

	threshold := st.Reviewer.Threshold

	// Passed is derived, never read from the stored verdict. A verdict stored
	// without its round was its attempt's first.
	out := &protocol.ArtifactReview{
		Reviewer: st.Reviewer.Persona, Score: v.Score, Threshold: threshold,
		Passed: v.passes(threshold), Round: cmp.Or(v.Round, 1),
		MaxRounds: maxReviewRounds, Summary: v.Summary, SentBack: rec.sentBack(st, v),
		Findings: make([]protocol.ReviewFinding, 0, len(v.Findings)),
	}

	for _, f := range v.Findings {
		out.Findings = append(out.Findings, protocol.ReviewFinding{
			ID: f.ID, Severity: f.Severity, Text: f.Text, Status: f.Status, VerifiedIn: f.VerifiedIn, ReviewerNote: f.Note,
		})
	}

	if !out.SentBack {
		return out
	}

	// The findings still open went to the next version, which may list them
	// as addressed, by id.
	sent := version + 1

	var claims map[string]string

	for _, a := range rec.arts[st.ID] {
		if a.Version == sent {
			claims = addressedClaims(a)
		}
	}

	for i, f := range v.Findings {
		if f.Status == findingFixed {
			continue
		}

		out.Findings[i].SentTo = sent

		if note, ok := claims[f.ID]; ok {
			out.Findings[i].AddressedIn, out.Findings[i].AddressedBy, out.Findings[i].AddressedNote = sent, st.Persona, note
		}
	}

	return out
}

// signoffRefs is one ref per sign-off version shown or recorded as approved
// automatically, dated when first shown or recorded.
func (rec record) signoffRefs() []protocol.ArtifactRef {
	var (
		order []int
		first = map[int]store.Event{}
	)

	for _, e := range rec.signoffs {
		v := signoffVersion(e)
		if _, seen := first[v]; !seen {
			order = append(order, v)
			first[v] = e
		}
	}

	refs := make([]protocol.ArtifactRef, 0, len(order))

	for i, v := range order {
		ref := protocol.ArtifactRef{Stage: signoffStage, Version: v, CreatedAt: first[v].Time}

		g, found := rec.decision(signoffStage, v)
		if found {
			at := g.DecidedAt
			ref.DecidedAt, ref.DecidedBy = &at, decider(g)
		}

		switch {
		case found && g.Decision == store.GateReject:
			ref.Status = artRejected
		case i < len(order)-1:
			ref.Status = artSuperseded
		case found && g.Decision == store.GateApprove:
			ref.Status = artApproved
		case rec.pos.end && rec.pos.ready && rec.pos.art.Version == v:
			ref.Status = artAtGate
		default:
			ref.Status = artCurrent
		}

		refs = append(refs, ref)
	}

	return refs
}
