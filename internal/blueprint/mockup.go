package blueprint

import (
	"context"
	"encoding/json"

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

// mockupCollapsed reports whether the mockup step does not run: the screen
// stage's newest artifact exists and does not state a screen. An artifact
// from before mockups has no screen key and so collapses it. screen is nil
// before the stage has an artifact.
func mockupCollapsed(pk *pack.Pack, screen *store.Artifact) bool {
	if pk.MockupStage() == "" || screen == nil {
		return false
	}

	var d struct {
		Screen bool `json:"screen"`
	}

	if len(screen.Data) == 0 || json.Unmarshal(screen.Data, &d) != nil {
		return true
	}

	return !d.Screen
}
