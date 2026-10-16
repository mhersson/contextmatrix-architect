package cli

import (
	"testing"

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
