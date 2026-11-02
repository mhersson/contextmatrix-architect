package cm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm/cmtest"
)

type fakeKnowledge struct {
	mu      sync.Mutex
	caller  string
	tag     string
	err     error
	sources []string
	ids     [][]string
}

func (f *fakeKnowledge) Caller(context.Context) string { return f.caller }

func (f *fakeKnowledge) Source(_ context.Context, source string, ids []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return "", f.err
	}

	f.sources = append(f.sources, source)
	f.ids = append(f.ids, ids)

	return f.tag, nil
}

func knowledgeToolsFor(t *testing.T, srv *cmtest.Knowledge, k Knowledge) map[string]tools.Tool {
	t.Helper()

	c, err := Connect(context.Background(), srv.URL, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.KnowledgeTools(context.Background(), k)
	require.NoError(t, err)

	out := map[string]tools.Tool{}
	for _, tool := range got {
		out[tool.Name()] = tool
	}

	return out
}

func TestKnowledgeTools_OffersTheKnowledgeReadsCMServes(t *testing.T) {
	tests := []struct {
		name   string
		served []string
		want   []string
	}{
		{"all", nil, []string{"cm_get_knowledge_context", "cm_query_knowledge", "cm_get_knowledge_item"}},
		{"older CM", []string{"get_card"}, nil},
		{"some", []string{"get_knowledge_item", "get_card"}, []string{"cm_get_knowledge_item"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cmtest.NewKnowledge(t, tt.served...)
			got := knowledgeToolsFor(t, srv, &fakeKnowledge{})

			names := make([]string, 0, len(got))

			for name, tool := range got {
				names = append(names, name)

				ro, ok := tool.(interface{ ReadOnly() bool })
				require.True(t, ok)
				assert.True(t, ro.ReadOnly())
				assert.Equal(t, name, tool.Schema().Function.Name)
			}

			assert.ElementsMatch(t, tt.want, names)
		})
	}
}

func TestKnowledgeTools_SchemaHidesTheCaller(t *testing.T) {
	got := knowledgeToolsFor(t, cmtest.NewKnowledge(t), &fakeKnowledge{})
	require.Len(t, got, 3)

	for name, tool := range got {
		var schema map[string]any
		require.NoError(t, json.Unmarshal(tool.Schema().Function.Parameters, &schema))

		props, _ := schema["properties"].(map[string]any)
		assert.NotContains(t, props, "caller", name)
		assert.NotContains(t, props, "agent_id", name)
		assert.Contains(t, props, "project", name)

		if name == "cm_get_knowledge_item" {
			assert.Equal(t, []any{"ids"}, schema["required"])
		}
	}
}

func TestKnowledgeTools_EveryCallCarriesTheRunsCallerAndNoAgentID(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	k := &fakeKnowledge{caller: "blueprint BP1 stage intake"}
	got := knowledgeToolsFor(t, srv, k)

	_, err := got["cm_get_knowledge_context"].Execute(context.Background(), map[string]any{
		"project": "alpha", "task": "the need", "limit": "5",
		"caller": "human:alice", "agent_id": "human:alice",
	})
	require.NoError(t, err)

	calls := srv.Calls("get_knowledge_context")
	require.Len(t, calls, 1)
	assert.Equal(t, "blueprint BP1 stage intake", calls[0]["caller"])
	assert.NotContains(t, calls[0], "agent_id")
	assert.InDelta(t, float64(5), calls[0]["limit"], 0)
}

func TestKnowledgeTools_AResultWithItemsLeadsWithItsSourceTag(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	srv.Answer("get_knowledge_context", cmtest.AgreedRule)

	k := &fakeKnowledge{tag: "S4"}
	got := knowledgeToolsFor(t, srv, k)

	res, err := got["cm_get_knowledge_context"].Execute(context.Background(), map[string]any{"project": "alpha", "task": "t"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(res.Text, "[S4] "))
	assert.Equal(t, []string{"knowledge:intent"}, k.sources)
	assert.Equal(t, [][]string{{"R-12"}}, k.ids)
	assert.NotContains(t, res.Text, "secret")

	var body struct {
		Items []map[string]any `json:"items"`
	}

	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(res.Text, "[S4] ")), &body))
	require.Len(t, body.Items, 1)
	assert.Equal(t, "R-12", body.Items[0]["id"])
}

func TestKnowledgeTools_AnAnswerWithoutItemsIsUntaggedAndNoSource(t *testing.T) {
	tests := []struct {
		name   string
		reply  string
		tagged bool
	}{
		{"no provider", cmtest.NoProvider, false},
		{"timed out", cmtest.ProviderTimedOut, false},
		{"empty items", `{"source":"knowledge:intent","items":[]}`, false},
		{"null items", `{"source":"knowledge:intent","items":null}`, false},
		{"not json", "the provider timed out", false},
		{"related only", `{"source":"knowledge:intent","items":[],"related":[{"id":"G-1","kind":"goal","title":"t","text":"x"}]}`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := cmtest.NewKnowledge(t)
			srv.Answer("get_knowledge_context", tt.reply)

			k := &fakeKnowledge{tag: "S4"}
			got := knowledgeToolsFor(t, srv, k)

			res, err := got["cm_get_knowledge_context"].Execute(context.Background(), map[string]any{"project": "alpha", "task": "t"})
			require.NoError(t, err)

			if tt.tagged {
				assert.True(t, strings.HasPrefix(res.Text, "[S4] "))
				assert.Equal(t, []string{"knowledge:intent"}, k.sources)
				assert.Equal(t, [][]string{{"G-1"}}, k.ids)

				return
			}

			assert.Equal(t, tt.reply, res.Text)
			assert.Empty(t, k.sources)
		})
	}
}

func TestKnowledgeTools_AToolErrorKeepsNoURLCredential(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	srv.Fail("get_knowledge_item", "unknown project https://u:secret@example.com/x")

	got := knowledgeToolsFor(t, srv, &fakeKnowledge{})

	_, err := got["cm_get_knowledge_item"].Execute(context.Background(), map[string]any{"project": "alpha", "ids": []any{"R-12"}})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret")
}

func TestKnowledgeTools_ASourceThatCannotBeRecordedIsAToolError(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	srv.Answer("get_knowledge_context", cmtest.AgreedRule)

	got := knowledgeToolsFor(t, srv, &fakeKnowledge{err: errors.New("store closed")})

	_, err := got["cm_get_knowledge_context"].Execute(context.Background(), map[string]any{"project": "alpha", "task": "t"})
	require.Error(t, err)
}

func TestKnowledgeTools_IDsAndKindsAsOneStringAreSentAsAList(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	got := knowledgeToolsFor(t, srv, &fakeKnowledge{})

	_, err := got["cm_get_knowledge_item"].Execute(context.Background(), map[string]any{"project": "alpha", "ids": "R-12"})
	require.NoError(t, err)
	require.Len(t, srv.Calls("get_knowledge_item"), 1)
	assert.Equal(t, []any{"R-12"}, srv.Calls("get_knowledge_item")[0]["ids"])

	_, err = got["cm_query_knowledge"].Execute(context.Background(), map[string]any{"project": "alpha", "query": "vat", "kinds": `["rule","decision"]`})
	require.NoError(t, err)
	require.Len(t, srv.Calls("query_knowledge"), 1)
	assert.Equal(t, []any{"rule", "decision"}, srv.Calls("query_knowledge")[0]["kinds"])
}

func TestOpenKnowledge_ClosesASessionWithoutKnowledgeTools(t *testing.T) {
	srv := cmtest.NewKnowledge(t, "get_card")

	ts, closer, err := OpenKnowledge(context.Background(), srv.URL, "mcp-key", nil, &fakeKnowledge{})
	require.NoError(t, err)
	assert.Empty(t, ts)
	require.NotNil(t, closer)
	closer()

	srv = cmtest.NewKnowledge(t)

	ts, closer, err = OpenKnowledge(context.Background(), srv.URL, "mcp-key", nil, &fakeKnowledge{})
	require.NoError(t, err)
	require.NotNil(t, closer)
	t.Cleanup(closer)
	assert.Len(t, ts, 3)

	bearers := srv.Bearers()
	require.NotEmpty(t, bearers)

	for _, b := range bearers {
		assert.Equal(t, "Bearer mcp-key", b)
	}
}

func TestKnowledgeReads_IsAFreshCopyOfTheAllowlist(t *testing.T) {
	reads := KnowledgeReads()
	assert.Equal(t, []string{"get_knowledge_context", "query_knowledge", "get_knowledge_item"}, reads)

	reads[0] = "create_card"

	fresh := KnowledgeReads()
	assert.Equal(t, "get_knowledge_context", fresh[0])
}
