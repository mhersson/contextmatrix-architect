package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
)

// FinalField marks the model_response event of a reply a run ends on: one
// that calls submit_artifact in a run whose mode takes it, or calls no tool,
// which ends the harness run or makes it force the terminal call. The record
// keeps the reply; the artifact, review or answer it restates has its own
// row in the transcript.
const FinalField = "final"

// replyWatch is the run's client, noting whether its newest reply ends the
// run: the reply's model_response event counts its tool calls but does not
// name them. The harness emits that event on the goroutine that sent, right
// after the reply returns.
type replyWatch struct {
	llm.LLM

	// submits says the run's mode takes submit_artifact; it is set before
	// the run starts and never changes.
	submits bool
	final   atomic.Bool
}

func (w *replyWatch) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	resp, err := w.LLM.Send(ctx, req)
	w.note(resp)

	return resp, err
}

func (w *replyWatch) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	resp, err := w.LLM.SendStream(ctx, req, onDelta)
	w.note(resp)

	return resp, err
}

func (w *replyWatch) note(resp llm.Response) {
	final := len(resp.ToolCalls) == 0

	for _, tc := range resp.ToolCalls {
		if w.submits && tc.Function.Name == submitArtifactName {
			final = true
		}
	}

	w.final.Store(final)
}

// finalEvents adds FinalField to the model_response line of a reply that
// ends the run.
type finalEvents struct {
	w     io.Writer
	watch *replyWatch
}

func (f finalEvents) Write(p []byte) (int, error) {
	line := p

	var ev map[string]any

	if f.watch.final.Load() && json.Unmarshal(bytes.TrimSpace(p), &ev) == nil && ev["kind"] == string(events.ModelResponse) {
		if data, ok := ev["data"].(map[string]any); ok {
			data[FinalField] = true

			if marked, err := json.Marshal(ev); err == nil {
				marked = append(marked, '\n')
				line = marked
			}
		}
	}

	if _, err := f.w.Write(line); err != nil {
		return 0, fmt.Errorf("stage: write a harness event: %w", err)
	}

	return len(p), nil
}
