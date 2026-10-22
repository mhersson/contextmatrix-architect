package blueprint

import (
	"context"
	"errors"
	"fmt"
	"slices"

	protocol "github.com/mhersson/contextmatrix-protocol"

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
