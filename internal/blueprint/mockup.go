package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// mockupState is a mockup round's design: the revision it shows, that
// revision's suggestions, the settings it opened with (Opening) and the
// last ones the architect knew (Settings), the auto responder's change
// requests so far, and once decided the action (use or skip), the chosen
// settings, who decided and the reply's message id.
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
