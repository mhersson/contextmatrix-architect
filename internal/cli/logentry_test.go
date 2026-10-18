package cli

import (
	"encoding/json"
	"strings"
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
		{"an automatic sign-off's record is for the sheet", blueprint.Event{Kind: blueprint.EventSignOffAuto, BlueprintID: "S1", Payload: map[string]any{"version": 1}}, "", nil, true},
		{"gate reply", blueprint.Event{Kind: blueprint.EventPolicy, BlueprintID: "S1", Payload: map[string]any{
			"stage": "intake", "policy": "auto", "open": "round",
		}}, "system", []string{"intake", "auto"}, false},
		{"refused gate reply", blueprint.Event{Kind: blueprint.EventPolicy, BlueprintID: "S1", Payload: map[string]any{
			"stage": "nosuch", "policy": "auto", "refused": "There is no stage nosuch.",
		}}, "system", []string{"There is no stage nosuch.", "gate <stage> human"}, false},
		{"gate plan", blueprint.Event{Kind: blueprint.EventGatePlan, BlueprintID: "S1", Payload: map[string]any{
			"stages": []any{
				map[string]any{"stage": "intake", "policy": "human", "gate": false},
				map[string]any{"stage": "shape", "policy": "auto", "gate": true},
			},
			"signoff": "human",
		}}, "text", []string{"intake: human", "shape: auto", "gate <stage> human", "gate <stage> auto"}, false},
		{"setup gaps", blueprint.Event{Kind: blueprint.EventSetupGaps, BlueprintID: "S1", Payload: map[string]any{"projects": []any{
			map[string]any{"project": "alpha", "has_verify": false, "has_instruction_file": false, "empty": true},
			map[string]any{"project": "beta", "has_verify": true, "has_instruction_file": false, "empty": false},
		}}}, "text", []string{
			"alpha declares no verify command in ContextMatrix",
			"alpha has no AGENTS.md or CLAUDE.md",
			"alpha is an empty repository",
			"beta has no AGENTS.md or CLAUDE.md",
		}, false},
		{"created", blueprint.Event{Kind: blueprint.EventCreated, BlueprintID: "S1", Payload: map[string]any{"projects": []string{"alpha", "beta"}, "mode": "yolo"}}, "system", []string{"alpha", "beta", "yolo"}, false},
		{"stage started", blueprint.Event{Kind: blueprint.EventStageStarted, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "persona": "product_owner", "pack": "default", "pack_version": "1"}}, "system", []string{"intake"}, false},
		{"artifact", blueprint.Event{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "version": 1, "body": "# Need\n\nk8s", "persona": "product_owner"}}, "text", []string{"intake rev 1"}, false},
		{"stage ended", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "status": "done", "cost_usd": 0.25}}, "system", []string{"intake", "done", "0.25"}, false},
		{"stage ended priced", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "status": "done", "cost_usd": 0.2, "priced": true}}, "system", []string{"0.20"}, false},
		{"stage ended unpriced", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "status": "done", "cost_usd": 0.2, "priced": false}}, "system", []string{"0.20"}, false},
		{"note", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "use helm"}}, "system", nil, false},
		{"aborted", blueprint.Event{Kind: blueprint.EventAborted, BlueprintID: "S1"}, "system", nil, false},
		{"exported", blueprint.Event{Kind: blueprint.EventExported, BlueprintID: "S1", Payload: map[string]any{"path": "/state/exports/S1/t", "files": 7}}, "system", []string{"/state/exports/S1/t", "7 files"}, false},
		{"error", blueprint.Event{Kind: blueprint.EventError, BlueprintID: "S1", Payload: map[string]any{"message": "cost cap"}}, "stderr", []string{"cost cap"}, false},
		{"answer is echoed by CM", blueprint.Event{Kind: blueprint.EventAnswer, BlueprintID: "S1", Payload: map[string]any{"text": "helm", "responder": "human"}}, "", nil, true},
		{"an auto answer", blueprint.Event{Kind: blueprint.EventAnswer, BlueprintID: "S1", Payload: map[string]any{
			"number": 3, "text": "helm", "responder": "auto", "reasoning": "a chart exists [S1]", "grounded": true,
		}}, "text", []string{"Q3", "helm", "a chart exists [S1]"}, false},
		{"harness lines go through the bridge", blueprint.Event{Kind: blueprint.EventHarness, BlueprintID: "S1"}, "", nil, true},
		{"CM ended the session itself", blueprint.Event{Kind: blueprint.EventEnded, BlueprintID: "S1"}, "", nil, true},
		{"the watch's state is internal", blueprint.Event{Kind: blueprint.EventWatchState, BlueprintID: "S1", Payload: map[string]any{"cards": []any{}}}, "", nil, true},
		{"reopened", blueprint.Event{Kind: blueprint.EventReopened, BlueprintID: "S1", Payload: map[string]any{"state": "idle"}}, "system", []string{"idle"}, false},
		{"a stopped emission awaited", blueprint.Event{Kind: blueprint.EventAwaitInput, BlueprintID: "S1", Payload: map[string]any{"stage": "signoff", "prompt": "Emission is stopped; reply approve to resume it."}}, "text", []string{"reply approve to resume it"}, false},
		{"awaited without a prompt", blueprint.Event{Kind: blueprint.EventAwaitInput, BlueprintID: "S1", Payload: map[string]any{"stage": "signoff", "prompt": ""}}, "text", []string{"signoff"}, false},
		{"note that re-runs a stage", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "go on", "retries": "grounding"}}, "system", []string{"grounding"}, false},
		{"research off", blueprint.Event{Kind: blueprint.EventResearchOff, BlueprintID: "S1", Payload: map[string]any{
			"stage": "intake", "reason": "ContextMatrix has research off",
		}}, "system", []string{"Research is off", "ContextMatrix has research off"}, false},
		{"a page read", blueprint.Event{Kind: blueprint.EventResearch, BlueprintID: "S1", Payload: map[string]any{
			"stage": "intake", "tool": "web_fetch", "url": "https://x.example/a", "final_url": "https://x.example/b", "tag": "S4", "title": "Backups",
		}}, "system", []string{"Backups", "https://x.example/b", "[S4]"}, false},
		{"a refused fetch", blueprint.Event{Kind: blueprint.EventResearch, BlueprintID: "S1", Payload: map[string]any{
			"stage": "intake", "tool": "web_fetch", "url": "https://x.example/a", "refused": "address",
		}}, "", nil, true},
		{"a failed fetch", blueprint.Event{Kind: blueprint.EventResearch, BlueprintID: "S1", Payload: map[string]any{
			"stage": "intake", "tool": "web_fetch", "url": "https://x.example/a", "error": "timeout",
		}}, "", nil, true},
		{"a search is the tool call's own entry", blueprint.Event{Kind: blueprint.EventResearch, BlueprintID: "S1", Payload: map[string]any{
			"stage": "intake", "tool": "web_search", "query": "kustomize overlays", "results": 8,
		}}, "", nil, true},
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
		{"watch ended with the cost", blueprint.Event{Kind: blueprint.EventWatchEnded, BlueprintID: "S1", Payload: map[string]any{
			"playbook": "", "cards": []any{map[string]any{"card": "ALPHA-001", "state": "done", "pr": "https://x/pr/1"}},
			"cost_usd": 0.42, "priced": true,
		}}, "text", []string{"- ALPHA-001 is done: https://x/pr/1", "The architect's runs cost 0.42 USD.", "Reply export for the bundle, or end the session."}, false},
		{"watch ended with a reported cost", blueprint.Event{Kind: blueprint.EventWatchEnded, BlueprintID: "S1", Payload: map[string]any{
			"playbook": "", "cards": []any{}, "cost_usd": 0.42, "priced": false,
		}}, "text", []string{"0.42 USD, as the endpoint reported it", "Reply export for the bundle, or end the session."}, false},
		{"signoff", blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: map[string]any{
			"cards":    []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing", "type": "feature", "priority": "medium"}},
			"order":    []any{"D1"},
			"manual":   []any{},
			"title":    "",
			"blocking": []any{"project alpha declares no repository"}, "warnings": []any{"moved"}, "auto_stopped": false, "assumptions": "- The parser stays in Go.",
		}}, "text", []string{"D1", "Add the thing", "declares no repository", "moved", "approve", "The parser stays in Go."}, false},
		{"signoff with assumptions as data", blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: map[string]any{
			"cards": []any{}, "order": []any{}, "manual": []any{}, "blocking": []any{}, "warnings": []any{},
			"assumptions": []any{
				map[string]any{"id": "A2", "text": "The API keeps its routes.", "status": "unverified", "evidence": ""},
				map[string]any{"id": "A1", "text": "The parser stays in Go.", "status": "verified", "evidence": "[S1] x.go"},
			},
		}}, "text", []string{"A2 (unverified) The API keeps its routes.", "A1 (verified: [S1] x.go) The parser stays in Go."}, false},
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
		{"gate", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 2, "actions": []any{
				map[string]any{"label": "Approve", "reply": "approve", "feedback": false},
				map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
			},
		}}, "text", []string{"shape", "`approve`", "`reject <feedback>`"}, false},
		{"decided at a stage's gate", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "decision": "approve", "responder": "human",
		}}, "system", []string{"shape"}, false},
		{"rejected at a stage's gate", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "decision": "reject", "responder": "human", "feedback": "smaller",
		}}, "system", []string{"shape"}, false},
		{"gate reply at an open gate", blueprint.Event{Kind: blueprint.EventPolicy, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "policy": "auto", "open": "gate",
		}}, "system", []string{"approve or reject"}, false},
		{"note while a stage runs", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{
			"text": "keep the API stable", "stage": "intake", "during_run": true,
		}}, "system", []string{"Noted"}, false},
		{"note with a hint", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "hm", "hint": "Reply approve or reject."}}, "system", []string{"Reply approve or reject."}, false},
		{"a review started", blueprint.Event{Kind: blueprint.EventStageStarted, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "kind": "reviewer", "persona": "reviewer",
		}}, "system", []string{"Review of shape"}, false},
		{"a review ended", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "kind": "reviewer", "status": "done", "cost_usd": 0.02,
		}}, "system", []string{"Review of shape", "0.02"}, false},
		{"a verdict", blueprint.Event{Kind: blueprint.EventReview, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 2, "score": 3, "threshold": 4, "findings": []any{"name the owner"}, "round": 1,
		}}, "system", []string{"shape", "3", "4", "name the owner"}, false},
		{"a gate with a verdict", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 7, "actions": []any{map[string]any{"label": "Approve", "reply": "approve", "feedback": false}},
			"review": map[string]any{"score": 2, "threshold": 4, "findings": []any{"split it"}, "round": 3, "passed": false},
		}}, "text", []string{"2", "4"}, false},
		{"a gate whose checks accepted problems", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 1, "actions": []any{},
			"review":   map[string]any{"score": 4, "threshold": 4, "findings": []any{"name the owner"}, "round": 1, "passed": true},
			"problems": []any{"Q1: unused_answer: the question was answered, but no decision rests on it"},
		}}, "text", []string{"- Q1: unused_answer: the question was answered, but no decision rests on it\n\n"}, false},
		{"a note that runs a review again", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{
			"text": "go on", "retries": "shape", "critique": true,
		}}, "system", []string{"shape"}, false},
		{"a fold", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "slicing", "decision": "fold", "feedback": "solution_design", "responder": "auto",
		}}, "system", []string{"slicing", "solution_design"}, false},
		{"a design superseded by a rejected slicing", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "decision": "superseded", "feedback": "slicing", "responder": "human",
		}}, "system", []string{"solution_design", "slicing"}, false},
		{"a folded gate", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 1, "folded": []any{map[string]any{"stage": "slicing", "version": 1}},
			"actions": []any{map[string]any{"label": "Reject slicing", "reply": "reject slicing", "feedback": true}},
		}}, "text", []string{"slicing", "`reject slicing <feedback>`"}, false},
		{"a folded gate names what it also judges", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 1, "folded": []any{map[string]any{"stage": "slicing", "version": 3}}, "actions": []any{},
		}}, "text", []string{"slicing", "3"}, false},
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

func TestLogEntryFor_ANoteAtAnEndedPipelineIsToldApart(t *testing.T) {
	note := func(ended bool) string {
		t.Helper()

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{
			"text": "go on", "stage": "", "during_run": false, "ended": ended,
		}})
		require.True(t, ok)

		return entry.Content
	}

	assert.NotEqual(t, note(false), note(true), "no stage starts after an ended pipeline, so the note cannot promise one")
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

func TestLogEntryFor_Prompts(t *testing.T) {
	tests := []struct {
		name     string
		event    blueprint.Event
		wantType string
		want     *protocol.LogPrompt
	}{
		{"a stage's gate", blueprint.Event{Kind: blueprint.EventGate, Payload: map[string]any{
			"stage": "shape", "version": 2, "actions": []any{
				map[string]any{"label": "Approve", "reply": "approve", "feedback": false},
				map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
			},
		}}, "text", &protocol.LogPrompt{ID: "gate:shape:v2", Kind: "gate", Actions: []protocol.LogAction{
			{Label: "Approve", Reply: "approve"}, {Label: "Reject", Reply: "reject", Feedback: true},
		}}},
		{"the sign-off", blueprint.Event{Kind: blueprint.EventSignOff, Payload: map[string]any{
			"version": 4, "cards": []any{}, "order": []any{}, "actions": []any{
				map[string]any{"label": "Approve and override warnings", "reply": "approve", "feedback": false},
				map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
			},
		}}, "text", &protocol.LogPrompt{ID: "gate:signoff:v4", Kind: "gate", Actions: []protocol.LogAction{
			{Label: "Approve and override warnings", Reply: "approve"}, {Label: "Reject", Reply: "reject", Feedback: true},
		}}},
		{"a sign-off stored before replies travelled with it", blueprint.Event{Kind: blueprint.EventSignOff, Payload: map[string]any{
			"version": 4, "cards": []any{}, "order": []any{},
		}}, "text", nil},
		{"a stopped emission", blueprint.Event{Kind: blueprint.EventError, Payload: map[string]any{
			"stage": "emission", "message": "Emission stopped.", "resume": "emission", "version": 3,
		}}, "text", &protocol.LogPrompt{ID: "emission:v3", Kind: "gate", Actions: []protocol.LogAction{{Label: "Resume emission", Reply: "approve"}}}},
		{"a refused run start", blueprint.Event{Kind: blueprint.EventError, Payload: map[string]any{
			"stage": "run", "message": "The run did not start.", "resume": "run", "version": 3,
		}}, "text", &protocol.LogPrompt{ID: "emission:v3", Kind: "gate", Actions: []protocol.LogAction{{Label: "Retry the run start", Reply: "approve"}}}},
		{"a run held for a first commit", blueprint.Event{Kind: blueprint.EventAwaitInput, Payload: map[string]any{
			"stage": "signoff", "prompt": "https://example.com/alpha.git has no commits, so the run was not started.", "resume": "held", "version": 3,
		}}, "text", &protocol.LogPrompt{ID: "emission:v3", Kind: "gate", Actions: []protocol.LogAction{{Label: "Start the run", Reply: "approve"}}}},
		{"a stopped emission shown on reopen", blueprint.Event{Kind: blueprint.EventAwaitInput, Payload: map[string]any{
			"stage": "signoff", "prompt": "Emission is stopped; reply approve to resume it.", "resume": "emission", "version": 3,
		}}, "text", &protocol.LogPrompt{ID: "emission:v3", Kind: "gate", Actions: []protocol.LogAction{{Label: "Resume emission", Reply: "approve"}}}},
		{"any other error", blueprint.Event{Kind: blueprint.EventError, Payload: map[string]any{"message": "cost cap"}}, "stderr", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.event.BlueprintID = "S1"

			entry, ok := logEntryFor(tt.event)
			require.True(t, ok)
			assert.Equal(t, tt.wantType, entry.Type)

			// Text has its own tests; this compares the rest of the prompt.
			got := entry.Prompt
			if got != nil {
				bare := *got
				bare.Text = ""
				got = &bare
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLogEntryFor_AGatesTextLeavesOutItsReplies(t *testing.T) {
	actions := []any{
		map[string]any{"label": "Approve", "reply": "approve", "feedback": false},
		map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
	}

	tests := []struct {
		name      string
		kind      string
		wants     map[string]any
		notInText []string
	}{
		{"a plain stage gate", blueprint.EventGate, map[string]any{"stage": "shape", "version": 2}, nil},
		{"a gate with a failing review and findings", blueprint.EventGate, map[string]any{
			"stage": "shape", "version": 2,
			"review": map[string]any{"passed": false, "round": 2, "score": 2, "threshold": 4, "findings": []any{"thin", "vague"}},
		}, []string{"thin", "vague"}},
		{"a gate with problems and a folded artifact with its own", blueprint.EventGate, map[string]any{
			"stage": "solution_design", "version": 3, "problems": []any{"p1"},
			"folded": []any{map[string]any{"stage": "slicing", "version": 1, "body": "# Slices", "problems": []any{"p2"}}},
		}, nil},
		{"a sign-off with warnings and steps", blueprint.EventSignOff, map[string]any{
			"version": 2, "title": "T", "warnings": []any{"w"}, "steps": []any{"s"},
			"cards": []any{
				map[string]any{"id": "D1", "project": "p", "title": "t", "type": "feature", "priority": "high", "depends_on": []any{"D0"}},
				map[string]any{"id": "D0", "project": "p", "title": "u", "type": "task", "priority": "low"},
			},
			"order": []any{"D0", "D1"},
		}, nil},
		{"a sign-off where preflight found nothing", blueprint.EventSignOff, map[string]any{
			"version": 1, "cards": []any{}, "order": []any{},
		}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.wants["actions"] = actions

			entry, ok := logEntryFor(blueprint.Event{Kind: tt.kind, BlueprintID: "S1", Payload: tt.wants})
			require.True(t, ok)
			require.NotNil(t, entry.Prompt)

			text := entry.Prompt.Text
			assert.NotEmpty(t, text)
			assert.Equal(t, "gate", entry.Prompt.Kind)
			require.True(t, strings.HasPrefix(entry.Content, text), "the fallback starts with the prompt's text")

			for _, f := range tt.notInText {
				assert.NotContains(t, text, f, "the review's findings are in the review entry, not the gate")
			}

			for _, a := range entry.Prompt.Actions {
				assert.NotContains(t, text, a.Reply, "a reply is a button, not text")
				assert.Contains(t, entry.Content[len(text):], a.Reply, "the fallback keeps the reply line")
			}
		})
	}
}

func TestLogEntryFor_AResumePromptsTextIsItsStop(t *testing.T) {
	tests := []struct {
		name        string
		event       blueprint.Event
		wantText    string
		wantContent string
	}{
		{"a stopped emission", blueprint.Event{Kind: blueprint.EventError, Payload: map[string]any{
			"stage": "emission", "message": "Stopped; then go on.", "text": "Stopped.", "resume": "emission", "version": 3,
		}}, "Stopped.", "Stopped; then go on."},
		{"a held run", blueprint.Event{Kind: blueprint.EventAwaitInput, Payload: map[string]any{
			"stage": "signoff", "prompt": "Held; then go on.", "text": "Held.", "resume": "held", "version": 3,
		}}, "Held.", "Held; then go on."},
		{"a stop stored before resume events carried a text", blueprint.Event{Kind: blueprint.EventError, Payload: map[string]any{
			"stage": "emission", "message": "Stopped; then go on.", "resume": "emission", "version": 3,
		}}, "", "Stopped; then go on."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.event.BlueprintID = "S1"

			entry, ok := logEntryFor(tt.event)
			require.True(t, ok)
			require.NotNil(t, entry.Prompt)
			assert.Equal(t, tt.wantText, entry.Prompt.Text)
			assert.Equal(t, "gate", entry.Prompt.Kind)
			assert.Equal(t, tt.wantContent, entry.Content)
		})
	}
}

func TestLogEntryFor_ANoteThatRunsAReviewIsNotCalledAStageRun(t *testing.T) {
	stage, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "go on", "retries": "shape"}})
	require.True(t, ok)

	review, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "go on", "retries": "shape", "critique": true}})
	require.True(t, ok)

	assert.NotEqual(t, stage.Content, review.Content)
}

func TestLogEntryFor_AGateShowsItsReviewFindingsOnce(t *testing.T) {
	const finding = "name the response fields"

	review, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventReview, BlueprintID: "S1", Payload: map[string]any{
		"stage": "solution_design", "version": 1, "score": 3, "threshold": 4, "findings": []any{finding}, "round": 3,
	}})
	require.True(t, ok)

	gate, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
		"stage": "solution_design", "version": 1, "actions": []any{},
		"review": map[string]any{"score": 3, "threshold": 4, "findings": []any{finding}, "round": 3, "passed": false},
	}})
	require.True(t, ok)

	assert.Equal(t, 1, strings.Count(review.Content+"\n"+gate.Content, finding), "the findings are shown once, in the review above the gate")
	assert.Contains(t, review.Content, finding)
}

func TestLogEntryFor_AFoldedGateShowsTheFoldedSheetAndNeverTheDesignBody(t *testing.T) {
	const designed, sliced = "# Design\n\nPostgres, managed.", "# Slices\n\nSL1 ships the endpoint."

	const designProblem, slicingProblem = "Q1: unused_answer: the question was answered, but no decision rests on it", "SL1: slice_title: a slice needs a title"

	events := []blueprint.Event{
		{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{"stage": "solution_design", "version": 2, "body": designed}},
		{Kind: blueprint.EventReview, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 2, "score": 5, "threshold": 4, "findings": []any{}, "round": 1,
		}},
		{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 2, "problems": []any{designProblem},
			"folded":  []any{map[string]any{"stage": "slicing", "version": 1, "body": sliced, "problems": []any{slicingProblem}}},
			"actions": []any{map[string]any{"label": "Approve", "reply": "approve", "feedback": false}},
		}},
	}

	var (
		panel strings.Builder
		gate  protocol.LogEntry
	)

	for _, e := range events {
		entry, ok := logEntryFor(e)
		require.True(t, ok)

		panel.WriteString(entry.Content + "\n")

		if e.Kind == blueprint.EventGate {
			gate = entry
		}
	}

	assert.Zero(t, strings.Count(panel.String(), designed), "the design body is on the sheet, never in the panel")
	assert.Contains(t, gate.Content, sliced, "the gate shows the folded artifact")
	assert.NotContains(t, gate.Content, designed)
	assert.Contains(t, gate.Content, "- "+designProblem)
	assert.Less(t, strings.Index(gate.Content, sliced), strings.Index(gate.Content, "- "+slicingProblem), "each folded artifact with its problems")
	assert.Equal(t, "gate:solution_design:v2", gate.Prompt.ID, "the prompt is the design gate's")
}

func TestLogEntryFor_ARevisedGateNamesTheVersionTheReviewerScored(t *testing.T) {
	gate := func(review map[string]any) protocol.LogEntry {
		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 7, "actions": []any{map[string]any{"label": "Approve", "reply": "approve", "feedback": false}},
			"review": review,
		}})
		require.True(t, ok)
		require.NotNil(t, entry.Prompt)

		return entry
	}

	base := func() map[string]any {
		return map[string]any{"passed": true, "round": 1, "score": 4, "threshold": 4, "findings": []any{"thin"}}
	}

	plain := gate(base())

	notRevised := base()
	notRevised["revised"], notRevised["judged"] = false, 7
	assert.Equal(t, plain.Content, gate(notRevised).Content, "an unrevised gate reads as one stored before revisions were recorded")

	revised := base()
	revised["revised"], revised["judged"] = true, 6
	got := gate(revised)

	assert.NotEqual(t, plain.Content, got.Content)
	assert.Contains(t, got.Content, "version 6")
	assert.Contains(t, got.Prompt.Text, "version 6", "the prompt's text carries the revision too")
	assert.NotContains(t, got.Content, "thin", "findings stay in the review above")
}

func TestLogEntryFor_SignoffShowsEachAssumptionsSourceInPayloadOrder(t *testing.T) {
	payload := map[string]any{
		"version": 1, "cards": []any{}, "order": []any{}, "actions": []any{map[string]any{"label": "Approve", "reply": "approve", "feedback": false}},
		"assumptions": []any{
			map[string]any{"id": "A1", "stage": "intake", "text": "first text", "status": "unverified"},
			map[string]any{"id": "A2", "stage": "intake", "text": "second text", "status": "contradicted", "evidence": "the log says otherwise"},
			map[string]any{"id": "A3", "stage": "intake", "text": "third text", "status": "assumed", "basis": "Q41", "answer": "use helm"},
			map[string]any{"id": "A4", "stage": "intake", "text": "fourth text", "status": "confirmed", "basis": "Q52", "answer": "keep the routes"},
			map[string]any{"id": "Q63", "stage": "intake", "text": "fifth text", "status": "assumed", "basis": "Q63"},
			map[string]any{"id": "A6", "stage": "intake", "text": "sixth text", "status": "verified", "evidence": "[S7] says so"},
			map[string]any{"id": "A7", "stage": "intake", "text": "seventh text", "status": "verified", "evidence": "[S8] found", "basis": "Q74", "answer": "ship weekly"},
			map[string]any{"id": "A8", "stage": "intake", "text": "eighth text", "status": "assumed", "basis": "Q85"},
		},
	}

	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: payload})
	require.True(t, ok)
	require.NotNil(t, entry.Prompt)

	for _, text := range []string{entry.Content, entry.Prompt.Text} {
		line := func(needle string) string {
			for _, l := range strings.Split(text, "\n") {
				if strings.Contains(l, needle) {
					return l
				}
			}

			t.Fatalf("no line carries %q", needle)

			return ""
		}

		assert.Equal(t, 1, strings.Count(line("third text"), "Q41"))
		assert.Equal(t, 1, strings.Count(line("fourth text"), "Q52"))
		assert.Equal(t, 1, strings.Count(line("fifth text"), "Q63"), "a basis equal to the id is not repeated")
		assert.Equal(t, 1, strings.Count(line("eighth text"), "Q85"), "a payload without the answer")
		assert.Equal(t, 1, strings.Count(line("third text"), "use helm"), "the auto answer it rests on")
		assert.Equal(t, 1, strings.Count(line("fourth text"), "keep the routes"), "the human answer it rests on")
		assert.Contains(t, line("seventh text"), "[S8] found")
		assert.Equal(t, 1, strings.Count(line("seventh text"), "Q74"), "a verified entry names the question it rests on")
		assert.Equal(t, 1, strings.Count(line("seventh text"), "ship weekly"), "and its answer")
		assumed, _, _ := strings.Cut(line("third text"), "third")
		confirmed, _, _ := strings.Cut(line("fourth text"), "fourth")

		assert.NotEqual(t, assumed, confirmed, "assumed and confirmed read differently")
		assert.Contains(t, line("second text"), "the log says otherwise")
		assert.Contains(t, line("sixth text"), "[S7] says so")

		last := -1

		for _, w := range []string{"first text", "second text", "third text", "fourth text", "fifth text", "sixth text", "seventh text", "eighth text"} {
			at := strings.Index(text, w)
			assert.Greater(t, at, last, "entries keep the payload's order")

			last = at
		}
	}
}

func TestLogEntryFor_AnAutoAnswerShowsItsEvidenceOnlyWhenGrounded(t *testing.T) {
	answer := func(grounded bool, evidence string) string {
		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventAnswer, BlueprintID: "S1", Payload: map[string]any{
			"number": 3, "responder": "auto", "text": "Use Postgres", "reasoning": "because", "grounded": grounded, "evidence": evidence,
		}})
		require.True(t, ok)

		return entry.Content
	}

	const evidence = "[S2] names the existing database"

	grounded, ungrounded := answer(true, evidence), answer(false, "")

	assert.Contains(t, grounded, evidence)
	assert.NotContains(t, ungrounded, evidence)
	assert.NotEqual(t, grounded, ungrounded)
}

func TestLogEntryFor_Markers(t *testing.T) {
	ev := func(kind string, p map[string]any) blueprint.Event {
		return blueprint.Event{Kind: kind, BlueprintID: "S1", Payload: p}
	}

	sheet := func(stage string, version int, persona, note string) *protocol.LogMarker {
		return &protocol.LogMarker{Kind: "artifact", Stage: stage, Version: version, Persona: persona, Note: note}
	}

	tests := []struct {
		name     string
		event    blueprint.Event
		wantType string
		want     *protocol.LogMarker
		carries  string
	}{
		{
			"a stage run's start is a stage divider",
			ev(blueprint.EventStageStarted, map[string]any{"stage": "solution_design", "kind": "stage", "persona": "system_designer", "round": 1}),
			"system", &protocol.LogMarker{Kind: "stage", Stage: "solution_design", Persona: "system_designer"}, "solution_design",
		},
		{"a builder round's start is no divider", ev(blueprint.EventStageStarted, map[string]any{"stage": "solution_design", "kind": "stage", "persona": "system_designer", "round": 2}), "system", nil, "solution_design"},
		{"a review's start is no divider", ev(blueprint.EventStageStarted, map[string]any{"stage": "solution_design", "kind": "reviewer", "persona": "senior_developer", "round": 1}), "system", nil, "Review of solution_design"},
		{
			"a first version is drawn by its persona",
			ev(blueprint.EventArtifact, map[string]any{"stage": "solution_design", "version": 1, "body": "# Design", "persona": "system_designer", "round": 1, "revised_for": 0}),
			"text", sheet("solution_design", 1, "system_designer", "Drawn by system designer"), "solution_design rev 1: Drawn by system designer.",
		},
		{
			"a revision says what it was revised for",
			ev(blueprint.EventArtifact, map[string]any{"stage": "solution_design", "version": 2, "body": "# Design", "persona": "system_designer", "round": 2, "revised_for": 3}),
			"text", sheet("solution_design", 2, "system_designer", "Revised for the 3 findings"), "solution_design rev 2",
		},
		{
			"a revision for one finding",
			ev(blueprint.EventArtifact, map[string]any{"stage": "slicing", "version": 2, "body": "# Slices", "persona": "product_owner", "round": 2, "revised_for": 1}),
			"text", sheet("slicing", 2, "product_owner", "Revised for the 1 finding"), "slicing rev 2",
		},
		{
			"a loop stage says its round",
			ev(blueprint.EventArtifact, map[string]any{"stage": "readiness", "version": 2, "body": "# Ready", "persona": "reviewer", "round": 1, "revised_for": 0, "loop_round": 2, "max_rounds": 3}),
			"text", sheet("readiness", 2, "reviewer", "Round 2 of 3"), "readiness rev 2",
		},
		{
			"cards accepted with findings say so",
			ev(blueprint.EventArtifact, map[string]any{"stage": "decomposition", "version": 1, "body": "# Cards", "persona": "senior_developer", "findings": []string{"D1: a"}, "accepted_round": 3}),
			"text", sheet("decomposition", 1, "senior_developer", "Refused by checks, accepted on round 3"), "decomposition rev 1",
		},
		{
			"a review stays a prose row",
			ev(blueprint.EventReview, map[string]any{"stage": "solution_design", "version": 1, "score": 4, "threshold": 4, "findings": []string{"a"}, "round": 1}),
			"system", nil, "4 of 5",
		},
		{
			"a gate decision stays a prose row",
			ev(blueprint.EventDecided, map[string]any{"stage": "solution_design", "decision": "approve", "responder": "human", "version": 2}),
			"system", nil, "Approved solution_design",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := logEntryFor(tt.event)
			require.True(t, ok)
			assert.Equal(t, tt.wantType, entry.Type)
			assert.Equal(t, tt.want, entry.Marker)
			assert.Contains(t, entry.Content, tt.carries, "the fallback line")

			for _, body := range []string{"# Design", "# Slices", "# Ready", "# Cards"} {
				assert.NotContains(t, entry.Content, body, "an artifact's body is no transcript text")
			}
		})
	}
}

func TestLogEntryFor_AReportWithoutARoundNamesNone(t *testing.T) {
	report := func(payload map[string]any) protocol.LogEntry {
		t.Helper()

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventReport, BlueprintID: "S1", Payload: payload})
		require.True(t, ok)

		return entry
	}

	ready := report(map[string]any{"ready": true, "rounds": 0})
	notReady := report(map[string]any{"ready": false, "rounds": 0, "findings": []string{"D1: two deliverables"}})

	assert.NotContains(t, ready.Content, "round")
	assert.NotContains(t, notReady.Content, "round")
	assert.Contains(t, notReady.Content, "D1: two deliverables")

	assert.Contains(t, report(map[string]any{"ready": true, "rounds": 2}).Content, "round 2",
		"the number is shown when a round applies")
}

func TestLogEntry_RoundFrame(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "BP1", Payload: roundPayloadForTest()})
	require.True(t, ok)
	require.NotNil(t, entry.Prompt)
	assert.Equal(t, "round:3", entry.Prompt.ID)
	assert.Equal(t, "round", entry.Prompt.Kind)
	require.NotNil(t, entry.Prompt.Round)
	assert.Equal(t, 7, entry.Prompt.Round.Questions[0].Number)
	assert.Equal(t, "text", entry.Type)
}

func TestLogEntry_ALeftRoundIsNotHeadedAnswered(t *testing.T) {
	render := func(change string) protocol.LogEntry {
		t.Helper()

		p := roundPayloadForTest()
		p["change"] = change

		frame, ok := p["frame"].(map[string]any)
		require.True(t, ok)

		frame["closed"] = true

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "BP1", Payload: p})
		require.True(t, ok)
		require.NotNil(t, entry.Prompt)
		require.NotNil(t, entry.Prompt.Round)

		return entry
	}

	answered, left := render("closed"), render(blueprint.ChangeLeft)

	assert.True(t, left.Prompt.Round.Closed, "the panel collapses a left round")
	assert.Equal(t, "round:3", left.Prompt.ID)
	assert.Contains(t, left.Prompt.Text, "Round 3")
	assert.NotEqual(t, answered.Prompt.Text, left.Prompt.Text, "a left round is not headed as answered")
	assert.Contains(t, left.Content, left.Prompt.Text, "the fallback line carries the same heading")
	assert.NotContains(t, left.Content, answered.Prompt.Text)
}

func TestLogEntry_ACloseInProgressIsNotShown(t *testing.T) {
	p := roundPayloadForTest()
	p["change"] = blueprint.ChangeClosing

	_, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "BP1", Payload: p})
	assert.False(t, ok, "a closing snapshot only reserves numbers; the closed one follows")
}

func TestLogEntry_TheFallbackListsTheAssumptionsTab(t *testing.T) {
	render := func(decisions ...any) string {
		t.Helper()

		p := roundPayloadForTest()

		frame, ok := p["frame"].(map[string]any)
		require.True(t, ok)

		frame["assumptions"] = []any{map[string]any{"id": "A1", "text": "One region."}, map[string]any{"id": "A2", "text": "Postgres stays."}}
		frame["decisions"] = decisions

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "BP1", Payload: p})
		require.True(t, ok)

		return entry.Content
	}

	undecided := render()
	accepted := render(map[string]any{"id": "A1", "accept": true})
	converted := render(map[string]any{"id": "A1", "accept": false})

	for _, text := range []string{"A1. One region.", "A2. Postgres stays."} {
		assert.Contains(t, undecided, text)
	}

	assert.NotEqual(t, undecided, accepted, "a decision shows")
	assert.NotEqual(t, accepted, converted, "accepting and turning into a question read apart")
}

func TestLogEntry_ATabOnlyRoundCountsItsAssumptions(t *testing.T) {
	p := roundPayloadForTest()

	frame, ok := p["frame"].(map[string]any)
	require.True(t, ok)

	frame["questions"] = []any{}
	frame["assumptions"] = []any{map[string]any{"id": "A1", "text": "One region."}, map[string]any{"id": "A2", "text": "Postgres stays."}}

	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "BP1", Payload: p})
	require.True(t, ok)
	require.NotNil(t, entry.Prompt)
	assert.Contains(t, entry.Prompt.Text, "2", "the tab's count")
	assert.NotContains(t, entry.Prompt.Text, "0 question", "no questions are counted")
}

func roundPayloadForTest() map[string]any {
	raw := []byte(`{"round":3,"change":"posted","frame":{"persona":"system designer","stage":"solution_design",
		"questions":[{"number":7,"title":"Deploy","text":"Helm or Kustomize?","options":[{"title":"Helm"}]}]}}`)

	var p map[string]any

	_ = json.Unmarshal(raw, &p)

	return p
}

func TestLogEntry_ASegmentStartIsNoDivider(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventStageStarted, BlueprintID: "BP1", Payload: map[string]any{
		"stage": "intake", "kind": "stage", "round": 1.0, "segment": blueprint.SegmentAnswers, "question_round": 2.0,
	}})
	require.True(t, ok)
	assert.Nil(t, entry.Marker)
}
