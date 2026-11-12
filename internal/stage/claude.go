package stage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

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

	task, kept, err := claudeTask(system, in, window)
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
		Env:          claudeEnv(home),
		OAuthToken:   in.Endpoint.OAuthToken,
		Model:        in.Model,
		SystemPrompt: system,
		Task:         task,
		Tools:        tools.NewRegistry(onBridge...),
		MaxTurns:     in.MaxTurns,
		MaxBudgetUSD: in.MaxCostUSD,
		Effort:       claudecode.NormalizeEffort(in.Endpoint.Effort),
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
	res.Messages = flat(res.Messages, kept, red(in.Task))

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
// transcript, and the messages it carried. Claude Code never compacts what a
// run returns, so past the limit they are the first user message and the
// newest turns that fit.
func claudeTask(system string, in Input, window int) (string, []llm.Message, error) {
	if len(in.History) == 0 {
		return in.Task, nil, preflight(system, in.Task, nil, window)
	}

	primer := func(msgs []llm.Message) string {
		return fmt.Sprintf(resumedTaskFormat, claudecode.RenderTranscript(msgs), in.Task)
	}

	// The claude run never sees the reasoning, so its record does not carry it.
	msgs := withoutReasoning(WithoutSystem(in.History))

	refused := preflight(system, primer(msgs), nil, window)
	if refused == nil {
		return primer(msgs), msgs, nil
	}

	first := slices.IndexFunc(msgs, func(m llm.Message) bool { return m.Role == "user" })
	if first < 0 {
		return "", nil, refused
	}

	// The kept turns open on a user or assistant message, so each tool result
	// stays with the call it answers.
	var starts []int

	for i := first + 1; i < len(msgs); i++ {
		if msgs[i].Role != "tool" {
			starts = append(starts, i)
		}
	}

	// A later start only shrinks the primer, so the first that fits keeps most.
	n := sort.Search(len(starts), func(i int) bool {
		return preflight(system, primer(trimmed(msgs, first, starts[i])), nil, window) == nil
	})
	if n == len(starts) {
		return "", nil, refused
	}

	kept := trimmed(msgs, first, starts[n])

	return primer(kept), kept, nil
}

// trimmed is msgs' first user message, at first, and msgs from from on, with
// omittedTurns at the gap, ending the first message unless an earlier trim
// left it there. A kept user message opening the turns joins the first, so
// no two user messages follow each other.
func trimmed(msgs []llm.Message, first, from int) []llm.Message {
	head, turns := msgs[first], slices.Clone(msgs[from:])

	if !strings.HasSuffix(head.Content, omittedTurns) {
		head.Content += "\n\n" + omittedTurns
	}

	if turns[0].Role == "user" {
		head.Content += "\n\n" + turns[0].Content
		turns = turns[1:]
	}

	return append([]llm.Message{head}, turns...)
}

// flat is a resumed run's messages as a harness resume stores them: the
// primer claudecode seeded becomes the messages it carried and then task. nil
// kept is a run that resumed nothing.
func flat(msgs, kept []llm.Message, task string) []llm.Message {
	at := slices.IndexFunc(msgs, func(m llm.Message) bool { return m.Role == "user" })
	if kept == nil || at < 0 {
		return msgs
	}

	out := append(slices.Clone(msgs[:at]), kept...)
	out = append(out, llm.Message{Role: "user", Content: task})

	return append(out, msgs[at+1:]...)
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

// claudeProxyEnv are the proxy variables the child inherits, so claude reaches
// the API through the same egress proxy as the architect's Go clients.
var claudeProxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// claudeEnv is the child's base env: PATH, its TMPDIR and the proxy variables set here.
func claudeEnv(home string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + home}

	for _, k := range claudeProxyEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}

	return env
}
