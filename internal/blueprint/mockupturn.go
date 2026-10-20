package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// maxMockupRequests is how many change requests the auto responder may send
// on one mockup; once they are spent its turn must use the revision.
const maxMockupRequests = 5

// reasonResponderFailed is the reason of the error a mockup turn records when
// its responder failed or a cost cap refused it; the revision is then used
// as it is set.
const reasonResponderFailed = "responder_failed"

// fromResponder is the author of the auto responder's chat entries on a
// mockup.
const fromResponder = "responder"

// reaction is the auto responder's verdict on a revision: the settings it
// flipped to, and either the one change it asks for or the use of the
// revision as it set it.
type reaction struct {
	Settings map[string]string
	Request  string
	Use      bool
}

// parseReaction reads a mockup responder's submit against the suggestions
// of the revision it judged: exactly one of a request, refused once left is
// 0, or use: true, with settings that name only the revision's suggestions
// and values they take.
func parseReaction(data map[string]any, sugs []stage.MockupSuggestion, left int) (reaction, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return reaction{}, fmt.Errorf("encode the reaction: %w", err)
	}

	var in struct {
		Settings map[string]any `json:"settings"`
		Request  string         `json:"request"`
		Use      bool           `json:"use"`
	}

	if err := json.Unmarshal(raw, &in); err != nil {
		return reaction{}, fmt.Errorf("the reaction does not decode: %w", err)
	}

	out := reaction{Request: strings.TrimSpace(in.Request), Use: in.Use}

	switch {
	case out.Request != "" && out.Use:
		return reaction{}, errors.New("send either request or use: true, not both")
	case out.Request == "" && !out.Use:
		return reaction{}, errors.New("send request, the one change you want next, or use: true to use the revision as you set it")
	case out.Request != "" && left <= 0:
		return reaction{}, fmt.Errorf("every one of the %d change requests a mockup takes is spent; send use: true", maxMockupRequests)
	}

	for _, id := range slices.Sorted(maps.Keys(in.Settings)) {
		i := slices.IndexFunc(sugs, func(s stage.MockupSuggestion) bool { return s.ID == id })
		if i < 0 {
			return reaction{}, fmt.Errorf("settings.%s: the revision has no such suggestion", id)
		}

		v, ok := in.Settings[id].(string)
		if !ok || !sugs[i].Accepts(v) {
			return reaction{}, fmt.Errorf("settings.%s takes one of %s", id, strings.Join(suggestionValues(sugs[i]), ", "))
		}

		if out.Settings == nil {
			out.Settings = map[string]string{}
		}

		out.Settings[id] = v
	}

	return out, nil
}

// suggestionValues are the values sug takes.
func suggestionValues(sug stage.MockupSuggestion) []string {
	if sug.Kind == stage.MockupSwitch {
		return []string{"off", "on"}
	}

	out := make([]string, 0, len(sug.Options))
	for _, o := range sug.Options {
		out = append(out, o.Value)
	}

	return out
}

// turnDue reports whether mockup round s waits for a turn of the auto
// responder: open, the responder's, undecided, and with no chat its
// designer has not seen.
func turnDue(s roundState) bool {
	return s.Mockup != nil && !s.Frame.Closed && s.Frame.Responder && s.Mockup.Action == "" && !s.chatPending()
}

// mockupTurn is one turn of the auto responder on the mockup round the
// pipeline stands at, steering it in the person's place: one responder run
// flips the settings and asks the designer for one change, which a chat run
// answers, or uses the revision as it set it. A responder that fails, or a
// cost cap refuses, uses the revision with the last known settings, marked
// failed, and the turn says so in an error event. A turn that finds the
// person taking the steering back (handsBack) runs nothing and hands the
// mockup back; one that finds it as it ends stores no use, its own or a
// failure's, and hands the mockup back with the settings the responder sent,
// while its request stands. stageAsked hands the round to nextSegment; a
// cancel leaves the blueprint idle, and a turn that cannot be read or stored
// fails it. The caller holds the run slot.
func (c *Controller) mockupTurn(ctx context.Context, r *run, sess *session, bp store.Blueprint) stageEnd {
	bg, au, id := context.WithoutCancel(ctx), sess.audit, bp.ID
	lock := c.roundLock(id)

	lock.Lock()
	s, due, err := c.turnStart(bg, au, bp)
	lock.Unlock()

	switch {
	case err != nil:
		c.fail(bg, au, id, 0, err.Error())

		return stageStopped
	case !due:
		return stageAsked
	}

	st, ok := c.stepAt(s.Frame.Stage)
	if !ok {
		c.fail(bg, au, id, 0, fmt.Sprintf("unknown stage %q", s.Frame.Stage))

		return stageStopped
	}

	m := s.Mockup
	left := maxMockupRequests - m.Requests

	task, err := c.mockupResponderTask(bg, bp, st, s)
	if err != nil {
		c.fail(bg, au, id, 0, err.Error())

		return stageStopped
	}

	out, err := c.runSide(ctx, r, sess, bp, sideRun{
		kind: store.RunKindResponder, stage: st.ID, persona: st.Persona, attempt: s.Attempt, round: m.Revision,
		in: stage.Input{
			Stage: st.ID + ".responder", Persona: c.opts.Pack.Persona(st.Persona),
			Instructions: withPlain(bp, c.opts.Pack.MockupResponderInstructions(), c.opts.Pack.PlainInstructions()), Task: task,
			Validate: func(_ context.Context, a stage.Artifact) error {
				_, err := parseReaction(a.Data, m.Suggestions, left)

				return err
			},
		},
	})

	if ctx.Err() != nil {
		// A turn Shutdown cut short writes nothing, as a stage run's.
		if !c.shuttingDown(r) {
			c.setState(bg, id, StateIdle)
		}

		return stageStopped
	}

	var re reaction
	if err == nil {
		re, err = parseReaction(out.Artifact.Data, m.Suggestions, left)
	}

	lock.Lock()
	defer lock.Unlock()

	cur, ok := c.newestRound(bg, id)
	if !ok || cur.Number != s.Number || !turnDue(cur) || cur.Mockup.Revision != m.Revision {
		return stageAsked
	}

	next := cur.clone()
	next.MessageID, next.Dropped = "", 0
	sugs := cur.Mockup.Suggestions

	// A switch to default while the turn ran takes the steering back, so the
	// use is the person's to make, from the settings the responder reached. A
	// request stands: the revision that answers it is the person's.
	if err != nil || re.Use {
		back, berr := c.handsBack(bg, bp, cur.Frame.Stage)
		if berr != nil {
			c.fail(bg, au, id, 0, berr.Error())

			return stageStopped
		}

		if back {
			if err == nil {
				next.Mockup.Settings, _ = cleanSettings(sugs, re.Settings, cur.Mockup.Settings)
			}

			if berr := c.handBack(bg, au, id, next); berr != nil {
				c.fail(bg, au, id, 0, berr.Error())

				return stageStopped
			}

			return stageAsked
		}
	}

	switch {
	case err != nil:
		next.Change = changeChosen
		next.Mockup.Action, next.Mockup.By, next.Mockup.Failed = protocol.MockupUse, store.ResponderAuto, true
		next.Mockup.Chosen, _ = cleanSettings(sugs, nil, cur.Mockup.Settings)

		au.event(bg, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
			"stage": st.ID, "reason": reasonResponderFailed, "revision": m.Revision,
			"message": fmt.Sprintf("The auto responder could not judge revision %d of the mockup (%v), so the architect uses it as it is set.", m.Revision, err),
		}})
	case re.Use:
		next.Change = changeChosen
		next.Mockup.Action, next.Mockup.By = protocol.MockupUse, store.ResponderAuto
		next.Mockup.Chosen, _ = cleanSettings(sugs, re.Settings, cur.Mockup.Settings)
	default:
		next.Change = changeChat
		next.Frame.Chat = append(next.Frame.Chat, protocol.RoundChat{From: fromResponder, Text: re.Request})
		next.Mockup.Settings, _ = cleanSettings(sugs, re.Settings, cur.Mockup.Settings)
		next.Mockup.Requests++
	}

	c.putRound(bg, au, id, 0, next)

	if !c.landed(bg, id, next) {
		c.fail(bg, au, id, 0, fmt.Sprintf("the auto responder's turn on revision %d of the mockup could not be stored", m.Revision))

		return stageStopped
	}

	return stageAsked
}

// turnStart reads the mockup round the pipeline stands at for a turn: due is
// false when no turn is due, and when the person takes the steering back
// (handsBack), in which case the mockup goes back to them first. The caller
// holds the round lock.
func (c *Controller) turnStart(ctx context.Context, au audit, bp store.Blueprint) (roundState, bool, error) {
	place, found, err := c.roundAt(ctx, bp.ID)
	if err != nil || !found || !turnDue(place.state) {
		return roundState{}, false, err
	}

	s := place.state

	back, err := c.handsBack(ctx, bp, s.Frame.Stage)

	switch {
	case err != nil:
		return roundState{}, false, err
	case !back:
		return s, true, nil
	}

	if err := c.handBack(ctx, au, bp.ID, s); err != nil {
		return roundState{}, false, err
	}

	return roundState{}, false, nil
}

// handsBack reports whether the person takes back the steering of a mockup
// of stageID that the auto responder holds: the blueprint is in default mode
// and the stage's policy is not auto. Only a mode switch hands one back: in
// another mode a gate reply acts from the next revision, as an open round
// keeps its responder. The caller holds the round lock, as a revision reads
// the policy under it, so a turn never runs, or ends, on a switch to default
// it raced.
func (c *Controller) handsBack(ctx context.Context, bp store.Blueprint, stageID string) (bool, error) {
	policy, err := c.policy(ctx, bp, stageID)
	if err != nil || policy == pack.PolicyAuto {
		return false, err
	}

	cur, err := c.opts.Store.GetBlueprint(ctx, bp.ID)
	if err != nil {
		return false, fmt.Errorf("blueprint: read the mode: %w", err)
	}

	return cur.Mode == "" || cur.Mode == protocol.ArchitectModeDefault, nil
}

// handBack stores mockup round s given back to the person, so the open
// prompt mockup:<n> returns with its use and skip. The caller holds the round
// lock.
func (c *Controller) handBack(ctx context.Context, au audit, id string, s roundState) error {
	back := s.clone()
	back.Frame.Responder, back.HandedOver = false, nil
	back.Change, back.MessageID, back.Dropped = changeHandedBack, "", 0
	c.putRound(ctx, au, id, 0, back)

	if !c.landed(ctx, id, back) {
		return errors.New("the mockup could not be handed back to the person")
	}

	return nil
}

// landed reports whether snapshot next of a turn is its round's newest. A
// store that failed leaves the turn due, and another pass would spin
// without a model call to slow it when a cap refuses the responder.
func (c *Controller) landed(ctx context.Context, id string, next roundState) bool {
	cur, ok := c.newestRound(ctx, id)

	return ok && cur.Number == next.Number && cur.Change == next.Change &&
		cur.Frame.Responder == next.Frame.Responder && len(cur.Frame.Chat) == len(next.Frame.Chat)
}

// mockupResponderTask is what the auto responder reads on its turn: the
// target projects, the need, the mockup step's inputs and the user's notes,
// then the revision mockup round s shows: its notes, its screens, its
// suggestions with the values each takes and how it is set now (the last
// known settings), its source, the conversation about the mockup so far and
// the change requests left.
func (c *Controller) mockupResponderTask(ctx context.Context, bp store.Blueprint, st pipelineStep, s roundState) (string, error) {
	var b strings.Builder

	if err := c.writeInputs(ctx, &b, bp, st, true, "responder task"); err != nil {
		return "", err
	}

	if err := c.writeNotes(ctx, &b, bp.ID); err != nil {
		return "", err
	}

	m := s.Mockup

	art, err := c.opts.Store.GetArtifact(ctx, bp.ID, s.Frame.Stage, m.Revision)
	if err != nil {
		return "", fmt.Errorf("blueprint: responder task: read mockup revision %d: %w", m.Revision, err)
	}

	var data mockupData
	if err := json.Unmarshal(art.Data, &data); err != nil {
		return "", fmt.Errorf("blueprint: responder task: decode mockup revision %d: %w", m.Revision, err)
	}

	fmt.Fprintf(&b, "## Revision %d of the mockup\n\n%s\n\nIts screens:\n\n", m.Revision, art.Body)

	for _, sc := range data.Screens {
		fmt.Fprintf(&b, "- %s (%s)\n", sc.Title, sc.ID)
	}

	b.WriteString("\nIts suggestions, each with the values it takes and how it is set now:\n\n")

	for _, sug := range m.Suggestions {
		values := make([]string, 0, len(sug.Options))
		for _, v := range suggestionValues(sug) {
			values = append(values, fmt.Sprintf("%s (%s)", v, sug.Label(v)))
		}

		set := m.Settings[sug.ID]
		fmt.Fprintf(&b, "- %s (%s): %s\n  Takes %s. Set to %s (%s).\n", sug.Title, sug.ID, sug.Why, strings.Join(values, ", "), set, sug.Label(set))
	}

	fmt.Fprintf(&b, "\n## The source of revision %d\n\n%s\n\n", m.Revision, data.Source)

	if len(s.Frame.Chat) > 0 {
		b.WriteString("## The conversation about the mockup so far\n\n")

		for _, e := range s.Frame.Chat {
			fmt.Fprintf(&b, "- %s: %s\n", chatSpeaker(e.From), e.Text)
		}

		b.WriteString("\n")
	}

	if left := maxMockupRequests - m.Requests; left > 0 {
		fmt.Fprintf(&b, "You may send %d more change request(s). Submit your settings with request, the one change you want next, or with use: true to use this revision as you set it.\n", left)
	} else {
		b.WriteString("Every change request a mockup takes is spent. Submit your settings with use: true.\n")
	}

	return b.String(), nil
}

// chatSpeaker names who wrote a mockup chat entry, as the auto responder
// reads it.
func chatSpeaker(from string) string {
	switch from {
	case "user":
		return "the person"
	case fromResponder:
		return "you"
	default:
		return strings.ReplaceAll(from, "_", " ")
	}
}
