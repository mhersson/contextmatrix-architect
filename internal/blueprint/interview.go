package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// maxRoundBytes bounds a round's encoded questions and assumptions:
// ContextMatrix keeps a round's frame in a 256 KiB row, chat included, and
// reads 1 MiB per line.
const maxRoundBytes = 96 << 10

// interviewer is the stage.Interviewer a stage run gets: it numbers the
// run's rounds and their questions and posts them. The run ends on the post;
// nothing waits in memory.
type interviewer struct {
	c     *Controller
	bp    store.Blueprint
	st    pipelineStep
	sess  *session
	runID int64
	r     *run
	// tools are the asking stage's, which its responder reads with; sink is
	// the asking run's, which a responder's cost is charged to.
	tools []tools.Tool
	sink  *harnessSink
	// max caps the questions the run's attempt may ask under auto, 0 none;
	// asked counts them, the attempt's earlier runs included.
	max, asked int
	// attempt, builderRound and findings are the run's place, which a
	// continuation of its round takes over.
	attempt, builderRound int
	findings              []string
	// qround is the round a chat run answers; posted the round the run
	// posted or answered, for its end event.
	qround, posted int
	// responded is set when the run's round was answered by the responder
	// under auto, so nothing waits on the person.
	responded bool
	// seen is how many chat entries a chat run was given; its reply follows
	// them.
	seen int
}

func (iv *interviewer) Ask(ctx context.Context, rd stage.Round) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c, bg := iv.c, context.WithoutCancel(ctx)

	if err := iv.takesAssumptions(rd.Assumptions); err != nil {
		return "", err
	}

	if err := c.reposted(bg, iv.bp.ID, rd.Assumptions); err != nil {
		return "", err
	}

	// Read at every round, so a gate reply during the run applies from the
	// next round on.
	policy, err := c.policy(bg, iv.bp, iv.st.ID)
	if err != nil {
		return "", err
	}

	auto := policy == pack.PolicyAuto

	switch {
	case auto && c.opts.Pack.ResponderInstructions() == "":
		return "", errNoResponder
	case auto && iv.max > 0 && iv.asked+len(rd.Questions) > iv.max:
		return "", fmt.Errorf("under the auto policy this stage may ask %d question(s) per attempt and %d are left; ask at most that many and decide the rest yourself, recording each decision as an assumption where this stage records them",
			iv.max, max(iv.max-iv.asked, 0))
	}

	if err := c.repeated(bg, iv.bp.ID, rd.Questions); err != nil {
		return "", err
	}

	sources, err := c.roundSources(bg, iv.bp.ID, rd.Questions)
	if err != nil {
		return "", err
	}

	lock := c.roundLock(iv.bp.ID)
	lock.Lock()
	s, err := iv.numbered(bg, rd, sources)

	if err == nil && !auto {
		c.putRound(bg, iv.sess.audit, iv.bp.ID, iv.runID, s)
		iv.posted = s.Number
	}

	lock.Unlock()

	if err != nil {
		return "", err
	}

	if auto {
		return iv.respondAll(ctx, s)
	}

	return postedText(s), nil
}

// numbered frames rd as the run's next round. The caller holds the
// blueprint's round lock.
func (iv *interviewer) numbered(ctx context.Context, rd stage.Round, sources map[string]protocol.RoundSource) (roundState, error) {
	c := iv.c

	number, err := c.nextRoundNumber(ctx, iv.bp.ID)
	if err != nil {
		return roundState{}, err
	}

	first, err := c.nextQuestionNumber(ctx, iv.bp.ID)
	if err != nil {
		return roundState{}, err
	}

	s := roundState{
		Number: number, Attempt: iv.attempt, BuilderRound: iv.builderRound, Findings: iv.findings,
		PostedBy: iv.runID, RunID: iv.runID, Change: changePosted,
		Frame: protocol.LogRound{Persona: personaName(iv.st.Persona), Stage: iv.st.ID, ResearchOff: !c.researchOn(iv.sess, iv.st.Stage)},
	}

	for i, q := range rd.Questions {
		s.Frame.Questions = append(s.Frame.Questions, frameQuestion(q, first+i, sources))
	}

	for i, text := range rd.Assumptions {
		s.Frame.Assumptions = append(s.Frame.Assumptions, protocol.RoundAssumption{ID: fmt.Sprintf("A%d", i+1), Text: text})
	}

	s.TabIssued = len(rd.Assumptions)

	if err := fits(s); err != nil {
		return roundState{}, err
	}

	return s, nil
}

// respondAll has the responder answer each question of auto round s inside
// the asking run, accepts every assumption on its tab, stores the round
// closed and read-only, and ends the run on it like a human's round; the
// stage continues at once. A cancel leaves no round; the answers already
// given stay recorded, and a re-run asks only what is still unanswered. A
// question the responder cannot answer is left to the stage, which decides
// it.
func (iv *interviewer) respondAll(ctx context.Context, s roundState) (string, error) {
	s.Frame.Responder = true
	iv.responded = true

	for _, a := range s.Frame.Assumptions {
		s.Frame.Decisions = append(s.Frame.Decisions, protocol.AssumptionDecision{ID: a.ID, Accept: true})
	}

	for _, q := range s.Frame.Questions {
		ans, err := iv.respond(ctx, q)

		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			ans = protocol.RoundAnswer{Number: q.Number, Text: err.Error()}
		}

		s.Frame.Answers = append(s.Frame.Answers, ans)
	}

	iv.asked += len(s.Frame.Questions)
	s.Change, s.Frame.Closed = changeClosed, true

	bg := context.WithoutCancel(ctx)

	lock := iv.c.roundLock(iv.bp.ID)
	lock.Lock()
	iv.c.putRound(bg, iv.sess.audit, iv.bp.ID, iv.runID, s)
	lock.Unlock()

	iv.posted = s.Number

	return postedText(s), nil
}

// Answer is a chat run's reply in its round's chat, with the questions it
// revises or adds. The round then names this run, whose conversation the
// next segment resumes and which ends asked, so the pipeline still stands
// at the round.
func (iv *interviewer) Answer(ctx context.Context, a stage.ChatAnswer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c, bg := iv.c, context.WithoutCancel(ctx)

	if err := iv.takesAssumptions(a.Assumptions); err != nil {
		return "", err
	}

	if err := c.reposted(bg, iv.bp.ID, a.Assumptions); err != nil {
		return "", err
	}

	var added []stage.RoundQuestion

	for _, q := range a.Questions {
		if q.Number == 0 {
			added = append(added, q)
		}
	}

	if err := c.repeated(bg, iv.bp.ID, added); err != nil {
		return "", err
	}

	sources, err := c.roundSources(bg, iv.bp.ID, a.Questions)
	if err != nil {
		return "", err
	}

	lock := c.roundLock(iv.bp.ID)
	lock.Lock()
	defer lock.Unlock()

	place, found, err := c.roundAt(bg, iv.bp.ID)
	if err != nil {
		return "", err
	}

	if !found || place.state.Number != iv.qround || place.state.Frame.Closed {
		return "", fmt.Errorf("round %d takes no changes now", iv.qround)
	}

	first, err := c.nextQuestionNumber(bg, iv.bp.ID)
	if err != nil {
		return "", err
	}

	next, err := revised(place.state, a.Questions, first, sources)
	if err != nil {
		return "", err
	}

	if a.Assumptions != nil {
		next = retabbed(next, a.Assumptions)
	}

	if len(next.Frame.Questions) > stage.MaxRoundQuestions {
		return "", fmt.Errorf("a round asks at most %d questions and round %d has %d; revise one of them instead of adding more",
			stage.MaxRoundQuestions, next.Number, len(place.state.Frame.Questions))
	}

	// The reply follows the entries this run was given; any the user wrote
	// meanwhile come after it, for the next chat run.
	seen := min(iv.seen, len(next.Frame.Chat))
	chat := append(slices.Clone(next.Frame.Chat[:seen]), protocol.RoundChat{From: iv.st.Persona, Text: a.Reply})
	next.Frame.Chat = append(chat, next.Frame.Chat[seen:]...)
	next.Seen, next.RunID, next.Change, next.MessageID, next.Dropped = seen+1, iv.runID, changeReply, "", 0

	if err := fits(next); err != nil {
		return "", err
	}

	c.putRound(bg, iv.sess.audit, iv.bp.ID, iv.runID, next)
	iv.posted = next.Number

	return fmt.Sprintf("Your reply is in round %d's chat. Your run ends here; the user answers in the block or writes again.", next.Number), nil
}

// reposted refuses an assumptions tab that posts again what the user
// already accepted on a closed round's tab, case, spacing and punctuation
// aside: the stage cites the acceptance's tag instead. An auto round
// accepts nothing a person did.
func (c *Controller) reposted(ctx context.Context, id string, tab []string) error {
	if len(tab) == 0 {
		return nil
	}

	accepted, err := c.acceptedAssumptions(ctx, id)
	if err != nil {
		return err
	}

	var problems []string

	for i, text := range tab {
		norm := normalText(text)

		for _, a := range accepted {
			if norm != "" && normalText(a.Text) == norm {
				problems = append(problems, fmt.Sprintf("assumption %d was accepted as %s already; leave it off the tab and cite %s instead", i+1, a.Tag, a.Tag))

				break
			}
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	return nil
}

// takesAssumptions refuses an assumptions tab on a stage that records no
// assumptions: nothing would state or check them.
func (iv *interviewer) takesAssumptions(tab []string) error {
	if len(tab) > 0 && !slices.Contains(iv.st.Checks, pack.CheckAssumptions) {
		return errors.New("this stage records no assumptions; ask questions only")
	}

	return nil
}

// fits refuses a round whose questions and assumptions would not fit the
// panel's frame.
func fits(s roundState) error {
	raw, err := json.Marshal(struct {
		Questions   []protocol.RoundQuestion
		Assumptions []protocol.RoundAssumption
	}{s.Frame.Questions, s.Frame.Assumptions})
	if err != nil {
		return fmt.Errorf("encode the round: %w", err)
	}

	if len(raw) > maxRoundBytes {
		return fmt.Errorf("the round's questions and assumptions take %d KiB and a round takes at most %d KiB; ask fewer questions, or shorten the options or the assumptions",
			len(raw)>>10, maxRoundBytes>>10)
	}

	return nil
}

// postedText is ask_questions' result: the round and its question numbers.
func postedText(s roundState) string {
	qs := s.Frame.Questions

	if len(qs) == 0 {
		return fmt.Sprintf("Round %d is posted with its assumptions tab. Your run ends here; the decisions arrive as your next message.", s.Number)
	}

	tags := fmt.Sprintf("Q%d", qs[0].Number)
	if len(qs) > 1 {
		tags += fmt.Sprintf("-Q%d", qs[len(qs)-1].Number)
	}

	return fmt.Sprintf("Round %d is posted as %s. Your run ends here; the answers arrive as your next message.", s.Number, tags)
}

func personaName(id string) string { return strings.ReplaceAll(id, "_", " ") }

// repeated refuses a question the blueprint already got answered with the
// same options, case, spacing and a trailing question mark aside: a round
// never asks an answered question again, while a changed option set makes a
// new question.
func (c *Controller) repeated(ctx context.Context, id string, qs []stage.RoundQuestion) error {
	answered, err := c.opts.Store.ListQuestions(ctx, id)
	if err != nil {
		return fmt.Errorf("blueprint: read the answers: %w", err)
	}

	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "?"))), " ")
	}

	same := func(x, y string) bool { return norm(x) == norm(y) }

	for _, q := range qs {
		titles := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			titles = append(titles, o.Title)
		}

		for _, a := range answered {
			if same(q.Text, a.Text) && slices.EqualFunc(titles, a.Options, same) {
				return fmt.Errorf("this was asked as Q%d and answered: %s; use that answer instead of asking again", a.Number, a.Answer)
			}
		}
	}

	return nil
}
