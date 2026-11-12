package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dummyToken stands in for the subscription token; no test holds a real one.
const dummyToken = "dummy-oauth-token-0000"

// fakeClaude stands in for claudecode.Run: it records each Spec and runs
// script on it. A nil script ends each run on an accepted submit.
type fakeClaude struct {
	script func(ctx context.Context, spec claudecode.Spec) (harness.Result, error)

	mu    sync.Mutex
	specs []claudecode.Spec
}

func (f *fakeClaude) run(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()

	if f.script == nil {
		return harness.Result{Completed: true, Reason: "done", CompletionArgs: json.RawMessage(`{"body":"B"}`)}, nil
	}

	return f.script(ctx, spec)
}

func (f *fakeClaude) calls() []claudecode.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.specs)
}

// claudeRunner is a Runner on f whose run homes go under a fresh TMPDIR, and
// that fails the test if a run builds an LLM client.
func claudeRunner(t *testing.T, f *fakeClaude, effort string, opts ClaudeOptions) *Runner {
	t.Helper()

	t.Setenv("TMPDIR", t.TempDir())

	r := NewRunner(effort, false, nil, opts)
	r.NewLLM = func(protocol.LLMEndpoint) llm.LLM {
		t.Error("a claude_code run built an LLM client")

		return &fakeLLM{}
	}
	r.RunClaude = f.run

	return r
}

func claudeInput(events *bytes.Buffer) Input {
	return Input{
		Stage: "intake", Persona: "You are the PO.", Instructions: "Produce the need.",
		Task: "containerise this project", Model: "claude-sonnet-test",
		Endpoint:   protocol.LLMEndpoint{Type: protocol.LLMEndpointTypeClaudeCode, OAuthToken: dummyToken},
		MaxCostUSD: 1.5, MaxTurns: 7, Events: events, Secrets: []string{dummyToken},
	}
}

// execute calls name on spec's bridge, as Claude Code would.
func execute(ctx context.Context, t *testing.T, spec claudecode.Spec, name string, args map[string]any) (tools.Result, error) {
	t.Helper()

	tool, ok := spec.Tools.Get(name)
	require.True(t, ok, "%s is on the bridge", name)

	return tool.Execute(ctx, args)
}

// acceptedWith is a run that ended on an accepted terminal call with args.
func acceptedWith(t *testing.T, args map[string]any) harness.Result {
	t.Helper()

	raw, err := json.Marshal(args)
	require.NoError(t, err)

	return harness.Result{Completed: true, Reason: "done", CompletionArgs: raw}
}

func schemaNames(ts []llm.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, tool := range ts {
		names = append(names, tool.Function.Name)
	}

	return names
}

func TestClaudeSpec(t *testing.T) {
	tests := []struct {
		name, effort, want string
	}{
		{"an effort claude takes is normalized", " XHigh", "xhigh"},
		{"an effort claude does not take is dropped", "minimal", ""},
		{"no effort", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				homeDuring bool
				echoed     string
			)

			f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
				_, err := os.Stat(spec.Home)
				homeDuring = err == nil

				res, err := execute(ctx, t, spec, "echo", map[string]any{"text": "[S2] the key is " + dummyToken})
				require.NoError(t, err)

				echoed = res.Text

				args := map[string]any{"body": "B"}
				_, err = execute(ctx, t, spec, submitArtifactName, args)
				require.NoError(t, err)

				return acceptedWith(t, args), nil
			}}

			r := claudeRunner(t, f, tt.effort, ClaudeOptions{Binary: "/opt/claude/bin/claude", MaxRuns: 2})

			in := claudeInput(&bytes.Buffer{})
			in.Interviewer = &stubInterviewer{}
			in.Tools = []tools.Tool{echoTool{}}

			out, err := r.Run(context.Background(), in)
			require.NoError(t, err)
			assert.Equal(t, "B", out.Artifact.Body)

			calls := f.calls()
			require.Len(t, calls, 1)

			spec := calls[0]

			assert.Equal(t, "/opt/claude/bin/claude", spec.Binary)
			assert.Equal(t, spec.Home, spec.Dir)
			assert.Equal(t, os.TempDir(), filepath.Dir(spec.Home))
			assert.True(t, strings.HasPrefix(filepath.Base(spec.Home), "architect-claude-"), spec.Home)
			assert.True(t, homeDuring, "the home exists while the run does")
			assert.NoDirExists(t, spec.Home, "the home is removed when the run ends")
			assert.Equal(t, []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + spec.Home}, spec.Env)
			assert.Equal(t, dummyToken, spec.OAuthToken)
			assert.Equal(t, in.Model, spec.Model)
			assert.Equal(t, in.Persona+"\n\n"+in.Instructions, spec.SystemPrompt)
			assert.Empty(t, spec.SystemPromptAppend)
			assert.Equal(t, in.Task, spec.Task)
			assert.Nil(t, spec.BuiltinTools, "no Claude Code built-in is offered")
			assert.Equal(t, in.MaxTurns, spec.MaxTurns)
			assert.InDelta(t, in.MaxCostUSD, spec.MaxBudgetUSD, 0)
			assert.Equal(t, tt.want, spec.Effort)
			assert.Empty(t, spec.Resume)

			_, err = uuid.Parse(spec.SessionID)
			require.NoError(t, err, "the session id is a UUID")

			require.NotNil(t, spec.Emitter)
			require.NotNil(t, spec.OnDelta)
			require.NotNil(t, spec.Redact)

			leaked := "x " + dummyToken + " y"
			assert.Equal(t, redact.New(in.Secrets).Apply(leaked), spec.Redact(leaked))
			assert.NotContains(t, spec.Redact(leaked), dummyToken)

			assert.NotContains(t, echoed, dummyToken, "a bridged result is redacted")
			assert.Equal(t, []string{"S2"}, out.SourceTags, "the run's tools keep their tags")

			harnessIn := in
			harnessIn.Endpoint = protocol.LLMEndpoint{Type: "openrouter", APIKey: "k"}
			fl := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", submitArtifactName, `{"body":"B"}`)}, FinishReason: "tool_calls"}}}

			_, err = runnerWith(fl).Run(context.Background(), harnessIn)
			require.NoError(t, err)
			require.NotEmpty(t, fl.requests)
			assert.Equal(t, schemaNames(fl.requests[0].Tools), schemaNames(spec.Tools.Schemas()), "the bridge offers the harness path's tools")
		})
	}
}

func TestClaudeSubmitArtifact(t *testing.T) {
	f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
		_, err := execute(ctx, t, spec, submitArtifactName, map[string]any{"body": ""})
		require.Error(t, err, "an empty body is refused")

		args := map[string]any{"body": "B"}
		_, err = execute(ctx, t, spec, submitArtifactName, args)
		require.NoError(t, err)

		return acceptedWith(t, args), nil
	}}

	drafts := newDraftLog(t)

	in := claudeInput(&bytes.Buffer{})
	in.Draft = drafts.add

	out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "B", out.Artifact.Body)

	refused := slices.IndexFunc(drafts.frames, func(fr DraftFrame) bool { return fr.Refused })
	require.GreaterOrEqual(t, refused, 0, "the refused submit is marked refused: %+v", drafts.frames)
	assert.Less(t, refused, len(drafts.frames)-1)
	assert.Equal(t, DraftFrame{Text: "B"}, drafts.frames[len(drafts.frames)-1], "the accepted body comes last")
}

func TestClaudeDraftFollowsTheStream(t *testing.T) {
	f := &fakeClaude{script: func(_ context.Context, spec claudecode.Spec) (harness.Result, error) {
		spec.Emitter.Emit(events.ModelRequest, map[string]any{})

		for _, frag := range []string{`{"bo`, `dy":"# Dra`, `ft`} {
			spec.OnDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "toolu_1", Name: submitArtifactName, ArgsFragment: frag}})
		}

		spec.Emitter.Emit(events.ErrorKind, map[string]any{"error": "stream ended"})

		return harness.Result{Reason: "error"}, errors.New("claudecode: stream ended without a result")
	}}

	drafts := newDraftLog(t)

	in := claudeInput(&bytes.Buffer{})
	in.Draft = drafts.add

	r := claudeRunner(t, f, "", ClaudeOptions{})
	r.draftEvery = time.Nanosecond

	_, err := r.Run(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, drafts.seen, "# Draft", "the streamed body is drafted")

	require.NotEmpty(t, drafts.frames)
	assert.True(t, drafts.frames[len(drafts.frames)-1].Void, "an error event voids the draft")
}

func TestClaudeAsked(t *testing.T) {
	var round map[string]any
	require.NoError(t, json.Unmarshal([]byte(twoQuestions), &round))

	f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
		_, err := execute(ctx, t, spec, askQuestionsName, round)
		require.NoError(t, err)

		return acceptedWith(t, round), nil
	}}

	iv := &stubInterviewer{}

	in := claudeInput(&bytes.Buffer{})
	in.Interviewer = iv

	out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)
	assert.Empty(t, out.Artifact.Body)
	require.Len(t, iv.asked, 1)
	assert.Len(t, iv.asked[0].Questions, 2)
}

func TestClaudeFatalCheck(t *testing.T) {
	const fatal = "slicing: no_slices: slicing needs at least one slice"

	f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
		args := map[string]any{"body": "# Slices"}
		_, err := execute(ctx, t, spec, submitArtifactName, args)
		require.NoError(t, err, "a refusal for good is a successful terminal call")

		return acceptedWith(t, args), nil
	}}

	in := claudeInput(&bytes.Buffer{})
	in.Validate = func(context.Context, Artifact) error { return &FatalCheckError{Problems: []string{fatal}} }

	out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)

	var refused *FatalCheckError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, []string{fatal}, refused.Problems)
	assert.Equal(t, ReasonFatalCheck, out.Result.Reason)
	assert.Empty(t, out.Artifact.Body)
}

// textStop is a first run that ended with text and no terminal call.
func textStop(cost float64) harness.Result {
	return harness.Result{Completed: true, Reason: "done", Turns: 2, TotalCostUSD: cost, Messages: []llm.Message{
		{Role: "system", Content: "S"},
		{Role: "user", Content: "containerise this project"},
		{Role: "assistant", Content: "Here is the need."},
	}}
}

// submitted calls submit_artifact on spec's bridge with body and returns the
// messages a resumed run records for it: the call's result or its refusal.
func submitted(ctx context.Context, t *testing.T, spec claudecode.Spec, body string) []llm.Message {
	t.Helper()

	args := map[string]any{"body": body}

	raw, err := json.Marshal(args)
	require.NoError(t, err)

	res, err := execute(ctx, t, spec, submitArtifactName, args)

	result := res.Text
	if err != nil {
		result = err.Error()
	}

	return []llm.Message{
		{Role: "system", Content: spec.SystemPrompt},
		{Role: "user", Content: spec.Task},
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("toolu_2", submitArtifactName, string(raw))}},
		{Role: "tool", ToolCallID: "toolu_2", Content: result},
	}
}

func TestClaudeForcedFollowUp(t *testing.T) {
	tests := []struct {
		name          string
		interviewer   Interviewer
		wantTerminals []string
	}{
		{"a stage without an interviewer", nil, []string{submitArtifactName}},
		{"an off-mode tool is not terminal", &stubInterviewer{}, []string{askQuestionsName, submitArtifactName}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := textStop(0.1)

			var (
				r                    *Runner
				second               []llm.Message
				homeDuring, slotHeld bool
			)

			f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
				if spec.Resume == "" {
					return first, nil
				}

				_, err := os.Stat(spec.Home)
				homeDuring = err == nil
				slotHeld = len(r.slots) == 1

				second = submitted(ctx, t, spec, "B")

				res := acceptedWith(t, map[string]any{"body": "B"})
				res.Turns, res.TotalCostUSD, res.Messages = 3, 0.05, second

				return res, nil
			}}

			in := claudeInput(&bytes.Buffer{})
			in.Interviewer = tt.interviewer
			in.Tools = []tools.Tool{echoTool{}}

			r = claudeRunner(t, f, "high", ClaudeOptions{MaxRuns: 1})

			out, err := r.Run(context.Background(), in)
			require.NoError(t, err)
			assert.Equal(t, "B", out.Artifact.Body)

			calls := f.calls()
			require.Len(t, calls, 2)

			run, follow := calls[0], calls[1]

			assert.Empty(t, follow.SessionID)
			assert.Equal(t, run.SessionID, follow.Resume, "the follow-up resumes the run's session")
			assert.Equal(t, fmt.Sprintf(forcedReprompt, strings.Join(tt.wantTerminals, " or ")), follow.Task)
			assert.Equal(t, forcedTurns, follow.MaxTurns)
			assert.InDelta(t, in.MaxCostUSD-0.1, follow.MaxBudgetUSD, 1e-9, "the follow-up gets what the run left of the cap")
			assert.Equal(t, tt.wantTerminals, schemaNames(follow.Tools.Schemas()), "only the terminal tools are offered")

			assert.Equal(t, run.Home, follow.Home)
			assert.Equal(t, run.Dir, follow.Dir)
			assert.True(t, homeDuring, "the follow-up runs before the home is removed")
			assert.True(t, slotHeld, "the follow-up runs in the run's slot")
			assert.NoDirExists(t, follow.Home)
			assert.Equal(t, run.Env, follow.Env)
			assert.Equal(t, run.SystemPrompt, follow.SystemPrompt)
			assert.Equal(t, run.Model, follow.Model)
			assert.Equal(t, run.Effort, follow.Effort)
			assert.Equal(t, run.OAuthToken, follow.OAuthToken)
			assert.Same(t, run.Emitter, follow.Emitter)

			assert.Equal(t, 5, out.Result.Turns, "both runs count")
			assert.InDelta(t, 0.15, out.Result.TotalCostUSD, 1e-9)
			assert.Equal(t, append(slices.Clone(first.Messages), WithoutSystem(second)...), out.Result.Messages)
		})
	}
}

func TestClaudeForcedRefused(t *testing.T) {
	f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
		if spec.Resume == "" {
			return textStop(0.1), nil
		}

		return harness.Result{Completed: true, Reason: "done", Messages: submitted(ctx, t, spec, "x")}, nil
	}}

	in := claudeInput(&bytes.Buffer{})
	in.Validate = func(context.Context, Artifact) error { return errors.New("x") }

	out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)
	require.ErrorIs(t, err, ErrNoArtifact)

	var refused *RefusedError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "submit_artifact rejected: x", refused.Last)
	assert.Equal(t, []string{submitArtifactName}, refused.Tools)
	assert.Equal(t, ReasonTerminalRefused, out.Result.Reason)
	assert.Len(t, f.calls(), 2, "one follow-up, never a loop")
}

func TestClaudeForcedSpentCap(t *testing.T) {
	f := &fakeClaude{script: func(context.Context, claudecode.Spec) (harness.Result, error) {
		return textStop(0.1), nil
	}}

	in := claudeInput(&bytes.Buffer{})
	in.MaxCostUSD = 0.1

	out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)
	require.ErrorIs(t, err, ErrNoArtifact)
	assert.Equal(t, "max_cost", out.Result.Reason)
	assert.Len(t, f.calls(), 1, "a spent cap gets no follow-up")
}

func TestClaudeFinalMark(t *testing.T) {
	tests := []struct {
		name string
		chat bool
		want []bool
	}{
		{"a submit ends a run that takes it", false, []bool{true, false}},
		{"a chat run does not end on a submit", true, []bool{false, false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeClaude{script: func(_ context.Context, spec claudecode.Spec) (harness.Result, error) {
				for _, name := range []string{submitArtifactName, "read_file"} {
					spec.Emitter.Emit(events.ModelRequest, map[string]any{})
					spec.OnDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{Name: name}})
					spec.Emitter.Emit(events.ModelResponse, map[string]any{"tool_calls": 1})
				}

				return harness.Result{Reason: "max_turns"}, nil
			}}

			var events bytes.Buffer

			in := claudeInput(&events)
			if tt.chat {
				in.Chat, in.Interviewer = true, &stubInterviewer{}
			}

			_, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)
			require.ErrorIs(t, err, ErrNoArtifact)
			assert.Equal(t, tt.want, finals(t, events.String()))
		})
	}
}

// seedingClaude answers each run as claudecode.Run seeds a result's
// messages: the system prompt and the task, then turns.
func seedingClaude(turns ...llm.Message) *fakeClaude {
	return &fakeClaude{script: func(_ context.Context, spec claudecode.Spec) (harness.Result, error) {
		msgs := append([]llm.Message{{Role: "system", Content: spec.SystemPrompt}, {Role: "user", Content: spec.Task}}, turns...)

		return harness.Result{Completed: true, Reason: "done", CompletionArgs: json.RawMessage(`{"body":"B"}`), Messages: msgs}, nil
	}}
}

// assertWellFormed checks that every tool message answers a call of the
// assistant message its run of tool messages follows.
func assertWellFormed(t *testing.T, msgs []llm.Message) {
	t.Helper()

	for i, m := range msgs {
		if m.Role != "tool" {
			continue
		}

		j := i - 1
		for j >= 0 && msgs[j].Role == "tool" {
			j--
		}

		require.GreaterOrEqual(t, j, 0, "tool message %d follows no assistant", i)
		assert.Equal(t, "assistant", msgs[j].Role, "tool message %d", i)
		assert.True(t, slices.ContainsFunc(msgs[j].ToolCalls, func(tc llm.ToolCall) bool { return tc.ID == m.ToolCallID }),
			"tool message %d answers a call of its assistant", i)
	}
}

// assertNoUserPairs checks that no two adjacent messages are both the user's.
func assertNoUserPairs(t *testing.T, msgs []llm.Message) {
	t.Helper()

	for i := 1; i < len(msgs); i++ {
		assert.False(t, msgs[i-1].Role == "user" && msgs[i].Role == "user", "messages %d and %d are both the user's", i-1, i)
	}
}

func TestClaudeHistoryPrimer(t *testing.T) {
	const system = "SYSTEM PROMPT OF THE EARLIER RUN"

	tests := []struct {
		name    string
		history []llm.Message
	}{
		{"a harness conversation", []llm.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: "the need"},
			{
				Role: "assistant", Content: "Reading first.",
				ReasoningDetails: json.RawMessage(`[{"type":"reasoning.text","text":"private thoughts","index":0}]`),
				ToolCalls:        []llm.ToolCall{toolCall("c1", "echo", `{"text":"x"}`)},
			},
			{Role: "tool", ToolCallID: "c1", Content: "x"},
		}},
		{"a claude conversation", []llm.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: "the need"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("toolu_1", askQuestionsName, `{"questions":[]}`)}},
			{Role: "tool", ToolCallID: "toolu_1", Content: "Round 1 is posted."},
		}},
	}

	reply := llm.Message{Role: "assistant", Content: "Answered."}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := seedingClaude(reply)

			in := claudeInput(&bytes.Buffer{})
			in.History = tt.history
			in.Task = "the person answered the round"

			out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)
			require.NoError(t, err)

			calls := f.calls()
			require.Len(t, calls, 1)

			assert.Equal(t, fmt.Sprintf(resumedTaskFormat, claudecode.RenderTranscript(WithoutSystem(tt.history)), in.Task), calls[0].Task)
			assert.NotContains(t, calls[0].Task, system, "the transcript holds no system text")
			assert.Equal(t, in.Persona+"\n\n"+in.Instructions, calls[0].SystemPrompt)

			// Stored flat, as a harness resume is; the reasoning the claude run
			// never saw is not attributed to it.
			want := append([]llm.Message{{Role: "system", Content: calls[0].SystemPrompt}}, WithoutSystem(tt.history)...)
			want = append(want, llm.Message{Role: "user", Content: in.Task}, reply)
			assert.Equal(t, withoutReasoning(want), out.Result.Messages)
		})
	}
}

// toolTurns are tool turns from to to, each about 2*size bytes, marked
// turn-NN and result-NN.
func toolTurns(from, to, size int) []llm.Message {
	var msgs []llm.Message

	for i := from; i <= to; i++ {
		id := fmt.Sprintf("c%02d", i)
		msgs = append(msgs,
			llm.Message{Role: "assistant", Content: fmt.Sprintf("turn-%02d ", i) + strings.Repeat("a", size), ToolCalls: []llm.ToolCall{toolCall(id, "echo", `{"text":"x"}`)}},
			llm.Message{Role: "tool", ToolCallID: id, Content: fmt.Sprintf("result-%02d ", i) + strings.Repeat("r", size)},
		)
	}

	return msgs
}

// turnsHistory is a stored conversation of task and n tool turns.
func turnsHistory(task string, n, size int) []llm.Message {
	return append([]llm.Message{{Role: "system", Content: "SYSTEM"}, {Role: "user", Content: task}}, toolTurns(1, n, size)...)
}

// trimWindow gives a compaction limit of 8500 tokens, about 21 kB.
const trimWindow = 10000

func TestClaudeTrimmedPrimer(t *testing.T) {
	tests := []struct {
		name     string
		history  []llm.Message
		trimmed  bool
		dropped  []string
		keptTail []string
	}{
		{
			name: "a history past the limit keeps the task and the newest turns", history: turnsHistory("ORIGINAL TASK", 20, 1000),
			trimmed: true, dropped: []string{"turn-01", "result-01", "turn-05"}, keptTail: []string{"turn-19", "result-19", "turn-20", "result-20"},
		},
		{
			name: "a task and newest turn past the limit is refused", history: turnsHistory(strings.Repeat("t", 20000), 3, 6000),
		},
	}

	reply := llm.Message{Role: "assistant", Content: "Answered."}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := seedingClaude(reply)

			in := claudeInput(&bytes.Buffer{})
			in.ContextWindow = trimWindow
			in.History = tt.history
			in.Task = "the person answered the round"

			out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)

			if !tt.trimmed {
				var tooBig *TooBigError
				require.ErrorAs(t, err, &tooBig)
				assert.Equal(t, ReasonTooBig, out.Result.Reason)
				assert.Empty(t, f.calls(), "a refused run starts no process")

				return
			}

			require.NoError(t, err)

			calls := f.calls()
			require.Len(t, calls, 1)

			task := calls[0].Task
			require.NoError(t, preflight(calls[0].SystemPrompt, task, nil, trimWindow), "the trimmed primer fits")
			assert.Contains(t, task, "ORIGINAL TASK")
			assert.True(t, strings.HasSuffix(task, in.Task), "the run's own task comes last")
			assert.Equal(t, 1, strings.Count(task, omittedTurns), "one line says earlier turns were left out")

			_, tail, found := strings.Cut(task, omittedTurns+"\n\n")
			require.True(t, found)
			assert.True(t, strings.HasPrefix(tail, "[assistant"), "the kept turns open on an assistant, not an orphaned tool result")

			for _, s := range tt.dropped {
				assert.NotContains(t, task, s)
			}

			for _, s := range tt.keptTail {
				assert.Contains(t, tail, s)
			}

			// The stored transcript is flat: the kept conversation, the task as
			// its own user message, then the run's own messages.
			msgs := out.Result.Messages
			require.GreaterOrEqual(t, len(msgs), 5)
			assert.Equal(t, "system", msgs[0].Role)
			assert.Equal(t, "user", msgs[1].Role)
			assert.True(t, strings.HasPrefix(msgs[1].Content, "ORIGINAL TASK"), "the original task comes first")
			assert.Equal(t, "assistant", msgs[2].Role, "the marker adds no user message")
			assert.Equal(t, llm.Message{Role: "user", Content: in.Task}, msgs[len(msgs)-2])
			assert.Equal(t, reply, msgs[len(msgs)-1])
			assert.Equal(t, task, fmt.Sprintf(resumedTaskFormat, claudecode.RenderTranscript(msgs[1:len(msgs)-2]), in.Task),
				"the stored conversation is what the primer carried")
			assertWellFormed(t, msgs)
			assertNoUserPairs(t, msgs)

			for _, m := range msgs {
				assert.NotContains(t, m.Content, "<transcript>", "no primer is stored")
			}
		})
	}
}

func TestTrimmedMarksTheGapWithoutAddingAUserMessage(t *testing.T) {
	marked := "the need\n\n" + omittedTurns
	call := []llm.ToolCall{toolCall("c1", "echo", `{}`)}

	tests := []struct {
		name string
		msgs []llm.Message
		want []llm.Message
	}{
		{
			"turns opening on an assistant",
			[]llm.Message{{Role: "user", Content: "the need"}, {Role: "assistant", Content: "old"}, {Role: "assistant", Content: "new", ToolCalls: call}, {Role: "tool", ToolCallID: "c1"}},
			[]llm.Message{{Role: "user", Content: marked}, {Role: "assistant", Content: "new", ToolCalls: call}, {Role: "tool", ToolCallID: "c1"}},
		},
		{
			"turns opening on a user message",
			[]llm.Message{{Role: "user", Content: "the need"}, {Role: "assistant", Content: "old"}, {Role: "user", Content: "round 1"}, {Role: "assistant", Content: "new"}},
			[]llm.Message{{Role: "user", Content: marked + "\n\nround 1"}, {Role: "assistant", Content: "new"}},
		},
		{
			"a first message marked by an earlier trim",
			[]llm.Message{{Role: "user", Content: marked}, {Role: "assistant", Content: "old"}, {Role: "user", Content: "round 1"}, {Role: "assistant", Content: "new"}},
			[]llm.Message{{Role: "user", Content: marked + "\n\nround 1"}, {Role: "assistant", Content: "new"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.msgs)

			got := trimmed(tt.msgs, 0, 2)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, before, tt.msgs, "the history is not changed")
			assertNoUserPairs(t, got)
		})
	}
}

func TestClaudeResumeOfATrimmedRunFits(t *testing.T) {
	in := claudeInput(&bytes.Buffer{})
	in.ContextWindow = trimWindow
	in.History = turnsHistory("ORIGINAL TASK", 20, 1000)
	in.Task = "the person answered round 1"

	first, err := claudeRunner(t, seedingClaude(toolTurns(21, 24, 1000)...), "", ClaudeOptions{}).Run(context.Background(), in)
	require.NoError(t, err)

	// The second resume reads the first's stored transcript, as a critique
	// or answered round does, past the limit again.
	f := seedingClaude(llm.Message{Role: "assistant", Content: "Answered."})
	in.History = first.Result.Messages
	in.Task = "the person answered round 2"

	out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)
	require.NoError(t, err)

	calls := f.calls()
	require.Len(t, calls, 1)

	task := calls[0].Task
	require.NoError(t, preflight(calls[0].SystemPrompt, task, nil, trimWindow), "the second trimmed primer fits")
	assert.Equal(t, 1, strings.Count(task, "<transcript>"), "no primer nests inside it")
	assert.Equal(t, 1, strings.Count(task, omittedTurns))
	assert.Contains(t, task, "ORIGINAL TASK")
	assert.Contains(t, task, "result-24", "the first resume's newest turn is kept")

	msgs := out.Result.Messages
	require.GreaterOrEqual(t, len(msgs), 2)
	assert.True(t, strings.HasPrefix(msgs[1].Content, "ORIGINAL TASK"), "the original task still comes first")
	assertWellFormed(t, msgs)
	assertNoUserPairs(t, msgs)
}

func TestClaudeTooBig(t *testing.T) {
	tests := []struct {
		name    string
		task    string
		history []llm.Message
	}{
		{name: "history past the window", history: []llm.Message{{Role: "user", Content: strings.Repeat("x", 10000)}}},
		{name: "fresh task past the window", task: strings.Repeat("x", 10000)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeClaude{}

			in := claudeInput(&bytes.Buffer{})
			in.ContextWindow = 1000
			in.History = tt.history

			if tt.task != "" {
				in.Task = tt.task
			}

			out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(context.Background(), in)

			var tooBig *TooBigError
			require.ErrorAs(t, err, &tooBig)
			assert.Equal(t, ReasonTooBig, out.Result.Reason)
			assert.Empty(t, f.calls(), "a refused run starts no process")
		})
	}
}

func TestClaudeCostCapCancel(t *testing.T) {
	errSentinel := errors.New("blueprint cost cap reached")

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	started := make(chan struct{})

	var home string

	f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
		home = spec.Home

		close(started)
		<-ctx.Done()

		return harness.Result{Reason: "canceled"}, fmt.Errorf("claudecode: %w", context.Cause(ctx))
	}}

	go func() {
		<-started
		cancel(errSentinel)
	}()

	out, err := claudeRunner(t, f, "", ClaudeOptions{}).Run(ctx, claudeInput(&bytes.Buffer{}))
	require.ErrorIs(t, err, errSentinel)
	assert.Equal(t, "canceled", out.Result.Reason)
	require.NotEmpty(t, home)
	assert.NoDirExists(t, home, "a canceled run's home is removed")
}

func TestClaudeSlotWaitCancel(t *testing.T) {
	tests := []struct {
		name    string
		maxRuns int
	}{
		{"one run at a time", 1},
		{"below one means one", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			startedA := make(chan struct{})
			release := make(chan struct{})

			f := &fakeClaude{script: func(_ context.Context, spec claudecode.Spec) (harness.Result, error) {
				if spec.Task == "A" {
					close(startedA)
					<-release
				}

				return harness.Result{Completed: true, Reason: "done", CompletionArgs: json.RawMessage(`{"body":"B"}`)}, nil
			}}

			r := claudeRunner(t, f, "", ClaudeOptions{MaxRuns: tt.maxRuns})

			run := func(ctx context.Context, task string) error {
				in := claudeInput(&bytes.Buffer{})
				in.Task = task

				_, err := r.Run(ctx, in)

				return err
			}

			doneA := make(chan error, 1)

			go func() { doneA <- run(context.Background(), "A") }()

			<-startedA

			ctxB, cancelB := context.WithCancelCause(context.Background())
			defer cancelB(nil)

			doneB := make(chan error, 1)

			go func() { doneB <- run(ctxB, "B") }()

			select {
			case err := <-doneB:
				t.Fatalf("B ran while A held the only slot: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			homes, err := filepath.Glob(filepath.Join(os.TempDir(), "architect-claude-*"))
			require.NoError(t, err)
			assert.Len(t, homes, 1, "a waiting run has no home yet")

			errCap := errors.New("blueprint cost cap reached")
			cancelB(errCap)

			errB := <-doneB
			require.ErrorIs(t, errB, context.Canceled)
			require.ErrorIs(t, errB, errCap, "the cause stays matchable")
			assert.Len(t, f.calls(), 1, "the canceled run never started")

			close(release)
			require.NoError(t, <-doneA)

			ctxC, cancelC := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelC()

			require.NoError(t, run(ctxC, "C"), "the released slot is taken again")

			calls := f.calls()
			require.Len(t, calls, 2)
			assert.Equal(t, "C", calls[1].Task)
		})
	}
}

// nestedRun is a tool whose call runs another stage on the call's context,
// as a responder runs inside its asker's round.
type nestedRun struct {
	run func(ctx context.Context) error
}

func (nestedRun) Name() string { return "respond" }

func (nestedRun) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "respond", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (n nestedRun) Execute(ctx context.Context, _ map[string]any) (tools.Result, error) {
	return tools.Result{Text: "answered"}, n.run(ctx)
}

func TestClaudeNestedRunSharesTheSlot(t *testing.T) {
	body := json.RawMessage(`{"body":"B"}`)

	var r *Runner

	var (
		homes               = map[string]string{}
		nestedErr, freshErr error
		nestedOut           Output
	)

	f := &fakeClaude{script: func(ctx context.Context, spec claudecode.Spec) (harness.Result, error) {
		homes[spec.Task] = spec.Home

		if spec.Task == "asker" {
			_, err := execute(ctx, t, spec, "respond", nil)
			require.NoError(t, err)

			// A run from a fresh context is not nested: it waits for the
			// slot the asker holds.
			fresh, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			in := claudeInput(&bytes.Buffer{})
			in.Task = "unrelated"
			_, freshErr = r.Run(fresh, in)
		}

		return harness.Result{Completed: true, Reason: "done", CompletionArgs: body}, nil
	}}

	r = claudeRunner(t, f, "", ClaudeOptions{MaxRuns: 1})

	// A regression waits for the slot until this deadline and fails, rather
	// than hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := claudeInput(&bytes.Buffer{})
	in.Task = "asker"
	in.Tools = []tools.Tool{nestedRun{run: func(ctx context.Context) error {
		responder := claudeInput(&bytes.Buffer{})
		responder.Task = "responder"
		nestedOut, nestedErr = r.Run(ctx, responder)

		return nil
	}}}

	out, err := r.Run(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, "B", out.Artifact.Body)

	require.NoError(t, nestedErr, "the nested run takes no second slot")
	assert.Equal(t, "B", nestedOut.Artifact.Body)

	require.ErrorIs(t, freshErr, context.DeadlineExceeded, "a run from a fresh context still waits for the slot")
	assert.NotContains(t, homes, "unrelated", "the waiting run never started")

	require.Contains(t, homes, "responder")
	assert.NotEqual(t, homes["asker"], homes["responder"], "the nested run has its own home")
	assert.NoDirExists(t, homes["responder"])
	assert.NoDirExists(t, homes["asker"])
}

func TestSweepClaudeHomes(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "architect-claude-1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "architect-claude-1", "x"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "architect-claude-2"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "other"), 0o700))

	n, err := SweepClaudeHomes(dir)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	left, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, "other", left[0].Name())
}
