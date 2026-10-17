// Package stage runs one unit of AI work: a persona, the stage instructions,
// the stage's tools and a terminal submit, as one harness run.
package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// ErrNoArtifact is returned when the run ended without submit_artifact.
var ErrNoArtifact = errors.New("stage: run ended without an artifact")

// ToolOutputMaxBytes caps one tool result as the model sees it, head and
// tail kept: a CM card read can carry a long activity log, and the cap
// matches the chat backend's.
const ToolOutputMaxBytes = 128 << 10

// A run whose prompt reaches 85% of a 128k window summarizes its older
// turns and goes on, keeping the last six messages verbatim: the chat
// backend's bounds. Dialog stages grow with a human's answers.
const (
	contextWindow       = 128000
	compactionThreshold = 0.85
	keepRecentMessages  = 6
)

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
	// Validate runs on every submit_artifact call with the call's context.
	// An error goes back to the model as the tool result and the run
	// continues; nil accepts.
	Validate func(ctx context.Context, a Artifact) error
	// Draft receives the artifact body while the model writes it: redacted
	// frames at most every draftInterval, the whole body when the submit
	// arrives, a refused frame when a check refuses it, and a void frame
	// when the harness abandons the turn that wrote it. nil drafts
	// nothing; reviewer and responder runs leave it nil.
	Draft func(DraftFrame)
}

// Output is the artifact and the harness result.
type Output struct {
	Artifact Artifact
	Result   harness.Result
}

// Runner runs stages. NewLLM builds the client per run; tests inject a fake.
type Runner struct {
	NewLLM func(protocol.LLMEndpoint) llm.LLM

	// draftNow and draftEvery replace the clock and the frame spacing in
	// tests; the zero values mean time.Now and draftInterval.
	draftNow   func() time.Time
	draftEvery time.Duration
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

	var drafts *draftExtractor

	if in.Draft != nil {
		now, every := r.draftNow, r.draftEvery
		if now == nil {
			now = time.Now
		}

		if every == 0 {
			every = draftInterval
		}

		drafts = newDraftExtractor(in.Secrets, in.Draft, now, every)
	}

	all := append([]tools.Tool{askHumanTool{asker: in.Asker}, submitArtifactTool{validate: in.Validate, drafts: drafts}}, in.Tools...)
	reg := tools.NewRegistry(all...)

	sink := in.Events
	if sink == nil {
		sink = io.Discard
	}

	if drafts != nil {
		sink = draftEvents{w: sink, drafts: drafts}
	}

	emit := events.NewEmitter(nil, sink)

	cfg := harness.Config{
		Model:              in.Model,
		SystemPrompt:       in.Persona + "\n\n" + in.Instructions,
		MaxTurns:           in.MaxTurns,
		MaxCostUSD:         in.MaxCostUSD,
		RedactToolOutput:   redact.New(in.Secrets).Apply,
		History:            in.History,
		ToolOutputMaxBytes: ToolOutputMaxBytes,
		ContextWindow:      contextWindow,
		Compaction:         &harness.Compaction{Threshold: compactionThreshold, KeepRecentTurns: keepRecentMessages},
	}

	if drafts != nil {
		cfg.OnDelta = drafts.delta
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

// draftEvents passes the harness's event lines on and keeps the draft true
// to them: a model call starts a fresh draft, an error or a compaction voids
// it (a turn the harness abandons sends no reset; a compaction that made no
// progress may leave the turn running, and its submit republishes the body),
// and a repaired submit is marked refused, since it never executes.
type draftEvents struct {
	w      io.Writer
	drafts *draftExtractor
}

func (v draftEvents) Write(p []byte) (int, error) {
	var ev struct {
		Kind events.Kind    `json:"kind"`
		Data map[string]any `json:"data"`
	}

	if json.Unmarshal(bytes.TrimSpace(p), &ev) == nil {
		switch {
		case ev.Kind == events.ModelRequest:
			v.drafts.newModelCall()
		case ev.Kind == events.ErrorKind,
			ev.Kind == events.StateChange && ev.Data["event"] == "compaction":
			v.drafts.void()
		case ev.Kind == events.ToolRepair && ev.Data["name"] == submitArtifactName:
			id, _ := ev.Data["id"].(string)
			reason, _ := ev.Data["error"].(string)
			v.drafts.refuseCall(id, reason)
		}
	}

	n, err := v.w.Write(p)
	if err != nil {
		return n, fmt.Errorf("stage: write a harness event: %w", err)
	}

	return n, nil
}
