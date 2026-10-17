package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// errNoResponder tells a stage whose policy is auto to decide a question
// itself; ask_human returns it as a tool error and the run goes on.
var errNoResponder = errors.New("nobody answers this stage's questions under its auto policy; decide yourself and, where this stage records assumptions, record the decision as one")

// ParseCommand recognises a reply that is a command rather than text for
// the blueprint: abort and export are the whole message, and a gate reply
// is exactly gate, a stage and human or auto, so an answer that starts with
// one of these words stays an answer. Case is ignored; the gate arguments
// come back lowercased. Any other text returns "".
func ParseCommand(text string) (string, []string) {
	fields := strings.Fields(strings.ToLower(text))

	switch {
	case len(fields) == 1 && (fields[0] == CommandAbort || fields[0] == CommandExport):
		return fields[0], nil
	case len(fields) == 3 && fields[0] == CommandGate && (fields[2] == pack.PolicyHuman || fields[2] == pack.PolicyAuto):
		return CommandGate, fields[1:]
	default:
		return "", nil
	}
}

// SetPolicy records a stage's responder policy for this blueprint, for its
// questions and its gate, from the next one on: a question or gate already
// open keeps the responder it was put to. The sign-off follows the mode, a
// human-sourced stage takes no policy and an unknown stage is refused; each
// is reported and never stored as a policy. Like export, it works while a
// stage runs.
func (c *Controller) SetPolicy(ctx context.Context, id, stageID, policy, msgID string) error {
	c.mu.Lock()
	sess, ok := c.sessions[id]
	r := c.runs[id]
	running := r != nil
	openStage := ""

	if r != nil && r.answer != nil {
		openStage = r.stage
	}

	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	payload := map[string]any{"stage": stageID, "policy": policy, "message_id": msgID, "during_run": running}

	st, known := c.stageNamed(stageID)

	switch {
	case strings.EqualFold(stageID, signoffStage):
		payload["refused"] = "The sign-off follows the mode: default and auto modes show it, auto+emit and yolo approve it when preflight finds nothing for a human."
	case !known:
		payload["refused"] = fmt.Sprintf("There is no stage %s; the stages are %s.", stageID, strings.Join(c.modelStages(), ", "))
	case st.Source == pack.SourceHuman:
		payload["refused"] = fmt.Sprintf("Stage %s is human-sourced and takes no policy; a gate reply may name %s.", stageID, strings.Join(c.modelStages(), ", "))
	default:
		payload["stage"] = st.ID

		switch {
		case openStage == st.ID:
			payload["open"] = "question"
		case !running && c.gateOpenAt(ctx, id, st.ID):
			payload["open"] = "gate"
		}
	}

	sess.audit.event(ctx, Event{Kind: EventPolicy, BlueprintID: id, Payload: payload})

	return nil
}

// stageNamed finds a pack stage by id, human-sourced ones included, ignoring
// case: a gate reply arrives lowercased.
func (c *Controller) stageNamed(name string) (pack.Stage, bool) {
	for _, st := range c.opts.Pack.Stages {
		if strings.EqualFold(st.ID, name) {
			return st, true
		}
	}

	return pack.Stage{}, false
}

// gateOpenAt reports whether stageID's gate is open, the blueprint waiting
// on its decision. A record that cannot be read reports no open gate and is
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

	return out.gate && out.stage == stageID
}

// modelStages names the pack's model stages in order: the stages a gate
// reply may name.
func (c *Controller) modelStages() []string {
	var out []string

	for _, st := range c.opts.Pack.Stages {
		if st.Source != pack.SourceHuman {
			out = append(out, st.ID)
		}
	}

	return out
}

// policy is who answers stageID's questions and decides its gate for this
// blueprint, the first of these that says: the blueprint's latest gate
// reply for the stage, a mode other than default (every policy auto), the
// pack.
func (c *Controller) policy(ctx context.Context, bp store.Blueprint, stageID string) (string, error) {
	events, err := c.opts.Store.ListEventsOfKind(ctx, bp.ID, EventPolicy)
	if err != nil {
		return "", fmt.Errorf("blueprint: read the gate replies: %w", err)
	}

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

	if bp.Mode != "" && bp.Mode != protocol.ArchitectModeDefault {
		return pack.PolicyAuto, nil
	}

	if st, ok := c.opts.Pack.Stage(stageID); ok && st.Policy != "" {
		return st.Policy, nil
	}

	return pack.PolicyAuto, nil
}

// postGatePlan tells the session, once the first stage has completed, who
// answers each model stage's questions and decides its gate. A policy that
// cannot be read skips the plan: it is advice, and every question and gate
// reads the policy again.
func (c *Controller) postGatePlan(ctx context.Context, sess *session, bp store.Blueprint) {
	stages := make([]any, 0, len(c.opts.Pack.Stages))

	for _, st := range c.opts.Pack.Stages {
		if st.Source == pack.SourceHuman {
			continue
		}

		policy, err := c.policy(ctx, bp, st.ID)
		if err != nil {
			c.opts.Logger.Warn("blueprint: gate plan not posted", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))

			return
		}

		stages = append(stages, map[string]any{"stage": st.ID, "policy": policy, "gate": st.Gate})
	}

	payload := map[string]any{"stages": stages}

	if c.cardsStage() != "" {
		payload["signoff"] = pack.PolicyHuman
		if emitsOnItsOwn(bp.Mode) {
			payload["signoff"] = pack.PolicyAuto
		}
	}

	sess.audit.event(ctx, Event{Kind: EventGatePlan, BlueprintID: bp.ID, Payload: payload})
}
