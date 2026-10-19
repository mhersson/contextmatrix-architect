package stage

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
// conversation. A cost cap the first run spent gets no follow-up: the first
// result is returned with reason max_cost, as a run the cap stopped.
func forceTerminal(ctx context.Context, client llm.LLM, terminals []tools.Tool, emit *events.Emitter, cfg harness.Config, first harness.Result) (harness.Result, error) {
	if len(terminals) == 0 {
		return first, nil
	}

	if cfg.MaxCostUSD > 0 {
		left := cfg.MaxCostUSD - first.TotalCostUSD
		if left <= 0 {
			first.Reason = "max_cost"

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

	reprompt := fmt.Sprintf(forcedReprompt, strings.Join(names, " or "))

	second, err := harness.Run(ctx, forcedChoice{LLM: client}, tools.NewRegistry(terminals...), emit, reprompt, cfg)

	out := merged(first, second)

	// Two turns that ended in text or in refused calls are not a turn limit
	// the operator can act on: name what the model did instead.
	if err == nil && second.CompletionArgs == nil && (second.Reason == "max_turns" || second.Reason == "done") {
		out.Reason = ReasonTerminalRefused

		return out, &RefusedError{Tools: names, Last: lastRefusal(second.Messages, reprompt, names)}
	}

	return out, err
}

// ReasonTerminalRefused is the reason of a forced follow-up that ended
// without a terminal call the run accepted.
const ReasonTerminalRefused = "terminal_refused"

// RefusedError is a forced follow-up that ended without an accepted terminal
// call. Last is the refusal of the last terminal call it made, empty when it
// made none.
type RefusedError struct {
	Tools []string
	Last  string
}

func (e *RefusedError) Error() string {
	names := strings.Join(e.Tools, " or ")
	if e.Last == "" {
		return fmt.Sprintf("the model ended its turn without calling %s and did not call it when asked again", names)
	}

	return fmt.Sprintf("the model ended its turn without calling %s, and when asked again its calls were refused or malformed, the last: %s", names, e.Last)
}

func (e *RefusedError) Unwrap() error { return ErrNoArtifact }

// lastRefusal is the refusal of the last terminal call in the part of msgs
// after the last user message equal to reprompt.
func lastRefusal(msgs []llm.Message, reprompt string, terminals []string) string {
	start := 0

	for i, m := range msgs {
		if m.Role == "user" && m.Content == reprompt {
			start = i + 1
		}
	}

	var callID, argsJSON string

	at := -1

	for i := start; i < len(msgs); i++ {
		if msgs[i].Role != "assistant" {
			continue
		}

		for _, tc := range msgs[i].ToolCalls {
			if slices.Contains(terminals, tc.Function.Name) {
				callID, argsJSON, at = tc.ID, tc.Function.Arguments, i
			}
		}
	}

	if at < 0 {
		return ""
	}

	// Decoded as the harness decodes arguments, so a malformed call is named
	// by the same error the model was shown.
	raw := strings.TrimSpace(argsJSON)
	if raw == "" {
		raw = "{}"
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "its arguments were not valid JSON: " + err.Error()
	}

	// Ids repeat across turns, so the result is looked for only among the
	// tool messages that directly follow the call's own assistant message.
	for _, m := range msgs[at+1:] {
		if m.Role != "tool" {
			break
		}

		if m.ToolCallID == callID {
			return strings.TrimPrefix(m.Content, "tool error: ")
		}
	}

	return ""
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
