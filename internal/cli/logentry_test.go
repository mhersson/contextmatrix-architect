package cli

import (
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

func TestLogEntryFor(t *testing.T) {
	cases := []struct {
		name     string
		event    blueprint.Event
		wantType string
		carries  []string
		skipped  bool
	}{
		{"created", blueprint.Event{Kind: blueprint.EventCreated, BlueprintID: "S1", Payload: map[string]any{"projects": []string{"alpha", "beta"}, "mode": "yolo"}}, "system", []string{"alpha", "beta", "yolo"}, false},
		{"stage started", blueprint.Event{Kind: blueprint.EventStageStarted, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "persona": "product_owner", "pack": "default", "pack_version": "1"}}, "system", []string{"intake"}, false},
		{"question", blueprint.Event{Kind: blueprint.EventQuestion, BlueprintID: "S1", Payload: map[string]any{"text": "Helm or kustomize?", "options": []string{"helm", "kustomize"}}}, "text", []string{"Helm or kustomize?", "helm", "kustomize"}, false},
		{"artifact", blueprint.Event{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "body": "# Need\n\nk8s"}}, "text", []string{"# Need\n\nk8s"}, false},
		{"stage ended", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "status": "done", "cost_usd": 0.25}}, "system", []string{"intake", "done", "0.25"}, false},
		{"stage ended priced", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "status": "done", "cost_usd": 0.2, "priced": true}}, "system", []string{"0.20"}, false},
		{"stage ended unpriced", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "status": "done", "cost_usd": 0.2, "priced": false}}, "system", []string{"0.20"}, false},
		{"note", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "use helm"}}, "system", nil, false},
		{"aborted", blueprint.Event{Kind: blueprint.EventAborted, BlueprintID: "S1"}, "system", nil, false},
		{"error", blueprint.Event{Kind: blueprint.EventError, BlueprintID: "S1", Payload: map[string]any{"message": "cost cap"}}, "stderr", []string{"cost cap"}, false},
		{"answer is echoed by CM", blueprint.Event{Kind: blueprint.EventAnswer, BlueprintID: "S1"}, "", nil, true},
		{"harness lines go through the bridge", blueprint.Event{Kind: blueprint.EventHarness, BlueprintID: "S1"}, "", nil, true},
		{"CM ended the session itself", blueprint.Event{Kind: blueprint.EventEnded, BlueprintID: "S1"}, "", nil, true},
		{"reopened", blueprint.Event{Kind: blueprint.EventReopened, BlueprintID: "S1", Payload: map[string]any{"state": "idle"}}, "system", []string{"idle"}, false},
		{"design awaited", blueprint.Event{Kind: blueprint.EventAwaitInput, BlueprintID: "S1", Payload: map[string]any{"stage": "design", "prompt": "Paste the design."}}, "text", []string{"Paste the design."}, false},
		{"awaited without a prompt", blueprint.Event{Kind: blueprint.EventAwaitInput, BlueprintID: "S1", Payload: map[string]any{"stage": "design", "prompt": ""}}, "text", []string{"design"}, false},
		{"pasted design", blueprint.Event{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{"stage": "design", "body": "the long design", "source": "human", "version": 1}}, "system", []string{"design"}, false},
		{"note that re-runs a stage", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "go on", "retries": "grounding"}}, "system", []string{"grounding"}, false},
		{"grounded", blueprint.Event{Kind: blueprint.EventGrounded, BlueprintID: "S1", Payload: map[string]any{"stage": "grounding", "sources": []any{
			map[string]any{"tag": "S1", "project": "alpha", "identity": "https://example.com/alpha.git", "sha": "0123456789abcdef0123"},
			map[string]any{"tag": "S2", "project": "alpha", "identity": "https://example.com/docs.git", "sha": "fedcba9876543210fedc"},
		}}}, "system", []string{"alpha at 0123456789ab [S1]", "fedcba987654 [S2]"}, false},
		{"report ready", blueprint.Event{Kind: blueprint.EventReport, BlueprintID: "S1", Payload: map[string]any{"body": "# Ready", "ready": true, "rounds": 1}}, "text", nil, false},
		{"report not ready", blueprint.Event{Kind: blueprint.EventReport, BlueprintID: "S1", Payload: map[string]any{
			"body": "# Cards", "ready": false, "rounds": 3, "findings": []string{"D1: two deliverables"},
		}}, "text", []string{"D1: two deliverables"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := logEntryFor(tc.event)
			if tc.skipped {
				assert.False(t, ok)

				return
			}

			require.True(t, ok)
			assert.Equal(t, "S1", entry.SessionID)
			assert.Equal(t, tc.wantType, entry.Type)
			assert.NotEmpty(t, entry.Content)

			for _, c := range tc.carries {
				assert.Contains(t, entry.Content, c)
			}

			assert.False(t, entry.Timestamp.IsZero())
		})
	}
}

func TestLogEntryFor_RefusedNeedShowsOnlyTheMessage(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventError, BlueprintID: "S1", Payload: map[string]any{
		"stage": "intake", "message": "cost cap 1.00 USD would be breached (spent 1.00)",
		"message_id": "m1", "need": "containerise this project",
	}})
	require.True(t, ok)
	assert.Equal(t, "cost cap 1.00 USD would be breached (spent 1.00)", entry.Content, "the need stays in the audit log, not the panel")
}

func TestLogEntryFor_PastedDesignIsNotEchoed(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{
		"stage": "design", "body": "the long design", "source": "human", "version": 1,
	}})
	require.True(t, ok)
	assert.NotContains(t, entry.Content, "the long design", "CM already shows the message the design came from")
}

func TestLogEntryFor_GroundedShowsNoRepositoryURL(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventGrounded, BlueprintID: "S1", Payload: map[string]any{
		"sources": []map[string]any{{"tag": "S1", "project": "alpha", "identity": "https://example.com/alpha.git", "sha": "abc"}},
	}})
	require.True(t, ok)
	assert.Equal(t, "Checked out alpha at abc [S1].", entry.Content)
}

func TestLogEntryFor_ReportIsTheVerdictNotTheBody(t *testing.T) {
	report := func(ready bool, findings ...string) protocol.LogEntry {
		t.Helper()

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventReport, BlueprintID: "S1", Payload: map[string]any{
			"stage": "readiness", "body": "the final artifact body", "ready": ready, "rounds": 2, "findings": findings,
		}})
		require.True(t, ok)

		return entry
	}

	ready := report(true)
	notReady := report(false, "D1: two deliverables", "D2: not judged")

	for _, entry := range []protocol.LogEntry{ready, notReady} {
		assert.Equal(t, "text", entry.Type)
		assert.NotEmpty(t, entry.Content)
		assert.NotContains(t, entry.Content, "the final artifact body", "the artifact entry already showed the body")
	}

	assert.Contains(t, notReady.Content, "D1: two deliverables")
	assert.Contains(t, notReady.Content, "D2: not judged")
	assert.NotEqual(t, ready.Content, report(false).Content, "ready and not ready read differently")
}
