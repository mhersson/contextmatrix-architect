package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// Question is what a stage asks a human (or the auto responder).
type Question struct {
	Text    string
	Options []string
	Context string
}

// Asker answers a stage's question. The call blocks until the answer exists
// or ctx ends; the controller implements it over the session's reply channel.
type Asker interface {
	Ask(ctx context.Context, q Question) (string, error)
}

// Artifact is what a stage submits.
type Artifact struct {
	Body string         `json:"body"`
	Data map[string]any `json:"data,omitempty"`
}

var errNoAsker = errors.New("ask_human: no responder is attached to this run")

type askHumanTool struct {
	asker Asker
}

func (askHumanTool) Name() string { return "ask_human" }

func (askHumanTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: "ask_human",
		Description: "Ask the person driving this blueprint one question you cannot decide yourself. " +
			"Blocks until they answer. Offer options when there are natural ones.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"question": {"type": "string", "description": "the question, one sentence"},
				"options": {"type": "array", "items": {"type": "string"}, "description": "candidate answers, optional"},
				"context": {"type": "string", "description": "why it matters, optional"}
			},
			"required": ["question"]
		}`),
	}}
}

func (t askHumanTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	q := Question{Text: strings.TrimSpace(stringArg(args, "question")), Context: stringArg(args, "context")}
	if q.Text == "" {
		return tools.Result{}, errors.New("ask_human: question is required")
	}

	if opts, ok := args["options"].([]any); ok {
		for _, o := range opts {
			if s, ok := o.(string); ok && strings.TrimSpace(s) != "" {
				q.Options = append(q.Options, s)
			}
		}
	}

	if t.asker == nil {
		return tools.Result{}, errNoAsker
	}

	answer, err := t.asker.Ask(ctx, q)
	if err != nil {
		return tools.Result{}, fmt.Errorf("ask_human: %w", err)
	}

	return tools.Result{Text: answer}, nil
}

type submitArtifactTool struct {
	validate func(Artifact) error
}

func (submitArtifactTool) Name() string   { return "submit_artifact" }
func (submitArtifactTool) Terminal() bool { return true }

func (submitArtifactTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        "submit_artifact",
		Description: "Submit the stage's finished artifact. Ends the stage.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"body": {"type": "string", "description": "the artifact as markdown"},
				"data": {"type": "object", "description": "the structured block the stage instructions ask for"}
			},
			"required": ["body"]
		}`),
	}}
}

// Execute validates the submit: shape first, then the stage's validator. A
// refusal is a tool error the model can correct within the same run, while a
// decode failure after the terminal call would fail the stage.
func (t submitArtifactTool) Execute(_ context.Context, args map[string]any) (tools.Result, error) {
	art := Artifact{Body: stringArg(args, "body")}

	if strings.TrimSpace(art.Body) == "" {
		return tools.Result{}, errors.New("submit_artifact: body is required")
	}

	if data, ok := args["data"]; ok && data != nil {
		obj, isObject := data.(map[string]any)
		if !isObject {
			return tools.Result{}, errors.New("submit_artifact: data must be a JSON object")
		}

		art.Data = obj
	}

	if t.validate != nil {
		if err := t.validate(art); err != nil {
			return tools.Result{}, fmt.Errorf("submit_artifact rejected: %w", err)
		}
	}

	return tools.Result{Text: "artifact accepted"}, nil
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)

	return s
}
