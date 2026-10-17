package blueprint

import (
	"context"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// askingController runs a stage that asks one human question and returns
// the answer it receives.
func askingController(t *testing.T) (*Controller, *store.Store, *recorder, chan string) {
	t.Helper()

	answered := make(chan string, 1)

	c, st, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		a, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?", Options: []string{"helm", "kustomize"}})
		if err != nil {
			return stage.Output{}, err
		}

		answered <- a

		return artifactOutput("# Need\n\n"+a, in), nil
	})

	return c, st, rec, answered
}

// answerOf reads the answer a stage got, failing the test rather than
// hanging when none arrives.
func answerOf(t *testing.T, ch <-chan string) string {
	t.Helper()

	select {
	case a := <-ch:
		return a
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the stage never got its answer")

		return ""
	}
}

func TestReshow_ExportDuringAQuestionShowsItAgain(t *testing.T) {
	c, st, rec, answered := askingController(t)
	c.opts.ExportDir = t.TempDir()
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	rec.waitFor(t, EventQuestion)

	require.NoError(t, c.Export(ctx, "BP1", "m2"))

	asked, ok := rec.first(EventQuestion)
	require.True(t, ok)

	again, ok := rec.first(EventReshow)
	require.True(t, ok, "the open question is published again")
	assert.Equal(t, EventQuestion, again.Payload["kind"])
	assert.Equal(t, asked.Payload, again.Payload["payload"], "with the same number, options and actions")
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"), "the question still waits")

	require.NoError(t, c.Message(ctx, "BP1", "helm", "m3"))
	assert.Equal(t, "helm", answerOf(t, answered), "the next message is still the answer")
	c.Wait("BP1")
}

func TestReshow_AGateReplyDuringAQuestionShowsItAgain(t *testing.T) {
	c, st, rec, answered := askingController(t)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	rec.waitFor(t, EventQuestion)

	asked, ok := rec.first(EventQuestion)
	require.True(t, ok)

	require.NoError(t, c.SetPolicy(ctx, "BP1", asked.Payload["stage"].(string), "auto", "m2"))

	again := rec.all(EventReshow)
	require.Len(t, again, 1)
	assert.Equal(t, EventQuestion, again[0].Payload["kind"])
	assert.Equal(t, asked.Payload, again[0].Payload["payload"])
	assert.Equal(t, 1, rec.count(EventQuestion), "the question was not asked twice")
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"), "the question still waits")

	require.NoError(t, c.Message(ctx, "BP1", "helm", "m3"))
	assert.Equal(t, "helm", answerOf(t, answered), "the next message is still the answer")
	c.Wait("BP1")
}

func TestReshow_NothingAtAnOpenGateShowsItAgain(t *testing.T) {
	c, _, _, rec := newGated(t, gatedScript())
	c.opts.ExportDir = t.TempDir()
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.Equal(t, 1, rec.count(EventGate))

	require.NoError(t, c.Message(ctx, "BP1", "looks fine to me", "n1"))
	c.Wait("BP1")
	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", "auto", "n2"))
	require.NoError(t, c.Export(ctx, "BP1", "n3"))
	require.NoError(t, c.Abort(ctx, "BP1", "n4"))

	assert.False(t, rec.has(EventReshow), "a gate stays open on CM's side")
	assert.Equal(t, 1, rec.count(EventGate), "no second gate prompt")
}

func TestReshow_ANoteAtTheSignoffShowsNothingAgain(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "looks fine", "m3"))
	c.Wait("BP1")

	assert.False(t, rec.has(EventReshow))
	assert.Equal(t, 1, rec.count(EventSignOff))
}

func TestReshow_AnAbortDuringAQuestionShowsNothing(t *testing.T) {
	c, _, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?"})

		return stage.Output{}, err
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	rec.waitFor(t, EventQuestion)

	require.NoError(t, c.Abort(ctx, "BP1", "m2"))
	c.Wait("BP1")

	assert.False(t, rec.has(EventReshow), "the abort ended the question")
}
