// Package stage runs one unit of AI work: a persona, the stage instructions,
// the stage's tools and a terminal submit, as one harness run.
package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// ErrNoArtifact is returned when the run ended without submit_artifact.
var ErrNoArtifact = errors.New("stage: run ended without an artifact")

// Input is everything one run needs.
type Input struct {
	Stage        string
	Persona      string
	Instructions string
	// Task is the first user message: the need, or the consumed artifacts.
	Task       string
	Model      string
	Endpoint   protocol.LLMEndpoint
	MaxCostUSD float64
	MaxTurns   int
	// Asker answers ask_human; nil makes ask_human fail as a tool error.
	Asker Asker
	// Events receives the harness JSONL event stream; nil discards it.
	Events io.Writer
	// Secrets are masked in every tool result.
	Secrets []string
	// History seeds the conversation for a critique round; nil starts fresh.
	History []llm.Message
	// Tools are offered to the model next to ask_human and submit_artifact;
	// the code stages pass CodeTools.
	Tools []tools.Tool
	// Validate runs on every submit_artifact call. An error goes back to
	// the model as the tool result and the run continues; nil accepts.
	Validate func(Artifact) error
}

// Output is the artifact and the harness result.
type Output struct {
	Artifact Artifact
	Result   harness.Result
}

// Runner runs stages. NewLLM builds the client per run; tests inject a fake.
type Runner struct {
	NewLLM func(protocol.LLMEndpoint) llm.LLM
}

// NewRunner returns a Runner that talks to the provisioned endpoint.
func NewRunner() *Runner {
	return &Runner{NewLLM: ClientFor}
}

// Run executes one stage and returns its artifact.
func (r *Runner) Run(ctx context.Context, in Input) (Output, error) {
	// The registry keeps the last tool under a name, so a clash would silently
	// replace a built-in.
	for _, t := range in.Tools {
		if name := t.Name(); name == "ask_human" || name == "submit_artifact" {
			return Output{}, fmt.Errorf("stage %s: tool %q clashes with a built-in tool", in.Stage, name)
		}
	}

	all := append([]tools.Tool{askHumanTool{asker: in.Asker}, submitArtifactTool{validate: in.Validate}}, in.Tools...)
	reg := tools.NewRegistry(all...)

	sink := in.Events
	if sink == nil {
		sink = io.Discard
	}

	emit := events.NewEmitter(nil, sink)

	cfg := harness.Config{
		Model:            in.Model,
		SystemPrompt:     in.Persona + "\n\n" + in.Instructions,
		MaxTurns:         in.MaxTurns,
		MaxCostUSD:       in.MaxCostUSD,
		RedactToolOutput: redact.New(in.Secrets).Apply,
		History:          in.History,
	}

	res, err := harness.Run(ctx, r.NewLLM(in.Endpoint), reg, emit, in.Task, cfg)
	if err != nil {
		return Output{Result: res}, fmt.Errorf("stage %s: %w", in.Stage, err)
	}

	if !res.Completed || res.CompletionArgs == nil {
		return Output{Result: res}, fmt.Errorf("%w: reason %s", ErrNoArtifact, res.Reason)
	}

	var art Artifact
	if err := json.Unmarshal(res.CompletionArgs, &art); err != nil {
		return Output{Result: res}, fmt.Errorf("stage %s: decode artifact: %w", in.Stage, err)
	}

	return Output{Artifact: art, Result: res}, nil
}
