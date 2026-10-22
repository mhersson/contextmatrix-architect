package stage

import (
	"fmt"
	"slices"

	"github.com/mhersson/contextmatrix-harness/llm"
)

// Reasons a run ends with: refused before any model call because its
// opening prompt cannot fit, and the harness's stop on a full window.
const (
	ReasonTooBig       = "too_big"
	ReasonContextLimit = "context_limit"
)

// bytesPerToken estimates tokens from UTF-8 bytes. Stage tasks are markdown
// and JSON: first calls of 50 kB or more measured 2.40 to 2.76 bytes per
// token, tool schemas included, so 2.5 errs towards refusing.
const bytesPerToken = 2.5

// reasoningBytesPerToken estimates tokens from a reply's reasoning details.
// Captured replies carried 4.7 to 10.6 bytes of details per billed reasoning
// token (signed or encrypted blocks beside their summary text), so 4 errs
// towards refusing.
const reasoningBytesPerToken = 4

// EstimateTokens is s's estimated size in tokens.
func EstimateTokens(s string) int {
	return int(float64(len(s)) / bytesPerToken)
}

// compactionLimit is the prompt size at which the harness stops a run it
// cannot summarize. On small windows it starts summarizing earlier, at the
// lesser of this and the window less its summary reserve.
func compactionLimit(window int) int {
	return int(compactionThreshold * float64(window))
}

// Part is one input of a refused run's task and its estimated tokens.
type Part struct {
	Name   string
	Tokens int
}

// TooBigError refuses a run whose opening prompt cannot fit: its
// irreducible part reaches the compaction limit, or the whole of it reaches
// the window.
type TooBigError struct {
	Estimate, Limit, Window int
}

func (e *TooBigError) Error() string {
	return fmt.Sprintf("the opening prompt is about %d tokens, and a run on a %d-token window must open below %d", e.Estimate, e.Window, e.Limit)
}

// ContextFullError is the harness's stop on a window compaction could not
// shrink. It is a run without an artifact.
type ContextFullError struct {
	Window, Limit int
}

func (e *ContextFullError) Error() string {
	return fmt.Sprintf("the conversation reached the %d tokens a %d-token window allows and could not be summarized", e.Limit, e.Window)
}

func (e *ContextFullError) Unwrap() error { return ErrNoArtifact }

// messageTokens is a message's estimated tokens: content, tool-call
// arguments and replayed reasoning details.
func messageTokens(m llm.Message) int {
	n := EstimateTokens(m.Content)

	for _, tc := range m.ToolCalls {
		n += EstimateTokens(tc.Function.Arguments)
	}

	return n + len(m.ReasoningDetails)/reasoningBytesPerToken
}

// tailStart is the index in seq (the history, then the task) from which
// compaction keeps messages verbatim. It mirrors the harness: the boundary
// keepRecentMessages from the end moves back over tool results to the
// assistant that issued them, and once more over an assistant with tool
// calls. A sequence compaction cannot split is kept whole.
func tailStart(seq []llm.Message) int {
	b := len(seq) - keepRecentMessages
	if b <= 1 {
		return 0
	}

	for b > 0 && seq[b].Role == "tool" {
		b--
	}

	if b > 0 && seq[b-1].Role == "assistant" && len(seq[b-1].ToolCalls) > 0 {
		b--
	}

	return b
}

// preflight refuses a run whose opening prompt cannot fit. Compaction keeps
// the system prompt and the newest messages, the task the last of them, so
// that irreducible tail reaching the compaction limit is refused. The
// harness compacts only after a reply, so the first call carries the whole
// history: that reaching the window is refused too.
func preflight(system, task string, history []llm.Message, window int) error {
	seq := append(slices.Clone(history), llm.Message{Role: "user", Content: task})
	base := EstimateTokens(system)
	tail, whole := base, base
	start := tailStart(seq)

	for i, m := range seq {
		n := messageTokens(m)
		whole += n

		if i >= start {
			tail += n
		}
	}

	if limit := compactionLimit(window); tail >= limit {
		return &TooBigError{Estimate: tail, Limit: limit, Window: window}
	}

	if whole >= window {
		return &TooBigError{Estimate: whole, Limit: window, Window: window}
	}

	return nil
}
