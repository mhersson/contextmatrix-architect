package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Assumption statuses. A later stage's verdict on the code is verified or
// contradicted. Before one, the answer an assumption rests on decides:
// confirmed by a human's, assumed by the responder's, verified when the
// responder's own tool calls back it; unverified without a basis.
const (
	assumptionVerified     = "verified"
	assumptionContradicted = "contradicted"
	assumptionUnverified   = "unverified"
	assumptionAssumed      = "assumed"
	assumptionConfirmed    = "confirmed"
)

// assumption is one decision a stage made that nobody grounded in the
// user's words, with the answered question it rests on, if any, and the
// latest verdict on it. Answer is that question's answer on a stated
// assumption; an auto answer's own Q<n> entry holds it in its text.
type assumption struct {
	ID       string
	Stage    string
	Text     string
	Basis    string
	Answer   string
	Status   string
	Evidence string
}

// stated is one assumption as a stage submitted it.
type stated struct {
	Text, Basis string
}

// verdict is a later stage's finding on one assumption.
type verdict struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

// assumptions lists the blueprint's assumptions with the latest status of
// each. The stated ones come from the latest artifact of every stage that
// checks assumptions, numbered A1, A2, ... in pack order and then submit
// order, so a number stays put while those artifacts do. Every auto answer
// follows as Q<n>, resting on itself, unless a stated one rests on it and so
// carries its status and the answer. Each starts with its basis's status
// (basisStatus). Verdicts come from every stage's latest artifact, a later
// stage's winning; one naming no listed assumption is ignored. A verdict
// applies only when its artifact was stored after the latest artifact of
// every stating stage up to the assumption's own: ids are positional, so an
// older verdict would judge whatever sentence holds the id now.
func (c *Controller) assumptions(ctx context.Context, id string) ([]assumption, error) {
	all, err := c.opts.Store.ListArtifacts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: assumptions: %w", err)
	}

	questions, err := c.opts.Store.ListQuestions(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: assumptions: %w", err)
	}

	evidence, err := answerEvidence(ctx, c.opts.Store, id)
	if err != nil {
		return nil, err
	}

	acceptances, err := c.acceptedAssumptions(ctx, id)
	if err != nil {
		return nil, err
	}

	accepted := acceptedTags(acceptances)

	answered := map[string]store.Question{}
	for _, q := range questions {
		answered[q.Tag()] = q
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
		named  = map[string]bool{}
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

		for _, s := range statedAssumptions(all[i]) {
			// The submit check keeps a basis naming no answered question or
			// accepted assumption on its last round; it reads as none.
			switch _, ok := answered[s.Basis]; {
			case ok:
				named[s.Basis] = true
			case !accepted[s.Basis]:
				s.Basis = ""
			}

			status, proof := basisStatus(answered, accepted, evidence, s.Basis)
			out = append(out, assumption{
				ID: fmt.Sprintf("A%d", len(out)+1), Stage: st.ID, Text: s.Text, Basis: s.Basis, Answer: answered[s.Basis].Answer,
				Status: status, Evidence: proof,
			})
			source = append(source, newest)
		}
	}

	for _, q := range questions {
		if !q.Assumption || named[q.Tag()] {
			continue
		}

		status, proof := basisStatus(answered, accepted, evidence, q.Tag())
		out = append(out, assumption{
			ID: q.Tag(), Stage: q.Stage, Basis: q.Tag(), Status: status, Evidence: proof,
			Text: fmt.Sprintf("%s The responder answered: %s", q.Text, q.Answer),
		})
		// A Q<n> id is stable, so any verdict applies to it.
		source = append(source, -1)
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
				if out[j].ID != v.ID || i <= source[j] {
					continue
				}

				// Not finding it in the code never hides what an answer
				// settled, such as a human's confirmation.
				if v.Status == assumptionUnverified {
					out[j].Status, out[j].Evidence = basisStatus(answered, accepted, evidence, out[j].Basis)
				} else {
					out[j].Status, out[j].Evidence = v.Status, v.Evidence
				}
			}
		}
	}

	return out, nil
}

// basisStatus is the status and evidence an assumption resting on basis, an
// answered question's or an accepted assumption's tag or "", starts with and
// returns to on an unverified verdict. evidence is answerEvidence's.
func basisStatus(answered map[string]store.Question, accepted map[string]bool, evidence map[int]string, basis string) (string, string) {
	q, ok := answered[basis]

	switch {
	case accepted[basis]:
		return assumptionConfirmed, ""
	case !ok:
		return assumptionUnverified, ""
	case !q.Assumption:
		return assumptionConfirmed, ""
	case q.Grounded && evidence[q.Number] != "":
		return assumptionVerified, evidence[q.Number]
	default:
		return assumptionAssumed, ""
	}
}

// acceptedTags is the set of accepted's tags.
func acceptedTags(accepted []acceptedAssumption) map[string]bool {
	out := map[string]bool{}
	for _, a := range accepted {
		out[a.Tag] = true
	}

	return out
}

var acceptanceTagForm = regexp.MustCompile(`^ROUND(\d+)\.A(\d+)$`)

// normalTag reads a tag a model wrote, such as "[q1]" or "[Round2.a1]", as
// the controller prints it, "Q1" or "round2.A1": tags are printed to the
// model bracketed.
func normalTag(s string) string {
	tag := strings.ToUpper(strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "[]")))

	if m := acceptanceTagForm.FindStringSubmatch(tag); m != nil {
		return "round" + m[1] + ".A" + m[2]
	}

	return tag
}

// statedAssumptions reads an artifact's assumptions, its data's list of
// sentences or {text, basis} entries, blanks dropped and bases normalised.
// The data is a model's submission, which the submit check lets through
// after its last round, so a malformed list reads as none and a malformed
// entry as nothing, rather than failing the pipeline.
func statedAssumptions(art store.Artifact) []stated {
	var data struct {
		Assumptions []any `json:"assumptions"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &data) != nil {
		return nil
	}

	var out []stated

	for _, a := range data.Assumptions {
		var s stated

		switch a := a.(type) {
		case string:
			s.Text = a
		case map[string]any:
			s.Text, _ = a["text"].(string)
			basis, _ := a["basis"].(string)
			s.Basis = normalTag(basis)
		}

		if s.Text = strings.TrimSpace(s.Text); s.Text != "" {
			out = append(out, s)
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
// then contradicted, assumed, confirmed and verified, each in id order.
func signoffOrder(list []assumption) []assumption {
	rank := map[string]int{
		assumptionUnverified: 0, assumptionContradicted: 1, assumptionAssumed: 2, assumptionConfirmed: 3, assumptionVerified: 4,
	}

	out := slices.Clone(list)
	slices.SortStableFunc(out, func(a, b assumption) int { return rank[a.Status] - rank[b.Status] })

	return out
}

// assumptionLines renders assumptions one per line for models and cards: the
// id, the status with who answered the question it rests on or any
// evidence, and the answer it rests on, then the sentence. The sign-off's
// text entry formats its line from the event payload.
func assumptionLines(list []assumption) string {
	lines := make([]string, 0, len(list))

	// "the user", not "you": a card's reader is the agent.
	backer := map[string]string{assumptionConfirmed: "the user", assumptionAssumed: "the responder"}

	for _, a := range list {
		status := a.Status

		if who, ok := backer[a.Status]; ok {
			status += " by " + who
			if a.Basis != a.ID {
				status += ", " + a.Basis
			}

			if a.Answer != "" {
				status += ": " + a.Answer
			}
		} else {
			if a.Evidence != "" {
				status += ": " + a.Evidence
			}

			if a.Answer != "" {
				status += "; rests on " + a.Basis + ": " + a.Answer
			}
		}

		lines = append(lines, fmt.Sprintf("- %s (%s) %s", a.ID, status, a.Text))
	}

	return strings.Join(lines, "\n")
}

// assumptionsPayload carries assumptions in an event.
func assumptionsPayload(list []assumption) []any {
	out := make([]any, 0, len(list))

	for _, a := range list {
		out = append(out, map[string]any{
			"id": a.ID, "stage": a.Stage, "text": a.Text, "basis": a.Basis, "answer": a.Answer, "status": a.Status, "evidence": a.Evidence,
		})
	}

	return out
}

// assumptionsValidator refuses a submit whose assumptions are not
// standalone sentences resting on the blueprint's answered questions and
// accepted assumptions, as boundedValidator bounds it.
func (c *Controller) assumptionsValidator(bp store.Blueprint) (func(context.Context, stage.Artifact) error, *validatorState) {
	lists := c.opts.Pack.Lists()

	return boundedValidator("assumptions", func(ctx context.Context, a stage.Artifact) ([]string, error) {
		questions, err := c.opts.Store.ListQuestions(ctx, bp.ID)
		if err != nil {
			return nil, fmt.Errorf("read the questions: %w", err)
		}

		accepted, err := c.acceptedAssumptions(ctx, bp.ID)
		if err != nil {
			return nil, err
		}

		bases := acceptedTags(accepted)
		for _, q := range questions {
			bases[q.Tag()] = true
		}

		return assumptionProblems(a.Data, lists, bases), nil
	})
}

// tabValidator refuses a submit of st, as boundedValidator bounds it, that
// states an assumption without a basis while st's policy, read at submit,
// is human, or that comes before an assumption the user turned into a
// question on a round of the attempt has no later round of the attempt posted.
func (c *Controller) tabValidator(bp store.Blueprint, st pack.Stage, attempt int) (func(context.Context, stage.Artifact) error, *validatorState) {
	return boundedValidator("assumptions tab", func(ctx context.Context, a stage.Artifact) ([]string, error) {
		policy, err := c.policy(ctx, bp, st.ID)
		if err != nil {
			return nil, err
		}

		var out []string

		if policy == pack.PolicyHuman {
			out = unbasedAssumptions(a.Data)
		}

		unasked, err := c.unaskedConversions(ctx, bp.ID, st.ID, attempt)
		if err != nil {
			return nil, err
		}

		return append(out, unasked...), nil
	})
}

// unbasedAssumptions reports every stated assumption that names no basis.
// The assumptions check reports a malformed list or entry.
func unbasedAssumptions(data map[string]any) []string {
	items, _ := data["assumptions"].([]any)

	var out []string

	for i, item := range items {
		text, basis := "", ""

		switch e := item.(type) {
		case string:
			text = e
		case map[string]any:
			text, _ = e["text"].(string)
			basis, _ = e["basis"].(string)
		default:
			continue
		}

		if strings.TrimSpace(text) != "" && normalTag(basis) == "" {
			out = append(out, fmt.Sprintf("assumption %d has no basis: show it on a round's assumptions tab first; state an accepted one with its tag (round<n>.A<k>) and one resting on an answered question with its Q<n>", i+1))
		}
	}

	return out
}

// unaskedConversions reports each assumption the user turned into a question
// on a round a run of stageID's attempt posted, when no run of the attempt
// posted a round after it.
func (c *Controller) unaskedConversions(ctx context.Context, id, stageID string, attempt int) ([]string, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	ofAttempt := map[int64]bool{}

	for _, r := range runs {
		if r.Kind == store.RunKindStage && r.Stage == stageID && r.Attempt == attempt {
			ofAttempt[r.ID] = true
		}
	}

	rounds, err := c.rounds(ctx, id)
	if err != nil {
		return nil, err
	}

	newest := 0

	for _, s := range rounds {
		if ofAttempt[s.PostedBy] {
			newest = max(newest, s.Number)
		}
	}

	var out []string

	for _, s := range rounds {
		if !ofAttempt[s.PostedBy] || s.Number < newest {
			continue
		}

		for _, id := range s.Converted {
			text := ""

			for _, a := range s.Frame.Assumptions {
				if a.ID == id {
					text = a.Text
				}
			}

			out = append(out, fmt.Sprintf("round %d's %s (%s) was turned into a question; ask it in a round before you submit", s.Number, id, text))
		}
	}

	return out, nil
}

var basisTags = regexp.MustCompile(`\b(?:Q\d+|(?i:round\d+\.a\d+))\b`)

// assumptionProblems checks a submit's assumptions: a list of entries, each
// a sentence or {text, basis}; every sentence non-empty, pointing at no text
// a card will not carry and naming none of bases, the tags of the answered
// questions and accepted assumptions, which belong in the basis so that no
// prose says who answered; every basis one of bases.
func assumptionProblems(data map[string]any, lists contract.Lists, bases map[string]bool) []string {
	raw, ok := data["assumptions"]
	if !ok || raw == nil {
		return nil
	}

	items, ok := raw.([]any)
	if !ok {
		return []string{`assumptions must be a list of sentences or {"text": "<sentence>", "basis": "<tag>"} entries, the tag an answered question's (Q<n>) or an accepted assumption's (round<n>.A<k>)`}
	}

	var out []string

	for i, item := range items {
		text, isText := item.(string)

		if entry, ok := item.(map[string]any); ok {
			text, isText = entry["text"].(string)

			if basis, ok := entry["basis"]; ok && basis != nil {
				tag, ok := basis.(string)

				switch {
				case !ok:
					out = append(out, fmt.Sprintf("assumption %d's basis must be an answered question's tag (Q<n>) or an accepted assumption's (round<n>.A<k>)", i+1))
				case normalTag(tag) != "" && !bases[normalTag(tag)]:
					out = append(out, fmt.Sprintf("assumption %d's basis %q is no answered question (Q<n>) or accepted assumption (round<n>.A<k>); give one or no basis", i+1, tag))
				}
			}
		}

		if !isText || strings.TrimSpace(text) == "" {
			out = append(out, fmt.Sprintf("assumption %d must be a sentence", i+1))

			continue
		}

		if ref := contract.DanglingReference(text, lists); ref != "" {
			out = append(out, fmt.Sprintf("assumption %d says %q, which points at text a card will not carry; state the fact itself", i+1, ref))
		}

		for _, named := range basisTags.FindAllString(text, -1) {
			if tag := normalTag(named); bases[tag] {
				out = append(out, fmt.Sprintf(`assumption %d names %s; give it as the basis ({"text": "<sentence>", "basis": %q}) and state the fact itself`, i+1, named, tag))

				break
			}
		}
	}

	return out
}
