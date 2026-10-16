package cli

import (
	"testing"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

// published drains every entry the hub has already delivered; Publish is
// synchronous, so nothing is in flight once the call under test returns.
func published(ch <-chan protocol.LogEntry) []protocol.LogEntry {
	var out []protocol.LogEntry

	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func newTestHub(t *testing.T) (*logbridge.Hub, <-chan protocol.LogEntry) {
	t.Helper()

	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)
	id, ch := hub.Subscribe("S1")

	t.Cleanup(func() { hub.Unsubscribe(id) })

	return hub, ch
}

func TestEventSink_IdleFollowsEveryWaitOnTheUser(t *testing.T) {
	cases := []struct {
		kind     string
		wantIdle bool
	}{
		{blueprint.EventQuestion, true},
		{blueprint.EventStageEnded, true},
		{blueprint.EventError, true},
		{blueprint.EventNote, true},
		{blueprint.EventAborted, true},
		{blueprint.EventCreated, false},
		{blueprint.EventStageStarted, false},
		{blueprint.EventArtifact, false},
		{blueprint.EventAnswer, false},
		{blueprint.EventEnded, false},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			hub, ch := newTestHub(t)

			eventSink(hub)(blueprint.Event{Kind: tc.kind, BlueprintID: "S1", Payload: map[string]any{"text": "q", "body": "b"}})

			entries := published(ch)
			for _, e := range entries[:max(len(entries)-1, 0)] {
				assert.NotEqual(t, "status", e.Type, "a status frame is always last")
			}

			if !tc.wantIdle {
				for _, e := range entries {
					assert.NotEqual(t, "status", e.Type)
				}

				return
			}

			require.NotEmpty(t, entries)
			last := entries[len(entries)-1]
			assert.Equal(t, "status", last.Type)
			assert.Equal(t, "idle", last.Content)
			assert.Equal(t, "S1", last.SessionID)
		})
	}
}

func TestHarnessForwarder_DropsStopLines(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantType string
	}{
		{"stop is not shown", `{"seq":9,"kind":"state_change","data":{"stop":"done","turns":2,"via":"terminating_tool"}}`, ""},
		{"stop on a cap is not shown", `{"seq":9,"kind":"state_change","data":{"stop":"max_cost","cost_usd":2}}`, ""},
		{"a tool call is shown", `{"seq":3,"kind":"tool_call","data":{"id":"c1","name":"ask_human","raw_args":"{}"}}`, "tool_call"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, ch := newTestHub(t)

			harnessForwarder(hub)("S1", []byte(tc.line))

			entries := published(ch)
			if tc.wantType == "" {
				assert.Empty(t, entries)

				return
			}

			require.Len(t, entries, 1)
			assert.Equal(t, tc.wantType, entries[0].Type)
		})
	}
}
