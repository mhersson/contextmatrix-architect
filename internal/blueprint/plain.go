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

// withPlain adds the pack's plain-language instructions after a run's own
// when the blueprint is plain; otherwise the instructions are unchanged.
func withPlain(bp store.Blueprint, instructions, plain string) string {
	if !bp.PlainLanguage || plain == "" {
		return instructions
	}

	return instructions + "\n\n" + plain
}

// askPlain posts round rd of a plain-language blueprint under the human
// policy in two halves. The responder first answers the technical questions
// on a closed round of their own, which also takes, accepted, every tab
// entry with no consequence: no person would notice it. The person then
// gets the experience questions and a tab of consequences, each with its
// technical text as detail: the stage's entries that carry one, then the
// answers the responder wrote one with. A half that would be empty is not
// posted; with no person's half the run ends on the responder's round, as
// on an auto round.
func (iv *interviewer) askPlain(ctx context.Context, rd stage.Round, sources map[string]protocol.RoundSource) (string, error) {
	c, bg := iv.c, context.WithoutCancel(ctx)

	var technical, experience stage.Round

	for _, q := range rd.Questions {
		if q.Audience == stage.AudienceTechnical {
			technical.Questions = append(technical.Questions, q)
		} else {
			experience.Questions = append(experience.Questions, q)
		}
	}

	// shown is aligned with experience.Assumptions.
	var shown []string

	for i, text := range rd.Assumptions {
		if consequence := consequenceAt(rd.Consequences, i); consequence != "" {
			experience.Assumptions, shown = append(experience.Assumptions, text), append(shown, consequence)
		} else {
			technical.Assumptions = append(technical.Assumptions, text)
		}
	}

	if len(technical.Questions) > 0 && c.opts.Pack.ResponderInstructions() == "" {
		return "", errors.New("this blueprint's technical questions go to the auto responder, which this pack lacks; decide them yourself and record each decision as an assumption")
	}

	lock := c.roundLock(iv.bp.ID)

	var tech roundState

	if len(technical.Questions) > 0 || len(technical.Assumptions) > 0 {
		// The person's half is sized first, so an ask too big to post
		// leaves no responder's round behind.
		lock.Lock()
		_, err := iv.personHalf(bg, experience, shown, sources)

		var s roundState
		if err == nil {
			s, err = iv.numbered(bg, technical, sources)
		}

		lock.Unlock()

		if err != nil {
			return "", err
		}

		// Stored before the person's half is numbered, which reads the
		// next numbers from the store.
		if tech, err = iv.respondRound(ctx, s); err != nil {
			return "", err
		}
	}

	lock.Lock()
	defer lock.Unlock()

	person, err := iv.personHalf(bg, experience, shown, sources)
	if err != nil {
		return "", err
	}

	person.Technical = tech.Number

	answers := map[int]protocol.RoundAnswer{}
	for _, a := range tech.Frame.Answers {
		answers[a.Number] = a
	}

	for _, q := range tech.Frame.Questions {
		consequence := iv.consequences[q.Number]
		if consequence == "" {
			continue
		}

		person.TabIssued++
		id := fmt.Sprintf("A%d", person.TabIssued)

		person.Frame.Assumptions = append(person.Frame.Assumptions, protocol.RoundAssumption{
			ID: id, Text: consequence, Detail: q.Text + " " + AnswerText(q, answers[q.Number]),
		})

		if person.Consequences == nil {
			person.Consequences = map[string]int{}
		}

		person.Consequences[id] = q.Number
	}

	if err := fits(person); err != nil {
		return "", err
	}

	if tech.Number > 0 && len(person.Frame.Questions) == 0 && len(person.Frame.Assumptions) == 0 {
		iv.responded, iv.posted = true, tech.Number

		return postedText(tech), nil
	}

	c.putRound(bg, iv.sess.audit, iv.bp.ID, iv.runID, person)
	iv.posted = person.Number

	if tech.Number == 0 {
		return postedText(person), nil
	}

	return splitText(tech, person, iv.consequences), nil
}

// personHalf frames the person's half of a split ask as the run's next
// round: the experience questions, and the stage's entries shown as their
// consequences (shown, aligned), each with its sentence as detail.
func (iv *interviewer) personHalf(ctx context.Context, experience stage.Round, shown []string, sources map[string]protocol.RoundSource) (roundState, error) {
	s, err := iv.numbered(ctx, experience, sources)
	if err != nil {
		return roundState{}, err
	}

	s.Plain = true

	for i := range s.Frame.Assumptions {
		a := &s.Frame.Assumptions[i]
		a.Text, a.Detail = shown[i], a.Text
	}

	if err := fits(s); err != nil {
		return roundState{}, err
	}

	return s, nil
}

// consequenceAt is the consequence aligned with tab entry i, "" for none.
func consequenceAt(consequences []string, i int) string {
	if i < len(consequences) {
		return consequences[i]
	}

	return ""
}

// splitText is ask_questions' result for an ask split between the
// responder's round and the person's: both rounds, and each answer the
// responder gave with the consequence the person was shown, so a chat that
// resumes this conversation knows them.
func splitText(tech, person roundState, consequences map[int]string) string {
	var b strings.Builder

	if qs := tech.Frame.Questions; len(qs) > 0 {
		fmt.Fprintf(&b, "Round %d's %s went to the auto responder; ", tech.Number, questionTags(qs))
	} else {
		fmt.Fprintf(&b, "Round %d took only the assumptions no person would notice; ", tech.Number)
	}

	if qs := person.Frame.Questions; len(qs) > 0 {
		fmt.Fprintf(&b, "round %d is posted as %s.", person.Number, questionTags(qs))
	} else {
		fmt.Fprintf(&b, "round %d is posted with its assumptions tab.", person.Number)
	}

	answers := map[int]protocol.RoundAnswer{}
	for _, a := range tech.Frame.Answers {
		answers[a.Number] = a
	}

	if len(tech.Frame.Questions) > 0 {
		b.WriteString("\n\nThe responder's answers:\n")
	}

	for _, q := range tech.Frame.Questions {
		fmt.Fprintf(&b, "- [Q%d] %s\n  Answer: %s\n", q.Number, q.Text, AnswerText(q, answers[q.Number]))

		if consequence := consequences[q.Number]; consequence != "" {
			fmt.Fprintf(&b, "  Shown to the person as: %s\n", consequence)
		}
	}

	if len(tech.Frame.Assumptions) > 0 {
		fmt.Fprintf(&b, "\nThe assumptions no person would notice were accepted on round %d; state them without a basis.\n", tech.Number)
	}

	b.WriteString("\nYour run ends here; every answer arrives as your next message.")

	return b.String()
}

// plainRetab replaces the stage's entries on plain round s's tab with texts,
// each shown as its consequence, followed by the entries of the responder's
// answers, unchanged; an entry that repeats one of those is dropped, so the
// person never sees it twice. An entry whose sentence and consequence are
// both unchanged keeps its id and its decision; any other takes the next id
// the round never issued, so no decision lands on a consequence the person
// did not see. An entry without a consequence is refused.
func plainRetab(s roundState, texts, consequences []string) (roundState, error) {
	out := s.clone()

	idOf := map[protocol.RoundAssumption][]string{}
	answered := map[protocol.RoundAssumption]bool{}

	var answers []protocol.RoundAssumption

	for _, a := range out.Frame.Assumptions {
		key := protocol.RoundAssumption{Text: a.Text, Detail: a.Detail}

		if _, ok := out.Consequences[a.ID]; ok {
			answers, answered[key] = append(answers, a), true

			continue
		}

		idOf[key] = append(idOf[key], a.ID)
	}

	var kept []protocol.RoundAssumption

	for i, text := range texts {
		consequence := consequenceAt(consequences, i)
		key := protocol.RoundAssumption{Text: consequence, Detail: text}

		switch {
		case answered[key]:
			continue
		case consequence == "":
			return roundState{}, fmt.Errorf("assumption %d has no consequence; an assumption with no effect a person would notice stays off their tab: state it without a basis", i+1)
		}

		id := ""

		if ids := idOf[key]; len(ids) > 0 {
			id, idOf[key] = ids[0], ids[1:]
		} else {
			out.TabIssued++
			id = fmt.Sprintf("A%d", out.TabIssued)
		}

		kept = append(kept, protocol.RoundAssumption{ID: id, Text: consequence, Detail: text})
	}

	kept = append(kept, answers...)
	out.Frame.Assumptions = kept
	out.Frame.Decisions = slices.DeleteFunc(out.Frame.Decisions, func(d protocol.AssumptionDecision) bool {
		return !slices.ContainsFunc(out.Frame.Assumptions, func(a protocol.RoundAssumption) bool { return a.ID == d.ID })
	})

	return out, nil
}

// plainTags maps the tag of each assumption accepted on a plain round to that
// round. The tag links a stated assumption to the person's acceptance of its
// consequence, which confirms no decision.
func plainTags(accepted []acceptedAssumption) map[string]int {
	out := map[string]int{}

	for _, a := range accepted {
		if a.Plain {
			out[a.Tag] = a.Round
		}
	}

	return out
}

// answerNote is what an auto answer's event records beside the answer: the
// consequence the responder wrote with it, and the question it asks again, 0
// for none.
type answerNote struct {
	Consequence string
	Supersedes  int
}

// answerNotes maps a question number to the note its auto answer event
// carries, as answerEvidence reads the evidence.
func answerNotes(ctx context.Context, s *store.Store, blueprintID string) (map[int]answerNote, error) {
	events, err := s.ListEventsOfKind(ctx, blueprintID, EventAnswer)
	if err != nil {
		return nil, fmt.Errorf("blueprint: answer notes: %w", err)
	}

	out := map[int]answerNote{}

	for _, e := range events {
		var p struct {
			Number      int    `json:"number"`
			Responder   string `json:"responder"`
			Consequence string `json:"consequence"`
		}

		if json.Unmarshal(e.Payload, &p) != nil || p.Responder != store.ResponderAuto {
			continue
		}

		out[p.Number] = answerNote{Consequence: p.Consequence}
	}

	return out, nil
}

// answerConsequenceDecisions reads, from the closed plain rounds, what a
// person decided on the consequences of the responder's answers: accepted
// maps an answer's question to the round its consequence was accepted on,
// turnedDown to the consequence turned down. What the responder decided on
// a round a mode switch handed over is no person's decision.
func (c *Controller) answerConsequenceDecisions(ctx context.Context, id string) (accepted map[int]int, turnedDown map[int]string, err error) {
	rounds, err := c.rounds(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	accepted, turnedDown = map[int]int{}, map[int]string{}

	for _, s := range rounds {
		if s.Change != changeClosed || !s.Plain {
			continue
		}

		decisions := map[string]bool{}
		for _, d := range s.Frame.Decisions {
			decisions[d.ID] = d.Accept
		}

		for _, a := range s.Frame.Assumptions {
			n, isAnswer := s.Consequences[a.ID]
			accept, decided := decisions[a.ID]

			switch {
			case !isAnswer || !decided || s.responderDecided(a.ID):
			case accept:
				accepted[n] = s.Number
			default:
				turnedDown[n] = a.Text
			}
		}
	}

	return accepted, turnedDown, nil
}

// consequenceRecord is what a plain-language blueprint's record says of the
// consequences its person was shown: the round each plain acceptance's tag
// was accepted on and the consequence it showed, every auto answer's note,
// and the round each answer's consequence was accepted on.
type consequenceRecord struct {
	tags    map[string]int
	shown   map[string]string
	notes   map[int]answerNote
	answers map[int]int
}

// consequenceRecord reads blueprint id's record of consequences; accepted is
// its acceptedAssumptions.
func (c *Controller) consequenceRecord(ctx context.Context, id string, accepted []acceptedAssumption) (consequenceRecord, error) {
	r := consequenceRecord{tags: plainTags(accepted), shown: map[string]string{}}

	for _, a := range accepted {
		if a.Plain {
			r.shown[a.Tag] = a.Consequence
		}
	}

	var err error

	if r.notes, err = answerNotes(ctx, c.opts.Store, id); err != nil {
		return consequenceRecord{}, err
	}

	if r.answers, _, err = c.answerConsequenceDecisions(ctx, id); err != nil {
		return consequenceRecord{}, err
	}

	return r, nil
}

// mark gives a the round a person accepted its consequence on, by the plain
// acceptance its basis names or by the auto answer it rests on or is, and
// its consequence: the sentence that person was shown, whatever the stage
// states with it now; otherwise the one stated with it, or else the
// answer's. Its status stays its basis's.
func (r consequenceRecord) mark(a *assumption, stated string, answered map[string]store.Question) {
	shown := ""

	if n, ok := r.tags[a.Basis]; ok {
		a.ConsequenceAccepted, shown = n, r.shown[a.Basis]
	} else if q, ok := answered[a.Basis]; ok {
		a.ConsequenceAccepted, shown = r.answers[q.Number], r.notes[q.Number].Consequence
	}

	a.Consequence = stated
	if a.Consequence == "" || (a.ConsequenceAccepted > 0 && shown != "") {
		a.Consequence = shown
	}
}
