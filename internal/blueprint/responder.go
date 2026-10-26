package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// answer is a responder's reply: the answer, why, whether a tool result
// settles it, the source tags and findings that say so, and what the answer
// means for the person using the result when they would notice it.
type answer struct {
	Answer      string `json:"answer"`
	Reasoning   string `json:"reasoning"`
	Grounded    bool   `json:"grounded"`
	Evidence    string `json:"evidence"`
	Consequence string `json:"consequence"`
}

// citesToolResult reports whether evidence cites a source tag one of the
// run's tool results led with (stage.Output.SourceTags). Tags in the task, in
// the model's own text or deeper in a result do not count: only a tool call
// of this run vouches.
func citesToolResult(evidence string, returned []string) bool {
	// A citation may carry the place it points at, as [S3 internal/x.go:42].
	for _, cited := range citation.FindAllString(evidence, -1) {
		for _, m := range sourceTag.FindAllStringSubmatch(cited, -1) {
			if slices.Contains(returned, "S"+m[1]) {
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

	a.Consequence = strings.TrimSpace(a.Consequence)

	return a, nil
}

// respond answers a question under the auto policy with a responder run:
// the asking stage's persona and tools, the responder instructions, and
// responderTask, never the asker's transcript. The answer is recorded as an
// assumption, grounded only when its evidence cites a tool result of its
// own run. A responder that fails leaves the stage to decide; a cancel ends
// the asking run too. The responder spends from the asking run's cap; on a
// handed-over round, which no stage run asks, it is capped as a run of its
// own. When q asks the turned-down answer to question reasks again, 0 for
// none, the responder is told the person's concern and the answer
// supersedes that one.
func (iv *interviewer) respond(ctx context.Context, q protocol.RoundQuestion, reasks int) (protocol.RoundAnswer, error) {
	c, au, stageID := iv.c, iv.sess.audit, iv.st.ID
	bg := context.WithoutCancel(ctx)

	undecided := func(err error) error {
		return fmt.Errorf("no answer (%w); decide yourself and, where this stage records assumptions, record the decision as one", err)
	}

	task, err := c.responderTask(bg, iv.bp, iv.st, iv.attempt, iv.poc, q, iv.concerns[q.Number])
	if err != nil {
		return protocol.RoundAnswer{}, undecided(err)
	}

	out, err := c.runSide(ctx, iv.r, iv.sess, iv.bp, sideRun{
		kind: store.RunKindResponder, stage: stageID, persona: iv.st.Persona, attempt: iv.attempt, round: q.Number,
		parent: iv.sink,
		in: stage.Input{
			Stage: stageID + ".responder", Persona: c.opts.Pack.Persona(iv.st.Persona),
			Instructions: withPlain(iv.bp, c.opts.Pack.ResponderInstructions(), c.opts.Pack.PlainInstructions()), Task: task, Tools: iv.tools,
			Validate: func(_ context.Context, art stage.Artifact) error {
				_, err := parseAnswer(art.Data)

				return err
			},
		},
	})

	switch {
	case ctx.Err() != nil:
		return protocol.RoundAnswer{}, ctx.Err()
	case err != nil:
		var refused refusal
		if errors.As(err, &refused) {
			// The stage still runs and decides, so this is not the blueprint's failure.
			au.event(bg, Event{Kind: EventError, BlueprintID: iv.bp.ID, RunID: iv.runID, Payload: map[string]any{
				"stage": stageID, "reason": "responder_refused",
				"message": fmt.Sprintf("The responder for %s did not run: %s. The stage decides Q%d itself.", c.stepName(bg, iv.bp.ID, stageID), refused.reason, q.Number),
			}})
		}

		return protocol.RoundAnswer{}, undecided(err)
	}

	ans, err := parseAnswer(out.Artifact.Data)
	if err != nil {
		return protocol.RoundAnswer{}, undecided(err)
	}

	grounded := ans.Grounded && citesToolResult(ans.Evidence, out.SourceTags)

	evidence := ""
	if grounded {
		evidence = ans.Evidence
	}

	payload := map[string]any{
		"number": q.Number, "text": ans.Answer, "responder": store.ResponderAuto,
		"reasoning": ans.Reasoning, "grounded": grounded, "evidence": evidence,
	}

	if reasks > 0 {
		payload["supersedes"] = reasks
	}

	// Only a plain-language blueprint records a consequence; its asking run
	// may put it on the person's tab.
	if iv.bp.PlainLanguage && ans.Consequence != "" {
		payload["consequence"] = ans.Consequence

		if iv.consequences == nil {
			iv.consequences = map[int]string{}
		}

		iv.consequences[q.Number] = ans.Consequence
	}

	au.event(bg, Event{Kind: EventAnswer, BlueprintID: iv.bp.ID, RunID: iv.runID, Payload: payload})

	options := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		options = append(options, o.Title)
	}

	if err := c.opts.Store.AddQuestion(bg, store.Question{
		BlueprintID: iv.bp.ID, RunID: iv.runID, Stage: stageID, Number: q.Number,
		Text: au.text(q.Text), Options: au.texts(options), Context: au.text(q.Why),
		Answer: au.text(ans.Answer), Responder: store.ResponderAuto, Assumption: true, Grounded: grounded,
		AnsweredAt: c.opts.Now().UTC(),
	}); err != nil {
		c.opts.Logger.Error("blueprint: record the auto answer", "blueprint", iv.bp.ID, "question", q.Number, "error", err)
	}

	return optionAnswer(q, ans.Answer), nil
}

// optionAnswer is text as an answer to q: the option whose title it names,
// case and surrounding space aside, else the text itself.
func optionAnswer(q protocol.RoundQuestion, text string) protocol.RoundAnswer {
	for i, o := range q.Options {
		if strings.EqualFold(strings.TrimSpace(o.Title), strings.TrimSpace(text)) {
			return protocol.RoundAnswer{Number: q.Number, Option: i + 1}
		}
	}

	return protocol.RoundAnswer{Number: q.Number, Text: text}
}

// responderTask is what a responder is given: the target projects, the
// need, the asking stage's inputs, the target projects that are proofs of
// concept when the asking stage shapes cards, the gate feedback the asking
// attempt addresses, after a sign-off rejection its feedback the gate
// feedback did not already give, the user's notes, every answer so far and
// the question with its options and context, followed by concern, the
// consequence of an earlier answer to it the person turned down, when there
// is one.
func (c *Controller) responderTask(ctx context.Context, bp store.Blueprint, st pipelineStep, attempt int, poc []string, q protocol.RoundQuestion, concern string) (string, error) {
	var b strings.Builder

	if err := c.writeInputs(ctx, &b, bp, st, true, "responder task"); err != nil {
		return "", err
	}

	writePOC(&b, poc)

	var given []string

	if st.Gate {
		var err error
		if given, err = c.writeGateFeedback(ctx, &b, bp, st, attempt); err != nil {
			return "", err
		}
	}

	if err := c.writeSignoffFeedback(ctx, &b, bp.ID, given); err != nil {
		return "", err
	}

	if err := c.writeNotes(ctx, &b, bp.ID); err != nil {
		return "", err
	}

	questions, err := c.opts.Store.ListQuestions(ctx, bp.ID)
	if err != nil {
		return "", fmt.Errorf("blueprint: responder task: %w", err)
	}

	if len(questions) > 0 {
		superseded, err := c.supersededAnswers(ctx, bp)
		if err != nil {
			return "", fmt.Errorf("blueprint: responder task: %w", err)
		}

		b.WriteString("## Answers so far\n\n")

		writeQuestions(&b, questions, superseded)
	}

	fmt.Fprintf(&b, "## The question, asked by %s\n\n%s\n", st.ID, q.Text)

	if q.Why != "" {
		b.WriteString("\n" + q.Why + "\n")
	}

	for i, o := range q.Options {
		marker := ""
		if i+1 == q.Recommended {
			marker = " (the stage recommends this one)"
		}

		line := fmt.Sprintf("%d. %s", i+1, o.Title)
		if o.Summary != "" {
			line += ": " + o.Summary
		}

		fmt.Fprintf(&b, "%s%s\n", line, marker)

		for _, p := range o.Pros {
			fmt.Fprintf(&b, "   + %s\n", p)
		}

		for _, cn := range o.Cons {
			fmt.Fprintf(&b, "   - %s\n", cn)
		}
	}

	if concern != "" {
		fmt.Fprintf(&b, "\n## The person's concern\n\nThe person did not accept what an earlier answer to this question meant for them: %s\n", concern)
	}

	return b.String(), nil
}
