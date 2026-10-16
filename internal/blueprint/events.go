package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
}

// Event kinds.
const (
	EventCreated      = "blueprint_created"
	EventReopened     = "blueprint_reopened"
	EventStageStarted = "stage_started"
	EventQuestion     = "question"
	EventAnswer       = "answer"
	EventArtifact     = "artifact"
	EventStageEnded   = "stage_ended"
	EventNote         = "note"
	EventAborted      = "aborted"
	EventError        = "error"
	// EventEnded records that CM ended the session; the blueprint persists.
	EventEnded = "session_ended"
	// EventHarness wraps one raw harness JSONL line; the payload is the line.
	EventHarness = "harness"
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
		a.c.opts.Sink(e)
	}
}

// harness stores one raw harness JSONL line as an event, then forwards it.
func (a audit) harness(ctx context.Context, id string, runID int64, line []byte) {
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

	if a.c.opts.HarnessLine != nil {
		a.c.opts.HarnessLine(id, masked)
	}
}

// artifact stores the next version of a stage's artifact and returns it.
func (a audit) artifact(ctx context.Context, id, stageID, body string, data map[string]any) (int, error) {
	var raw json.RawMessage

	if data != nil {
		b, err := json.Marshal(a.maskFields(data))
		if err != nil {
			return 0, fmt.Errorf("blueprint: encode %s artifact data: %w", stageID, err)
		}

		raw = b
	}

	version, err := a.c.opts.Store.PutArtifact(ctx, store.Artifact{
		BlueprintID: id, Stage: stageID, Body: a.text(body), Data: raw,
	})
	if err != nil {
		return 0, fmt.Errorf("blueprint: store %s artifact: %w", stageID, err)
	}

	return version, nil
}

// transcript stores a run's message history.
func (a audit) transcript(ctx context.Context, runID int64, messages any) error {
	b, err := json.Marshal(messages)
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
		if t == nil {
			return t
		}

		out := make([]string, len(t))
		for i, s := range t {
			out[i] = a.text(s)
		}

		return out
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
