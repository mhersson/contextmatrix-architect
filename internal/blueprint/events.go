package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Event is one controller event. Every event is stored and handed to the sink.
type Event struct {
	Kind        string
	BlueprintID string
	RunID       int64
	Time        time.Time
	Payload     map[string]any
	// Sliced is set on the way to the sink and never stored: the blueprint
	// runs per slice with two or more slices, so text a person reads names
	// a per-slice step's slice.
	Sliced bool
	// Plain is set on the way to the sink and never stored: the blueprint
	// runs in plain language, so what a person reads is worded plainly.
	Plain bool
}

// Event kinds.
const (
	EventCreated      = "blueprint_created"
	EventReopened     = "blueprint_reopened"
	EventStageStarted = "stage_started"
	// EventQuestion is a question asked alone, in a record from before rounds;
	// question numbers continue after them.
	EventQuestion = "question"
	EventAnswer   = "answer"
	// EventRound is one snapshot of a question round, the whole round each
	// time: as posted, after answers, chat or a revision, and when closed.
	// The newest snapshot of a round number is the round.
	EventRound      = "round"
	EventArtifact   = "artifact"
	EventStageEnded = "stage_ended"
	EventNote       = "note"
	EventAborted    = "aborted"
	EventExported   = "exported"
	EventError      = "error"
	// EventAwaitInput says a stopped emission or run start waits for
	// approve; the payload carries stage signoff and the prompt to show.
	EventAwaitInput = "await_input"
	// EventGrounded lists the sources a code stage checked out.
	EventGrounded = "grounded"
	// EventResearch is one web_search or web_fetch call: the query and the
	// result count, or the URL with the page's final URL, title and source
	// tag, or the guard rule that refused it, or the error. A search's
	// result list is never in it.
	EventResearch = "research"
	// EventResearchOff says, once per session, at the first stage with the
	// research tools, that research is off and why.
	EventResearchOff = "research_off"
	// EventSetupGaps lists, when the first stage with the code tools ends,
	// what each target project lacks for an agent run: a verify command, an
	// instruction file, any commit. None of it blocks; decomposition plans
	// it as setup. The payload also carries the blueprint's mode.
	EventSetupGaps = "setup_gaps"
	// EventPolicy records a gate <stage> human|auto reply: the stage's new
	// policy, or why the reply changed nothing.
	EventPolicy = "policy"
	// EventCap records a cap <usd>|off reply: the cap it changed from and to
	// and the stage it resumes, or why it changed nothing.
	EventCap = "cap"
	// EventMode records a mode <m> reply: the mode it switched from and to,
	// the round it handed to the responder, the step key of the stage gate it
	// decided (gate) and whether it ran the sign-off again, or why it
	// changed nothing.
	EventMode = "mode"
	// EventResume records a resume or continue reply: the stopped stage it
	// runs again, or that nothing waits to resume and the stage running.
	EventResume = "resume"
	// EventGatePlan lists, once the first stage has completed, who answers
	// each stage's questions and decides its gate.
	EventGatePlan = "gate_plan"
	// EventReport is the pipeline's end: the final artifact, whether the
	// cards are ready and the findings that say why not, and whether the
	// sign-off follows, as it does over the card findings readiness's last
	// round left open.
	EventReport = "report"
	// EventCardCreated records one card emission created, with CM's lint
	// warnings; EventPlaybookCreated the playbook.
	EventCardCreated     = "card_created"
	EventPlaybookCreated = "playbook_created"
	// EventEmitted says every card (and the playbook) exists in ContextMatrix.
	EventEmitted = "emitted"
	// EventRunStarted records the yolo run start: run_card for one card,
	// run_playbook for several.
	EventRunStarted = "run_started"
	// EventWatch is one change the watch saw in ContextMatrix. EventWatchEnded
	// is the closing summary at the end of the run, posted once: the pull
	// requests, the compare links and the architect's total cost.
	EventWatch      = "watch"
	EventWatchEnded = "watch_ended"
	// EventWatchState stores the snapshot the watch last reported, so a
	// watch that restarts posts only what changed; the panel never shows it.
	EventWatchState = "watch_state"
	// EventSignOff shows the cards, the card findings readiness's last round
	// left open (not_ready, empty when every card passed), the preflight
	// result and how to decide; the blueprint waits for approve or reject.
	EventSignOff = "signoff"
	// EventSignOffAuto records the sign-off an emitting mode approved on its
	// own, with the payload a shown sign-off carries less its replies and its
	// auto_stopped flag, so the sheet shows what was approved. It follows the
	// approval and is never shown in the transcript.
	EventSignOffAuto = "signoff_auto"
	// EventGate shows a stage's gate: the stage, the version of its artifact
	// that waits, the artifacts folded into it, and the replies that decide
	// it.
	EventGate = "gate"
	// EventReview is a reviewer's verdict on a stage's artifact: the score,
	// the threshold, whether it passed, the round, the reviewer, the ledger
	// (each finding's id, severity, text, status, the version a re-check
	// verified, the reviewer's note and the builder's claim) and the
	// critical findings still open.
	EventReview = "review"
	// EventDecided records a decision at a stage's gate or the sign-off, by a
	// human or the auto policy, or a stage's fold into a later gate.
	EventDecided = "decided"
	// EventMockupOffer records the offer of a first look before the mockup
	// step runs: its state (shown, with the replies it takes; accepted;
	// declined), the mockup stage, the message id of the reply that decided
	// it, and by "mode" when a mode switch took it. The newest one is the
	// offer.
	EventMockupOffer = "mockup_offer"
	// EventRetroReply answers a retrospective reply that starts no pass, or
	// ends a pass that did not complete: the reason code, the message, the
	// reply's message id and whether a run goes on working after it.
	EventRetroReply = "retro_reply"
	// EventRetroReport is a pass's findings message: every struggled card
	// with its signals, its finding and its proposed lesson. It is the
	// pass's only record of the findings.
	EventRetroReport = "retro_report"
	// EventRetroStopOffered is the offer of the retrospective at the first
	// emitted card the watch sees parked, stalled or blocked: its line. It is
	// stored once per blueprint and starts nothing.
	EventRetroStopOffered = "retro_stop_offered"
	// EventLessonDecided records the decision a lesson round's answer made
	// on one lesson: its id, new state, scope and text, its project when the
	// scope is a project's, the merged lists' error when an approval was
	// refused, and the answering reply's message id.
	EventLessonDecided = "lesson_decided"
	// EventLessonList is the reply to a lessons reply: every lesson of every
	// blueprint with its state, scope, project, text, rationale, evidence card
	// ids and decider, the reply's message id and whether a run goes on
	// working after it.
	EventLessonList = "lesson_list"
	// EventEnded records that CM ended the session; the blueprint persists.
	EventEnded = "session_ended"
	// EventHarness wraps one raw harness JSONL line; the payload is the line.
	EventHarness = "harness"
	// EventState records a change of the blueprint's state. It is stored,
	// never sent to the panel: the view's sequence is the newest events row,
	// so every change of what the view shows writes one.
	EventState = "state"
)

var errNotJSON = errors.New("not a JSON document")

// audit writes a session's record and publishes its events. Every text it
// stores or hands on passes the session's redactor first: this is the one
// place secrets are masked.
type audit struct {
	c   *Controller
	red *redact.Redactor
}

func (c *Controller) newAudit(secrets ...string) audit {
	return audit{c: c, red: redact.New(secrets)}
}

// text masks the session's secrets in s.
func (a audit) text(s string) string {
	return a.red.Apply(s)
}

// texts masks the session's secrets in every string of ss.
func (a audit) texts(ss []string) []string {
	if ss == nil {
		return nil
	}

	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = a.text(s)
	}

	return out
}

// event stores e, then hands it to the sink.
func (a audit) event(ctx context.Context, e Event) {
	if e.Time.IsZero() {
		e.Time = a.c.opts.Now().UTC()
	}

	e.Payload = a.maskFields(e.Payload)

	payload, err := marshalPayload(e.Payload)
	if err != nil {
		a.c.opts.Logger.Error("blueprint: encode event", "kind", e.Kind, "error", err)
	}

	if err := a.c.opts.Store.AppendEvent(ctx, store.Event{
		Time: e.Time, BlueprintID: e.BlueprintID,
		RunID: e.RunID, Kind: e.Kind, Payload: payload,
	}); err != nil {
		a.c.opts.Logger.Error("blueprint: store event", "kind", e.Kind, "blueprint", e.BlueprintID, "error", err)
	}

	if a.c.opts.Sink != nil {
		// The caller's cancel must not hide the slices from the sink.
		e.Sliced = a.c.sliced(context.WithoutCancel(ctx), e.BlueprintID)

		// A read error leaves it false: the sink then words the event as
		// without the toggle.
		if bp, err := a.c.opts.Store.GetBlueprint(context.WithoutCancel(ctx), e.BlueprintID); err == nil {
			e.Plain = bp.PlainLanguage
		}

		a.c.opts.Sink(e)
	}

	a.c.observe(e)
}

// harness stores one raw harness JSONL line as an event, then forwards it
// unless forward is false.
func (a audit) harness(ctx context.Context, id string, runID int64, line []byte, forward bool) {
	masked, err := a.maskJSON(line)
	if err != nil {
		a.c.opts.Logger.Warn("blueprint: harness line dropped", "blueprint", id, "error", err)

		return
	}

	if err := a.c.opts.Store.AppendEvent(ctx, store.Event{
		Time: a.c.opts.Now().UTC(), BlueprintID: id,
		RunID: runID, Kind: EventHarness, Payload: masked,
	}); err != nil {
		a.c.opts.Logger.Error("blueprint: store harness event", "blueprint", id, "error", err)
	}

	if forward && a.c.opts.HarnessLine != nil {
		a.c.opts.HarnessLine(id, masked)
	}

	a.c.live.touch(id)
}

// artifact stores the next version of a stage's artifact and returns it.
func (a audit) artifact(ctx context.Context, id, stageID, body string, data map[string]any) (int, error) {
	var raw json.RawMessage

	if data != nil {
		b, err := json.Marshal(a.maskFields(data))
		if err != nil {
			return 0, fmt.Errorf("blueprint: encode %s artifact data: %w", a.c.stepName(ctx, id, stageID), err)
		}

		raw = b
	}

	version, err := a.c.opts.Store.PutArtifact(ctx, store.Artifact{
		BlueprintID: id, Stage: stageID, Body: a.text(body), Data: raw,
	})
	if err != nil {
		return 0, fmt.Errorf("blueprint: store %s artifact: %w", a.c.stepName(ctx, id, stageID), err)
	}

	return version, nil
}

// transcript stores a run's message history. A resume sends reasoning details
// back and the provider refuses a changed block but not a missing one, so a
// message whose details masking would change loses them whole.
func (a audit) transcript(ctx context.Context, runID int64, messages []llm.Message) error {
	kept, cloned := messages, false

	for i, m := range messages {
		if len(m.ReasoningDetails) == 0 || !a.changes(m.ReasoningDetails) {
			continue
		}

		if !cloned {
			kept, cloned = slices.Clone(messages), true
		}

		kept[i].ReasoningDetails = nil
	}

	b, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("blueprint: encode transcript: %w", err)
	}

	masked, err := a.maskJSON(b)
	if err != nil {
		return err
	}

	if err := a.c.opts.Store.PutTranscript(ctx, runID, masked); err != nil {
		return fmt.Errorf("blueprint: store transcript: %w", err)
	}

	return nil
}

// maskFields masks every string in a payload. Payload values are strings,
// string slices, numbers, bools and JSON-decoded maps and slices; any other
// type passes unchanged.
func (a audit) maskFields(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}

	out := make(map[string]any, len(m))
	for k, v := range m {
		out[a.text(k)] = a.mask(v)
	}

	return out
}

func (a audit) mask(v any) any {
	switch t := v.(type) {
	case string:
		return a.text(t)
	case []string:
		return a.texts(t)
	case []any:
		if t == nil {
			return t
		}

		out := make([]any, len(t))
		for i, x := range t {
			out[i] = a.mask(x)
		}

		return out
	case map[string]any:
		return a.maskFields(t)
	default:
		return v
	}
}

// changes reports whether masking changes any string or key in raw; a
// document it cannot decode counts as changed.
func (a audit) changes(raw json.RawMessage) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return true
	}

	return !reflect.DeepEqual(v, a.mask(v))
}

// maskJSON masks every string in a JSON document. Decoding first catches a
// secret whose characters JSON escapes; numbers are kept verbatim.
func (a audit) maskJSON(raw []byte) (json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, errNotJSON
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("blueprint: decode for masking: %w", err)
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(a.mask(v)); err != nil {
		return nil, fmt.Errorf("blueprint: encode masked document: %w", err)
	}

	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func marshalPayload(p map[string]any) (json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}

	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("blueprint: marshal payload: %w", err)
	}

	return b, nil
}
