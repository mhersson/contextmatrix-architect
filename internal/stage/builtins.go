package stage

import (
	"context"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/tools"
)

// builtinTools are the control tools a run is offered. The list depends on
// the Interviewer and the Designer alone, so every run of a stage sends the
// same tools and a resumed conversation keeps its cached prefix on either
// endpoint. A tool the run's mode does not take is offered as offMode.
func builtinTools(in Input, mark *terminalMark, drafts *draftExtractor) []tools.Tool {
	type builtin struct {
		tool tools.Tool
		// live says the run's mode takes the tool.
		live bool
	}

	var set []builtin

	switch {
	case in.Designer != nil && in.Interviewer != nil:
		set = []builtin{
			{answerChatTool{iv: in.Interviewer, mark: mark, plain: in.PlainLanguage}, in.Chat},
			{showMockupTool{d: in.Designer, mark: mark}, true},
		}
	case in.Designer != nil:
		set = []builtin{{showMockupTool{d: in.Designer, mark: mark}, true}}
	case in.Interviewer != nil:
		set = []builtin{
			{askQuestionsTool{iv: in.Interviewer, mark: mark, plain: in.PlainLanguage}, !in.Chat},
			{answerChatTool{iv: in.Interviewer, mark: mark, plain: in.PlainLanguage}, in.Chat},
			{submitArtifactTool{validate: in.Validate, drafts: drafts, mark: mark}, !in.Chat},
		}
	default:
		set = []builtin{{submitArtifactTool{validate: in.Validate, drafts: drafts, mark: mark}, true}}
	}

	var live []string

	for _, b := range set {
		if b.live {
			live = append(live, b.tool.Name())
		}
	}

	why := "no chat waits for this run"
	if in.Chat {
		why = "a chat run only replies in the round's chat"
	}

	out := make([]tools.Tool, 0, len(set))

	for _, b := range set {
		if b.live {
			out = append(out, b.tool)
		} else {
			out = append(out, offMode{Tool: b.tool, why: why, live: live})
		}
	}

	return out
}

// offMode is a built-in offered to a run whose mode does not take it: its
// schema keeps the run's tool list the stage's, and a call is a tool error
// that names the tools that end the run. It embeds tools.Tool, so it never
// implements tools.Terminal: neither the harness nor terminalTools counts
// it, and a forced follow-up never offers it.
type offMode struct {
	tools.Tool

	why  string
	live []string
}

func (t offMode) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, fmt.Errorf("%s: %s; end the run with %s", t.Name(), t.why, strings.Join(t.live, " or "))
}
