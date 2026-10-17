package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
	"github.com/mhersson/contextmatrix-harness/llm"
)

// answer is a responder's reply: the answer, why, whether a tool result
// settles it, and the source tags and findings that say so.
type answer struct {
	Answer    string `json:"answer"`
	Reasoning string `json:"reasoning"`
	Grounded  bool   `json:"grounded"`
	Evidence  string `json:"evidence"`
}

var (
	evidenceTags = regexp.MustCompile(`\[\s*(S\d+(?:\s*,\s*S\d+)*)\s*\]`)
	leadingTags  = regexp.MustCompile(`^\s*\[\s*(S\d+(?:\s*,\s*S\d+)*)\s*\]`)
)

// citesToolResult reports whether evidence cites a source tag that the
// leading bracket of a tool message carries. Tags in other roles' text or
// deeper in a result do not count: only a tool call of this run vouches.
func citesToolResult(evidence string, msgs []llm.Message) bool {
	returned := map[string]bool{}

	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}

		if lead := leadingTags.FindStringSubmatch(m.Content); lead != nil {
			for tag := range strings.SplitSeq(lead[1], ",") {
				returned[strings.TrimSpace(tag)] = true
			}
		}
	}

	for _, cited := range evidenceTags.FindAllStringSubmatch(evidence, -1) {
		for tag := range strings.SplitSeq(cited[1], ",") {
			if returned[strings.TrimSpace(tag)] {
				return true
			}
		}
	}

	return false
}

// answerEvidence maps a question number to the evidence its auto answer
// event carries. An event from before evidence was recorded has none.
func answerEvidence(ctx context.Context, s *store.Store, blueprintID string) (map[int]string, error) {
	events, err := s.ListEventsOfKind(ctx, blueprintID, EventAnswer)
	if err != nil {
		return nil, fmt.Errorf("blueprint: answer evidence: %w", err)
	}

	out := map[int]string{}

	for _, e := range events {
		var p struct {
			Number    int    `json:"number"`
			Responder string `json:"responder"`
			Evidence  string `json:"evidence"`
		}

		if json.Unmarshal(e.Payload, &p) != nil || p.Responder != store.ResponderAuto {
			continue
		}

		out[p.Number] = p.Evidence
	}

	return out, nil
}

// parseAnswer reads a responder's submit; the answer is required.
func parseAnswer(data map[string]any) (answer, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return answer{}, fmt.Errorf("encode the answer: %w", err)
	}

	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return answer{}, fmt.Errorf("the answer does not decode: %w", err)
	}

	a.Answer, a.Reasoning, a.Evidence = strings.TrimSpace(a.Answer), strings.TrimSpace(a.Reasoning), strings.TrimSpace(a.Evidence)
	if a.Answer == "" {
		return answer{}, errors.New("answer is required")
	}

	return a, nil
}

// respond answers a question under the auto policy with a responder run:
// the asking stage's persona and tools, the responder instructions, and
// responderTask, never the asker's transcript. The answer is recorded as an
// assumption, grounded only when its evidence cites a tool result of its
// own run. A pack without responder instructions, or a responder that
// fails, leaves the stage to decide; a cancel ends the asking run too. The
// responder spends from the asking run's cap.
func (a *asker) respond(ctx context.Context, q stage.Question, number int) (string, error) {
	c := a.c
	bg := context.WithoutCancel(ctx)

	if c.opts.Pack.ResponderInstructions() == "" {
		return "", errNoResponder
	}

	undecided := func(err error) error {
		return fmt.Errorf("no answer (%w); decide yourself and, where this stage records assumptions, record the decision as one", err)
	}

	st, _ := c.opts.Pack.Stage(a.stage)

	task, err := c.responderTask(bg, a.bp, st, a.attempt, q)
	if err != nil {
		return "", undecided(err)
	}

	out, err := c.runSide(ctx, a.run, a.sess, a.bp, sideRun{
		kind: store.RunKindResponder, stage: a.stage, persona: st.Persona, attempt: a.attempt, round: number,
		parent: a.sink,
		in: stage.Input{
			Stage: a.stage + ".responder", Persona: c.opts.Pack.Persona(st.Persona),
			Instructions: c.opts.Pack.ResponderInstructions(), Task: task, Tools: a.tools,
			Validate: func(_ context.Context, art stage.Artifact) error {
				_, err := parseAnswer(art.Data)

				return err
			},
		},
	})

	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		var refused refusal
		if errors.As(err, &refused) {
			// The stage still runs and decides, so this is not the blueprint's failure.
			a.audit.event(bg, Event{Kind: EventError, BlueprintID: a.bp.ID, RunID: a.runID, Payload: map[string]any{
				"stage": a.stage, "reason": "responder_refused",
				"message": fmt.Sprintf("The responder for %s did not run: %s. The stage decides Q%d itself.", a.stage, refused.reason, number),
			}})
		}

		return "", undecided(err)
	}

	ans, err := parseAnswer(out.Artifact.Data)
	if err != nil {
		return "", undecided(err)
	}

	grounded := ans.Grounded && citesToolResult(ans.Evidence, out.Result.Messages)

	evidence := ""
	if grounded {
		evidence = ans.Evidence
	}

	a.audit.event(bg, Event{Kind: EventAnswer, BlueprintID: a.bp.ID, RunID: a.runID, Payload: map[string]any{
		"number": number, "text": ans.Answer, "responder": store.ResponderAuto,
		"reasoning": ans.Reasoning, "grounded": grounded, "evidence": evidence,
	}})

	if err := c.opts.Store.AddQuestion(bg, store.Question{
		BlueprintID: a.bp.ID, RunID: a.runID, Stage: a.stage, Number: number,
		Text: a.audit.text(q.Text), Options: a.audit.texts(q.Options), Context: a.audit.text(q.Context),
		Answer: a.audit.text(ans.Answer), Responder: store.ResponderAuto, Assumption: true, Grounded: grounded,
		AnsweredAt: c.opts.Now().UTC(),
	}); err != nil {
		c.opts.Logger.Error("blueprint: record the auto answer", "blueprint", a.bp.ID, "question", number, "error", err)
	}

	return fmt.Sprintf("Q%d: %s", number, ans.Answer), nil
}

// responderTask is what a responder is given: the target projects, the
// need, the asking stage's inputs, the gate feedback the asking attempt
// addresses, the user's notes, every answer so far and the question with its
// options and context.
func (c *Controller) responderTask(ctx context.Context, bp store.Blueprint, st pack.Stage, attempt int, q stage.Question) (string, error) {
	var b strings.Builder

	if err := c.writeInputs(ctx, &b, bp, st, true, "responder task"); err != nil {
		return "", err
	}

	if st.Gate {
		if err := c.writeGateFeedback(ctx, &b, bp, st, attempt); err != nil {
			return "", err
		}
	}

	if err := c.writeNotes(ctx, &b, bp.ID); err != nil {
		return "", err
	}

	questions, err := c.opts.Store.ListQuestions(ctx, bp.ID)
	if err != nil {
		return "", fmt.Errorf("blueprint: responder task: %w", err)
	}

	if len(questions) > 0 {
		b.WriteString("## Answers so far\n\n")

		writeQuestions(&b, questions)
	}

	fmt.Fprintf(&b, "## The question, asked by %s\n\n%s\n", st.ID, q.Text)

	if q.Context != "" {
		b.WriteString("\n" + q.Context + "\n")
	}

	for i, o := range q.Options {
		marker := ""
		if i+1 == q.Recommended {
			marker = " (the stage recommends this one)"
		}

		fmt.Fprintf(&b, "%d. %s%s\n", i+1, o, marker)
	}

	return b.String(), nil
}
