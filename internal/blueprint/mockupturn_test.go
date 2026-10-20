package blueprint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// reacting is a mockup responder that, turn by turn, returns the given
// reactions; its last one again after that. Each passes the run's own check
// first, as a submit would.
func reacting(rs ...map[string]any) func(context.Context, stage.Input) (stage.Output, error) {
	var n atomic.Int32

	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		r := rs[min(int(n.Add(1)), len(rs))-1]
		if err := in.Validate(ctx, stage.Artifact{Data: r}); err != nil {
			return stage.Output{}, err
		}

		return done("", r), nil
	}
}

// responderChat is the text of each of the mockup round's chat entries the
// auto responder wrote.
func responderChat(s roundState) []string {
	var out []string

	for _, m := range s.Frame.Chat {
		if m.From == "responder" {
			out = append(out, m.Text)
		}
	}

	return out
}

// onlyMockupRound is BP1's one round, a mockup's.
func onlyMockupRound(t *testing.T, c *Controller) roundState {
	t.Helper()

	rounds, err := c.rounds(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, rounds, 1)
	require.NotNil(t, rounds[0].Mockup)

	return rounds[0]
}

// noMockupPrompt checks that no mockup prompt waits for the person; the
// sign-off an auto mode stops at may.
func noMockupPrompt(t *testing.T, c *Controller) {
	t.Helper()

	if open := view(t, c).Open; open != nil {
		assert.NotEqual(t, promptMockup, open.Kind)
	}
}

// mockupGates are BP1's gates rows of the mockup step.
func mockupGates(t *testing.T, st *store.Store) []store.Gate {
	t.Helper()

	gates, err := st.ListGates(context.Background(), "BP1", "mockup")
	require.NoError(t, err)

	return gates
}

// chosenOf is the chosen settings the artifact read of version v adds.
func chosenOf(t *testing.T, c *Controller, v int) map[string]string {
	t.Helper()

	art, err := c.Artifact(context.Background(), "BP1", "mockup", "", v)
	require.NoError(t, err)

	var data mockupData
	require.NoError(t, json.Unmarshal(art.Data, &data))

	return data.Chosen
}

func TestParseReaction(t *testing.T) {
	sugs := bookingRevision().Suggestions // slots (list|grid), staff (switch)

	tests := []struct {
		name string
		data map[string]any
		left int
		want reaction
		ok   bool
	}{
		{
			"a request",
			map[string]any{"settings": map[string]any{"slots": "grid"}, "request": " Show the next free time first. "},
			2,
			reaction{Settings: map[string]string{"slots": "grid"}, Request: "Show the next free time first."},
			true,
		},
		{
			"a use",
			map[string]any{"settings": map[string]any{"slots": "grid", "staff": "on"}, "use": true},
			2,
			reaction{Settings: map[string]string{"slots": "grid", "staff": "on"}, Use: true},
			true,
		},
		{"a use without settings", map[string]any{"use": true}, 2, reaction{Use: true}, true},
		{"a use with no request left", map[string]any{"use": true}, 0, reaction{Use: true}, true},
		{"a request with no request left", map[string]any{"request": "Bigger buttons."}, 0, reaction{}, false},
		{"both", map[string]any{"request": "Bigger buttons.", "use": true}, 2, reaction{}, false},
		{"neither", map[string]any{"settings": map[string]any{"slots": "grid"}}, 2, reaction{}, false},
		{"a blank request", map[string]any{"request": "  "}, 2, reaction{}, false},
		{"an id the revision lacks", map[string]any{"settings": map[string]any{"colour": "red"}, "use": true}, 2, reaction{}, false},
		{"a value its suggestion does not take", map[string]any{"settings": map[string]any{"slots": "tiles"}, "use": true}, 2, reaction{}, false},
		{"a value that is no string", map[string]any{"settings": map[string]any{"staff": true}, "use": true}, 2, reaction{}, false},
		{"settings that are no object", map[string]any{"settings": "grid", "use": true}, 2, reaction{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseReaction(tt.data, sugs, tt.left)
			if !tt.ok {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMockupTurn_TheResponderAsksForChangesThenUses(t *testing.T) {
	two := bookingRevision()
	two.Notes = "The next free slot comes first."

	var task atomic.Value

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision(), two)
	react := reacting(
		map[string]any{"settings": map[string]any{"slots": "grid"}, "request": "Show the next free slot first."},
		map[string]any{"settings": map[string]any{"slots": "grid", "staff": "on"}, "use": true},
	)
	script["mockup.responder"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if task.Load() == nil {
			task.Store(in.Task)
		}

		return react(ctx, in)
	}

	c, st, runner, rec, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Contains(t, stagesRun(runner), "slicing")
	assert.Equal(t, map[string]string{"slots": "grid", "staff": "off"}, storedRevision(t, st, 2).Settings, "the request's settings carry over")

	gates := mockupGates(t, st)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, 2, gates[0].Version)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)

	assert.Equal(t, map[string]string{"slots": "grid", "staff": "on"}, chosenOf(t, c, 2))

	assert.Equal(t, "automatic", refOf(t, view(t, c), "mockup", 2).DecidedBy)
	noMockupPrompt(t, c)
	assert.NotEmpty(t, rec.all(EventRound), "the exchange is recorded")

	s := onlyMockupRound(t, c)
	assert.Equal(t, []string{"Show the next free slot first."}, responderChat(s))
	assert.Equal(t, 1, s.Mockup.Requests)
	assert.Equal(t, store.ResponderAuto, s.Mockup.By)
	assert.False(t, s.Mockup.Failed)

	for _, e := range rec.all(EventDecided) {
		assert.NotContains(t, e.Payload, "failed", "the responder's own use")
	}

	assert.Len(t, inputsOf(runner, "mockup.responder"), 2, "one turn per revision")

	responder := inputsOf(runner, "mockup.responder")[0]
	assert.Equal(t, mockupPack(t).Persona("designer"), responder.Persona, "the stage's persona")
	assert.Equal(t, mockupPack(t).MockupResponderInstructions(), responder.Instructions)

	first, _ := task.Load().(string)
	assert.Contains(t, first, bookingSource, "the responder reads the revision's source")
	assert.Contains(t, first, bookingRevision().Notes)

	for _, q := range mockupQuestions(t, st) {
		assert.Equal(t, store.ResponderAuto, q.Responder)
		assert.True(t, q.Assumption)
	}
}

func TestMockupTurn_FiveRequestsThenItMustChoose(t *testing.T) {
	var (
		turns   atomic.Int32
		refused atomic.Bool
	)

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	script["mockup.responder"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		n := turns.Add(1)
		request := map[string]any{"request": fmt.Sprintf("Change %d, please.", n)}

		if n <= maxMockupRequests {
			return done("", request), nil
		}

		refused.Store(in.Validate(ctx, stage.Artifact{Data: request}) != nil)

		return done("", map[string]any{"use": true}), nil
	}

	c, st, runner, _, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.True(t, refused.Load(), "the sixth turn's check refuses a request")

	for v := 1; v <= maxMockupRequests+1; v++ {
		assert.Equal(t, v, storedRevision(t, st, v).Revision)
	}

	gates := mockupGates(t, st)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, maxMockupRequests+1, gates[0].Version)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)

	s := onlyMockupRound(t, c)
	assert.Len(t, responderChat(s), maxMockupRequests)
	assert.Equal(t, maxMockupRequests, s.Mockup.Requests)
	assert.Len(t, inputsOf(runner, "mockup.responder"), maxMockupRequests+1)
	assert.Contains(t, stagesRun(runner), "slicing")
}

func TestMockupTurn_AFailedResponderUsesTheRevisionAsDrawn(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	script["mockup.responder"] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{}, errors.New("model down")
	}

	c, st, runner, rec, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	gates := mockupGates(t, st)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, 1, gates[0].Version)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)

	assert.Equal(t, storedRevision(t, st, 1).Settings, chosenOf(t, c, 1), "the revision as it opened")

	var failed []Event

	for _, e := range rec.all(EventError) {
		if e.Payload["reason"] == reasonResponderFailed {
			failed = append(failed, e)
		}
	}

	require.Len(t, failed, 1)
	assert.Equal(t, "mockup", failed[0].Payload["stage"])
	assert.Equal(t, 1, failed[0].Payload["revision"])

	assert.Contains(t, stagesRun(runner), "slicing")
	assert.Len(t, inputsOf(runner, "mockup.responder"), 1)
	assert.Equal(t, "done", stepOf(t, view(t, c), "mockup").Status)
	assert.True(t, onlyMockupRound(t, c).Mockup.Failed, "the use is marked the architect's, not the responder's")

	var decided []Event

	for _, e := range rec.all(EventDecided) {
		if e.Payload["mockup"] == true {
			decided = append(decided, e)
		}
	}

	require.Len(t, decided, 1)
	assert.Equal(t, true, decided[0].Payload["failed"], "the settle's row says the architect used it")
}

// The responder steers from the settings the person last sent: a use that
// names none, and a failed turn, keep them, not the revision's opening ones.
func TestMockupTurn_TheResponderSteersFromThePersonsSettings(t *testing.T) {
	responders := map[string]func(context.Context, stage.Input) (stage.Output, error){
		"a use that names no settings": reacting(map[string]any{"use": true}),
		"a failed turn": func(context.Context, stage.Input) (stage.Output, error) {
			return stage.Output{}, errors.New("model down")
		},
	}

	for name, responder := range responders {
		t.Run(name, func(t *testing.T) {
			script := withScreen(dialogScript(), true)
			first := showing(bookingRevision())
			script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				if !in.Chat {
					return first(ctx, in)
				}

				if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "A grid suits a tablet."}); err != nil {
					return stage.Output{}, err
				}

				return askedOutput(in), nil
			}
			script["mockup.responder"] = responder

			c, st, _, _, _ := newMockup(t, script)
			drive(t, c, "BP1")
			takeOffer(t, c)
			chatMockup(t, c, "Does a grid read better?", map[string]string{"slots": "grid"})

			require.NoError(t, c.SetMode(context.Background(), "BP1", protocol.ArchitectModeAuto, "m-mode"))
			c.Wait("BP1")

			sent := map[string]string{"slots": "grid", "staff": "off"}
			require.NotEqual(t, storedRevision(t, st, 1).Settings, sent)
			assert.Equal(t, sent, chosenOf(t, c, 1))
		})
	}
}

func TestMockupTurn_ASwitchHandsTheSteeringOverAndBack(t *testing.T) {
	t.Run("to auto while the person looks", func(t *testing.T) {
		script := withScreen(dialogScript(), true)
		script["mockup"] = showing(bookingRevision())
		script["mockup.responder"] = reacting(map[string]any{"settings": map[string]any{"staff": "on"}, "use": true})

		c, st, runner, rec, _ := newMockup(t, script)
		drive(t, c, "BP1")
		takeOffer(t, c)

		open := view(t, c).Open
		require.NotNil(t, open)
		require.Equal(t, promptMockup, open.Kind)

		require.NoError(t, c.SetMode(context.Background(), "BP1", protocol.ArchitectModeAuto, "m-mode"))
		c.Wait("BP1")

		assert.Len(t, inputsOf(runner, "mockup.responder"), 1, "the responder's turn ran")

		gates := mockupGates(t, st)
		require.Len(t, gates, 1)
		assert.Equal(t, store.GateApprove, gates[0].Decision)
		assert.Equal(t, 1, gates[0].Version)
		assert.Equal(t, store.ResponderAuto, gates[0].Responder)
		assert.Equal(t, map[string]string{"slots": "list", "staff": "on"}, chosenOf(t, c, 1))

		noMockupPrompt(t, c)
		assert.Contains(t, stagesRun(runner), "slicing")

		e := onlyMode(t, rec)
		assert.Equal(t, true, e.Payload["mockup"], "the switch handed over a mockup, not a round")
		assert.NotZero(t, e.Payload["hands_over"])
	})

	t.Run("to default while the responder steers", func(t *testing.T) {
		two := bookingRevision()
		two.Notes = "The next free slot comes first."

		started, release := make(chan struct{}), make(chan struct{})
		unblock := closer(t, release)

		script := withScreen(dialogScript(), true)
		show := showing(bookingRevision(), two)
		script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
			if in.Chat {
				close(started)
				<-release
			}

			return show(ctx, in)
		}
		script["mockup.responder"] = reacting(
			map[string]any{"request": "Show the next free slot first."},
			map[string]any{"use": true},
		)

		c, st, runner, _, _ := newMockup(t, script)
		ctx := context.Background()

		require.NoError(t, c.Start(ctx, autoStart("BP1")))
		require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
		receive(t, started, "the designer's chat run")

		require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeDefault, "m-mode"))
		unblock()
		c.Wait("BP1")

		assert.Equal(t, 2, storedRevision(t, st, 2).Revision)

		v := view(t, c)
		require.NotNil(t, v.Open)
		assert.Equal(t, promptMockup, v.Open.Kind)
		assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)

		assert.Len(t, inputsOf(runner, "mockup.responder"), 1, "the responder ran once")
		assert.Empty(t, mockupGates(t, st))
		assert.False(t, onlyMockupRound(t, c).Frame.Responder)
	})
}

// A switch to default while the responder's turn waits to run, as after an
// abort, hands the mockup back at once.
func TestMockupTurn_ASwitchToDefaultAfterAnAbortHandsItBack(t *testing.T) {
	started := make(chan struct{})

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	script["mockup.responder"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(started)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	}

	c, st, runner, rec, _ := newMockup(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "the responder's turn")
	require.NoError(t, c.Abort(ctx, "BP1", "m-abort"))
	c.Wait("BP1")

	require.Nil(t, view(t, c).Open, "the mockup is still the responder's")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeDefault, "m-mode"))
	c.Wait("BP1")

	v := view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, promptMockup, v.Open.Kind)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)

	s := onlyMockupRound(t, c)
	assert.False(t, s.Frame.Responder)
	assert.Nil(t, s.HandedOver)
	assert.Equal(t, changeHandedBack, s.Change)

	assert.Len(t, inputsOf(runner, "mockup.responder"), 1, "the hand-back runs no responder")
	assert.Empty(t, mockupGates(t, st))
	assert.NotContains(t, onlyMode(t, rec).Payload, "hands_over")
}

// A switch to default while the designer's run on the responder's request
// has failed hands the mockup back at once; before it the step reads failed,
// as nobody waits on it.
func TestMockupTurn_ASwitchToDefaultAfterAFailedDesignerRunHandsItBack(t *testing.T) {
	first := showing(bookingRevision())

	script := withScreen(dialogScript(), true)
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.Chat {
			return stage.Output{}, errors.New("model down")
		}

		return first(ctx, in)
	}
	script["mockup.responder"] = reacting(map[string]any{"request": "Show the next free slot first."})

	c, st, runner, _, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	require.Len(t, inputsOf(runner, "mockup"), 2, "the first run and the failed chat run")

	v := view(t, c)
	assert.Nil(t, v.Open)
	assert.Equal(t, "failed", stepOf(t, v, "mockup").Status)

	require.NoError(t, c.SetMode(context.Background(), "BP1", protocol.ArchitectModeDefault, "m-mode"))
	c.Wait("BP1")

	s := onlyMockupRound(t, c)
	assert.False(t, s.Frame.Responder)
	assert.Nil(t, s.HandedOver)
	assert.Equal(t, changeHandedBack, s.Change)

	v = view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, protocol.OpenPrompt{PromptID: mockupPromptID(s.Number), Kind: promptMockup}, *v.Open)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)

	assert.Len(t, inputsOf(runner, "mockup"), 2, "the hand-back runs no designer")
	assert.Len(t, inputsOf(runner, "mockup.responder"), 1)
	assert.Empty(t, mockupGates(t, st))
}

// A switch to default while the responder's turn runs takes the steering
// back as the turn ends: the use it reaches, its own or the one a failure
// leaves, is not stored, and the mockup waits for the person with the
// settings the responder sent.
func TestMockupTurn_ASwitchToDefaultDuringATurnHandsBackItsUse(t *testing.T) {
	tests := map[string]struct {
		react func(context.Context, stage.Input) (stage.Output, error)
		want  map[string]string
	}{
		"a use": {
			react: reacting(map[string]any{"settings": map[string]any{"slots": "grid", "staff": "on"}, "use": true}),
			want:  map[string]string{"slots": "grid", "staff": "on"},
		},
		"a failed responder": {
			react: func(context.Context, stage.Input) (stage.Output, error) {
				return stage.Output{}, errors.New("model down")
			},
			want: map[string]string{"slots": "list", "staff": "off"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			unblock := closer(t, release)

			script := withScreen(dialogScript(), true)
			script["mockup"] = showing(bookingRevision())
			script["mockup.responder"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				signal(started)
				<-release

				return tt.react(ctx, in)
			}

			c, st, runner, _, _ := newMockup(t, script)
			ctx := context.Background()

			require.NoError(t, c.Start(ctx, autoStart("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			receive(t, started, "the responder's turn")

			require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeDefault, "m-mode"))
			unblock()
			c.Wait("BP1")

			assert.Empty(t, mockupGates(t, st), "the turn's use is not stored")
			assert.NotContains(t, stagesRun(runner), "slicing")
			assert.Len(t, inputsOf(runner, "mockup.responder"), 1)

			s := onlyMockupRound(t, c)
			assert.False(t, s.Frame.Responder)
			assert.Nil(t, s.HandedOver)
			assert.Empty(t, s.Mockup.Action)
			assert.Equal(t, changeHandedBack, s.Change)
			assert.Equal(t, tt.want, s.Mockup.Settings, "the responder's settings")

			v := view(t, c)
			require.NotNil(t, v.Open)
			assert.Equal(t, protocol.OpenPrompt{PromptID: mockupPromptID(s.Number), Kind: promptMockup}, *v.Open)
			assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)
		})
	}
}

// A reopen after the session's end cut the responder's turn short runs the
// turn again.
func TestMockupTurn_AReopenRunsTheTurnTheSessionsEndCutShort(t *testing.T) {
	started := make(chan struct{})

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	script["mockup.responder"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(started)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	}

	c, st, _, _, ws := newMockup(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "the responder's turn")
	require.NoError(t, c.End(ctx, "BP1"))

	again := withScreen(dialogScript(), true)
	again["mockup"] = showing(bookingRevision())
	again["mockup.responder"] = reacting(map[string]any{"use": true})

	c2, _, runner2, _, _ := newPipelineOn(t, st, again, ws)
	c2.opts.Pack = mockupPack(t)
	require.NoError(t, c2.Start(ctx, autoStart("BP1")))
	c2.Wait("BP1")

	assert.Len(t, inputsOf(runner2, "mockup.responder"), 1)
	assert.NotContains(t, stagesRun(runner2), "mockup", "the designer does not draw again")
	assert.Contains(t, stagesRun(runner2), "slicing")

	gates := mockupGates(t, st)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestMockupTurn_ATurnThatCannotBeStoredFailsInsteadOfSpinning(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "architect.db")

	st, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// A second connection refuses every store of a decided mockup, as a full
	// disk would, while reads still work.
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	_, err = raw.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER probe BEFORE INSERT ON events
		WHEN NEW.kind = '%s' AND json_extract(NEW.payload, '$.change') = '%s'
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`, EventRound, changeChosen))
	require.NoError(t, err)

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	script["mockup.responder"] = reacting(map[string]any{"use": true})

	c, _, runner, _, _ := newPipelineOn(t, st, script, nil)
	c.opts.Pack = mockupPack(t)
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, string(StateFailed), stateOf(t, st, "BP1"))
	assert.Len(t, inputsOf(runner, "mockup.responder"), 1, "one turn, then the blueprint stops")
	assert.NotContains(t, stagesRun(runner), "slicing")
}

// A mockup a switch handed to the responder reads as one the responder
// steers: no prompt, the step running while the responder's turn runs and
// waiting between turns, as after an abort.
func TestMockupTurn_AHandedOverMockupReadsAsTheResponders(t *testing.T) {
	started, release := make(chan struct{}, 2), make(chan struct{})
	unblock := closer(t, release)

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	use := reacting(map[string]any{"use": true})
	script["mockup.responder"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		signal(started)

		select {
		case <-release:
			return use(ctx, in)
		case <-ctx.Done():
			return stage.Output{}, ctx.Err()
		}
	}

	c, _, _, _, _ := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	receive(t, started, "the responder's turn")

	v := view(t, c)
	assert.Nil(t, v.Open)
	assert.Equal(t, "running", stepOf(t, v, "mockup").Status)
	require.NotNil(t, v.Current)
	assert.Equal(t, store.RunKindResponder, v.Current.Kind)

	require.NoError(t, c.Abort(ctx, "BP1", "m-abort"))
	c.Wait("BP1")

	v = view(t, c)
	assert.Nil(t, v.Open)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)

	unblock()
	require.NoError(t, c.Resume(ctx, "BP1", "m-continue"))
	c.Wait("BP1")

	assert.Equal(t, "done", stepOf(t, view(t, c), "mockup").Status)
}

// A designer run that fails on a handed-over mockup waits for a message, as
// on a mockup posted under auto: a reopen does not run it again by itself.
func TestMockupTurn_AFailedDesignerRunOnAHandedOverMockupWaitsForAMessage(t *testing.T) {
	designer := func() func(context.Context, stage.Input) (stage.Output, error) {
		first := showing(bookingRevision())

		return func(ctx context.Context, in stage.Input) (stage.Output, error) {
			if in.Chat {
				return stage.Output{}, errors.New("model down")
			}

			return first(ctx, in)
		}
	}

	script := withScreen(dialogScript(), true)
	script["mockup"] = designer()
	script["mockup.responder"] = reacting(map[string]any{"request": "Show the next free slot first."})

	c, st, runner, _, ws := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)
	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	require.Len(t, inputsOf(runner, "mockup"), 2, "the first run and the failed chat run")
	require.NoError(t, c.End(ctx, "BP1"))

	again := withScreen(dialogScript(), true)
	again["mockup"] = designer()
	again["mockup.responder"] = reacting(map[string]any{"use": true})

	c2, _, runner2, _, _ := newPipelineOn(t, st, again, ws)
	c2.opts.Pack = mockupPack(t)
	require.NoError(t, c2.Start(ctx, autoStart("BP1")))
	c2.Wait("BP1")

	assert.Empty(t, inputsOf(runner2, "mockup"), "the reopen waits")

	require.NoError(t, c2.Message(ctx, "BP1", "try again", "m-again"))
	c2.Wait("BP1")

	assert.Len(t, inputsOf(runner2, "mockup"), 1, "the message runs it again")
}

// A gate reply leaves the mockup the responder holds with it: the reply acts
// from the next revision, so a turn after it still runs under auto mode.
func TestMockupTurn_AGateReplyLeavesTheHeldMockupWithTheResponder(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := closer(t, release)

	script := withScreen(dialogScript(), true)
	first := showing(bookingRevision())
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !in.Chat {
			return first(ctx, in)
		}

		close(started)
		<-release

		if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "The next free slot already comes first."}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
	script["mockup.responder"] = reacting(
		map[string]any{"request": "Show the next free slot first."},
		map[string]any{"use": true},
	)

	c, st, runner, rec, _ := newMockup(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "the designer's chat run")

	require.NoError(t, c.SetPolicy(ctx, "BP1", "mockup", "human", "m-gate"))
	unblock()
	c.Wait("BP1")

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.Equal(t, "mockup", change.Payload["open"], "an open mockup is no round")

	assert.Len(t, inputsOf(runner, "mockup.responder"), 2, "the responder takes the next turn")

	gates := mockupGates(t, st)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)

	for _, e := range rec.all(EventRound) {
		assert.NotEqual(t, changeHandedBack, e.Payload["change"])
	}
}

// A plain-language blueprint's mockup responder reads the plain
// instructions too, as every other run does.
func TestMockupTurn_APlainBlueprintsResponderReadsThePlainInstructions(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	script["mockup.responder"] = reacting(map[string]any{"use": true})

	c, _, runner, _, _ := newMockup(t, script)

	in := plainStart("BP1")
	in.Mode = "auto"
	driveIn(t, c, in)

	runs := inputsOf(runner, "mockup.responder")
	require.Len(t, runs, 1)
	assert.NotEqual(t, mockupPack(t).MockupResponderInstructions(), runs[0].Instructions)
}
