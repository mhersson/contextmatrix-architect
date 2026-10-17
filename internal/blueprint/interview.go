package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// maxRoundBytes bounds a round's encoded questions: ContextMatrix keeps a
// round's frame in a 256 KiB row, chat included, and reads 1 MiB per line.
const maxRoundBytes = 96 << 10

// interviewer is the stage.Interviewer a stage run gets: it numbers the
// run's rounds and their questions and posts them. The run ends on the post;
// nothing waits in memory.
type interviewer struct {
	c     *Controller
	bp    store.Blueprint
	st    pack.Stage
	sess  *session
	runID int64
	// attempt, builderRound and revisedFor are the run's place, which a
	// continuation of its round takes over.
	attempt, builderRound, revisedFor int
	// qround is the round a chat run answers; posted the round the run
	// posted or answered, for its end event.
	qround, posted int
}

func (iv *interviewer) Ask(ctx context.Context, rd stage.Round) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c, bg := iv.c, context.WithoutCancel(ctx)

	if err := c.repeated(bg, iv.bp.ID, rd.Questions); err != nil {
		return "", err
	}

	sources, err := c.roundSources(bg, iv.bp.ID, rd.Questions)
	if err != nil {
		return "", err
	}

	lock := c.roundLock(iv.bp.ID)
	lock.Lock()
	defer lock.Unlock()

	number, err := c.nextRoundNumber(bg, iv.bp.ID)
	if err != nil {
		return "", err
	}

	first, err := c.nextQuestionNumber(bg, iv.bp.ID)
	if err != nil {
		return "", err
	}

	s := roundState{
		Number: number, Attempt: iv.attempt, BuilderRound: iv.builderRound, RevisedFor: iv.revisedFor,
		PostedBy: iv.runID, RunID: iv.runID, Change: changePosted,
		Frame: protocol.LogRound{Persona: personaName(iv.st.Persona), Stage: iv.st.ID, ResearchOff: !c.researchOn(iv.sess, iv.st)},
	}

	for i, q := range rd.Questions {
		s.Frame.Questions = append(s.Frame.Questions, frameQuestion(q, first+i, sources))
	}

	if err := fits(s); err != nil {
		return "", err
	}

	c.putRound(bg, iv.sess.audit, iv.bp.ID, iv.runID, s)
	iv.posted = number

	return postedText(s), nil
}

// Answer is the chat reply; no run answers a chat yet.
func (iv *interviewer) Answer(context.Context, stage.ChatAnswer) (string, error) {
	return "", errors.New("this run answers no round's chat")
}

// fits refuses a round whose questions would not fit the panel's frame.
func fits(s roundState) error {
	raw, err := json.Marshal(s.Frame.Questions)
	if err != nil {
		return fmt.Errorf("encode the round: %w", err)
	}

	if len(raw) > maxRoundBytes {
		return fmt.Errorf("the round's questions take %d KiB and a round takes at most %d KiB; ask fewer questions or shorten the options", len(raw)>>10, maxRoundBytes>>10)
	}

	return nil
}

// postedText is ask_questions' result: the round and its question numbers.
func postedText(s roundState) string {
	qs := s.Frame.Questions

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
