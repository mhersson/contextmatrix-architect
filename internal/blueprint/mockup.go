package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

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
