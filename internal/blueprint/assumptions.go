package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Assumption statuses: what a later stage found in the code, verified or
// contradicted; unverified until then.
const (
	assumptionVerified     = "verified"
	assumptionContradicted = "contradicted"
	assumptionUnverified   = "unverified"
)

// assumption is one decision a stage made that nobody grounded in the
// user's words, with the latest verdict on it.
type assumption struct {
	ID       string
	Stage    string
	Text     string
	Status   string
	Evidence string
}

// verdict is a later stage's finding on one assumption.
type verdict struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

// assumptions lists the blueprint's assumptions with the latest verdict on
// each. The stated ones come from the latest artifact of every stage that
// checks assumptions, numbered A1, A2, ... in pack order and then submit
// order, so a number stays put while those artifacts do. Verdicts come from
// every stage's latest artifact, a later stage's winning; one naming no
// assumption is ignored. A verdict applies only when its artifact was
// stored after the latest artifact of every stating stage up to the
// assumption's own: ids are positional, so an older verdict would judge
// whatever sentence holds the id now.
func (c *Controller) assumptions(ctx context.Context, id string) ([]assumption, error) {
	all, err := c.opts.Store.ListArtifacts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: assumptions: %w", err)
	}

	// latest maps a stage to the position of its newest artifact in all,
	// which is in storage order.
	latest := map[string]int{}
	for i, art := range all {
		latest[art.Stage] = i
	}

	var (
		out    []assumption
		source []int
		newest = -1
	)

	for _, st := range c.opts.Pack.Stages {
		i, ok := latest[st.ID]
		if !ok || !slices.Contains(st.Checks, pack.CheckAssumptions) {
			continue
		}

		// An id's number depends on every earlier stating stage, so a
		// re-run of one shifts the ids after it: source is the newest
		// artifact among the stating stages up to this one.
		newest = max(newest, i)

		for _, text := range statedAssumptions(all[i]) {
			out = append(out, assumption{ID: fmt.Sprintf("A%d", len(out)+1), Stage: st.ID, Text: text, Status: assumptionUnverified})
			source = append(source, newest)
		}
	}

	for _, st := range c.opts.Pack.Stages {
		i, ok := latest[st.ID]
		if !ok {
			continue
		}

		verdicts, skipped := verdictsOf(all[i])
		if skipped > 0 {
			c.opts.Logger.Warn("blueprint: verdicts skipped as malformed", "blueprint", id, "stage", st.ID, "version", all[i].Version, "skipped", skipped)
		}

		for _, v := range verdicts {
			for j := range out {
				if out[j].ID == v.ID && i > source[j] {
					out[j].Status, out[j].Evidence = v.Status, v.Evidence
				}
			}
		}
	}

	return out, nil
}

// statedAssumptions reads an artifact's assumptions, its data's list of
// sentences, blanks dropped. The data is a model's submission, which the
// submit check lets through after its last round, so a malformed list reads
// as none rather than failing the pipeline.
func statedAssumptions(art store.Artifact) []string {
	var data struct {
		Assumptions []any `json:"assumptions"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &data) != nil {
		return nil
	}

	var out []string

	for _, a := range data.Assumptions {
		if s, ok := a.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}

	return out
}

// verdictsOf reads an artifact's verdicts on assumptions and counts the
// entries it skipped: one that is not an object of the right shape, or whose
// status is not a verdict. Like statedAssumptions, the data is a model's
// submission, and no check guards a stage that only grounds, so a malformed
// entry costs only itself.
func verdictsOf(art store.Artifact) ([]verdict, int) {
	var data struct {
		Verdicts json.RawMessage `json:"verdicts"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &data) != nil || len(data.Verdicts) == 0 {
		return nil, 0
	}

	var entries []json.RawMessage
	if json.Unmarshal(data.Verdicts, &entries) != nil {
		return nil, 1
	}

	var (
		out     []verdict
		skipped int
	)

	for _, raw := range entries {
		var v verdict
		if json.Unmarshal(raw, &v) != nil {
			skipped++

			continue
		}

		v.ID, v.Evidence = strings.TrimSpace(v.ID), strings.TrimSpace(v.Evidence)
		v.Status = strings.ToLower(strings.TrimSpace(v.Status))

		switch v.Status {
		case assumptionVerified, assumptionContradicted, assumptionUnverified:
			out = append(out, v)
		default:
			skipped++
		}
	}

	return out, skipped
}

// signoffOrder puts first what a human should look at first: unverified,
// then contradicted, then verified, each in id order.
func signoffOrder(list []assumption) []assumption {
	rank := map[string]int{assumptionUnverified: 0, assumptionContradicted: 1, assumptionVerified: 2}

	out := slices.Clone(list)
	slices.SortStableFunc(out, func(a, b assumption) int { return rank[a.Status] - rank[b.Status] })

	return out
}

// assumptionLines renders assumptions one per line: the id, the status with
// any evidence, then the sentence. The sign-off's text entry formats the
// same line from the event payload.
func assumptionLines(list []assumption) string {
	lines := make([]string, 0, len(list))

	for _, a := range list {
		status := a.Status
		if a.Evidence != "" {
			status += ": " + a.Evidence
		}

		lines = append(lines, fmt.Sprintf("- %s (%s) %s", a.ID, status, a.Text))
	}

	return strings.Join(lines, "\n")
}

// assumptionsPayload carries assumptions in an event.
func assumptionsPayload(list []assumption) []any {
	out := make([]any, 0, len(list))

	for _, a := range list {
		out = append(out, map[string]any{"id": a.ID, "stage": a.Stage, "text": a.Text, "status": a.Status, "evidence": a.Evidence})
	}

	return out
}

// assumptionsValidator refuses a submit whose assumptions are not
// standalone sentences. The third failing submit is accepted as it is, so a
// stage never loops until its turns are spent.
func (c *Controller) assumptionsValidator() func(context.Context, stage.Artifact) error {
	lists := c.opts.Pack.Lists()

	var (
		mu     sync.Mutex
		rounds int
	)

	return func(_ context.Context, a stage.Artifact) error {
		problems := assumptionProblems(a.Data, lists)

		mu.Lock()
		defer mu.Unlock()

		rounds++

		if len(problems) == 0 || rounds >= maxContractRounds {
			return nil
		}

		return fmt.Errorf("%d problem(s) with the assumptions; fix them and submit again (submit %d of %d):\n%s",
			len(problems), rounds, maxContractRounds, strings.Join(problems, "\n"))
	}
}

// assumptionProblems checks a submit's assumptions: a list of sentences,
// each non-empty and pointing at no text a card will not carry.
func assumptionProblems(data map[string]any, lists contract.Lists) []string {
	raw, ok := data["assumptions"]
	if !ok || raw == nil {
		return nil
	}

	items, ok := raw.([]any)
	if !ok {
		return []string{"assumptions must be a list of sentences"}
	}

	var out []string

	for i, item := range items {
		s, ok := item.(string)

		switch {
		case !ok || strings.TrimSpace(s) == "":
			out = append(out, fmt.Sprintf("assumption %d must be a sentence", i+1))
		case contract.DanglingReference(s, lists) != "":
			out = append(out, fmt.Sprintf("assumption %d says %q, which points at text a card will not carry; state the fact itself",
				i+1, contract.DanglingReference(s, lists)))
		}
	}

	return out
}
