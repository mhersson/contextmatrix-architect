package stage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// forcedTurns lets a refused terminal call be corrected once inside the
// follow-up run.
const forcedTurns = 2

const forcedReprompt = "You ended your turn without calling %s, so nothing was recorded. Call it now with your finished work."

// forcedChoice makes every model call that offers tools demand one: the
// named function when one is offered, "required" otherwise. A gateway that
// refuses tool_choice for a model answers 400; the call is then sent once
// more without it. Send passes through: compaction calls it without tools.
type forcedChoice struct {
	llm.LLM
}

func (f forcedChoice) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	if len(req.Tools) == 0 {
		return f.LLM.SendStream(ctx, req, onDelta)
	}

	forced := req
	forced.ToolChoice = toolChoice(req.Tools)

	resp, err := f.LLM.SendStream(ctx, forced, onDelta)
	if err != nil && strings.Contains(err.Error(), "llm endpoint status 400") {
		return f.LLM.SendStream(ctx, req, onDelta)
	}

	return resp, err
}

func toolChoice(offered []llm.Tool) json.RawMessage {
	if len(offered) == 1 {
		name, _ := json.Marshal(offered[0].Function.Name)

		return json.RawMessage(`{"type":"function","function":{"name":` + string(name) + `}}`)
	}

	return json.RawMessage(`"required"`)
}

// forceTerminal follows a run that stopped with text and no terminal call:
// one more run on its conversation, offered only the terminal tools and
// forced to call one. The result carries both runs' usage and the whole
// conversation. A cost cap the first run spent leaves the first result as
// it is.
func forceTerminal(ctx context.Context, client llm.LLM, terminals []tools.Tool, emit *events.Emitter, cfg harness.Config, first harness.Result) (harness.Result, error) {
	if len(terminals) == 0 {
		return first, nil
	}

	if cfg.MaxCostUSD > 0 {
		left := cfg.MaxCostUSD - first.TotalCostUSD
		if left <= 0 {
			return first, nil
		}

		cfg.MaxCostUSD = left
	}

	names := make([]string, 0, len(terminals))
	for _, t := range terminals {
		names = append(names, t.Name())
	}

	cfg.History = WithoutSystem(first.Messages)
	cfg.MaxTurns = forcedTurns

	second, err := harness.Run(ctx, forcedChoice{LLM: client}, tools.NewRegistry(terminals...), emit,
		fmt.Sprintf(forcedReprompt, strings.Join(names, " or ")), cfg)

	return merged(first, second), err
}

// merged is second with first's usage added, the conversation second's.
func merged(first, second harness.Result) harness.Result {
	out := second
	out.Turns += first.Turns
	out.TotalCostUSD += first.TotalCostUSD
	out.PromptTokens += first.PromptTokens
	out.CompletionTokens += first.CompletionTokens
	out.CacheReadTokens += first.CacheReadTokens
	out.CacheCreationTokens += first.CacheCreationTokens
	out.ToolCallCount += first.ToolCallCount
	out.ToolCallFailures += first.ToolCallFailures
	out.RepairCount += first.RepairCount

	if out.ModelUsed == "" {
		out.ModelUsed = first.ModelUsed
	}

	if len(out.Messages) == 0 {
		out.Messages = first.Messages
	}

	return out
}

// WithoutSystem drops a conversation's system message: a run's own comes
// first, from its persona and instructions.
func WithoutSystem(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))

	for _, m := range msgs {
		if m.Role != "system" {
			out = append(out, m)
		}
	}

	return out
}
