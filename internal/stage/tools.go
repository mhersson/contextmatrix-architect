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

// Artifact is what a stage submits.
type Artifact struct {
	Body string         `json:"body"`
	Data map[string]any `json:"data,omitempty"`
}

// recommendedMarker is what a model sometimes writes at the end of an option
// instead of giving its position as recommended. The panel marks the
// recommended option itself, so a marker left in the text shows twice and
// reaches the stage as part of the answer.
const recommendedMarker = "(recommended)"

// cutRecommended removes a trailing recommendedMarker, case ignored, with the
// whitespace around it, and reports whether there was one.
func cutRecommended(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if len(t) < len(recommendedMarker) || !strings.EqualFold(t[len(t)-len(recommendedMarker):], recommendedMarker) {
		return s, false
	}

	return strings.TrimSpace(t[:len(t)-len(recommendedMarker)]), true
}

// recommendedPosition resolves a question's recommended option. kept[i] is
// the position, from 1, of the i-th given option among the kept ones, 0 for a
// blank one; marked holds the kept positions whose text carried the marker.
// v is the model's "recommended" argument, nil when it gave none.
func recommendedPosition(v any, given int, kept, marked []int) (int, error) {
	if v != nil {
		if given == 0 {
			return 0, errors.New("recommended needs options")
		}

		n, isNumber := v.(float64)
		if !isNumber || n != float64(int(n)) || int(n) < 1 || int(n) > given || kept[int(n)-1] == 0 {
			return 0, fmt.Errorf("recommended is the position of one of the options, from 1 to %d", given)
		}

		return kept[int(n)-1], nil
	}

	// The position wins over a marker, and markers on several options name
	// none.
	if len(marked) == 1 {
		return marked[0], nil
	}

	return 0, nil
}

type submitArtifactTool struct {
	validate func(context.Context, Artifact) error
	drafts   *draftExtractor
}

func (submitArtifactTool) Name() string   { return submitArtifactName }
func (submitArtifactTool) Terminal() bool { return true }

func (submitArtifactTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:        submitArtifactName,
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
func (t submitArtifactTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	art := Artifact{Body: stringArg(args, "body")}

	// The submit's own body is the authority on what the draft shows.
	t.drafts.final(art.Body)

	refuse := func(err error) (tools.Result, error) {
		t.drafts.refuse(err.Error())

		return tools.Result{}, err
	}

	if strings.TrimSpace(art.Body) == "" {
		return refuse(errors.New("submit_artifact: body is required"))
	}

	if data, ok := args["data"]; ok && data != nil {
		obj, isObject := data.(map[string]any)
		if !isObject {
			return refuse(errors.New("submit_artifact: data must be a JSON object"))
		}

		art.Data = obj
	}

	if t.validate != nil {
		if err := t.validate(ctx, art); err != nil {
			t.drafts.refuse(err.Error())

			return tools.Result{}, fmt.Errorf("submit_artifact rejected: %w", err)
		}
	}

	return tools.Result{Text: "artifact accepted"}, nil
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)

	return s
}
