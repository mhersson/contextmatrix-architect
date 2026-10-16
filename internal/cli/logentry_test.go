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
		{"exported", blueprint.Event{Kind: blueprint.EventExported, BlueprintID: "S1", Payload: map[string]any{"path": "/state/exports/S1/t", "files": 7}}, "system", []string{"/state/exports/S1/t", "7 files"}, false},
		{"error", blueprint.Event{Kind: blueprint.EventError, BlueprintID: "S1", Payload: map[string]any{"message": "cost cap"}}, "stderr", []string{"cost cap"}, false},
		{"answer is echoed by CM", blueprint.Event{Kind: blueprint.EventAnswer, BlueprintID: "S1"}, "", nil, true},
		{"harness lines go through the bridge", blueprint.Event{Kind: blueprint.EventHarness, BlueprintID: "S1"}, "", nil, true},
		{"CM ended the session itself", blueprint.Event{Kind: blueprint.EventEnded, BlueprintID: "S1"}, "", nil, true},
		{"the watch's state is internal", blueprint.Event{Kind: blueprint.EventWatchState, BlueprintID: "S1", Payload: map[string]any{"cards": []any{}}}, "", nil, true},
		{"reopened", blueprint.Event{Kind: blueprint.EventReopened, BlueprintID: "S1", Payload: map[string]any{"state": "idle"}}, "system", []string{"idle"}, false},
		{"design awaited", blueprint.Event{Kind: blueprint.EventAwaitInput, BlueprintID: "S1", Payload: map[string]any{"stage": "design", "prompt": "Paste the design."}}, "text", []string{"Paste the design."}, false},
		{"awaited without a prompt", blueprint.Event{Kind: blueprint.EventAwaitInput, BlueprintID: "S1", Payload: map[string]any{"stage": "design", "prompt": ""}}, "text", []string{"design"}, false},
		{"pasted design", blueprint.Event{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{"stage": "design", "body": "the long design", "source": "human", "version": 1}}, "system", []string{"design"}, false},
		{"note that re-runs a stage", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "go on", "retries": "grounding"}}, "system", []string{"grounding"}, false},
		{"grounded", blueprint.Event{Kind: blueprint.EventGrounded, BlueprintID: "S1", Payload: map[string]any{"stage": "grounding", "sources": []any{
			map[string]any{"tag": "S1", "project": "alpha", "identity": "https://example.com/alpha.git", "sha": "0123456789abcdef0123"},
			map[string]any{"tag": "S2", "project": "alpha", "identity": "https://example.com/docs.git", "sha": "fedcba9876543210fedc"},
		}}}, "system", []string{"alpha at 0123456789ab [S1]", "fedcba987654 [S2]"}, false},
		{"grounded empty repository", blueprint.Event{Kind: blueprint.EventGrounded, BlueprintID: "S1", Payload: map[string]any{"stage": "grounding", "sources": []any{
			map[string]any{"tag": "S1", "project": "alpha", "sha": ""},
		}}}, "system", []string{"alpha, an empty repository [S1]"}, false},
		{"report ready", blueprint.Event{Kind: blueprint.EventReport, BlueprintID: "S1", Payload: map[string]any{"body": "# Ready", "ready": true, "rounds": 1}}, "text", nil, false},
		{"report not ready", blueprint.Event{Kind: blueprint.EventReport, BlueprintID: "S1", Payload: map[string]any{
			"body": "# Cards", "ready": false, "rounds": 3, "findings": []string{"D1: two deliverables"},
		}}, "text", []string{"D1: two deliverables"}, false},
		{"card created", blueprint.Event{Kind: blueprint.EventCardCreated, BlueprintID: "S1", Payload: map[string]any{"card": "ALPHA-001", "draft": "D1", "project": "alpha", "title": "Add the parser", "warnings": []string{"w1"}}}, "system", []string{"ALPHA-001", "w1"}, false},
		{"playbook created", blueprint.Event{Kind: blueprint.EventPlaybookCreated, BlueprintID: "S1", Payload: map[string]any{"playbook": "ship-the-parser", "entries": 3}}, "system", []string{"ship-the-parser"}, false},
		{"emitted", blueprint.Event{Kind: blueprint.EventEmitted, BlueprintID: "S1", Payload: map[string]any{"cards": []any{map[string]any{"card": "ALPHA-001"}}, "playbook": "", "manual": []any{"Check the dashboard"}}}, "text", []string{"ALPHA-001", "Check the dashboard"}, false},
		{"run started", blueprint.Event{Kind: blueprint.EventRunStarted, BlueprintID: "S1", Payload: map[string]any{"kind": "playbook", "target": "ship-the-parser"}}, "system", []string{"ship-the-parser"}, false},
		{"watch", blueprint.Event{Kind: blueprint.EventWatch, BlueprintID: "S1", Payload: map[string]any{"line": "ALPHA-001 (D1) Add: todo"}}, "system", []string{"ALPHA-001 (D1) Add: todo"}, false},
		{"watch ended", blueprint.Event{Kind: blueprint.EventWatchEnded, BlueprintID: "S1", Payload: map[string]any{
			"playbook": "p",
			"repos":    []any{map[string]any{"project": "alpha", "compare_url": "https://x/compare"}},
			"cards":    []any{map[string]any{"card": "ALPHA-001", "state": "done", "pr": "https://x/pr/1"}},
		}}, "text", []string{"https://x/compare", "\n\nCards:\n- ALPHA-001 is done: https://x/pr/1"}, false},
		{"signoff", blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: map[string]any{
			"cards":    []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing", "type": "feature", "priority": "medium"}},
			"order":    []any{"D1"},
			"manual":   []any{},
			"title":    "",
			"blocking": []any{"project alpha declares no repository"}, "warnings": []any{"moved"}, "auto_stopped": false, "assumptions": "- The parser stays in Go.",
		}}, "text", []string{"D1", "Add the thing", "declares no repository", "moved", "approve", "The parser stays in Go."}, false},
		{"signoff marks the instruction card", blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: map[string]any{
			"cards":    []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing", "type": "feature", "priority": "medium", "writes": "AGENTS.md"}},
			"order":    []any{"D1"},
			"manual":   []any{},
			"blocking": []any{}, "warnings": []any{},
		}}, "text", []string{"D1 alpha: Add the thing (feature, medium; also writes AGENTS.md)"}, false},
		{"emitted with a step", blueprint.Event{Kind: blueprint.EventEmitted, BlueprintID: "S1", Payload: map[string]any{
			"cards": []any{map[string]any{"card": "ALPHA-001"}}, "playbook": "",
			"steps": []any{"set verify command `go test ./...` on project alpha in ContextMatrix"},
		}}, "text", []string{"ALPHA-001", "set verify command `go test ./...` on project alpha in ContextMatrix"}, false},
		{"decided by the mode", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{"decision": "approve", "responder": "auto"}}, "system", []string{"needs a human first"}, false},
		{"decided reject", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{"decision": "reject", "responder": "human", "feedback": "split it"}}, "system", []string{"Rejected"}, false},
		{"note with a hint", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "hm", "hint": "Reply approve or reject."}}, "system", []string{"Reply approve or reject."}, false},
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

func TestLogEntryFor_SignoffSaysWhatPreflightFound(t *testing.T) {
	cases := []struct {
		name      string
		preflight map[string]any
		carries   []string
		lacks     []string
	}{
		{
			name: "clean", preflight: map[string]any{"blocking": []any{}, "warnings": []any{}},
			carries: []string{"Preflight found nothing for a human."}, lacks: []string{"overrides the warnings"},
		},
		{
			name: "warnings", preflight: map[string]any{"blocking": []any{}, "warnings": []any{"moved"}},
			carries: []string{"moved", "(this also overrides the warnings)"}, lacks: []string{"found nothing"},
		},
		{
			name: "blocking", preflight: map[string]any{"blocking": []any{"project alpha declares no repository"}, "warnings": []any{}},
			carries: []string{"declares no repository"}, lacks: []string{"found nothing", "overrides the warnings"},
		},
		{
			name: "steps", preflight: map[string]any{"blocking": []any{}, "warnings": []any{}, "steps": []any{"set verify command `go test ./...` on project alpha in ContextMatrix"}},
			carries: []string{"Manual steps for you before the cards run", "set verify command `go test ./...` on project alpha in ContextMatrix"},
			lacks:   []string{"found nothing", "overrides the warnings"},
		},
		{
			name: "not run", preflight: map[string]any{"blocking": []any{}, "warnings": []any{}, "preflight_error": "connect: refused"},
			carries: []string{"connect: refused"}, lacks: []string{"found nothing", "overrides the warnings"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"cards": []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing", "type": "feature", "priority": "medium"}},
				"order": []any{"D1"}, "manual": []any{}, "title": "", "auto_stopped": false, "assumptions": "",
			}
			for k, v := range tc.preflight {
				payload[k] = v
			}

			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: payload})
			require.True(t, ok)

			for _, s := range tc.carries {
				assert.Contains(t, entry.Content, s)
			}

			for _, s := range tc.lacks {
				assert.NotContains(t, entry.Content, s)
			}

			assert.Contains(t, entry.Content, "`reject <feedback>`")
		})
	}
}

func TestLogEntryFor_ARunAlreadyThereIsRecordedNotStarted(t *testing.T) {
	cases := []struct {
		name     string
		payload  map[string]any
		want     string
		wantSays string
	}{
		{
			name:    "a card",
			payload: map[string]any{"kind": "card", "target": "ALPHA-001", "existing": true},
			want:    "ALPHA-001 is already running in ContextMatrix; the architect recorded it.",
		},
		{
			name:    "a playbook",
			payload: map[string]any{"kind": "playbook", "target": "ship-the-parser", "existing": true},
			want:    "Playbook ship-the-parser already runs in ContextMatrix; the architect recorded it.",
		},
		{
			name:     "a card the architect started",
			payload:  map[string]any{"kind": "card", "target": "ALPHA-001", "existing": false},
			wantSays: "Started ALPHA-001",
		},
		{
			name:     "a playbook the architect started",
			payload:  map[string]any{"kind": "playbook", "target": "ship-the-parser", "existing": false},
			wantSays: "Started playbook ship-the-parser",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRunStarted, BlueprintID: "S1", Payload: tc.payload})
			require.True(t, ok)
			assert.Equal(t, "system", entry.Type)

			if tc.want != "" {
				assert.Equal(t, tc.want, entry.Content)

				return
			}

			assert.Contains(t, entry.Content, tc.wantSays)
		})
	}
}
