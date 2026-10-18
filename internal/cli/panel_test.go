package cli

import (
	"testing"
	"time"

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
		name     string
		kind     string
		payload  map[string]any
		wantIdle bool
	}{
		{"gate reply on an idle blueprint", blueprint.EventPolicy, map[string]any{"during_run": false}, true},
		{"gate reply while a stage runs", blueprint.EventPolicy, map[string]any{"during_run": true}, false},
		{"cap change on an idle blueprint", blueprint.EventCap, map[string]any{"from": 2.0, "to": 4.0, "during_run": false}, true},
		{"cap change while a stage runs", blueprint.EventCap, map[string]any{"from": 2.0, "to": 4.0, "during_run": true}, false},
		{"cap change that resumes a stage", blueprint.EventCap, map[string]any{"from": 2.0, "to": 4.0, "during_run": false, "resumes": "readiness"}, false},
		{"refused cap on an idle blueprint", blueprint.EventCap, map[string]any{"from": 2.0, "to": 1.0, "during_run": false, "refused": "spent"}, true},
		{"gate plan", blueprint.EventGatePlan, nil, false},
		{"stage canceled", blueprint.EventStageEnded, map[string]any{"status": "canceled"}, true},
		{"stage done, what follows decides", blueprint.EventStageEnded, map[string]any{"status": "done"}, false},
		{"a stopped emission awaited", blueprint.EventAwaitInput, map[string]any{"stage": "signoff", "prompt": "Emission is stopped; reply approve to resume it."}, true},
		{"report", blueprint.EventReport, map[string]any{"ready": true}, true},
		{"report the sign-off follows", blueprint.EventReport, map[string]any{"ready": true, "signoff": true}, false},
		{"signoff", blueprint.EventSignOff, map[string]any{"cards": []any{}}, true},
		{"gate", blueprint.EventGate, map[string]any{"stage": "shape"}, true},
		{"decided", blueprint.EventDecided, map[string]any{"decision": "approve"}, false},
		{"emitted", blueprint.EventEmitted, map[string]any{"cards": []any{}}, true},
		{"emitted, the run start follows", blueprint.EventEmitted, map[string]any{"cards": []any{}, "run_follows": true}, false},
		{"run started", blueprint.EventRunStarted, map[string]any{"kind": "card", "target": "ALPHA-001"}, true},
		{"watch line", blueprint.EventWatch, map[string]any{"line": "ALPHA-001: todo"}, false},
		{"watch ended", blueprint.EventWatchEnded, map[string]any{"playbook": "p"}, true},
		{"watch state", blueprint.EventWatchState, map[string]any{"cards": []any{}}, false},
		{"card created", blueprint.EventCardCreated, nil, false},
		{"playbook created", blueprint.EventPlaybookCreated, nil, false},
		{"grounded", blueprint.EventGrounded, nil, false},
		{"note that re-runs a stage", blueprint.EventNote, map[string]any{"during_run": false, "retries": "grounding"}, false},
		{"error", blueprint.EventError, nil, true},
		{"a refused responder, the stage runs on", blueprint.EventError, map[string]any{"reason": "responder_refused"}, false},
		{"note on an idle blueprint", blueprint.EventNote, map[string]any{"during_run": false}, true},
		{"note while a stage runs", blueprint.EventNote, map[string]any{"during_run": true}, false},
		{"aborted", blueprint.EventAborted, nil, true},
		{"export on an idle blueprint", blueprint.EventExported, map[string]any{"during_run": false}, true},
		{"export while a stage runs", blueprint.EventExported, map[string]any{"during_run": true}, false},
		{"created", blueprint.EventCreated, nil, false},
		{"stage started", blueprint.EventStageStarted, nil, false},
		{"artifact", blueprint.EventArtifact, nil, false},
		{"answer", blueprint.EventAnswer, nil, false},
		{"ended", blueprint.EventEnded, nil, false},
		{"verdict", blueprint.EventReview, nil, false},
		{"responder failed, the stage runs on", blueprint.EventStageEnded, map[string]any{"status": "failed", "kind": "responder"}, false},
		{"an open round a human answers", blueprint.EventRound, map[string]any{"round": 1.0, "frame": map[string]any{"questions": []any{
			map[string]any{"number": 1.0, "title": "Deploy", "text": "Helm?", "options": []any{}},
		}}}, true},
		{"an answered round, its close and continuation follow", blueprint.EventRound, map[string]any{"round": 1.0, "frame": map[string]any{
			"questions": []any{map[string]any{"number": 1.0, "title": "Deploy", "text": "Helm?", "options": []any{}}},
			"answers":   []any{map[string]any{"number": 1.0, "text": "yes"}},
		}}, false},
		{"a stale reply while a run works", blueprint.EventError, map[string]any{"reason": "stale_reply", "during_run": true}, false},
		{"a stale reply on an idle blueprint", blueprint.EventError, map[string]any{"reason": "stale_reply", "during_run": false}, true},
		{"chat the stage has not seen, a chat run answers it", blueprint.EventRound, map[string]any{"round": 1.0, "frame": map[string]any{
			"questions": []any{map[string]any{"number": 1.0, "title": "Deploy", "text": "Helm?", "options": []any{}}},
			"chat":      []any{map[string]any{"from": "user", "text": "why?"}},
		}}, false},
		{"a chat reply, the round waits again", blueprint.EventRound, map[string]any{"round": 1.0, "seen": 2.0, "frame": map[string]any{
			"questions": []any{map[string]any{"number": 1.0, "title": "Deploy", "text": "Helm?", "options": []any{}}},
			"chat":      []any{map[string]any{"from": "user", "text": "why?"}, map[string]any{"from": "product_owner", "text": "because"}},
		}}, true},
		{"a chat reply with a message written meanwhile, the next chat run follows", blueprint.EventRound, map[string]any{"round": 1.0, "seen": 2.0, "frame": map[string]any{
			"questions": []any{map[string]any{"number": 1.0, "title": "Deploy", "text": "Helm?", "options": []any{}}},
			"chat": []any{
				map[string]any{"from": "user", "text": "why?"},
				map[string]any{"from": "product_owner", "text": "because"},
				map[string]any{"from": "user", "text": "and?"},
			},
		}}, false},
		{"a closed round, its stage goes on", blueprint.EventRound, map[string]any{"round": 1.0, "frame": map[string]any{"closed": true}}, false},
		{"a round the responder answers", blueprint.EventRound, map[string]any{"round": 1.0, "frame": map[string]any{"responder": true}}, false},
		{"a run that asked, its round decides", blueprint.EventStageEnded, map[string]any{"status": "asked", "kind": "stage"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, ch := newTestHub(t)

			payload := map[string]any{"text": "q", "body": "b"}
			for k, v := range tc.payload {
				payload[k] = v
			}

			eventSink(hub)(blueprint.Event{Kind: tc.kind, BlueprintID: "S1", Payload: payload})

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
		{"a tool call is shown", `{"seq":3,"kind":"tool_call","data":{"id":"c1","name":"ask_questions","raw_args":"{}"}}`, "tool_call"},
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

func TestEventSink_RetainsUntilTheConsumerConnects(t *testing.T) {
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)
	sink := eventSink(hub)

	sink(blueprint.Event{Kind: blueprint.EventCreated, BlueprintID: "BP1", Payload: map[string]any{"projects": []string{"alpha"}, "mode": "default"}})

	id, ch := hub.Subscribe("BP1")

	select {
	case e := <-ch:
		assert.Equal(t, "system", e.Type)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("the created entry published before the subscriber connected was not replayed")
	}

	// The consumer leaves, so only a retained key could hold what follows.
	hub.Unsubscribe(id)

	sink(blueprint.Event{Kind: blueprint.EventEnded, BlueprintID: "BP1"})
	hub.Publish(protocol.LogEntry{SessionID: "BP1", Type: "system", Content: "after end"})

	_, again := hub.Subscribe("BP1")

	select {
	case e := <-again:
		t.Fatalf("nothing is retained after the session ended, got %q", e.Content)
	case <-time.After(30 * time.Millisecond):
	}
}
