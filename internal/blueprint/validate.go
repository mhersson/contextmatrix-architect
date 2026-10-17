package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// boundedValidator refuses a submit while check finds problems, up to the
// third failing submit, which is accepted as it is so a stage never loops
// until its turns are spent. The state keeps the problems of the latest
// submit judged, so the accepted one's stay with its artifact. An error from
// check refuses the submit without spending a round. what names the checked
// thing in the refusal.
func boundedValidator(what string, check func(context.Context, stage.Artifact) ([]string, error)) (func(context.Context, stage.Artifact) error, *validatorState) {
	state := &validatorState{}

	return func(ctx context.Context, a stage.Artifact) error {
		problems, err := check(ctx, a)
		if err != nil {
			return fmt.Errorf("the %s were not judged (%w); submit again", what, err)
		}

		state.mu.Lock()
		defer state.mu.Unlock()

		state.rounds++
		state.findings = problems

		if len(problems) == 0 || state.rounds >= maxContractRounds {
			return nil
		}

		return fmt.Errorf("%d problem(s) with the %s; fix them and submit again (submit %d of %d):\n%s",
			len(problems), what, state.rounds, maxContractRounds, strings.Join(problems, "\n"))
	}, state
}

// problemsKey is the artifact data key for the problems the bounded checks
// still found on the submit they accepted; it is the controller's on every
// stage, whatever the model put there.
const problemsKey = "check_problems"

// stageChecks are a run's submit checks that keep what they found: the cards
// check, nil unless the stage checks cards, and every bounded check.
type stageChecks struct {
	cards   *validatorState
	bounded []*validatorState
}

// unresolved is what the bounded checks found on the latest submit; once the
// run submitted, the problems its artifact was accepted with.
func (s stageChecks) unresolved() []string {
	var out []string

	for _, b := range s.bounded {
		out = append(out, b.last()...)
	}

	return out
}

// keepProblems returns data with problems under problemsKey, or without the
// key when there are none.
func keepProblems(data map[string]any, problems []string) map[string]any {
	if _, set := data[problemsKey]; !set && len(problems) == 0 {
		return data
	}

	data = maps.Clone(data)
	if data == nil {
		data = map[string]any{}
	}

	if len(problems) == 0 {
		delete(data, problemsKey)
	} else {
		data[problemsKey] = problems
	}

	return data
}

// problemsOf reads the problems an artifact was accepted with.
func problemsOf(a store.Artifact) []string {
	var data struct {
		Problems []string `json:"check_problems"`
	}

	if len(a.Data) > 0 && json.Unmarshal(a.Data, &data) != nil {
		return []string{"the stored check problems do not decode"}
	}

	return data.Problems
}

// problem is one finding of a stage check: where, the check's name, why.
func problem(where, check, message string) string {
	return where + ": " + check + ": " + message
}
