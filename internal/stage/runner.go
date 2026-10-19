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
	"slices"
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

// A run whose prompt reaches 85% of its model's window summarizes its older
// turns and goes on, keeping the last six messages verbatim: the chat
// backend's bounds. CM sends the window with the session start; a model it
// does not know gets 128000.
const (
	defaultContextWindow = 128000
	compactionThreshold  = 0.85
	keepRecentMessages   = 6
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
	// ContextWindow is the model's context length in tokens; 0 is unknown
	// and runs on defaultContextWindow.
	ContextWindow int
	// Interviewer posts the run's rounds (ask_questions); nil offers no round
	// tool.
	Interviewer Interviewer
	// Chat makes the run answer the chat about the open round: answer_chat,
	// which needs the Interviewer, is its only built-in tool and ends it.
	Chat bool
	// Events receives the harness JSONL event stream; nil discards it.
	Events io.Writer
	// Secrets are masked in every tool result.
	Secrets []string
	// History seeds the conversation for a critique round; nil starts fresh.
	History []llm.Message
	// Tools are offered to the model next to the built-in tools;
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
	// Asked is set when the run ended by posting a round or answering its
	// chat; Artifact is then empty.
	Asked bool
}

// Transient marks a tool whose results the model reads in its run but the
// record never keeps. When Run returns, each of the tool's results in
// Output.Result.Messages is what Stored makes of it. Stored returns a result
// it did not produce unchanged, and one it already made unchanged too, so a
// resumed history passes through again as it was stored.
type Transient interface {
	Stored(result string) string
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

// Run executes one stage and returns its artifact. On every return its
// messages are as the record keeps them: see Transient.
func (r *Runner) Run(ctx context.Context, in Input) (Output, error) {
	out, err := r.run(ctx, in)
	out.Result.Messages = recorded(out.Result.Messages, in.Tools)

	return out, err
}

// recorded is msgs with every transient tool's results as the tool stores
// them. A tool message answers the calls of the assistant message before
// it, so a call id a model reuses in a later turn is told apart. msgs itself
// is never changed: the requests already sent share it.
func recorded(msgs []llm.Message, ts []tools.Tool) []llm.Message {
	transient := map[string]Transient{}

	for _, t := range ts {
		if tr, ok := t.(Transient); ok {
			transient[t.Name()] = tr
		}
	}

	if len(transient) == 0 {
		return msgs
	}

	var (
		out   []llm.Message
		calls map[string]Transient
	)

	for i, m := range msgs {
		if m.Role != "tool" {
			calls = map[string]Transient{}

			for _, tc := range m.ToolCalls {
				if tr, ok := transient[tc.Function.Name]; ok {
					calls[tc.ID] = tr
				}
			}

			continue
		}

		tr, ok := calls[m.ToolCallID]
		if !ok {
			continue
		}

		if stored := tr.Stored(m.Content); stored != m.Content {
			if out == nil {
				out = slices.Clone(msgs)
			}

			out[i].Content = stored
		}
	}

	if out == nil {
		return msgs
	}

	return out
}

// run is Run before the record's view of the messages.
func (r *Runner) run(ctx context.Context, in Input) (Output, error) {
	// The registry keeps the last tool under a name, so a clash would silently
	// replace a built-in.
	for _, t := range in.Tools {
		if name := t.Name(); name == askQuestionsName || name == AnswerChatName || name == submitArtifactName {
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

	if in.Chat && in.Interviewer == nil {
		return Output{}, fmt.Errorf("stage %s: a chat run needs an interviewer", in.Stage)
	}

	window := in.ContextWindow
	if window <= 0 {
		window = defaultContextWindow
	}

	system := in.Persona + "\n\n" + in.Instructions

	if err := preflight(system, in.Task, in.History, window); err != nil {
		return Output{Result: harness.Result{Reason: ReasonTooBig}}, fmt.Errorf("stage %s: %w", in.Stage, err)
	}

	mark := &terminalMark{}

	var builtins []tools.Tool

	if in.Chat {
		builtins = append(builtins, answerChatTool{iv: in.Interviewer, mark: mark})
	} else {
		if in.Interviewer != nil {
			builtins = append(builtins, askQuestionsTool{iv: in.Interviewer, mark: mark})
		}

		builtins = append(builtins, submitArtifactTool{validate: in.Validate, drafts: drafts})
	}

	reg := tools.NewRegistry(append(builtins, in.Tools...)...)

	watch := &replyWatch{LLM: r.NewLLM(in.Endpoint)}

	sink := in.Events
	if sink == nil {
		sink = io.Discard
	}

	if drafts != nil {
		sink = draftEvents{w: sink, drafts: drafts}
	}

	sink = finalEvents{w: sink, watch: watch}

	emit := events.NewEmitter(nil, sink)

	cfg := harness.Config{
		Model:              in.Model,
		SystemPrompt:       system,
		MaxTurns:           in.MaxTurns,
		MaxCostUSD:         in.MaxCostUSD,
		RedactToolOutput:   redact.New(in.Secrets).Apply,
		History:            in.History,
		ToolOutputMaxBytes: ToolOutputMaxBytes,
		ContextWindow:      window,
		Compaction:         &harness.Compaction{Threshold: compactionThreshold, KeepRecentTurns: keepRecentMessages},
	}

	if drafts != nil {
		cfg.OnDelta = drafts.delta
	}

	client := llm.LLM(watch)

	res, err := harness.Run(ctx, client, reg, emit, in.Task, cfg)
	if err == nil && res.Completed && res.CompletionArgs == nil {
		res, err = forceTerminal(ctx, client, terminalTools(builtins), emit, cfg, res)
	}

	if err != nil {
		return Output{Result: res}, fmt.Errorf("stage %s: %w", in.Stage, err)
	}

	if mark.asked {
		return Output{Asked: true, Result: res}, nil
	}

	if !res.Completed || res.CompletionArgs == nil {
		if res.Reason == ReasonContextLimit {
			return Output{Result: res}, fmt.Errorf("stage %s: %w", in.Stage, &ContextFullError{Window: window, Limit: compactionLimit(window)})
		}

		return Output{Result: res}, fmt.Errorf("%w: reason %s", ErrNoArtifact, res.Reason)
	}

	var art Artifact
	if err := json.Unmarshal(res.CompletionArgs, &art); err != nil {
		return Output{Result: res}, fmt.Errorf("stage %s: decode artifact: %w", in.Stage, err)
	}

	return Output{Artifact: art, Result: res}, nil
}

// terminalTools are the tools among ts that end a run.
func terminalTools(ts []tools.Tool) []tools.Tool {
	var out []tools.Tool

	for _, t := range ts {
		if term, ok := t.(tools.Terminal); ok && term.Terminal() {
			out = append(out, t)
		}
	}

	return out
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
