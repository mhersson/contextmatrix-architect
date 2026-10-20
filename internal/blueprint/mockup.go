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

// mockupState is a mockup round's design: the revision it shows, that
// revision's suggestions, the settings it opened with (Opening) and the
// last ones the architect knew (Settings), the auto responder's change
// requests so far, and once decided the action (use or skip), the chosen
// settings, who decided and the reply's message id. Failed marks a use the
// auto responder never made: it failed, and the revision is used as set.
type mockupState struct {
	Revision    int                      `json:"revision"`
	Suggestions []stage.MockupSuggestion `json:"suggestions"`
	Opening     map[string]string        `json:"opening"`
	Settings    map[string]string        `json:"settings"`
	Requests    int                      `json:"requests,omitempty"`
	Action      string                   `json:"action,omitempty"`
	Chosen      map[string]string        `json:"chosen,omitempty"`
	By          string                   `json:"by,omitempty"`
	MessageID   string                   `json:"message_id,omitempty"`
	Failed      bool                     `json:"failed,omitempty"`
}

// mockupData is a revision's artifact data, in the wire's shape: chosen is
// never stored, the artifact read adds it.
type mockupData struct {
	Revision    int                      `json:"revision"`
	Screens     []stage.MockupScreen     `json:"screens"`
	Suggestions []stage.MockupSuggestion `json:"suggestions"`
	Settings    map[string]string        `json:"settings"`
	Source      string                   `json:"source"`
	Chosen      map[string]string        `json:"chosen,omitempty"`
}

// fields is d as artifact data.
func (d mockupData) fields() map[string]any {
	raw, _ := json.Marshal(d) // A mockupData is plain data and always encodes.

	var out map[string]any

	_ = json.Unmarshal(raw, &out)

	return out
}

// mockupPromptID is the open prompt of mockup round n while it waits for the
// person; it stays the same across the round's revisions.
func mockupPromptID(n int) string { return fmt.Sprintf("mockup:%d", n) }

// How a mockup was decided, as its settle run's reason records it.
const (
	mockupUsed    = "used"
	mockupSkipped = "skipped"
)

// reasonStaleRevision is the reason of the error a use or a skip that names
// a revision other than the newest gets, and reasonEmptyChat that of a chat
// with no text; nothing is stored.
const (
	reasonStaleRevision = "stale_revision"
	reasonEmptyChat     = "empty_chat"
)

// changeChosen: the person's use or skip, stored on the mockup round.
const changeChosen = "chosen"

// carriedSettings is what a revision with suggestions sugs opens with: for
// each suggestion its value in last while sugs still takes it, else its
// default.
func carriedSettings(sugs []stage.MockupSuggestion, last map[string]string) map[string]string {
	out := make(map[string]string, len(sugs))

	for _, s := range sugs {
		v, ok := last[s.ID]
		if !ok || !s.Accepts(v) {
			v = s.Default
		}

		out[s.ID] = v
	}

	return out
}

// cleanSettings is given as far as sugs takes it: each suggestion keeps its
// given value while valid and takes fallback's (its default when fallback
// has none valid) otherwise. dropped counts the given entries left out, an
// id sugs lacks or a value its suggestion does not take.
func cleanSettings(sugs []stage.MockupSuggestion, given, fallback map[string]string) (map[string]string, int) {
	out := carriedSettings(sugs, fallback)
	dropped := 0

	for id, v := range given {
		i := slices.IndexFunc(sugs, func(s stage.MockupSuggestion) bool { return s.ID == id })
		if i < 0 || !sugs[i].Accepts(v) {
			dropped++

			continue
		}

		out[id] = v
	}

	return out, dropped
}

// Mockup takes the person's reply to the mockup waiting for them: a use or a
// skip of the newest revision, stored on the mockup round, which then closes
// and settles, or chat about it, which a designer's chat run answers. A use
// keeps its settings as far as the revision takes them; an id it leaves out
// takes the revision's opening value. A reply for a mockup that does not
// wait for the person, or a use or skip of an older revision, stores
// nothing and says so; so does chat with no text. Like a round reply it
// works while a chat run is in flight, so a use wins over the revision that
// run is writing.
func (c *Controller) Mockup(ctx context.Context, id string, mr protocol.MockupReply, content, msgID string) error {
	switch mr.Action {
	case protocol.MockupUse, protocol.MockupSkip, protocol.MockupChat:
	default:
		return fmt.Errorf("blueprint: mockup reply: unknown action %q", mr.Action)
	}

	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	lock := c.roundLock(id)
	lock.Lock()

	r, runCtx, err := c.storeMockup(ctx, sess, id, mr, strings.TrimSpace(content), msgID)

	lock.Unlock()

	if r != nil {
		c.settleOn(runCtx, r, id)
	}

	return err
}

// storeMockup stores mr on the mockup round waiting for the person and
// pokes, or reports why it stores nothing. The caller holds id's round lock
// and settles the slot poke claimed, if any, once it has released the lock.
// A chat run in flight does not make the mockup stale: what it then shows is
// refused once a use or skip is stored.
func (c *Controller) storeMockup(ctx context.Context, sess *session, id string, mr protocol.MockupReply, content, msgID string) (*run, context.Context, error) {
	place, found, err := c.roundAt(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	s := place.state
	if !found || s.Mockup == nil || s.Frame.Closed || s.Frame.Responder || s.Mockup.Action != "" || mr.PromptID != mockupPromptID(s.Number) {
		c.staleReply(ctx, sess, id, mr.PromptID, msgID)

		return nil, nil, nil
	}

	next := s.clone()
	next.MessageID, next.Dropped = msgID, 0

	switch mr.Action {
	case protocol.MockupChat:
		if content == "" {
			sess.audit.event(ctx, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
				"reason": reasonEmptyChat, "prompt_id": mr.PromptID, "message_id": msgID, "during_run": c.running(id),
				"message": "That message has no text; nothing was stored.",
			}})

			return nil, nil, nil
		}

		next.Frame.Chat = append(next.Frame.Chat, protocol.RoundChat{From: "user", Text: content})
		next.Change = changeChat

		if mr.Settings != nil {
			next.Mockup.Settings, next.Dropped = cleanSettings(s.Mockup.Suggestions, mr.Settings, s.Mockup.Settings)
		}
	default:
		if mr.Revision != s.Mockup.Revision {
			c.staleRevision(ctx, sess, id, mr, msgID, s.Mockup.Revision)

			return nil, nil, nil
		}

		next.Change = changeChosen
		next.Mockup.Action, next.Mockup.By, next.Mockup.MessageID = mr.Action, store.ResponderHuman, msgID

		if mr.Action == protocol.MockupUse {
			next.Mockup.Chosen, next.Dropped = cleanSettings(s.Mockup.Suggestions, mr.Settings, s.Mockup.Opening)
		}
	}

	c.putRound(ctx, sess.audit, id, 0, next)

	r, runCtx := c.poke(id)

	return r, runCtx, nil
}

// staleRevision reports a use or skip of revision mr.Revision while newest is
// the one shown; the mockup stays open.
func (c *Controller) staleRevision(ctx context.Context, sess *session, id string, mr protocol.MockupReply, msgID string, newest int) {
	sess.audit.event(ctx, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
		"reason": reasonStaleRevision, "prompt_id": mr.PromptID, "message_id": msgID, "revision": newest, "during_run": c.running(id),
		"message": fmt.Sprintf("That reply is for revision %d, but revision %d is the newest; nothing was stored. Look at revision %d, then use or skip it.", mr.Revision, newest, newest),
	}})
}

// closeMockup records a decided mockup round: on a use, one questions row
// per suggestion of the revision used, its title the question, its labels
// the options (Off and On for a switch), its why the context and the chosen
// label the answer, numbered after every question so far; then the closed
// snapshot. A skip writes no rows. Rows a close cut short already wrote are
// kept, and the rest follow them.
func (c *Controller) closeMockup(ctx context.Context, au audit, bp store.Blueprint, s roundState) (roundState, error) {
	m := s.Mockup

	if m.Action == protocol.MockupUse {
		written, err := c.opts.Store.ListQuestions(ctx, bp.ID)
		if err != nil {
			return roundState{}, fmt.Errorf("blueprint: close the mockup: %w", err)
		}

		// The round's rows are written in suggestion order, so those that
		// exist are the first ones.
		have := 0

		for _, q := range written {
			if q.Stage == s.Frame.Stage && q.RunID == s.PostedBy {
				have++
			}
		}

		number, err := c.nextQuestionNumber(ctx, bp.ID)
		if err != nil {
			return roundState{}, fmt.Errorf("blueprint: close the mockup: %w", err)
		}

		for _, sug := range m.Suggestions[min(have, len(m.Suggestions)):] {
			if err := c.opts.Store.AddQuestion(ctx, store.Question{
				BlueprintID: bp.ID, RunID: s.PostedBy, Stage: s.Frame.Stage, Number: number,
				Text: au.text(sug.Title), Options: au.texts(suggestionLabels(sug)), Context: au.text(sug.Why),
				Answer: au.text(sug.Label(m.Chosen[sug.ID])), Responder: m.By,
				Assumption: m.By == store.ResponderAuto, MessageID: m.MessageID, AnsweredAt: c.opts.Now().UTC(),
			}); err != nil {
				return roundState{}, fmt.Errorf("blueprint: close the mockup: record Q%d: %w", number, err)
			}

			number++
		}
	}

	closed := s.clone()
	closed.Change, closed.MessageID, closed.Dropped, closed.Frame.Closed = changeClosed, "", 0, true
	c.putRound(ctx, au, bp.ID, 0, closed)

	return closed, nil
}

// suggestionLabels are the values of sug as a person reads them.
func suggestionLabels(sug stage.MockupSuggestion) []string {
	if sug.Kind == stage.MockupSwitch {
		return []string{sug.Label("off"), sug.Label("on")}
	}

	out := make([]string, 0, len(sug.Options))
	for _, o := range sug.Options {
		out = append(out, o.Label)
	}

	return out
}

// settleMockup ends the mockup step on the closed mockup round the pipeline
// stands at: an approve (use) or reject (skip) gates row on the revision,
// by whoever decided, with the reply's message id (its event marks a use a
// failed responder left to the architect), then the step's settle
// run, a stage run with no model and no cost that ends done with reason
// used or skipped, so the step is done by the rule every stage follows.
// Each part is written once, so a reopen finishes what a restart cut short.
// The caller holds the run slot.
func (c *Controller) settleMockup(ctx context.Context, r *run, sess *session, bp store.Blueprint) stageEnd {
	bg, au, id := context.WithoutCancel(ctx), sess.audit, bp.ID

	place, found, err := c.roundAt(bg, id)

	switch {
	case err != nil:
		c.fail(bg, au, id, 0, err.Error())

		return stageStopped
	case !found || place.state.Mockup == nil || !place.state.Frame.Closed || place.state.Mockup.Action == "":
		c.fail(bg, au, id, 0, "no decided mockup to settle")

		return stageStopped
	}

	s, m := place.state, place.state.Mockup
	stageID := s.Frame.Stage

	decision, reason := store.GateApprove, mockupUsed
	if m.Action == protocol.MockupSkip {
		decision, reason = store.GateReject, mockupSkipped
	}

	have, err := c.gateDecision(bg, id, stageID, m.Revision)
	if err != nil {
		c.fail(bg, au, id, 0, err.Error())

		return stageStopped
	}

	extra := map[string]any{"mockup": true}
	if m.Failed {
		extra["failed"] = true
	}

	if have == "" && !c.decideWith(bg, au, id, stageID, m.Revision, decision, "", m.By, m.MessageID, extra) {
		return stageStopped
	}

	c.mu.Lock()
	r.stage = stageID
	c.mu.Unlock()

	// Under the round's attempt and builder round, so a settle run a crash cut
	// short leaves the round in place for the reopen to settle.
	runID, err := c.opts.Store.StartRun(bg, store.Run{
		BlueprintID: id, Stage: stageID, Kind: store.RunKindStage, Attempt: s.Attempt, Round: s.BuilderRound,
		Status: store.RunRunning, StartedAt: c.opts.Now().UTC(),
	})
	if err != nil {
		c.fail(bg, au, id, 0, "start run failed: "+err.Error())

		return stageStopped
	}

	c.endRun(bg, runID, store.RunDone, reason, store.RunUsage{})
	c.setState(bg, id, StateIdle)
	au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
		"stage": stageID, "kind": store.RunKindStage, "status": store.RunDone, "cost_usd": 0.0, "priced": true, "mockup": true, "reason": reason,
	}})

	return stageStored
}

// mockupResult is how the blueprint's mockup was decided: its newest closed
// mockup round's outcome (mockupUsed or mockupSkipped), the revision it
// decided, the chosen settings of a use, and that revision's artifact.
type mockupResult struct {
	Outcome  string
	Revision int
	Chosen   map[string]string
	Art      store.Artifact
}

// mockupResult reads blueprint id's mockup result; the zero result when no
// mockup round closed decided.
func (c *Controller) mockupResult(ctx context.Context, id string) (mockupResult, error) {
	rounds, err := c.rounds(ctx, id)
	if err != nil {
		return mockupResult{}, err
	}

	for _, s := range slices.Backward(rounds) {
		if s.Mockup == nil || !s.Frame.Closed || s.Mockup.Action == "" {
			continue
		}

		art, err := c.opts.Store.GetArtifact(ctx, id, s.Frame.Stage, s.Mockup.Revision)
		if err != nil {
			return mockupResult{}, fmt.Errorf("blueprint: read mockup revision %d: %w", s.Mockup.Revision, err)
		}

		outcome := mockupUsed
		if s.Mockup.Action == protocol.MockupSkip {
			outcome = mockupSkipped
		}

		return mockupResult{Outcome: outcome, Revision: s.Mockup.Revision, Chosen: s.Mockup.Chosen, Art: art}, nil
	}

	return mockupResult{}, nil
}

// withChosen is the data of mockup artifact a with the settings "Use this
// design" chose added, when an approve row names its version; else a's data
// as stored. The settings are read from the closed round, never stored on
// the artifact, which is append-only.
func (c *Controller) withChosen(ctx context.Context, id string, a store.Artifact) (json.RawMessage, error) {
	decision, err := c.gateDecision(ctx, id, a.Stage, a.Version)
	if err != nil || decision != store.GateApprove {
		return a.Data, err
	}

	res, err := c.mockupResult(ctx, id)
	if err != nil {
		return nil, err
	}

	if res.Outcome != mockupUsed || res.Revision != a.Version {
		return a.Data, nil
	}

	var d mockupData
	if err := json.Unmarshal(a.Data, &d); err != nil {
		return nil, fmt.Errorf("blueprint: decode mockup revision %d: %w", a.Version, err)
	}

	d.Chosen = res.Chosen

	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("blueprint: encode mockup revision %d: %w", a.Version, err)
	}

	return raw, nil
}

// writeMockupLook tells a chat run which revision of mockup round s the
// person is looking at and with which settings, in words; without the run's
// conversation also that revision's notes and its suggestions.
func writeMockupLook(b *strings.Builder, s roundState, notes string, full bool) {
	m := s.Mockup

	if full {
		fmt.Fprintf(b, "## Revision %d, which you showed\n\n%s\n\nIts suggestions:\n\n", m.Revision, notes)

		for _, sug := range m.Suggestions {
			fmt.Fprintf(b, "- %s (%s): %s Takes %s.\n", sug.Title, sug.ID, sug.Why, strings.Join(suggestionLabels(sug), ", "))
		}

		b.WriteString("\n")
	}

	fmt.Fprintf(b, "## What the person is looking at\n\nRevision %d, set to:\n\n", m.Revision)

	for _, sug := range m.Suggestions {
		fmt.Fprintf(b, "- %s: %s\n", sug.Title, sug.Label(m.Settings[sug.ID]))
	}

	b.WriteString("\n")
}

// Show stores revision m of the mockup as the stage's next artifact
// version, whose number is the revision's, and posts the step's one mockup
// round with it or, from a chat run, the round that run answers: the notes
// are the designer's chat entry and the settings carry over from the
// round's last ones. The round is the auto responder's when the stage's
// policy is auto now. The run then ends asked.
func (iv *interviewer) Show(ctx context.Context, m stage.MockupRevision) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c, bg, id := iv.c, context.WithoutCancel(ctx), iv.bp.ID

	lock := c.roundLock(id)
	lock.Lock()
	defer lock.Unlock()

	// Read under the lock: a mode switch stores its mode before its handover
	// takes the lock, so a revision never undoes a handover it raced.
	policy, err := c.policy(bg, iv.bp, iv.st.ID)
	if err != nil {
		return "", err
	}

	s, err := iv.mockupRound(bg)
	if err != nil {
		return "", err
	}

	v := 1

	switch latest, err := c.opts.Store.LatestArtifact(bg, id, iv.st.ID); {
	case err == nil:
		v = latest.Version + 1
	case !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("blueprint: read the mockup: %w", err)
	}

	var last map[string]string
	if s.Mockup != nil {
		last = s.Mockup.Settings
	}

	sugs := m.Suggestions
	if sugs == nil {
		sugs = []stage.MockupSuggestion{}
	}

	settings := carriedSettings(sugs, last)
	au := iv.sess.audit

	data := mockupData{Revision: v, Screens: m.Screens, Suggestions: sugs, Settings: settings, Source: m.Source}
	if _, err := au.artifact(bg, id, iv.st.ID, m.Notes, data.fields()); err != nil {
		return "", err
	}

	// The artifact row holds the source; the event only names the version.
	au.event(bg, Event{Kind: EventArtifact, BlueprintID: id, RunID: iv.runID, Payload: map[string]any{
		"stage": iv.st.ID, "version": v, "persona": iv.st.Persona, "revision": v,
	}})

	requests := 0
	if s.Mockup != nil {
		requests = s.Mockup.Requests
	}

	auto := policy == pack.PolicyAuto

	s.Mockup = &mockupState{Revision: v, Suggestions: sugs, Opening: settings, Settings: maps.Clone(settings), Requests: requests}
	s.Frame.Responder = auto

	// Steering is the person's again from a revision drawn under the human
	// policy.
	if !auto {
		s.HandedOver = nil
	}

	// The notes follow the entries this run was given; any written meanwhile
	// come after them, for the next chat run.
	seen := min(iv.seen, len(s.Frame.Chat))
	chat := append(slices.Clone(s.Frame.Chat[:seen]), protocol.RoundChat{From: iv.st.Persona, Text: m.Notes})
	s.Frame.Chat = append(chat, s.Frame.Chat[seen:]...)
	s.Seen = seen + 1

	c.putRound(bg, au, id, iv.runID, s)
	iv.posted, iv.revision = s.Number, v

	return fmt.Sprintf("Revision %d is shown. Your run ends here; the person's reaction arrives as your next message.", v), nil
}

// mockupRound is the round a revision of this run goes on: a new round for
// a first run, which no open mockup round may wait for, or a copy of the
// open mockup round a chat run answers while nobody chose a revision yet.
// The caller holds the round lock.
func (iv *interviewer) mockupRound(ctx context.Context) (roundState, error) {
	c := iv.c

	place, found, err := c.roundAt(ctx, iv.bp.ID)
	if err != nil {
		return roundState{}, err
	}

	cur := place.state
	open := found && cur.Mockup != nil && !cur.Frame.Closed

	switch {
	case iv.qround == 0 && open:
		return roundState{}, fmt.Errorf("revision %d is shown already; your run has ended with it", cur.Mockup.Revision)
	case iv.qround == 0:
		number, err := c.nextRoundNumber(ctx, iv.bp.ID)
		if err != nil {
			return roundState{}, err
		}

		return roundState{
			Number: number, Attempt: iv.attempt, BuilderRound: iv.builderRound, Findings: iv.findings,
			PostedBy: iv.runID, RunID: iv.runID, Change: changePosted,
			Frame: protocol.LogRound{Persona: personaName(iv.st.Persona), Stage: iv.st.ID},
		}, nil
	case !open || cur.Number != iv.qround:
		return roundState{}, fmt.Errorf("round %d takes no changes now", iv.qround)
	case cur.Mockup.Action != "":
		return roundState{}, fmt.Errorf("round %d takes no changes now: the person chose revision %d; reply with answer_chat", iv.qround, cur.Mockup.Revision)
	}

	next := cur.clone()
	next.Change, next.RunID, next.MessageID, next.Dropped = changeRevision, iv.runID, "", 0

	return next, nil
}

// screenProblems checks a submit says whether the need has a screen, and
// when it has, whether it is a re-design of an existing one.
func screenProblems(data map[string]any) []string {
	screen, ok := data["screen"].(bool)
	if !ok {
		return []string{"screen_shape: data.screen must be true when the need has a screen a person looks at, else false"}
	}

	if _, ok := data["redesign"].(bool); screen && !ok {
		return []string{"redesign_shape: data.redesign must be true when the person asks to re-design an existing screen, else false"}
	}

	return nil
}

// screenValidator refuses a submit that does not say whether there is a
// screen, as boundedValidator bounds it.
func screenValidator() (func(context.Context, stage.Artifact) error, *validatorState) {
	return boundedValidator("screen", func(_ context.Context, a stage.Artifact) ([]string, error) {
		return screenProblems(a.Data), nil
	})
}

// mockupCollapsed reports whether the mockup step of blueprint id does not
// run: the screen stage's newest artifact exists and does not state a
// screen, or the newest offer was declined. It is read from the store alone,
// so it holds across restarts; an artifact from before mockups has no screen
// key and so collapses the step.
func (c *Controller) mockupCollapsed(ctx context.Context, id string) (bool, error) {
	pk := c.opts.Pack
	if pk.MockupStage() == "" {
		return false, nil
	}

	screen, err := c.opts.Store.LatestArtifact(ctx, id, pk.ScreenStage())

	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("blueprint: read %s: %w", pack.StageName(pk.ScreenStage(), false), err)
	}

	if !statesScreen(screen) {
		return true, nil
	}

	state, err := c.offerState(ctx, id)

	return state == offerDeclined, err
}

// statesScreen reports whether the screen stage's artifact holds screen: true.
func statesScreen(art store.Artifact) bool {
	var d struct {
		Screen bool `json:"screen"`
	}

	return len(art.Data) > 0 && json.Unmarshal(art.Data, &d) == nil && d.Screen
}

// mockupShown reports whether in is the mockup step with an approved
// mockup: a skip, a declined offer and a need without a screen leave later
// stages nothing of one.
func (c *Controller) mockupShown(ctx context.Context, id string, in pipelineStep) (bool, error) {
	if in.Collapsed || in.Stage.ID != c.opts.Pack.MockupStage() {
		return false, nil
	}

	res, err := c.mockupResult(ctx, id)

	return res.Outcome == mockupUsed, err
}

// writeMockupInput gives a step that consumes the mockup the approved design
// in words: the notes, the screens and each suggestion with the value the
// person chose. The source stays out; read_artifact returns it.
func (c *Controller) writeMockupInput(ctx context.Context, b *strings.Builder, id string, in pipelineStep) error {
	if in.Collapsed {
		return nil
	}

	res, err := c.mockupResult(ctx, id)
	if err != nil {
		return err
	}

	if res.Outcome != mockupUsed {
		return nil
	}

	var d mockupData
	if err := json.Unmarshal(res.Art.Data, &d); err != nil {
		return fmt.Errorf("blueprint: decode mockup revision %d: %w", res.Revision, err)
	}

	fmt.Fprintf(b, "## %s\n\nRevision %d of the mockup, as the person approved it. read_artifact with stage %q returns its HTML source.\n\n%s\n\n",
		in.ID, res.Revision, in.Stage.ID, res.Art.Body)

	if len(d.Screens) > 0 {
		b.WriteString("Screens:\n")

		for _, s := range d.Screens {
			fmt.Fprintf(b, "- %s: %s\n", s.ID, s.Title)
		}

		b.WriteString("\n")
	}

	if len(d.Suggestions) > 0 {
		b.WriteString("Chosen:\n")

		for _, s := range d.Suggestions {
			fmt.Fprintf(b, "- %s: %s (%s)\n", s.Title, s.Label(chosenValue(res.Chosen, d.Settings, s.ID)), s.Why)
		}

		b.WriteString("\n")
	}

	return nil
}
