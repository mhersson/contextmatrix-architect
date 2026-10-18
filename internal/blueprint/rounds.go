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
	// ChangeLeft marks a round the pipeline went past without its answers;
	// the panel collapses it like a closed one.
	ChangeLeft = "left"
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
	Dropped int               `json:"dropped,omitempty"`
	Frame   protocol.LogRound `json:"frame"`
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
// blueprint has numbered: answered ones, those its rounds asked, and those
// asked alone before rounds, which were numbered from 1 in order.
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

// AnswerText is an answer as a model and a reader see it: the chosen
// option's title, or the person's own words.
func AnswerText(q protocol.RoundQuestion, a protocol.RoundAnswer) string {
	if a.Option > 0 && a.Option <= len(q.Options) {
		return q.Options[a.Option-1].Title
	}

	return a.Text
}

// closeRound records an answered round: one questions row per question, as
// the run that posted it asked it, and the closed snapshot. Rows a close cut
// short already wrote are kept and never written twice.
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
			Assumption: responder == store.ResponderAuto, MessageID: s.MessageID, AnsweredAt: c.opts.Now().UTC(),
		}); err != nil {
			return roundState{}, fmt.Errorf("blueprint: close round %d: record Q%d: %w", s.Number, q.Number, err)
		}
	}

	closed := s.clone()
	closed.Change, closed.Frame.Closed = changeClosed, true
	c.putRound(ctx, au, bp.ID, 0, closed)

	return closed, nil
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
