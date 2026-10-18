package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Segments: runs that go on from a question round.
const (
	SegmentAnswers = "answers"
	SegmentChat    = "chat"
)

// What a round snapshot records.
const (
	changePosted  = "posted"
	changeAnswers = "answers"
	changeClosed  = "closed"
	// changeShown: a reopen shows an open round again.
	changeShown = "shown"
	// changeChat: a user's chat message; changeReply: a chat run's reply.
	changeChat  = "chat"
	changeReply = "reply"
	// ChangeLeft marks a round the pipeline went past without its answers;
	// the panel collapses it like a closed one.
	ChangeLeft = "left"
	// ChangeClosing is stored, never shown: the acceptance numbers of a close
	// in progress.
	ChangeClosing = "closing"
)

// roundState is one snapshot of a question round: the frame the panel shows
// and what resuming the stage needs.
type roundState struct {
	Number       int   `json:"round"`
	Attempt      int   `json:"attempt"`
	BuilderRound int   `json:"builder_round"`
	RevisedFor   int   `json:"revised_for,omitempty"`
	PostedBy     int64 `json:"posted_by"`
	// RunID is the run whose stored conversation the next segment resumes:
	// the one that posted the round, or the last that answered its chat.
	RunID int64 `json:"run_id"`
	// Seen counts the chat entries that conversation has seen.
	Seen      int    `json:"seen,omitempty"`
	Change    string `json:"change"`
	MessageID string `json:"message_id,omitempty"`
	// Dropped counts the entries the reply that made this snapshot dropped.
	Dropped int `json:"dropped,omitempty"`
	// Accepted maps an assumption the human accepted to the question number
	// its acceptance is recorded under; Converted lists those turned into
	// questions. Both are set when the round closes.
	Accepted  map[string]int    `json:"accepted,omitempty"`
	Converted []string          `json:"converted,omitempty"`
	Frame     protocol.LogRound `json:"frame"`
}

func roundPromptID(n int) string { return fmt.Sprintf("round:%d", n) }

// payload is s as an event payload.
func (s roundState) payload() map[string]any {
	raw, _ := json.Marshal(s) // A roundState is plain data and always encodes.

	var out map[string]any

	_ = json.Unmarshal(raw, &out)

	return out
}

// clone is a deep copy, so a change never writes into the snapshot it read.
func (s roundState) clone() roundState {
	raw, _ := json.Marshal(s) // A roundState is plain data and always encodes.

	var out roundState

	_ = json.Unmarshal(raw, &out)

	return out
}

// complete reports whether every question has an answer and every
// assumption a decision.
func (s roundState) complete() bool { return RoundAnswered(s.Frame) }

// RoundAnswered reports whether every question of round f has an answer and
// every assumption a decision: an open round like that closes and its stage
// goes on.
func RoundAnswered(f protocol.LogRound) bool {
	answered := map[int]bool{}
	for _, a := range f.Answers {
		answered[a.Number] = true
	}

	decided := map[string]bool{}
	for _, d := range f.Decisions {
		decided[d.ID] = true
	}

	for _, q := range f.Questions {
		if !answered[q.Number] {
			return false
		}
	}

	for _, a := range f.Assumptions {
		if !decided[a.ID] {
			return false
		}
	}

	return true
}

// chatPending reports chat entries the stage's conversation has not seen.
func (s roundState) chatPending() bool { return len(s.Frame.Chat) > s.Seen }

// rounds returns the newest snapshot of each of the blueprint's rounds, in
// round order.
func (c *Controller) rounds(ctx context.Context, id string) ([]roundState, error) {
	events, err := c.opts.Store.ListEventsOfKind(ctx, id, EventRound)
	if err != nil {
		return nil, fmt.Errorf("blueprint: read the rounds: %w", err)
	}

	byNumber := map[int]roundState{}

	for _, e := range events {
		var s roundState
		if err := json.Unmarshal(e.Payload, &s); err != nil {
			return nil, fmt.Errorf("blueprint: decode round event %d: %w", e.ID, err)
		}

		byNumber[s.Number] = s
	}

	out := make([]roundState, 0, len(byNumber))
	for _, n := range slices.Sorted(maps.Keys(byNumber)) {
		out = append(out, byNumber[n])
	}

	return out, nil
}

// putRound stores and publishes snapshot s; runID is the run that made the
// change, 0 for a reply.
func (c *Controller) putRound(ctx context.Context, au audit, id string, runID int64, s roundState) {
	au.event(ctx, Event{Kind: EventRound, BlueprintID: id, RunID: runID, Payload: s.payload()})
}

// nextQuestionNumber numbers a new question after every question the
// blueprint has numbered: answered ones, those its rounds asked, the numbers
// a close reserved for accepted assumptions, and those asked alone before
// rounds, which were numbered from 1 in order.
func (c *Controller) nextQuestionNumber(ctx context.Context, id string) (int, error) {
	n, err := c.opts.Store.CountEvents(ctx, id, EventQuestion)
	if err != nil {
		return 0, fmt.Errorf("blueprint: number the question: %w", err)
	}

	answered, err := c.opts.Store.ListQuestions(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("blueprint: number the question: %w", err)
	}

	for _, q := range answered {
		n = max(n, q.Number)
	}

	rounds, err := c.rounds(ctx, id)
	if err != nil {
		return 0, err
	}

	for _, s := range rounds {
		for _, q := range s.Frame.Questions {
			n = max(n, q.Number)
		}

		for _, number := range s.Accepted {
			n = max(n, number)
		}
	}

	return n + 1, nil
}

// nextRoundNumber numbers a new round after every stored one.
func (c *Controller) nextRoundNumber(ctx context.Context, id string) (int, error) {
	rounds, err := c.rounds(ctx, id)
	if err != nil {
		return 0, err
	}

	if len(rounds) == 0 {
		return 1, nil
	}

	return rounds[len(rounds)-1].Number + 1, nil
}

// roundSources resolves every source tag the questions' options cite; a tag
// that is no source of the blueprint is refused, since a link the panel
// shows must lead somewhere the architect read.
func (c *Controller) roundSources(ctx context.Context, id string, qs []stage.RoundQuestion) (map[string]protocol.RoundSource, error) {
	known, err := c.opts.Store.ListSources(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list sources: %w", err)
	}

	byTag := map[string]protocol.RoundSource{}

	for _, src := range known {
		rs := protocol.RoundSource{Tag: src.Tag(), Title: src.Identity}
		if src.Kind == "url" {
			rs.URL = src.Identity
		}

		byTag[src.Tag()] = rs
	}

	out := map[string]protocol.RoundSource{}

	for _, q := range qs {
		for _, o := range q.Options {
			for _, tag := range o.Sources {
				rs, ok := byTag[tag]
				if !ok {
					return nil, fmt.Errorf("option %q of %q cites %s, which no tool result of this blueprint returned; cite only the tags your tool results carry", o.Title, q.Text, tag)
				}

				out[tag] = rs
			}
		}
	}

	return out, nil
}

// frameQuestion is q as the panel shows it, numbered. Options is never nil:
// the wire keeps an empty list.
func frameQuestion(q stage.RoundQuestion, number int, sources map[string]protocol.RoundSource) protocol.RoundQuestion {
	out := protocol.RoundQuestion{
		Number: number, Title: q.Title, Text: q.Text, Why: q.Why,
		Options: make([]protocol.RoundOption, 0, len(q.Options)), Recommended: q.Recommended,
	}

	for _, o := range q.Options {
		ro := protocol.RoundOption{Title: o.Title, Summary: o.Summary, Pros: o.Pros, Cons: o.Cons}
		for _, tag := range o.Sources {
			ro.Sources = append(ro.Sources, sources[tag])
		}

		out.Options = append(out.Options, ro)
	}

	return out
}

// mergeReply applies a reply's answers and decisions to s, the later of two
// for one question winning, and counts the entries it drops: a number or an
// assumption the round lacks, an option out of range, or own words that are
// blank. A pick drops any own words sent with it.
func mergeReply(s roundState, answers []protocol.RoundAnswer, decisions []protocol.AssumptionDecision) (roundState, int) {
	out := s.clone()
	dropped := 0

	questions := map[int]protocol.RoundQuestion{}
	for _, q := range out.Frame.Questions {
		questions[q.Number] = q
	}

	for _, a := range answers {
		q, ok := questions[a.Number]
		a.Text = strings.TrimSpace(a.Text)

		switch {
		case !ok, a.Option < 0, a.Option > len(q.Options), a.Option == 0 && a.Text == "":
			dropped++

			continue
		case a.Option > 0:
			a.Text = ""
		}

		out.Frame.Answers = slices.DeleteFunc(out.Frame.Answers, func(x protocol.RoundAnswer) bool { return x.Number == a.Number })
		out.Frame.Answers = append(out.Frame.Answers, a)
	}

	assumptions := map[string]bool{}
	for _, a := range out.Frame.Assumptions {
		assumptions[a.ID] = true
	}

	for _, d := range decisions {
		if !assumptions[d.ID] {
			dropped++

			continue
		}

		out.Frame.Decisions = slices.DeleteFunc(out.Frame.Decisions, func(x protocol.AssumptionDecision) bool { return x.ID == d.ID })
		out.Frame.Decisions = append(out.Frame.Decisions, d)
	}

	// Question order, so a frame reads like the tabs.
	order := map[int]int{}
	for i, q := range out.Frame.Questions {
		order[q.Number] = i
	}

	slices.SortStableFunc(out.Frame.Answers, func(x, y protocol.RoundAnswer) int { return order[x.Number] - order[y.Number] })

	return out, dropped
}

// revised applies a chat run's revisions to round s: a question with a
// number replaces that question and keeps its answer unless its options
// changed, since answers are option positions; one without is added under
// the next number.
func revised(s roundState, qs []stage.RoundQuestion, first int, sources map[string]protocol.RoundSource) (roundState, error) {
	out := s.clone()

	for _, q := range qs {
		if q.Number == 0 {
			out.Frame.Questions = append(out.Frame.Questions, frameQuestion(q, first, sources))
			first++

			continue
		}

		i := slices.IndexFunc(out.Frame.Questions, func(x protocol.RoundQuestion) bool { return x.Number == q.Number })
		if i < 0 {
			return roundState{}, fmt.Errorf("round %d has no Q%d; revise one of its questions or add a new one", s.Number, q.Number)
		}

		fq := frameQuestion(q, q.Number, sources)
		if !sameOptions(out.Frame.Questions[i], fq) {
			out.Frame.Answers = slices.DeleteFunc(out.Frame.Answers, func(a protocol.RoundAnswer) bool { return a.Number == q.Number })
		}

		out.Frame.Questions[i] = fq
	}

	return out, nil
}

// sameOptions reports whether two versions of a question offer the same
// options in the same order.
func sameOptions(a, b protocol.RoundQuestion) bool {
	return slices.EqualFunc(a.Options, b.Options, func(x, y protocol.RoundOption) bool { return x.Title == y.Title })
}

// retabbed replaces round s's assumptions tab with texts, labelled A1..Ak in
// their order. A decision follows its assumption when the text is unchanged;
// the others are dropped, so a new or reworded assumption waits for one.
func retabbed(s roundState, texts []string) roundState {
	out := s.clone()

	textOf := map[string]string{}
	for _, a := range out.Frame.Assumptions {
		textOf[a.ID] = a.Text
	}

	accepts := map[string]bool{}
	for _, d := range out.Frame.Decisions {
		accepts[textOf[d.ID]] = d.Accept
	}

	out.Frame.Assumptions, out.Frame.Decisions = nil, nil

	for i, text := range texts {
		id := fmt.Sprintf("A%d", i+1)
		out.Frame.Assumptions = append(out.Frame.Assumptions, protocol.RoundAssumption{ID: id, Text: text})

		if accept, ok := accepts[text]; ok {
			out.Frame.Decisions = append(out.Frame.Decisions, protocol.AssumptionDecision{ID: id, Accept: accept})
		}
	}

	return out
}

// writeOpenRound gives a run without the conversation the round as it
// stands: each question, its options and its answer so far, and the
// assumptions tab with each decision so far.
func writeOpenRound(b *strings.Builder, s roundState) {
	answers := map[int]protocol.RoundAnswer{}
	for _, a := range s.Frame.Answers {
		answers[a.Number] = a
	}

	fmt.Fprintf(b, "## Your round %d as it stands\n\n", s.Number)

	for _, q := range s.Frame.Questions {
		fmt.Fprintf(b, "- [Q%d] %s\n", q.Number, q.Text)

		for i, o := range q.Options {
			fmt.Fprintf(b, "  %d. %s\n", i+1, o.Title)
		}

		if a, ok := answers[q.Number]; ok {
			fmt.Fprintf(b, "  Answer so far: %s\n", AnswerText(q, a))
		}
	}

	decisions := map[string]bool{}
	for _, d := range s.Frame.Decisions {
		decisions[d.ID] = d.Accept
	}

	for _, a := range s.Frame.Assumptions {
		fmt.Fprintf(b, "- %s: %s\n", a.ID, a.Text)

		if accept, ok := decisions[a.ID]; ok {
			fmt.Fprintf(b, "  Decision so far: %s\n", decisionText(accept))
		}
	}

	b.WriteString("\n")
}

// decisionText is an assumption decision as a model and a reader see it.
func decisionText(accept bool) string {
	if accept {
		return "accepted"
	}

	return "turned into a question"
}

// AnswerText is an answer as a model and a reader see it: the chosen
// option's title, or the person's own words.
func AnswerText(q protocol.RoundQuestion, a protocol.RoundAnswer) string {
	if a.Option > 0 && a.Option <= len(q.Options) {
		return q.Options[a.Option-1].Title
	}

	return a.Text
}

// closeRound records an answered round: one questions row per question, as
// the run that posted it asked it, one per assumption a human accepted, and
// the closed snapshot. Rows a close cut short already wrote are kept and
// never written twice.
func (c *Controller) closeRound(ctx context.Context, au audit, bp store.Blueprint, s roundState) (roundState, error) {
	written, err := c.opts.Store.ListQuestions(ctx, bp.ID)
	if err != nil {
		return roundState{}, fmt.Errorf("blueprint: close round %d: %w", s.Number, err)
	}

	have := map[int]bool{}
	for _, q := range written {
		have[q.Number] = true
	}

	answers := map[int]protocol.RoundAnswer{}
	for _, a := range s.Frame.Answers {
		answers[a.Number] = a
	}

	responder := store.ResponderHuman
	if s.Frame.Responder {
		responder = store.ResponderAuto
	}

	msgID, err := c.answeringMessage(ctx, bp.ID, s)
	if err != nil {
		return roundState{}, fmt.Errorf("blueprint: close round %d: %w", s.Number, err)
	}

	for _, q := range s.Frame.Questions {
		if have[q.Number] {
			continue
		}

		options := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			options = append(options, o.Title)
		}

		if err := c.opts.Store.AddQuestion(ctx, store.Question{
			BlueprintID: bp.ID, RunID: s.PostedBy, Stage: s.Frame.Stage, Number: q.Number,
			Text: au.text(q.Text), Options: au.texts(options), Context: au.text(q.Why),
			Answer: au.text(AnswerText(q, answers[q.Number])), Responder: responder,
			Assumption: responder == store.ResponderAuto, MessageID: msgID, AnsweredAt: c.opts.Now().UTC(),
		}); err != nil {
			return roundState{}, fmt.Errorf("blueprint: close round %d: record Q%d: %w", s.Number, q.Number, err)
		}
	}

	s, err = c.recordAcceptances(ctx, au, bp, s, have, msgID)
	if err != nil {
		return roundState{}, err
	}

	closed := s.clone()
	closed.Change, closed.Frame.Closed = changeClosed, true
	c.putRound(ctx, au, bp.ID, 0, closed)

	return closed, nil
}

// recordAcceptances records each assumption a human accepted on round s's
// tab as an answered question, under a number reserved once, and lists the
// ones turned into questions; s with both set. have holds the numbers
// already written. A responder's round records none: under auto nothing is
// confirmed.
func (c *Controller) recordAcceptances(ctx context.Context, au audit, bp store.Blueprint, s roundState, have map[int]bool, msgID string) (roundState, error) {
	if s.Frame.Responder {
		return s, nil
	}

	if s.Accepted == nil {
		var accepted []string

		for _, d := range s.Frame.Decisions {
			if d.Accept {
				accepted = append(accepted, d.ID)
			}
		}

		if len(accepted) > 0 {
			first, err := c.nextQuestionNumber(ctx, bp.ID)
			if err != nil {
				return roundState{}, err
			}

			// The numbers are stored before any row, so a close cut short
			// writes the same rows again, never new ones.
			s = s.clone()
			s.Accepted = map[string]int{}

			for i, id := range accepted {
				s.Accepted[id] = first + i
			}

			s.Change = ChangeClosing
			c.putRound(ctx, au, bp.ID, 0, s)
		}
	}

	for _, a := range s.Frame.Assumptions {
		n, ok := s.Accepted[a.ID]
		if !ok || have[n] {
			continue
		}

		if err := c.opts.Store.AddQuestion(ctx, store.Question{
			BlueprintID: bp.ID, RunID: s.PostedBy, Stage: s.Frame.Stage, Number: n,
			Text: au.text(a.Text), Options: []string{"Accept", "Turn into a question"},
			Context: fmt.Sprintf("Round %d's assumptions tab, %s", s.Number, a.ID),
			Answer:  "Accepted", Responder: store.ResponderHuman, MessageID: msgID, AnsweredAt: c.opts.Now().UTC(),
		}); err != nil {
			return roundState{}, fmt.Errorf("blueprint: close round %d: record %s as Q%d: %w", s.Number, a.ID, n, err)
		}
	}

	s.Converted = nil

	for _, d := range s.Frame.Decisions {
		if !d.Accept {
			s.Converted = append(s.Converted, d.ID)
		}
	}

	return s, nil
}

// answeringMessage is the message whose reply last stored answers in round
// s, which a chat message or a chat run's reply may follow; s's own when no
// stored answers snapshot names one.
func (c *Controller) answeringMessage(ctx context.Context, id string, s roundState) (string, error) {
	if s.Change == changeAnswers {
		return s.MessageID, nil
	}

	events, err := c.opts.Store.ListEventsOfKind(ctx, id, EventRound)
	if err != nil {
		return "", fmt.Errorf("read the rounds: %w", err)
	}

	msgID := s.MessageID

	for _, e := range events {
		var snap roundState
		if err := json.Unmarshal(e.Payload, &snap); err != nil {
			return "", fmt.Errorf("decode round event %d: %w", e.ID, err)
		}

		if snap.Number == s.Number && snap.Change == changeAnswers {
			msgID = snap.MessageID
		}
	}

	return msgID, nil
}

// roundNext is what the round the pipeline stands at waits for.
type roundNext int

const (
	// roundWait: answers.
	roundWait roundNext = iota
	// roundChat: a chat segment for the user's messages its conversation has
	// not seen.
	roundChat
	// roundClose: every question is answered, so the round closes and the
	// stage continues.
	roundClose
	// roundContinue: closed, so the stage continues from it.
	roundContinue
)

// roundPlace is the round the pipeline stands at, what it waits for, and
// whether the run after it failed or was aborted (stalled): that run waits
// for the next message, as a failed stage does.
type roundPlace struct {
	state   roundState
	next    roundNext
	stalled bool
}

// roundAt finds the round the pipeline stands at: the blueprint's newest
// round while the stage run that last touched it ended asked, and no run of
// its stage since then completed, posted another round, or belongs to a
// later attempt or builder round. found is false otherwise. A run that
// session end or a restart cut short leaves the round where it was.
func (c *Controller) roundAt(ctx context.Context, id string) (roundPlace, bool, error) {
	rounds, err := c.rounds(ctx, id)
	if err != nil || len(rounds) == 0 {
		return roundPlace{}, false, err
	}

	s := rounds[len(rounds)-1]

	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return roundPlace{}, false, fmt.Errorf("blueprint: list runs: %w", err)
	}

	var (
		touched bool
		last    store.Run
	)

	for _, r := range runs {
		if r.Kind != store.RunKindStage || r.Stage != s.Frame.Stage || r.ID < s.RunID {
			continue
		}

		if r.ID == s.RunID {
			touched = r.Status == store.RunAsked

			continue
		}

		if r.Attempt != s.Attempt || r.Round != s.BuilderRound || r.Status == store.RunDone || r.Status == store.RunAsked {
			return roundPlace{}, false, nil
		}

		last = r
	}

	if !touched {
		return roundPlace{}, false, nil
	}

	place := roundPlace{state: s}
	place.stalled = last.Status == store.RunFailed || (last.Status == store.RunCanceled && last.Reason != reasonSessionEnded)

	switch {
	case s.Frame.Closed:
		place.next = roundContinue
	case s.complete():
		place.next = roundClose
	case s.chatPending():
		place.next = roundChat
	default:
		place.next = roundWait
	}

	return place, true, nil
}

// continuation is the run that goes on from closed round s: its attempt and
// builder round, the conversation of the run that last touched it, and as
// the next message the answers, the chat and the notes that conversation has
// not seen. Without that conversation a first build starts over in its
// attempt, given every answered question by its task; a builder round
// returns nil, and its critique runs it again from the build it revises.
func (c *Controller) continuation(ctx context.Context, bp store.Blueprint, s roundState) (*round, error) {
	rd := &round{attempt: s.Attempt, number: s.BuilderRound, findings: s.RevisedFor, segment: SegmentAnswers, qround: s.Number}

	rd.history = c.runHistory(ctx, bp.ID, s.RunID)
	if rd.history == nil {
		if s.BuilderRound > 1 {
			return nil, nil
		}

		return rd, nil
	}

	var b strings.Builder

	writeRoundAnswers(&b, s)
	writeRoundAssumptions(&b, s)
	writeRoundChat(&b, s)

	if err := c.writeNewNotes(ctx, &b, bp.ID, s.RunID); err != nil {
		return nil, err
	}

	b.WriteString("Go on with your stage. Where an answer is vague, contradicts another or raises a new question, ask a follow-up round; otherwise submit your artifact.\n")
	rd.task = b.String()

	return rd, nil
}

// writeRoundAnswers lists a round's questions with their answers and who
// gave them, in the tagged form a basis cites.
func writeRoundAnswers(b *strings.Builder, s roundState) {
	who := answeredByUser
	if s.Frame.Responder {
		who = answeredByResponder
	}

	answers := map[int]protocol.RoundAnswer{}
	for _, a := range s.Frame.Answers {
		answers[a.Number] = a
	}

	fmt.Fprintf(b, "## Answers to round %d\n\n", s.Number)

	for _, q := range s.Frame.Questions {
		fmt.Fprintf(b, "- [Q%d] %s\n  Answer (%s): %s\n", q.Number, q.Text, who, AnswerText(q, answers[q.Number]))
	}

	b.WriteString("\n")
}

// writeRoundAssumptions tells a resumed conversation how each assumption on
// round s's tab was decided, with the tag an accepted one is cited by.
func writeRoundAssumptions(b *strings.Builder, s roundState) {
	if len(s.Frame.Assumptions) == 0 {
		return
	}

	converted := map[string]bool{}
	for _, id := range s.Converted {
		converted[id] = true
	}

	fmt.Fprintf(b, "## Assumptions tab of round %d\n\n", s.Number)

	for _, a := range s.Frame.Assumptions {
		fmt.Fprintf(b, "- %s: %s\n", a.ID, a.Text)

		n, accepted := s.Accepted[a.ID]

		switch {
		case s.Frame.Responder:
			b.WriteString("  Accepted automatically under the auto policy; it stays unverified: state it without a basis.\n")
		case accepted:
			fmt.Fprintf(b, "  Accepted by the user as [Q%d]: state it with the basis Q%d.\n", n, n)
		case converted[a.ID]:
			b.WriteString("  Turned into a question by the user: research it if you can, then ask it in your next round.\n")
		}
	}

	b.WriteString("\n")
}

// writeRoundChat gives a resumed conversation the chat entries it has not
// seen, oldest first.
func writeRoundChat(b *strings.Builder, s roundState) {
	if !s.chatPending() {
		return
	}

	fmt.Fprintf(b, "## Chat about round %d you have not answered\n\n", s.Number)

	for _, m := range s.Frame.Chat[s.Seen:] {
		fmt.Fprintf(b, "- %s: %s\n", m.From, m.Text)
	}

	b.WriteString("\n")
}
