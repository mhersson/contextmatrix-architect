package cm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emitServer answers every emission tool with a canned reply and records
// each call's arguments. run_playbook is served only when withRunPlaybook.
type emitServer struct {
	mu   sync.Mutex
	args map[string][]map[string]any
}

func (s *emitServer) calls(name string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.args[name]
}

func newEmitServer(t *testing.T, withRunPlaybook bool) (*emitServer, string) {
	t.Helper()

	es := &emitServer{args: map[string][]map[string]any{}}
	server := mcp.NewServer(&mcp.Implementation{Name: "cm-test", Version: "0"}, nil)

	replies := map[string]string{
		"create_card":     `{"id":"ALPHA-007","title":"Add the thing","state":"todo","warnings":["reference to another project's repo"]}`,
		"create_playbook": `{"id":"ship-the-thing","title":"Ship the thing","complete":0,"total":3}`,
		"get_card":        `{"id":"ALPHA-007","project":"alpha","state":"in_progress","phase":"pr_gates","worker_status":"running","pr_url":"https://example.com/pr/1","review_attempts":1,"body":"## Context\nwhy","subtasks":["ALPHA-008","ALPHA-009"],"token_usage":{"estimated_cost_usd":1.5},"subtask_cost_usd":0.25,"activity_log":[{"action":"parked","message":"needs a key"}]}`,
		"get_playbook":    `{"id":"ship-the-thing","runnable":true,"run":{"status":"waiting","entry":"e2","reason":"awaiting check-off"},"repos":[{"project":"alpha","compare_url":"https://example.com/compare"}],"complete":1,"total":3,"entries":[{"id":"e1","type":"card","project":"alpha","card":"ALPHA-007","card_state":"done","complete":true},{"id":"e2","type":"manual","text":"check staging","complete":false}]}`,
		"list_playbooks":  `{"playbooks":[{"id":"other","title":"Other","runnable":true}]}`,
		"run_card":        `{"id":"ALPHA-007","project":"alpha","state":"todo","worker_status":"queued"}`,
	}

	if withRunPlaybook {
		replies["run_playbook"] = `{"id":"ship-the-thing","runnable":true}`
	}

	for name, reply := range replies {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var m map[string]any

				_ = json.Unmarshal(req.Params.Arguments, &m)

				es.mu.Lock()
				es.args[name] = append(es.args[name], m)
				es.mu.Unlock()

				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}}, nil
			})
	}

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(hs.Close)

	return es, hs.URL
}

func connectTo(t *testing.T, url string) *Client {
	t.Helper()

	c, err := Connect(context.Background(), url, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestCreateCard_SendsTheCardAndReturnsIDAndWarnings(t *testing.T) {
	es, url := newEmitServer(t, true)
	c := connectTo(t, url)

	got, err := c.CreateCard(context.Background(), CardInput{
		Project: "alpha", Title: "Add the thing", Type: "feature", Priority: "medium",
		Body: "## Context\nwhy", DependsOn: []string{"ALPHA-006"}, AgentID: "human:architect",
	})
	require.NoError(t, err)
	assert.Equal(t, "ALPHA-007", got.ID)
	assert.Equal(t, []string{"reference to another project's repo"}, got.Warnings)

	sent := es.calls("create_card")
	require.Len(t, sent, 1)
	assert.Equal(t, "alpha", sent[0]["project"])
	assert.Equal(t, "medium", sent[0]["priority"])
	assert.Equal(t, []any{"ALPHA-006"}, sent[0]["depends_on"])
	assert.Equal(t, "human:architect", sent[0]["agent_id"])
	assert.NotContains(t, sent[0], "skills", "empty skills inherit the project's default")
}

func TestCreatePlaybook_ReturnsTheID(t *testing.T) {
	es, url := newEmitServer(t, true)
	c := connectTo(t, url)

	id, err := c.CreatePlaybook(context.Background(), PlaybookInput{
		Title: "Ship the thing", Description: "summary", AgentID: "human:architect",
		Entries: []PlaybookEntryInput{{Type: "card", Project: "alpha", Card: "ALPHA-007"}, {Type: "manual", Text: "check staging"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "ship-the-thing", id)

	sent := es.calls("create_playbook")
	require.Len(t, sent, 1)

	entries, _ := sent[0]["entries"].([]any)
	require.Len(t, entries, 2)
	assert.Equal(t, map[string]any{"type": "manual", "text": "check staging"}, entries[1])
}

func TestGetCardAndPlaybook_DecodeTheWatchFields(t *testing.T) {
	es, url := newEmitServer(t, true)
	c := connectTo(t, url)
	ctx := context.Background()

	card, err := c.GetCard(ctx, "alpha", "ALPHA-007")
	require.NoError(t, err)
	assert.Equal(t, Card{ID: "ALPHA-007", Project: "alpha", State: "in_progress", Phase: "pr_gates", WorkerStatus: "running", PRURL: "https://example.com/pr/1", ReviewAttempts: 1}, card)

	sent := es.calls("get_card")
	require.Len(t, sent, 1)
	assert.Equal(t, false, sent[0]["include_images"])
	assert.Equal(t, false, sent[0]["include_activity_log"])

	pb, err := c.GetPlaybook(ctx, "ship-the-thing")
	require.NoError(t, err)
	require.NotNil(t, pb.Run)
	assert.Equal(t, "waiting", pb.Run.Status)
	assert.Equal(t, "awaiting check-off", pb.Run.Reason)
	assert.Equal(t, []PlaybookRepo{{Project: "alpha", CompareURL: "https://example.com/compare"}}, pb.Repos)
	require.Len(t, pb.Entries, 2)
	assert.Equal(t, "check staging", pb.Entries[1].Text)

	list, err := c.ListPlaybooks(ctx)
	require.NoError(t, err)
	assert.Equal(t, []PlaybookSummary{{ID: "other", Title: "Other", Runnable: true}}, list)
}

func TestGetEndedCard_ReadsTheBodyAndTheRunSignals(t *testing.T) {
	es, url := newEmitServer(t, true)
	c := connectTo(t, url)

	card, err := c.GetEndedCard(context.Background(), "alpha", "ALPHA-007")
	require.NoError(t, err)
	assert.Equal(t, "ALPHA-007", card.ID)
	assert.Equal(t, 1, card.ReviewAttempts)
	assert.Equal(t, "## Context\nwhy", card.Body)
	assert.Equal(t, []string{"ALPHA-008", "ALPHA-009"}, card.Subtasks)
	require.NotNil(t, card.TokenUsage)
	assert.InDelta(t, 1.5, card.TokenUsage.EstimatedCostUSD, 1e-9)
	assert.InDelta(t, 0.25, card.SubtaskCostUSD, 1e-9)
	assert.Equal(t, []ActivityEntry{{Action: "parked", Message: "needs a key"}}, card.ActivityLog)

	sent := es.calls("get_card")
	require.Len(t, sent, 1)
	assert.Equal(t, true, sent[0]["include_activity_log"])
	assert.Equal(t, false, sent[0]["include_images"])
}

func TestRunCardAndRunPlaybook(t *testing.T) {
	es, url := newEmitServer(t, true)
	c := connectTo(t, url)
	ctx := context.Background()

	card, err := c.RunCard(ctx, "alpha", "ALPHA-007", "human:architect")
	require.NoError(t, err)
	assert.Equal(t, "queued", card.WorkerStatus)
	assert.Equal(t, "human:architect", es.calls("run_card")[0]["agent_id"])

	require.NoError(t, c.RunPlaybook(ctx, "ship-the-thing", "human:architect"))
	assert.Equal(t, map[string]any{"id": "ship-the-thing", "agent_id": "human:architect"}, es.calls("run_playbook")[0])
}

func TestRunPlaybook_MissingToolIsAClearError(t *testing.T) {
	_, url := newEmitServer(t, false)
	c := connectTo(t, url)

	err := c.RunPlaybook(context.Background(), "ship-the-thing", "human:architect")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run_playbook")
}

// serveReply serves one tool that answers every call with reply, as an
// error result when isErr.
func serveReply(t *testing.T, tool, reply string, isErr bool) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "cm-test", Version: "0"}, nil)
	server.AddTool(&mcp.Tool{Name: tool, InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}, IsError: isErr}, nil
		})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(hs.Close)

	return hs.URL
}

func TestEmissionCalls_ReplyErrors(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		isErr bool
		want  []string
	}{
		{name: "error result", reply: "type \"epic\" is not valid for project alpha", isErr: true, want: []string{"create_card", `type "epic" is not valid for project alpha`}},
		{name: "reply is not json", reply: "card created", want: []string{"cm: decode create_card"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := connectTo(t, serveReply(t, "create_card", tt.reply, tt.isErr))

			_, err := c.CreateCard(context.Background(), CardInput{Project: "alpha", Title: "Add the thing", Type: "epic", Priority: "medium"})
			require.Error(t, err)

			for _, want := range tt.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}
