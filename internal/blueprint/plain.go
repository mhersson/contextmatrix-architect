package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
// on an auto round. turnedDown is reasked's, for the questions that ask an
// answer again.
func (iv *interviewer) askPlain(ctx context.Context, rd stage.Round, sources map[string]protocol.RoundSource, turnedDown map[int]string) (string, error) {
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

		iv.noteConcerns(s, turnedDown)

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
// consequence the responder wrote with it, and the question whose turned-down
// answer it supersedes, 0 for none.
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
			Supersedes  int    `json:"supersedes"`
		}

		if json.Unmarshal(e.Payload, &p) != nil || p.Responder != store.ResponderAuto {
			continue
		}

		out[p.Number] = answerNote{Consequence: p.Consequence, Supersedes: p.Supersedes}
	}

	return out, nil
}

// supersededBy maps each answer a re-ask superseded to the newest answer that
// did, from the auto answers' notes.
func supersededBy(notes map[int]answerNote) map[int]int {
	out := map[int]int{}

	for n, note := range notes {
		if note.Supersedes > 0 {
			out[note.Supersedes] = max(out[note.Supersedes], n)
		}
	}

	return out
}

// supersededAnswers is supersededBy for blueprint bp, nil unless it is plain.
func (c *Controller) supersededAnswers(ctx context.Context, bp store.Blueprint) (map[int]int, error) {
	if !bp.PlainLanguage {
		return nil, nil
	}

	notes, err := answerNotes(ctx, c.opts.Store, bp.ID)
	if err != nil {
		return nil, err
	}

	return supersededBy(notes), nil
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
// the round each answer's consequence was accepted on, and the answer that
// supersedes each answer asked again.
type consequenceRecord struct {
	tags       map[string]int
	shown      map[string]string
	notes      map[int]answerNote
	answers    map[int]int
	superseded map[int]int
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

	r.superseded = supersededBy(r.notes)

	if r.answers, _, err = c.answerConsequenceDecisions(ctx, id); err != nil {
		return consequenceRecord{}, err
	}

	return r, nil
}

// mark gives a the round a person accepted its consequence on, by the plain
// acceptance its basis names or by the auto answer it rests on or is, which
// also says the answer that supersedes it, and its consequence: the sentence
// that person was shown, whatever the stage states with it now; otherwise
// the one stated with it, or else the answer's. Its status stays its
// basis's.
func (r consequenceRecord) mark(a *assumption, stated string, answered map[string]store.Question) {
	shown := ""

	if n, ok := r.tags[a.Basis]; ok {
		a.ConsequenceAccepted, shown = n, r.shown[a.Basis]
	} else if q, ok := answered[a.Basis]; ok {
		a.ConsequenceAccepted, shown = r.answers[q.Number], r.notes[q.Number].Consequence

		if m := r.superseded[q.Number]; m > 0 {
			a.SupersededBy = fmt.Sprintf("Q%d", m)
		}
	}

	a.Consequence = stated
	if a.Consequence == "" || (a.ConsequenceAccepted > 0 && shown != "") {
		a.Consequence = shown
	}
}

// writePlainWording gives a reviewer of a plain-language blueprint what it
// judges of the wording: the experience questions the attempt's person's
// rounds asked (is one a technical question in disguise?), each stated
// consequence beside its technical text, each consequence of a stage's entry
// those rounds showed that no stated one repeats, and each answer
// consequence they showed beside the question and answer it describes (does
// it state the effect truly?). It writes nothing when there is none.
func (c *Controller) writePlainWording(ctx context.Context, b *strings.Builder, bp store.Blueprint, st pipelineStep, art store.Artifact, attempt int) error {
	ofAttempt, err := c.attemptRuns(ctx, bp.ID, st.ID, attempt)
	if err != nil {
		return fmt.Errorf("blueprint: plain wording for %s: %w", st.ID, err)
	}

	rounds, err := c.rounds(ctx, bp.ID)
	if err != nil {
		return fmt.Errorf("blueprint: plain wording for %s: %w", st.ID, err)
	}

	var statedPart strings.Builder

	// stated holds each stated consequence beside its technical text, as
	// normalText makes them, so a tab entry it repeats is listed once.
	stated := map[[2]string]bool{}

	for _, a := range statedAssumptions(art) {
		if a.Consequence == "" {
			continue
		}

		stated[[2]string{normalText(a.Text), normalText(a.Consequence)}] = true

		// The data's blueprint-wide id, which the reviewer's verdicts name;
		// an entry without one is listed without.
		label := "- "
		if a.ID != "" {
			label += a.ID + ": "
		}

		fmt.Fprintf(&statedPart, "%s%s\n  Consequence: %s\n", label, a.Text, a.Consequence)
	}

	var section, tabPart strings.Builder

	shown := map[int]bool{}
	asked := map[int]bool{}

	for _, s := range rounds {
		if !s.Plain || !ofAttempt[s.PostedBy] {
			continue
		}

		for _, q := range s.Frame.Questions {
			if s.Audiences[q.Number] != stage.AudienceExperience || asked[q.Number] {
				continue
			}

			asked[q.Number] = true

			fmt.Fprintf(&section, "- Q%d: %s\n", q.Number, q.Text)

			if q.Why != "" {
				fmt.Fprintf(&section, "  Why: %s\n", q.Why)
			}

			for _, o := range q.Options {
				fmt.Fprintf(&section, "  Option: %s", o.Title)

				if o.Summary != "" {
					fmt.Fprintf(&section, " - %s", o.Summary)
				}

				section.WriteString("\n")

				for _, p := range o.Pros {
					fmt.Fprintf(&section, "    Pro: %s\n", p)
				}

				for _, con := range o.Cons {
					fmt.Fprintf(&section, "    Con: %s\n", con)
				}
			}
		}

		for _, n := range s.Consequences {
			shown[n] = true
		}

		for _, a := range s.Frame.Assumptions {
			key := [2]string{normalText(a.Detail), normalText(a.Text)}

			if _, isAnswer := s.Consequences[a.ID]; isAnswer || a.Detail == "" || stated[key] {
				continue
			}

			stated[key] = true

			fmt.Fprintf(&tabPart, "- %s: %s\n  Consequence: %s\n", acceptanceTag(s.Number, a.ID), a.Detail, a.Text)
		}
	}

	var questionsPart strings.Builder

	if section.Len() > 0 {
		questionsPart.WriteString("Experience questions the person was asked; judge each: is it a technical question in disguise?\n\n")
		questionsPart.WriteString(section.String())
		questionsPart.WriteString("\n")
	}

	var answersPart strings.Builder

	if len(shown) > 0 {
		notes, err := answerNotes(ctx, c.opts.Store, bp.ID)
		if err != nil {
			return fmt.Errorf("blueprint: plain wording for %s: %w", st.ID, err)
		}

		questions, err := c.opts.Store.ListQuestions(ctx, bp.ID)
		if err != nil {
			return fmt.Errorf("blueprint: plain wording for %s: %w", st.ID, err)
		}

		for _, q := range questions {
			if note := notes[q.Number]; shown[q.Number] && note.Consequence != "" {
				fmt.Fprintf(&answersPart, "- Q%d: %s Answer: %s\n  Consequence: %s\n", q.Number, q.Text, q.Answer, note.Consequence)
			}
		}
	}

	if questionsPart.Len() == 0 && statedPart.Len() == 0 && tabPart.Len() == 0 && answersPart.Len() == 0 {
		return nil
	}

	b.WriteString("## Plain language to judge\n\n")
	b.WriteString(questionsPart.String())

	if statedPart.Len() > 0 {
		b.WriteString("Each consequence the artifact states beside its technical text; judge each: does it state the effect truly?\n\n")
		b.WriteString(statedPart.String())
		b.WriteString("\n")
	}

	if tabPart.Len() > 0 {
		b.WriteString("Each consequence the person was shown for an assumption, by its tab entry's tag, beside its technical text; judge each the same way:\n\n")
		b.WriteString(tabPart.String())
		b.WriteString("\n")
	}

	if answersPart.Len() > 0 {
		b.WriteString("Each consequence shown for an answer; judge each the same way:\n\n")
		b.WriteString(answersPart.String())
		b.WriteString("\n")
	}

	return nil
}

// plainTagForm is the form of an acceptance's tag, which is a basis a plain
// round's acceptance gave a stated assumption.
var plainTagForm = regexp.MustCompile(`^round\d+\.A\d+$`)

// plainSummary is the sign-off's summary for a person: what will be built,
// a line per slice; what they can do, the need's outcomes; and each
// consequence they accepted, once, by the id of the first of the sign-off's
// assumption entries that carry it. It is built from the record without a
// model run, so it cannot drift from it. list is the sign-off's assumptions
// in its order, empty when the sign-off falls back to prose.
func (c *Controller) plainSummary(ctx context.Context, bp store.Blueprint, list []assumption, title string) map[string]any {
	build := c.sliceLines(ctx, bp.ID)
	if len(build) == 0 {
		build = []any{title}
	}

	canDo := []any{}

	if len(c.opts.Pack.Stages) > 0 {
		art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, c.opts.Pack.Stages[0].ID)

		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound):
			c.opts.Logger.Warn("blueprint: plain summary outcomes", "blueprint", bp.ID, "error", err.Error())
		case err == nil:
			for _, o := range outcomesOf(art) {
				canDo = append(canDo, o.Text)
			}
		}
	}

	accepted := []any{}

	// A read error reads every entry as unchanged.
	var versions []store.Artifact

	if slices.ContainsFunc(list, func(a assumption) bool { return a.ConsequenceAccepted > 0 }) {
		var err error
		if versions, err = c.opts.Store.ListArtifacts(ctx, bp.ID); err != nil {
			c.opts.Logger.Warn("blueprint: plain summary changes", "blueprint", bp.ID, "error", err.Error())
		}
	}

	// Decisions resting on one acceptance carry its sentence alike: the
	// person reads it once, at the first, changed when any of them changed.
	listed := map[string]map[string]any{}

	for _, a := range list {
		if a.ConsequenceAccepted == 0 {
			continue
		}

		entry, seen := listed[normalText(a.Consequence)]
		if !seen {
			entry = map[string]any{"id": a.ID, "text": a.Consequence}
			listed[normalText(a.Consequence)] = entry
			accepted = append(accepted, entry)
		}

		if changedSince(a, versions) {
			entry["changed"] = true
		}
	}

	return map[string]any{"build": build, "can_do": canDo, "accepted": accepted}
}

// sliceLines is a line per slice of the slicing stage's latest artifact: its
// title, and its value after a colon when it has one. A read error is logged
// and reads as no slices.
func (c *Controller) sliceLines(ctx context.Context, id string) []any {
	key := c.opts.Pack.SlicingStage()
	if key == "" {
		return nil
	}

	art, err := c.opts.Store.LatestArtifact(ctx, id, key)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			c.opts.Logger.Warn("blueprint: plain summary slices", "blueprint", id, "error", err.Error())
		}

		return nil
	}

	var data struct {
		Slices []struct {
			Title string `json:"title"`
			Value string `json:"value"`
		} `json:"slices"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &data) != nil {
		return nil
	}

	var out []any

	for _, s := range data.Slices {
		title, value := strings.TrimSpace(s.Title), strings.TrimSpace(s.Value)

		switch {
		case title == "":
			continue
		case value != "":
			out = append(out, title+": "+value)
		default:
			out = append(out, title)
		}
	}

	return out
}

// changedSince reports whether a decision a person accepted the consequence
// of has changed since: its answer was superseded, or its stage's versions
// (in versions, in storage order) state it differently since the first that
// stated it: the latest consequence they give against the first, or, under a
// plain-round basis, the latest sentence against the first. A first
// statement that words the consequence otherwise than the person was shown
// is no change: comparing versions of the stage's own words keeps a
// paraphrase from reading as one. A consequence is optional beside a basis,
// so a version that gives none, or gives one at last, revises nothing.
func changedSince(a assumption, versions []store.Artifact) bool {
	if a.SupersededBy != "" {
		return true
	}

	norm := normalText(a.Text)

	// own holds, per version of a's stage, the entries resting on its basis.
	var own [][]stated

	for _, art := range versions {
		if art.Stage != a.Stage {
			continue
		}

		var entries []stated

		for _, s := range statedAssumptions(art) {
			if s.Basis == a.Basis {
				entries = append(entries, s)
			}
		}

		own = append(own, entries)
	}

	if len(own) == 0 {
		return false
	}

	// A plain round's tag names the decision across versions, so a revised
	// sentence is a change: a version's entry is the one with a's sentence,
	// else the one at a's place among the tag's entries in the latest version.
	// Several can rest on one answer's Q<n>, so there the decision is its
	// sentence alone.
	tagged := plainTagForm.MatchString(a.Basis)
	at := slices.IndexFunc(own[len(own)-1], func(s stated) bool { return normalText(s.Text) == norm })

	var (
		first                   stated
		found                   bool
		firstGiven, latestGiven string
	)

	for _, entries := range own {
		i := slices.IndexFunc(entries, func(s stated) bool { return normalText(s.Text) == norm })
		if i < 0 && tagged && at >= 0 && at < len(entries) {
			i = at
		}

		if i < 0 {
			continue
		}

		s := entries[i]

		if !found {
			first, found = s, true
		}

		if s.Consequence != "" {
			if firstGiven == "" {
				firstGiven = s.Consequence
			}

			latestGiven = s.Consequence
		}
	}

	return found && (normalText(first.Text) != norm || normalText(firstGiven) != normalText(latestGiven))
}
