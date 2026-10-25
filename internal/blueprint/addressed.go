package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// addressedKey is the data key under which a builder round after a review
// lists the findings it addressed: [{finding, note}], finding being a
// finding's id as the round was shown it (R1, R2, ...).
const addressedKey = "addressed"

// addressed is one finding a builder round says it addressed, and how.
type addressed struct {
	finding string
	note    string
}

// addressedValidator refuses a builder round's submit whose addressed list
// is malformed or names a finding the round was not given; ids are the
// findings it was given. It is bounded, so a third failing submit is
// accepted with its problems kept.
func addressedValidator(ids []string) (func(context.Context, stage.Artifact) error, *validatorState) {
	return boundedValidator("addressed findings", func(_ context.Context, a stage.Artifact) ([]string, error) {
		return addressedProblems(a.Data, ids), nil
	})
}

// addressedProblems checks a submit's addressed list against the ids its
// round was given. No list is no problem: a round need not say.
func addressedProblems(data map[string]any, ids []string) []string {
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
		id, isID := e["finding"].(string)
		note, hasNote := e["note"]
		_, noteIsText := note.(string)

		switch {
		case !isObject || !isID || strings.TrimSpace(id) == "":
			problems = append(problems, problem(where, "addressed_shape", `each entry needs finding, the id of a finding as the round listed it, such as "R1", and a note`))
		case hasNote && !noteIsText:
			problems = append(problems, problem(where, "addressed_shape", "note must be a sentence"))
		case !slices.Contains(ids, id):
			problems = append(problems, problem(where, "addressed_unknown",
				fmt.Sprintf("finding %s was not given to this round; its findings are %s", id, strings.Join(ids, ", "))))
		}
	}

	return problems
}

// addressedOf reads the findings an artifact says it addressed, by id. The
// data is a model's, accepted after its round's last check, so an entry that
// does not read is skipped. A revision stored before findings had ids names
// them by number: n is R<n>.
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
			Finding any    `json:"finding"`
			Note    string `json:"note"`
		}

		if json.Unmarshal(raw, &e) != nil {
			continue
		}

		id := ""

		switch f := e.Finding.(type) {
		case string:
			if findingNumber(f) > 0 {
				id = f
			}
		case float64:
			if f >= 1 && f <= math.MaxInt32 && f == math.Trunc(f) {
				id = findingID(int(f))
			}
		}

		if id != "" {
			out = append(out, addressed{finding: id, note: e.Note})
		}
	}

	return out
}

// addressedClaims maps each finding an artifact says it addressed to its
// note: the builder's claim, never the verdict.
func addressedClaims(a store.Artifact) map[string]string {
	out := map[string]string{}

	for _, e := range addressedOf(a) {
		out[e.finding] = e.note
	}

	return out
}
