package cm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolPrefix names CM tools in a stage's registry, so none can replace a
// harness or code tool of the same name.
const ToolPrefix = "cm_"

// StageReads returns the CM tools a stage run may call, a fresh slice on
// every call so no caller can widen the allowlist. Writes and run starts are
// controller calls, never model tools: the caller identity that passes CM's
// human gates must not reach the model.
func StageReads() []string {
	return []string{"list_projects", "list_cards", "get_card", "get_playbook"}
}

// ReadTools wraps the tools in allow that CM serves as harness tools; a
// name CM does not serve is skipped. agent_id is removed from every schema
// and every call, so CM treats the caller as an agent and keeps redacting
// unvetted imported text, and a tool that can attach images never does.
// Every result is prefixed with the source tag.
func (c *Client) ReadTools(ctx context.Context, allow []string, tag string) ([]tools.Tool, error) {
	listed, err := c.listTools(ctx)
	if err != nil {
		return nil, err
	}

	var out []tools.Tool

	for _, t := range listed {
		if !slices.Contains(allow, t.Name) {
			continue
		}

		params, images, err := modelSchema(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("cm: schema of %s: %w", t.Name, err)
		}

		out = append(out, &readTool{
			client: c, name: t.Name, description: t.Description,
			params: params, types: schemaPropTypes(t.InputSchema), images: images, tag: tag,
		})
	}

	return out, nil
}

// listTools returns every tool CM serves, following pagination.
func (c *Client) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return nil, ErrClosed
	}

	sess := c.session
	c.mu.Unlock()

	var out []*mcp.Tool

	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("cm: list tools: %w", err)
		}

		out = append(out, t)
	}

	return out, nil
}

// modelSchema is the input schema the model sees: CM's, without agent_id
// and include_images. images reports whether CM's schema declared
// include_images, so every call can turn it off.
func modelSchema(input any) (json.RawMessage, bool, error) {
	if input == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`), false, nil
	}

	raw, err := json.Marshal(input)
	if err != nil {
		return nil, false, err
	}

	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, false, err
	}

	images := false

	if props, ok := schema["properties"].(map[string]any); ok {
		_, images = props["include_images"]

		delete(props, "agent_id")
		delete(props, "include_images")
	}

	if req, ok := schema["required"].([]any); ok {
		kept := make([]any, 0, len(req))

		for _, r := range req {
			if r != "agent_id" && r != "include_images" {
				kept = append(kept, r)
			}
		}

		if len(kept) > 0 {
			schema["required"] = kept
		} else {
			delete(schema, "required")
		}
	}

	out, err := json.Marshal(schema)

	return out, images, err
}

// readTool is one CM read offered to a stage model.
type readTool struct {
	client      *Client
	name        string
	description string
	params      json.RawMessage
	types       propTypes
	images      bool
	tag         string
}

func (t *readTool) Name() string   { return ToolPrefix + t.name }
func (t *readTool) ReadOnly() bool { return true }

func (t *readTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: t.Name(), Description: t.description, Parameters: t.params,
	}}
}

func (t *readTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	call := coerceArgs(args, t.types)
	delete(call, "agent_id")

	if t.images {
		call["include_images"] = false
	}

	text, err := t.client.CallText(ctx, t.name, call)
	if err != nil {
		return tools.Result{}, scrubbedError{err}
	}

	return tools.Result{Text: "[" + t.tag + "] " + stripURLUserinfo(text)}, nil
}

// urlUserinfo matches a url's scheme and userinfo: everything from "://" to
// the last "@" of the authority. The class is exclusion-based so an unusual
// password character cannot leak: it stops only at a path, query or
// fragment start, whitespace, a double quote (a JSON string boundary) or a
// backslash that is not a JSON \uXXXX escape. CM replies are json.Marshal
// output, so "&" arrives as \u0026 and a newline as \n, which ends the
// match before any later email address. There is no word boundary: the
// record is append-only, so a credential after "_" or a digit must go too.
// Over-scrubbing the model's view is preferred to leaking.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)(?:[^/?#\s"\\]|\\u[0-9A-Fa-f]{4})*@`)

// scrubbedError reports the wrapped error's text with url credentials
// removed and still unwraps to the original, for callers that match on it.
type scrubbedError struct{ err error }

func (e scrubbedError) Error() string { return stripURLUserinfo(e.err.Error()) }

func (e scrubbedError) Unwrap() error { return e.err }

// stripURLUserinfo removes the userinfo from every url in s. CM returns a
// project's config as stored, so a repository url may carry a credential,
// and the harness redacts only the session's own secrets. What a tool
// returns reaches the model, the transcript and the event log, and those
// are append-only: a credential that got there could never be removed. An
// address without a scheme, such as an email or git's scp form, is left
// as it is.
func stripURLUserinfo(s string) string {
	return urlUserinfo.ReplaceAllString(s, "$1")
}
