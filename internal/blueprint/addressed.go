package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// addressedKey is the data key under which a builder round after a review
// lists the findings it addressed: [{finding, note}], finding being the
// number the round was shown, 1 for the first.
const addressedKey = "addressed"

// addressed is one finding a builder round says it addressed, and how.
type addressed struct {
	finding int
	note    string
}

// addressedValidator refuses a builder round's submit whose addressed list
// is malformed or names a finding the round was not given; the round was
// given n findings, numbered from 1. It is bounded like every other check,
// so a third failing submit is accepted with its problems kept.
func addressedValidator(n int) (func(context.Context, stage.Artifact) error, *validatorState) {
	return boundedValidator("addressed findings", func(_ context.Context, a stage.Artifact) ([]string, error) {
		return addressedProblems(a.Data, n), nil
	})
}

// addressedProblems checks a submit's addressed list against the n findings
// its round was given. No list is no problem: a round need not say.
func addressedProblems(data map[string]any, n int) []string {
	raw, ok := data[addressedKey]
	if !ok || raw == nil {
		return nil
	}

	items, isList := raw.([]any)
	if !isList {
		return []string{problem(addressedKey, "addressed_shape", "addressed must be a list of {finding, note} entries")}
	}

	var problems []string

	for i, item := range items {
		where := fmt.Sprintf("%s[%d]", addressedKey, i+1)
		e, isObject := item.(map[string]any)
		number, isNumber := e["finding"].(float64)
		note, hasNote := e["note"]
		_, noteIsText := note.(string)

		switch {
		case !isObject || !isNumber || number != math.Trunc(number):
			problems = append(problems, problem(where, "addressed_shape", "each entry needs finding, the number of a finding as the round listed it, and a note"))
		case hasNote && !noteIsText:
			problems = append(problems, problem(where, "addressed_shape", "note must be a sentence"))
		case number < 1 || number > float64(n):
			problems = append(problems, problem(where, "addressed_unknown",
				fmt.Sprintf("finding %d does not exist; the reviewer's findings are numbered 1 to %d", int(number), n)))
		}
	}

	return problems
}

// addressedOf reads the findings an artifact says it addressed. The data is
// a model's, accepted after its round's last check, so an entry that does
// not read is skipped rather than failing the reader.
func addressedOf(a store.Artifact) []addressed {
	var data struct {
		Addressed []json.RawMessage `json:"addressed"`
	}

	if len(a.Data) == 0 || json.Unmarshal(a.Data, &data) != nil {
		return nil
	}

	var out []addressed

	for _, raw := range data.Addressed {
		var e struct {
			Finding float64 `json:"finding"`
			Note    string  `json:"note"`
		}

		if json.Unmarshal(raw, &e) != nil || e.Finding < 1 || e.Finding != math.Trunc(e.Finding) {
			continue
		}

		out = append(out, addressed{finding: int(e.Finding), note: e.Note})
	}

	return out
}
