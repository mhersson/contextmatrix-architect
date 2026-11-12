package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// bridged wraps a tool served over the loopback MCP bridge, where the
// harness's own redaction and output cap do not run. Terminal is the
// inner tool's, so offMode stays non-terminal.
func bridged(t tools.Tool, redact func(string) string) tools.Tool {
	return bridgedTool{Tool: t, redact: redact}
}

type bridgedTool struct {
	tools.Tool

	redact func(string) string
}

func (b bridgedTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	res, err := b.Tool.Execute(ctx, args)
	res.Text = tools.HeadTail(b.redact(res.Text), ToolOutputMaxBytes)

	if err != nil {
		// Keep the original error (and its wrapping) when nothing changed.
		if msg := tools.HeadTail(b.redact(err.Error()), ToolOutputMaxBytes); msg != err.Error() {
			err = errors.New(msg)
		}
	}

	return res, err
}

func (b bridgedTool) Terminal() bool {
	term, ok := b.Tool.(tools.Terminal)

	return ok && term.Terminal()
}

// callWatch notes the tool names of the newest API call, fed by the stream's
// deltas and reset by the call's model_request line, so finalEvents can tell
// whether the reply ends the run when its model_response line passes.
type callWatch struct {
	// w receives every line written; submits says the run's mode takes
	// submit_artifact. Both are set before the run starts.
	w       io.Writer
	submits bool

	mu    sync.Mutex
	names []string
}

func (w *callWatch) delta(d llm.Delta) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if d.ToolCall != nil && d.ToolCall.Name != "" {
		w.names = append(w.names, d.ToolCall.Name)
	}
}

func (w *callWatch) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(events.ModelRequest)) {
		var ev struct {
			Kind string `json:"kind"`
		}

		if json.Unmarshal(bytes.TrimSpace(p), &ev) == nil && ev.Kind == string(events.ModelRequest) {
			w.mu.Lock()
			w.names = nil
			w.mu.Unlock()
		}
	}

	n, err := w.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("stage: write a bridged event: %w", err)
	}

	return n, nil
}

func (w *callWatch) final() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.names) == 0 {
		return true
	}

	if w.submits {
		for _, n := range w.names {
			if n == submitArtifactName {
				return true
			}
		}
	}

	return false
}
