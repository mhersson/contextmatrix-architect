package stage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
)

// resumedTaskFormat opens a resumed claude_code run: a fresh claude process
// cannot be seeded with another run's messages, so they go in as text.
const resumedTaskFormat = "This run continues an earlier conversation, given here as a transcript.\n\n<transcript>\n%s\n</transcript>\n\n%s"

// omittedTurns marks where a trimmed transcript left out earlier turns.
const omittedTurns = "[earlier turns omitted to fit the context window]"

// claudeHomePattern names a run's HOME and working directory under
// os.TempDir().
const claudeHomePattern = "architect-claude-*"

// slotKey keys the slots channel whose slot the ctx's run holds. A run
// started from a tool call of that run, a responder inside its asker's round,
// shares the slot: waiting for another would deadlock once the slots are full.
type slotKey struct{}

// runClaude is run on a claude_code endpoint: a claude process offered no
// built-in, only the run's Go tools on vendorkit's loopback bridge, since on
// a workstation a Read built-in could read the operator's home.
func (r *Runner) runClaude(ctx context.Context, in Input, tags *returnedTags) (out Output, err error) {
	if err = checkInput(in); err != nil {
		return Output{}, err
	}

	drafts := r.newDrafts(in)
	window := contextWindow(in.ContextWindow)
	system := in.Persona + "\n\n" + in.Instructions

	task, err := claudeTask(system, in, window)
	if err != nil {
		return Output{Result: harness.Result{Reason: ReasonTooBig}}, fmt.Errorf("stage %s: %w", in.Stage, err)
	}

	if held, _ := ctx.Value(slotKey{}).(chan struct{}); r.slots != nil && held != r.slots {
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		case <-ctx.Done():
			return Output{Result: harness.Result{Reason: "canceled"}}, fmt.Errorf("stage %s: wait for a claude slot: %w", in.Stage, canceled(ctx))
		}

		ctx = context.WithValue(ctx, slotKey{}, r.slots)
	}

	home, err := os.MkdirTemp("", claudeHomePattern)
	if err != nil {
		return Output{}, fmt.Errorf("stage %s: create the claude home: %w", in.Stage, err)
	}

	defer func() {
		if rerr := os.RemoveAll(home); rerr != nil {
			err = errors.Join(err, fmt.Errorf("stage %s: remove the claude home: %w", in.Stage, rerr))
		}
	}()

	red := redact.New(in.Secrets).Apply
	mark := &terminalMark{}

	all := append(builtinTools(in, mark, drafts), tags.watch(in.Tools)...)
	onBridge := make([]tools.Tool, 0, len(all))

	for _, t := range all {
		onBridge = append(onBridge, bridged(t, red))
	}

	watch := &callWatch{submits: !in.Chat && in.Designer == nil}
	watch.w = finalEvents{w: eventSink(in, drafts), final: watch.final}

	onDelta := watch.delta
	if drafts != nil {
		onDelta = func(d llm.Delta) {
			drafts.delta(d)
			watch.delta(d)
		}
	}

	spec := claudecode.Spec{
		Binary:       r.claudeBinary,
		Home:         home,
		Dir:          home,
		Env:          []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + home},
		OAuthToken:   in.Endpoint.OAuthToken,
		Model:        in.Model,
		SystemPrompt: system,
		Task:         task,
		Tools:        tools.NewRegistry(onBridge...),
		MaxTurns:     in.MaxTurns,
		MaxBudgetUSD: in.MaxCostUSD,
		Effort:       r.claudeEffort,
		SessionID:    uuid.NewString(),
		Emitter:      events.NewEmitter(nil, watch),
		Redact:       red,
		OnDelta:      onDelta,
	}

	start := r.RunClaude
	if start == nil {
		start = claudecode.Run
	}

	res, err := start(ctx, spec)
	if err == nil && res.Completed && res.CompletionArgs == nil {
		first := res
		// The resume reads the session vendorkit stored under this Home, so it
		// runs here, before the home is removed and in the same slot.
		follow := func(ctx context.Context, terminals []tools.Tool, reprompt string, maxTurns int, maxCostUSD float64) (harness.Result, error) {
			resumed := spec
			resumed.SessionID, resumed.Resume = "", spec.SessionID
			resumed.Task = reprompt
			resumed.Tools = tools.NewRegistry(terminals...)
			resumed.MaxTurns, resumed.MaxBudgetUSD = maxTurns, maxCostUSD

			second, err := start(ctx, resumed)
			second.Messages = append(slices.Clone(first.Messages), WithoutSystem(second.Messages)...)

			return second, err
		}

		res, err = forceTerminal(ctx, terminalTools(onBridge), spec.MaxBudgetUSD, first, follow)
	}

	return outcome(in.Stage, res, err, mark, window)
}

// claudeTask is in.Task, or on a resume a primer carrying in.History as a
// transcript. Claude Code never compacts what a run returns, so a primer past
// the limit keeps the first user message and the newest messages that fit.
func claudeTask(system string, in Input, window int) (string, error) {
	if len(in.History) == 0 {
		return in.Task, preflight(system, in.Task, nil, window)
	}

	msgs := WithoutSystem(in.History)
	whole := fmt.Sprintf(resumedTaskFormat, claudecode.RenderTranscript(msgs), in.Task)

	refused := preflight(system, whole, nil, window)
	if refused == nil {
		return whole, nil
	}

	first := slices.IndexFunc(msgs, func(m llm.Message) bool { return m.Role == "user" })
	if first < 0 || first == len(msgs)-1 {
		return "", refused
	}

	head := claudecode.RenderTranscript(msgs[first : first+1])
	trimmed := func(from int) string {
		transcript := head + "\n\n" + omittedTurns + "\n\n" + claudecode.RenderTranscript(msgs[from:])

		return fmt.Sprintf(resumedTaskFormat, transcript, in.Task)
	}

	// Fewer messages only shrink the primer, so the first start that fits
	// keeps the most.
	tail := msgs[first+1:]
	n := sort.Search(len(tail), func(i int) bool { return preflight(system, trimmed(first+1+i), nil, window) == nil })

	if n == len(tail) {
		return "", refused
	}

	// A tool result whose call was left out opens no turn.
	from := first + 1 + n
	for from < len(msgs)-1 && msgs[from].Role == "tool" {
		from++
	}

	return trimmed(from), nil
}

// canceled is ctx's error with the cause it was canceled for, so a cost cap
// stays matchable.
func canceled(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != ctx.Err() {
		return fmt.Errorf("%w: %w", ctx.Err(), cause)
	}

	return ctx.Err()
}

// SweepClaudeHomes removes the run homes under dir that a stopped process
// left behind, and returns how many it removed.
func SweepClaudeHomes(dir string) (int, error) {
	homes, err := filepath.Glob(filepath.Join(dir, claudeHomePattern))
	if err != nil {
		return 0, fmt.Errorf("stage: find claude homes: %w", err)
	}

	var (
		removed int
		errs    []error
	)

	for _, home := range homes {
		if err := os.RemoveAll(home); err != nil {
			errs = append(errs, fmt.Errorf("stage: remove claude home: %w", err))

			continue
		}

		removed++
	}

	return removed, errors.Join(errs...)
}
