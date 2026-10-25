// Package cm is the architect's client for ContextMatrix's MCP server: the
// Bearer-keyed Streamable HTTP endpoint, with the typed calls the stages and
// the controller need. It mirrors the chat backend's bridge without the
// chat-session header.
package cm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrClosed is returned by a call on a closed Client.
var ErrClosed = errors.New("cm: client is closed")

// Client is one MCP session to CM. A poisoned session is redialled once; a
// closed Client never dials again.
type Client struct {
	mu      sync.Mutex
	session *mcp.ClientSession
	dial    func(ctx context.Context) (*mcp.ClientSession, error)
	closed  bool
}

type bearerTransport struct {
	key  string
	base http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.key)

	return t.base.RoundTrip(r)
}

// Connect dials <baseURL>/mcp with apiKey as the bearer over base, the
// transport that trusts ca_cert_file; nil is http.DefaultTransport. Errors
// never carry the key.
func Connect(ctx context.Context, baseURL, apiKey string, base http.RoundTripper) (*Client, error) {
	if base == nil {
		base = http.DefaultTransport
	}

	httpClient := &http.Client{Transport: &bearerTransport{key: apiKey, base: base}}
	endpoint := strings.TrimRight(baseURL, "/") + "/mcp"

	dial := func(ctx context.Context) (*mcp.ClientSession, error) {
		// No server-to-client handlers are registered, so the standalone SSE
		// stream would carry nothing and only accumulate retry failures.
		transport := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true}
		client := mcp.NewClient(&mcp.Implementation{Name: "contextmatrix-architect", Version: "0.1.0"}, nil)

		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return nil, fmt.Errorf("cm: connect to mcp endpoint: %w", err)
		}

		return session, nil
	}

	session, err := dial(ctx)
	if err != nil {
		return nil, err
	}

	return &Client{session: session, dial: dial}, nil
}

// Close ends the session. Later calls fail with ErrClosed; closing twice is
// a no-op.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true

	if err := c.session.Close(); err != nil {
		return fmt.Errorf("cm: close mcp session: %w", err)
	}

	return nil
}

// CallText calls a tool and returns the concatenated text content. A tool
// result flagged IsError is returned as an error carrying that text.
func (c *Client) CallText(ctx context.Context, tool string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}

	return c.callArgs(ctx, tool, args)
}

// callArgs calls a tool with any JSON-encodable arguments and returns its
// text, as CallText does.
func (c *Client) callArgs(ctx context.Context, tool string, args any) (string, error) {
	result, err := c.call(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("cm: call %s: %w", tool, err)
	}

	var sb strings.Builder

	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}

	if result.IsError {
		return "", fmt.Errorf("cm: %s: %s", tool, sb.String())
	}

	return sb.String(), nil
}

func (c *Client) call(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return nil, ErrClosed
	}

	sess := c.session
	c.mu.Unlock()

	result, err := sess.CallTool(ctx, params)
	if err == nil || (!errors.Is(err, mcp.ErrConnectionClosed) && !errors.Is(err, mcp.ErrSessionMissing)) {
		return result, err
	}

	c.mu.Lock()

	// A Close during the call closed the session; it is not redialled.
	if c.closed {
		c.mu.Unlock()

		return nil, errors.Join(ErrClosed, err)
	}

	if c.session == sess {
		fresh, derr := c.dial(ctx)
		if derr != nil {
			c.mu.Unlock()

			return nil, errors.Join(err, derr)
		}

		_ = sess.Close()
		c.session = fresh
	}

	sess = c.session
	c.mu.Unlock()

	return sess.CallTool(ctx, params)
}

// Repo is one repository of a project.
type Repo struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Primary bool   `json:"primary"`
}

// Verify is the project's verify command.
type Verify struct {
	Command string `json:"command"`
}

// Project is the subset of a CM project the architect reads.
type Project struct {
	Name string `json:"name"`
	// Prefix starts the project's card ids, as in PREFIX-012.
	Prefix string  `json:"prefix"`
	Repo   string  `json:"repo"`
	Repos  []Repo  `json:"repos"`
	Verify *Verify `json:"verify"`
	// POC is set on a project ContextMatrix flags as a proof of concept; a
	// ContextMatrix that predates the flag omits it, which reads as false.
	POC bool `json:"poc"`

	Types      []string `json:"types"`
	Priorities []string `json:"priorities"`
}

// RepoURLs returns the project's repositories: the repos list when set,
// else the single repo, else nothing.
func (p Project) RepoURLs() []string {
	if len(p.Repos) > 0 {
		out := make([]string, 0, len(p.Repos))
		for _, r := range p.Repos {
			if r.URL != "" {
				out = append(out, r.URL)
			}
		}

		return out
	}

	if p.Repo != "" {
		return []string{p.Repo}
	}

	return nil
}

// ListProjects returns every project CM serves.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	text, err := c.CallText(ctx, "list_projects", nil)
	if err != nil {
		return nil, err
	}

	var out struct {
		Projects []Project `json:"projects"`
	}

	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("cm: decode list_projects: %w", err)
	}

	return out.Projects, nil
}
