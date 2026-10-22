package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Commands: replies the webhook routes past Message, so they work while a
// stage runs and are never an answer.
const (
	CommandAbort  = "abort"
	CommandExport = "export"
	CommandGate   = "gate"
	CommandCap    = "cap"
	CommandMode   = "mode"
	CommandResume = "resume"
)

// commandContinue is CommandResume's synonym; ParseCommand returns
// CommandResume for it.
const commandContinue = "continue"

// capOff is the cap reply that removes the cap.
const capOff = "off"

// capAmount is a cap reply's amount: one plain decimal number, so neither an
// exponent nor a word such as "inf" reads as one.
var capAmount = regexp.MustCompile(`^-?(\d+(\.\d+)?|\.\d+)$`)

// errNoResponder tells a stage whose policy is auto, in a pack without
// stages/responder.md, to decide a question itself; ask_questions returns it
// as a tool error and the run goes on.
var errNoResponder = errors.New("nobody answers this stage's questions under its auto policy; decide yourself and, where this stage records assumptions, record the decision as one")

// ParseCommand recognises a reply that is a command rather than text for
// the blueprint: abort, export and resume (or its synonym continue) are the
// whole message, a gate reply is exactly gate, a stage and human or auto,
// a cap reply is exactly cap and an amount or off, and a mode reply exactly
// mode and one of the modes, so an answer that starts with one of these
// words stays an answer. Case is ignored; the
// arguments come back lowercased. Any other text returns "".
func ParseCommand(text string) (string, []string) {
	fields := strings.Fields(strings.ToLower(text))

	switch {
	case len(fields) == 1 && (fields[0] == CommandAbort || fields[0] == CommandExport):
		return fields[0], nil
	case len(fields) == 1 && (fields[0] == CommandResume || fields[0] == commandContinue):
		return CommandResume, nil
	case len(fields) == 3 && fields[0] == CommandGate && (fields[2] == pack.PolicyHuman || fields[2] == pack.PolicyAuto):
		return CommandGate, fields[1:]
	case len(fields) == 2 && fields[0] == CommandCap && (fields[1] == capOff || capAmount.MatchString(fields[1])):
		return CommandCap, fields[1:]
	case len(fields) == 2 && fields[0] == CommandMode && protocol.ValidArchitectMode(fields[1]):
		return CommandMode, fields[1:]
	default:
		return "", nil
	}
}

// SetPolicy records a stage's responder policy for this blueprint, for its
// questions and its gate, from the next one on: a round, gate or mockup
// offer already open keeps the responder it was put to, and an open mockup
// whoever steers it until its next revision. The sign-off follows the
// mode and an unknown stage is refused; each is reported and never stored as
// a policy. Like export, it works while a stage runs.
func (c *Controller) SetPolicy(ctx context.Context, id, stageID, policy, msgID string) error {
	c.mu.Lock()
	sess, ok := c.sessions[id]
	r := c.runs[id]
	running := r != nil

	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	payload := map[string]any{"stage": stageID, "policy": policy, "message_id": msgID, "during_run": running}

	st, known := c.stageNamed(stageID)

	switch {
	case strings.EqualFold(stageID, signoffStage):
		payload["refused"] = "The sign-off follows the mode: default and auto modes show it, auto+emit and yolo approve it unless something needs a person (preflight, a critical finding or cards not ready)."
	case !known:
		payload["refused"] = fmt.Sprintf("There is no stage %s; the stages are %s.", stageID, strings.Join(c.stageIDs(), ", "))
	default:
		payload["stage"] = st.ID

		switch open := c.openRoundAt(ctx, id, st.ID); {
		case open != "":
			payload["open"] = open
		case !running && c.gateOpenAt(ctx, id, st.ID):
			payload["open"] = "gate"
		case !running && c.offerOpenFor(ctx, id, st.ID):
			payload["open"] = "offer"
		}
	}

	sess.audit.event(ctx, Event{Kind: EventPolicy, BlueprintID: id, Payload: payload})

	return nil
}

// SetCap records the blueprint's cost cap: value is an amount in USD or
// off, as ParseCommand returns it, and off stores 0, no cap. An amount not
// above what the blueprint has spent is refused and changes nothing. Like
// a gate reply it works while a stage runs: the cap binds the runs in
// flight from their next model call, and every later run. With nothing in
// flight, a cap above the one a cap stop happened under, or no cap, runs
// the stopped stage again, once, as a retry message would but without a
// note; nothing else starts.
func (c *Controller) SetCap(ctx context.Context, id, value, msgID string) error {
	to := 0.0

	if value != capOff {
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("blueprint: cap %q: %w", value, err)
		}

		to = v
	}

	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		return fmt.Errorf("blueprint: cap: %w", err)
	}

	spent, err := c.opts.Store.BlueprintCost(ctx, id)
	if err != nil {
		return fmt.Errorf("blueprint: cap: %w", err)
	}

	payload := map[string]any{"from": bp.CostCapUSD, "to": to, "message_id": msgID}

	if value != capOff && to <= spent {
		payload["refused"] = fmt.Sprintf("the blueprint has spent $%.2f; a cap must be above that", spent)
		payload["during_run"] = c.running(id)
		sess.audit.event(ctx, Event{Kind: EventCap, BlueprintID: id, Payload: payload})

		return nil
	}

	if err := c.capOf(id).set(to, func() error { return c.opts.Store.SetBlueprintCostCap(ctx, id, to) }); err != nil {
		return fmt.Errorf("blueprint: cap: %w", err)
	}

	bp.CostCapUSD = to

	// The slot is held while the stop is read, so nothing starts between
	// the read and the resume.
	r, runCtx, claimed, busy := c.claimAttached(id, sess)
	payload["during_run"] = busy

	var at stopPoint

	if claimed {
		p, found, err := c.stoppedAt(ctx, bp)
		if err != nil {
			// The cap is stored; only the resume is lost, and a message
			// retries the stage.
			c.opts.Logger.Warn("blueprint: read the stop a cap change resumes", "blueprint", id, "error", sess.audit.text(err.Error()))
		}

		// The stop's own cap, not the one stored: a raise after its run was
		// cut is stored already, and a cap at or below the stop's stops again.
		if found && p.byCap && (to == 0 || to > p.capUSD) {
			at = p
			payload["resumes"] = p.stage
		}
	}

	sess.audit.event(ctx, Event{Kind: EventCap, BlueprintID: id, Payload: payload})

	switch {
	case at.stage != "":
		c.resume(ctx, runCtx, r, sess, bp, at)
	case claimed:
		c.release(id, r)
	}

	return nil
}

// Resume runs the stage the pipeline stands stopped at again, once, as a
// retry message would but storing no note (see stoppedAt). A cap stop is
// one too, and its run meets the cap again. While a run is in flight, or
// where nothing is stopped, nothing starts and the event says so.
func (c *Controller) Resume(ctx context.Context, id, msgID string) error {
	c.mu.Lock()

	sess, ok := c.sessions[id]
	if !ok {
		c.mu.Unlock()

		return ErrNotFound
	}

	if r := c.runs[id]; r != nil {
		running := r.stage
		c.mu.Unlock()

		sess.audit.event(ctx, nothingToResume(id, msgID, running, true))

		return nil
	}

	if c.closing {
		c.mu.Unlock()

		return ErrShuttingDown
	}

	// The slot is held while the stop is read, so nothing starts between
	// the read and the resume.
	r, runCtx, _ := c.claimLocked(id)
	c.mu.Unlock()

	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		c.release(id, r)

		return fmt.Errorf("blueprint: resume: %w", err)
	}

	p, found, err := c.stoppedAt(ctx, bp)

	switch {
	case err != nil:
		c.unreadable(ctx, sess.audit, id, err)
		c.release(id, r)
	case !found:
		sess.audit.event(ctx, nothingToResume(id, msgID, "", false))
		c.release(id, r)
	default:
		sess.audit.event(ctx, Event{Kind: EventResume, BlueprintID: id, Payload: map[string]any{"stage": p.stage, "message_id": msgID}})
		c.resume(ctx, runCtx, r, sess, bp, p)
	}

	return nil
}

// nothingToResume is the resume event when nothing waits to resume: running
// is the stage in flight, "" when none is or it has not started yet, and
// duringRun tells the panel a run goes on working after it.
func nothingToResume(id, msgID, running string, duringRun bool) Event {
	return Event{Kind: EventResume, BlueprintID: id, Payload: map[string]any{
		"nothing": true, "message_id": msgID, "running": running, "during_run": duringRun,
	}}
}

// running reports whether id's run slot is held.
func (c *Controller) running(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.runs[id] != nil
}

// stageNamed finds a pack stage by id, ignoring case: a gate reply arrives
// lowercased.
func (c *Controller) stageNamed(name string) (pack.Stage, bool) {
	for _, st := range c.opts.Pack.Stages {
		if strings.EqualFold(st.ID, name) {
			return st, true
		}
	}

	return pack.Stage{}, false
}

// gateOpenAt reports whether stageID's gate, of any slice, is open, the
// blueprint waiting on its decision; a gate whose critique is pending was
// never shown. A record that cannot be read reports no open gate and is
// logged: it only drops a hint.
func (c *Controller) gateOpenAt(ctx context.Context, id, stageID string) bool {
	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the blueprint for an open gate", "blueprint", id, "stage", stageID, "error", err)

		return false
	}

	out, _, err := c.position(ctx, bp)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the position for an open gate", "blueprint", id, "stage", stageID, "error", err)

		return false
	}

	gated, _ := pack.SplitStep(out.stage)

	return out.gate && !out.critique && gated == stageID
}

// openRoundAt is what of stageID's, of any slice, is open at its round:
// "round" for a round that waits for answers, "mockup" for a mockup, which
// is never a round to the person, and "" for none. An open round keeps the
// responder it was put to, and a mockup whoever steers it until its next
// revision. A record that cannot be read reports none and is logged: it
// only drops a hint.
func (c *Controller) openRoundAt(ctx context.Context, id, stageID string) string {
	place, found, err := c.roundAt(ctx, id)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the round for a gate reply", "blueprint", id, "stage", stageID, "error", err)

		return ""
	}

	asking, _ := pack.SplitStep(place.state.Frame.Stage)

	switch {
	case !found || place.state.Frame.Closed || asking != stageID:
		return ""
	case place.state.Mockup != nil:
		return "mockup"
	default:
		return "round"
	}
}

// stageIDs names the pack's stages in order: the stages a gate reply may name.
func (c *Controller) stageIDs() []string {
	var out []string

	for _, st := range c.opts.Pack.Stages {
		out = append(out, st.ID)
	}

	return out
}

// policy is who answers stageID's questions for this blueprint, the first of
// these that says: the blueprint's latest gate reply for the stage, a stored
// mode other than default (every policy auto), the pack's policy.
func (c *Controller) policy(ctx context.Context, bp store.Blueprint, stageID string) (string, error) {
	return c.policyFor(ctx, bp, stageID, false)
}

// gatePolicy is who decides stageID's gate for this blueprint: the same
// reply and mode as policy, then the pack's gate policy.
func (c *Controller) gatePolicy(ctx context.Context, bp store.Blueprint, stageID string) (string, error) {
	return c.policyFor(ctx, bp, stageID, true)
}

func (c *Controller) policyFor(ctx context.Context, bp store.Blueprint, stageID string, gate bool) (string, error) {
	// The stored mode, not bp's: a chain of runs holds bp from its start, and
	// a mode switch applies from the next question or gate.
	cur, err := c.opts.Store.GetBlueprint(ctx, bp.ID)
	if err != nil {
		return "", fmt.Errorf("blueprint: read the mode: %w", err)
	}

	events, err := c.opts.Store.ListEventsOfKind(ctx, bp.ID, EventPolicy)
	if err != nil {
		return "", fmt.Errorf("blueprint: read the gate replies: %w", err)
	}

	return c.resolvePolicy(cur.Mode, cur.PlainLanguage, events, stageID, gate)
}

// resolvePolicy is policy, or gatePolicy when gate is set, under mode over
// the blueprint's policy events, oldest first, so a caller resolving several
// stages reads them once. A gate reply sets both. Every mode but default makes
// both auto, and plain language makes every gate auto. A per-slice step
// resolves as its stage.
func (c *Controller) resolvePolicy(mode string, plain bool, events []store.Event, stageID string, gate bool) (string, error) {
	stageID, _ = pack.SplitStep(stageID)

	for i := len(events) - 1; i >= 0; i-- {
		var p struct {
			Stage   string `json:"stage"`
			Policy  string `json:"policy"`
			Refused string `json:"refused"`
		}

		if err := json.Unmarshal(events[i].Payload, &p); err != nil {
			return "", fmt.Errorf("blueprint: read gate reply %d: %w", events[i].ID, err)
		}

		if p.Refused == "" && p.Stage == stageID {
			return p.Policy, nil
		}
	}

	if mode != "" && mode != protocol.ArchitectModeDefault {
		return pack.PolicyAuto, nil
	}

	if gate && plain {
		return pack.PolicyAuto, nil
	}

	st, ok := c.opts.Pack.Stage(stageID)

	switch {
	case ok && gate && st.GatePolicy != "":
		return st.GatePolicy, nil
	case ok && st.Policy != "":
		return st.Policy, nil
	default:
		return pack.PolicyAuto, nil
	}
}

// postGatePlan tells the session, once the first stage has completed, who
// answers each model stage's questions and decides its gate; a gated
// stage's entry carries its gate policy, and the entries say which stages
// repeat per slice and which collapses with one slice. A policy that cannot
// be read skips the plan: it is advice, and every question and gate reads
// the policy again.
func (c *Controller) postGatePlan(ctx context.Context, sess *session, bp store.Blueprint) {
	// The stored mode, not bp's: a chain of runs holds bp from its start.
	cur, err := c.opts.Store.GetBlueprint(ctx, bp.ID)
	if err != nil {
		c.opts.Logger.Warn("blueprint: gate plan not posted", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))

		return
	}

	events, err := c.opts.Store.ListEventsOfKind(ctx, bp.ID, EventPolicy)
	if err != nil {
		c.opts.Logger.Warn("blueprint: gate plan not posted", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))

		return
	}

	steps, _, err := c.steps(ctx, bp.ID)
	if err != nil {
		c.opts.Logger.Warn("blueprint: gate plan not posted", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))

		return
	}

	stages := make([]any, 0, len(c.opts.Pack.Stages))

	for _, st := range c.opts.Pack.Stages {
		// A need without a screen never shows the mockup step.
		if i := stepIndex(steps, st.ID); i >= 0 && steps[i].Collapsed && st.ID == c.opts.Pack.MockupStage() {
			continue
		}

		policy, err := c.resolvePolicy(cur.Mode, cur.PlainLanguage, events, st.ID, false)
		if err != nil {
			c.opts.Logger.Warn("blueprint: gate plan not posted", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))

			return
		}

		entry := map[string]any{"stage": st.ID, "policy": policy, "gate": st.Gate}

		if st.Gate {
			if entry["gate_policy"], err = c.resolvePolicy(cur.Mode, cur.PlainLanguage, events, st.ID, true); err != nil {
				c.opts.Logger.Warn("blueprint: gate plan not posted", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))

				return
			}
		}

		if st.Fold != nil {
			// An entry is a pack stage, so it names the stage its fold lands
			// on, not a slice's step of it.
			if into, _ := pack.SplitStep(foldTarget(steps, st.ID)); into != "" {
				entry["folds_into"], entry["fold_field"], entry["fold_max"] = into, st.Fold.Field, st.Fold.Max
			}
		}

		if st.PerSlice {
			entry["per_slice"] = true
		}

		if st.Shared {
			entry["shared"] = true
		}

		stages = append(stages, entry)
	}

	payload := map[string]any{"stages": stages}

	if c.cardsStage() != "" {
		payload["signoff"] = pack.PolicyHuman
		if emitsOnItsOwn(cur.Mode) {
			payload["signoff"] = pack.PolicyAuto
		}
	}

	sess.audit.event(ctx, Event{Kind: EventGatePlan, BlueprintID: bp.ID, Payload: payload})
}
