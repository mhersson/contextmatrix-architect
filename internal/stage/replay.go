package stage

import (
	"slices"

	"github.com/mhersson/contextmatrix-harness/llm"
)

// withoutReasoning is a copy of msgs with every message's reasoning details
// dropped whole; msgs itself is never written to.
func withoutReasoning(msgs []llm.Message) []llm.Message {
	if msgs == nil {
		return nil
	}

	out := slices.Clone(msgs)
	for i := range out {
		out[i].ReasoningDetails = nil
	}

	return out
}

// hasReasoning reports whether any message carries reasoning details.
func hasReasoning(msgs []llm.Message) bool {
	return slices.ContainsFunc(msgs, func(m llm.Message) bool { return len(m.ReasoningDetails) > 0 })
}
