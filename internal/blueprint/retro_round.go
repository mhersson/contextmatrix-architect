package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/retro"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// errLessonRoundTooBig wraps fits's refusal of a lesson round.
var errLessonRoundTooBig = errors.New("the lesson round does not fit in one round")

// lessonRoundAt is the blueprint's open lesson round: its newest round, when
// its frame's stage is RetroStage and it is not closed. The caller holds
// id's round lock.
//
// roundAt never finds a lesson round, as no stage run of RetroStage exists,
// so a reopen, the view, a mode switch and chat all pass it by.
func (c *Controller) lessonRoundAt(ctx context.Context, id string) (roundState, bool, error) {
	rounds, err := c.rounds(ctx, id)
	if err != nil || len(rounds) == 0 {
		return roundState{}, false, err
	}

	s := rounds[len(rounds)-1]
	if s.Frame.Stage != RetroStage || s.Frame.Closed {
		return roundState{}, false, nil
	}

	return s, true, nil
}

// postLessonRound posts one question per lesson, numbered after every
// question of the blueprint; fits's error when the round would not fit, and
// then nothing is posted. Each lesson is matched to its finding by card, and
// the questions follow finding order. Only the frame size bounds the count.
func (c *Controller) postLessonRound(ctx context.Context, au audit, id string, runID int64, attempt int, lessons []store.Lesson, findings []retro.Finding) error {
	lock := c.roundLock(id)
	lock.Lock()
	defer lock.Unlock()

	number, err := c.nextRoundNumber(ctx, id)
	if err != nil {
		return err
	}

	first, err := c.nextQuestionNumber(ctx, id)
	if err != nil {
		return err
	}

	s := roundState{
		Number: number, Attempt: attempt, PostedBy: runID, RunID: runID, Change: changePosted,
		Lessons: map[int]int64{},
		Frame:   protocol.LogRound{Persona: RetroStage, Stage: RetroStage, ResearchOff: true},
	}

	for _, f := range findings {
		i := slices.IndexFunc(lessons, func(l store.Lesson) bool { return slices.Contains(l.Evidence, f.Card) })
		if i < 0 {
			continue
		}

		n := first + len(s.Frame.Questions)
		s.Frame.Questions = append(s.Frame.Questions, frameQuestion(retro.LessonQuestion(lessons[i], f), n, nil))
		s.Lessons[n] = lessons[i].ID
	}

	if err := fits(s); err != nil {
		return fmt.Errorf("%w: %w", errLessonRoundTooBig, err)
	}

	c.putRound(ctx, au, id, runID, s)

	return nil
}

// replyLessons stores rr's answers in open lesson round s and, once every
// question has an answer, closes it and decides its lessons. The caller holds
// id's round lock; no run slot is taken, so an answer lands while a run or a
// command holds the slot.
func (c *Controller) replyLessons(ctx context.Context, sess *session, id string, s roundState, rr protocol.RoundReply, msgID string) error {
	next, dropped := mergeReply(s, rr.Answers, rr.Decisions)
	next.Change, next.MessageID, next.Dropped = changeAnswers, msgID, dropped
	c.putRound(ctx, sess.audit, id, 0, next)

	if !next.complete() {
		return nil
	}

	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		return fmt.Errorf("blueprint: close lesson round %d: %w", next.Number, err)
	}

	closed, err := c.closeRound(ctx, sess.audit, bp, next)
	if err != nil {
		return err
	}

	return c.decideLessons(ctx, sess.audit, closed, msgID)
}

// decideLessons decides each lesson of answered lesson round s by its
// question's answer, in question order, and posts each decision. A lesson no
// longer proposed is left as it is. A store error is returned, and the
// lessons after it stay proposed.
func (c *Controller) decideLessons(ctx context.Context, au audit, s roundState, msgID string) error {
	answers := map[int]protocol.RoundAnswer{}
	for _, a := range s.Frame.Answers {
		answers[a.Number] = a
	}

	for _, q := range s.Frame.Questions {
		id, isLesson := s.Lessons[q.Number]
		a, answered := answers[q.Number]

		if !isLesson || !answered {
			continue
		}

		l, err := c.opts.Store.GetLesson(ctx, id)
		if err != nil {
			return fmt.Errorf("blueprint: decide L%d: %w", id, err)
		}

		d, refused := c.lessonDecision(au, l, a)

		decided, err := c.opts.Store.DecideLesson(ctx, l.ID, d)
		if err != nil {
			return fmt.Errorf("blueprint: decide %s: %w", l.Tag(), err)
		}

		if !decided {
			continue
		}

		payload := map[string]any{"lesson": l.ID, "state": d.State, "scope": d.Scope, "text": d.Text, "message_id": msgID}
		if d.Scope == store.ScopeProject {
			payload["project"] = l.Project
		}

		if refused != nil {
			payload["error"] = refused.Error()
		}

		au.event(ctx, Event{Kind: EventLessonDecided, BlueprintID: l.BlueprintID, Payload: payload})
	}

	return nil
}

// lessonDecision is answer a's decision on lesson l. The options are
// retro.LessonQuestion's: approve for the project, approve for the pack,
// reject; own words reword the lesson and approve it at its proposed scope.
// An approval whose overlay the pack's lists cannot take is a rejection,
// returned with the error that refused it.
func (c *Controller) lessonDecision(au audit, l store.Lesson, a protocol.RoundAnswer) (store.LessonDecision, error) {
	d := store.LessonDecision{State: store.LessonActive, Scope: l.Scope, Text: l.Text, By: store.ResponderHuman, At: c.opts.Now().UTC()}

	switch a.Option {
	case 1:
		d.Scope = store.ScopeProject
	case 2:
		d.Scope = store.ScopePack
	case 3:
		d.State = store.LessonRejected

		return d, nil
	default:
		d.Text = au.text(a.Text)
	}

	var overlay contract.Overlay

	err := json.Unmarshal(l.Overlay, &overlay)
	if err != nil {
		err = fmt.Errorf("decode the overlay: %w", err)
	} else {
		_, err = c.opts.Pack.MergeLists([]contract.Overlay{overlay})
	}

	if err != nil {
		d.State, d.Scope, d.Text = store.LessonRejected, l.Scope, l.Text

		return d, err
	}

	return d, nil
}
