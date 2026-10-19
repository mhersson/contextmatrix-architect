package blueprint

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// readinessValidator refuses a readiness verdict that does not stand, as
// boundedValidator bounds it: the draft is every card of the stage it loops
// to, read at each submit.
func (c *Controller) readinessValidator(bp store.Blueprint, key string) (func(context.Context, stage.Artifact) error, *validatorState) {
	st, _ := c.stepAt(key)

	return boundedValidator("readiness verdict", func(ctx context.Context, a stage.Artifact) ([]string, error) {
		var draft []string

		if st.LoopsTo != "" {
			steps, _, err := c.steps(ctx, bp.ID)
			if err != nil {
				return nil, err
			}

			if _, draft, err = c.targetCards(ctx, bp.ID, loopTargets(steps, st.LoopsTo)); err != nil {
				return nil, err
			}
		}

		return readinessProblems(a.Data, draft), nil
	})
}

const cardEntryShape = `{"id": "<card id>", "ready": true or false, "findings": ["<sentence>"]}`

// readinessProblems checks a readiness verdict: cards, one entry per card
// of draft, ready with no finding or not ready with the findings that block
// it; and cross, the findings across the blueprint (crossProblems). Without
// a draft no card is unjudged or unknown.
func readinessProblems(data map[string]any, draft []string) []string {
	var out []string

	items, isList := data["cards"].([]any)
	if !isList {
		out = append(out, problem("cards", "readiness_shape", "cards must be a list of "+cardEntryShape+" entries"))
	}

	judged := map[string]bool{}

	for i, item := range items {
		where := fmt.Sprintf("card %d", i+1)

		e, isObject := item.(map[string]any)
		id, _ := e["id"].(string)
		ready, isFlag := e["ready"].(bool)
		findings, findingsOK := texts(e["findings"])

		// Ids match the loop's, which reads them untrimmed.
		if strings.TrimSpace(id) != "" {
			where = id
		}

		switch {
		case !isObject || strings.TrimSpace(id) == "" || !isFlag || !findingsOK:
			out = append(out, problem(where, "card_entry", "each card entry is "+cardEntryShape))

			// A card with an id counts as judged, so it gets this one
			// problem and not also card_unjudged.
			if isObject && strings.TrimSpace(id) != "" {
				judged[id] = true
			}

			continue
		case judged[id]:
			out = append(out, problem(id, "card_entry", "the card is judged twice; give it one entry"))
		case ready && len(findings) > 0:
			out = append(out, problem(id, "card_entry", "a card with a finding is not ready"))
		case !ready && len(findings) == 0:
			out = append(out, problem(id, "card_entry", "a card that is not ready names the findings that block it"))
		}

		judged[id] = true

		if len(draft) > 0 && !slices.Contains(draft, id) {
			out = append(out, problem(id, "card_unknown", "no card of the draft has this id"))
		}
	}

	for _, id := range draft {
		if !judged[id] {
			out = append(out, problem(id, "card_unjudged", "the card has no entry; judge every card of the draft"))
		}
	}

	return append(out, crossProblems(data["cross"])...)
}

// texts reads a list of non-empty strings; absent is none, and ok is false
// for anything else.
func texts(v any) ([]string, bool) {
	if v == nil {
		return nil, true
	}

	items, isList := v.([]any)
	if !isList {
		return nil, false
	}

	out := make([]string, 0, len(items))

	for _, item := range items {
		s, isText := item.(string)
		if !isText || strings.TrimSpace(s) == "" {
			return nil, false
		}

		out = append(out, s)
	}

	return out, true
}

var crossForm = regexp.MustCompile(`^X[1-9][0-9]*$`)

const crossShape = `{"id": "X<n>", "severity": "critical", "major" or "minor", "text": "<the finding>", "refs": ["<an id it names>"]}`

// crossProblems checks readiness's findings across the blueprint: absent,
// or a list of entries, each X<n> once, with a severity of the set, its
// text and the ids it names.
func crossProblems(v any) []string {
	if v == nil {
		return nil
	}

	items, isList := v.([]any)
	if !isList {
		return []string{problem("cross", "cross_shape", "cross must be a list of "+crossShape+" entries")}
	}

	var out []string

	seen := map[string]bool{}

	for i, item := range items {
		where := fmt.Sprintf("cross %d", i+1)

		e, isObject := item.(map[string]any)
		if !isObject {
			out = append(out, problem(where, "cross_shape", "each finding is "+crossShape))

			continue
		}

		id, _ := e["id"].(string)
		severity, _ := e["severity"].(string)
		text, _ := e["text"].(string)
		_, refsOK := texts(e["refs"])

		if id = strings.TrimSpace(id); id != "" {
			where = id
		}

		switch {
		case !crossForm.MatchString(id):
			out = append(out, problem(where, "cross_id", "a finding's id is X<n>, such as X1"))
		case seen[id]:
			out = append(out, problem(where, "cross_id", "the id is used twice"))
		}

		seen[id] = true

		if !slices.Contains([]string{severityCritical, severityMajor, severityMinor}, severity) {
			out = append(out, problem(where, "cross_severity", "severity is critical, major or minor"))
		}

		if strings.TrimSpace(text) == "" {
			out = append(out, problem(where, "cross_text", "a finding needs its text"))
		}

		if !refsOK {
			out = append(out, problem(where, "cross_refs", `refs is a list of the ids the finding names, such as ["DEC2", "AC4"]`))
		}
	}

	return out
}
