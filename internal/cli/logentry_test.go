package cli

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestLogEntryFor_AnAbortNamesContinueOnlyWhenItCutAStage(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		want  bool
	}{
		{"a stage's run", "solution_design", true},
		{"nothing in flight", "", false},
		{"the sign-off's emission or run start", "signoff", false},
		{"a retrospective's pass", blueprint.RetroStage, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventAborted, BlueprintID: "S1", Payload: map[string]any{
				"stage": tc.stage, "message_id": "m1",
			}})
			require.True(t, ok)
			assert.Equal(t, tc.want, strings.Contains(entry.Content, "`continue`"))
		})
	}
}

func TestLogEntryFor_AnErrorNamesContinueOnlyWhenItStoppedAStageRun(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"a stage failure", map[string]any{"stage": "solution_design", "reason": "error", "message": "stage failed."}, true},
		{"a stage run's own cap", map[string]any{"stage": "solution_design", "reason": "max_cost", "message": "run cost cap reached."}, true},
		{"a reviewer run's own cap", map[string]any{"stage": "solution_design", "reason": "max_cost", "message": "run cost cap reached."}, true},
		{"a bound blueprint cap stop", map[string]any{"stage": "solution_design", "reason": "max_cost", "cap": "blueprint", "message": "cap used up."}, false},
		{"a refusal cap stop", map[string]any{"stage": "solution_design", "cap": "blueprint", "message": "cap used up."}, false},
		{"a refused responder", map[string]any{"stage": "solution_design", "reason": "responder_refused", "message": "refused."}, false},
		{"a mockup responder that failed, whose revision is used", map[string]any{"stage": "mockup", "reason": "responder_failed", "revision": 1, "message": "failed."}, false},
		{"a stage error that gave no reason", map[string]any{"stage": "solution_design", "message": "stopped."}, false},
		{"a stale reply", map[string]any{"reason": "stale_reply", "message": "answers nothing."}, false},
		{"a failure with no stage", map[string]any{"message": "boom."}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventError, BlueprintID: "S1", Payload: tc.payload})
			require.True(t, ok)
			assert.Equal(t, tc.want, strings.Contains(entry.Content, "`continue`"))
		})
	}
}

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
		}}, "system", []string{"Intake", "auto"}, false},
		{"refused gate reply", blueprint.Event{Kind: blueprint.EventPolicy, BlueprintID: "S1", Payload: map[string]any{
			"stage": "nosuch", "policy": "auto", "refused": "There is no stage nosuch.",
		}}, "system", []string{"There is no stage nosuch.", "gate <stage> human"}, false},
		{"a cap raise that resumes a stage", blueprint.Event{Kind: blueprint.EventCap, BlueprintID: "S1", Payload: map[string]any{
			"from": 2.0, "to": 4.0, "resumes": "solution_design",
		}}, "system", []string{"$2.00", "$4.00", "Solution design"}, false},
		{"a mode switch", blueprint.Event{Kind: blueprint.EventMode, BlueprintID: "S1", Payload: map[string]any{
			"from": "default", "to": "auto+emit",
		}}, "system", []string{"Default", "Auto + emit"}, false},
		{"a mode switch that hands a round over", blueprint.Event{Kind: blueprint.EventMode, BlueprintID: "S1", Payload: map[string]any{
			"from": "default", "to": "auto", "hands_over": 3,
		}}, "system", []string{"Default", "Auto", "3"}, false},
		{"a mode switch that decides a gate", blueprint.Event{Kind: blueprint.EventMode, BlueprintID: "S1", Payload: map[string]any{
			"from": "default", "to": "auto", "gate": "shape",
		}}, "system", []string{"Default", "Auto", "Shape"}, false},
		{"a refused mode switch", blueprint.Event{Kind: blueprint.EventMode, BlueprintID: "S1", Payload: map[string]any{
			"from": "yolo", "to": "default", "reason": "emission_started", "refused": "Emission has started, so the mode stays yolo.",
		}}, "system", []string{"Emission has started, so the mode stays yolo."}, false},
		{"a resume that runs a stopped stage", blueprint.Event{Kind: blueprint.EventResume, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "message_id": "m1",
		}}, "system", []string{"Solution design"}, false},
		{"gate plan", blueprint.Event{Kind: blueprint.EventGatePlan, BlueprintID: "S1", Payload: map[string]any{
			"stages": []any{
				map[string]any{"stage": "intake", "policy": "human", "gate": false},
				map[string]any{"stage": "shape", "policy": "auto", "gate": true, "folds_into": "solution_design", "fold_field": "slices", "fold_max": 1},
			},
			"signoff": "human",
		}}, "text", []string{"Intake: human", "Shape: auto", "Solution design", "gate <stage> human", "gate <stage> auto"}, false},
		{"gate plan with a gate of its own policy", blueprint.Event{Kind: blueprint.EventGatePlan, BlueprintID: "S1", Payload: map[string]any{
			"stages": []any{map[string]any{"stage": "intake", "policy": "human", "gate": true, "gate_policy": "auto"}},
		}}, "text", []string{"Intake: human; its gate is auto"}, false},
		{"gate plan with per-slice and shared stages", blueprint.Event{Kind: blueprint.EventGatePlan, BlueprintID: "S1", Payload: map[string]any{
			"stages": []any{
				map[string]any{"stage": "shared_decisions", "policy": "human", "gate": true, "shared": true},
				map[string]any{"stage": "solution_design", "policy": "human", "gate": true, "per_slice": true},
			},
		}}, "text", []string{"Shared decisions: human, with a gate, skipped with one slice", "Solution design: human, with a gate, once per slice"}, false},
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
		{"stage started", blueprint.Event{Kind: blueprint.EventStageStarted, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "persona": "product_owner", "pack": "default", "pack_version": "1"}}, "system", []string{"Intake"}, false},
		{"artifact", blueprint.Event{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "version": 1, "body": "# Need\n\nk8s", "persona": "product_owner"}}, "text", []string{"Intake rev 1"}, false},
		{"stage ended", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{"stage": "intake", "status": "done", "cost_usd": 0.25}}, "system", []string{"Intake", "done", "0.25"}, false},
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
		{"awaited without a prompt", blueprint.Event{Kind: blueprint.EventAwaitInput, BlueprintID: "S1", Payload: map[string]any{"stage": "signoff", "prompt": ""}}, "text", []string{"Signoff"}, false},
		{"note that re-runs a stage", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "go on", "retries": "grounding"}}, "system", []string{"Grounding"}, false},
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
		{"emitted with a notice", blueprint.Event{Kind: blueprint.EventEmitted, BlueprintID: "S1", Payload: map[string]any{
			"cards": []any{map[string]any{"card": "ALPHA-001"}}, "playbook": "",
			"notices": []any{"project alpha declares no verify command, so each card's agent looks for one"},
		}}, "text", []string{"Notices (no step needed before the cards run):", "project alpha declares no verify command, so each card's agent looks for one"}, false},
		{"decided by the mode", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{"decision": "approve", "responder": "auto"}}, "system", []string{"needs a human first"}, false},
		{"decided reject", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{"decision": "reject", "responder": "human", "feedback": "split it"}}, "system", []string{"Rejected"}, false},
		{"gate", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 2, "actions": []any{
				map[string]any{"label": "Approve", "reply": "approve", "feedback": false},
				map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
			},
		}}, "text", []string{"Shape", "`approve`", "`reject <feedback>`"}, false},
		{"decided at a stage's gate", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "decision": "approve", "responder": "human",
		}}, "system", []string{"Shape"}, false},
		{"rejected at a stage's gate", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "decision": "reject", "responder": "human", "feedback": "smaller",
		}}, "system", []string{"Shape"}, false},
		{"gate reply at an open gate", blueprint.Event{Kind: blueprint.EventPolicy, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "policy": "auto", "open": "gate",
		}}, "system", []string{"approve or reject"}, false},
		{"note while a stage runs", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{
			"text": "keep the API stable", "stage": "intake", "during_run": true,
		}}, "system", []string{"Noted"}, false},
		{"note with a hint", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{"text": "hm", "hint": "Reply approve or reject."}}, "system", []string{"Reply approve or reject."}, false},
		{"a retrospective's reply", blueprint.Event{Kind: blueprint.EventRetroReply, BlueprintID: "S1", Payload: map[string]any{
			"reason": "no_struggle", "message": "None of the 3 emitted cards struggled.", "message_id": "m1", "during_run": false,
		}}, "system", []string{"None of the 3 emitted cards struggled."}, false},
		{"a retrospective's findings", blueprint.Event{Kind: blueprint.EventRetroReport, BlueprintID: "S1", Payload: map[string]any{
			"run": 7, "read": 4, "struggled": 2, "cards": []any{
				map[string]any{
					"card": "ALPHA-001", "project": "alpha", "title": "Add the parser", "state": "in_progress", "worker": "parked",
					"review_attempts": 0, "cost_usd": 0.4, "subtasks": 2, "reason": "no key", "why": []any{"parked"},
					"rule": "Where", "evidence": "needs a key", "at_fault": true,
					"lesson": map[string]any{"id": 3, "text": "Name where the key comes from", "scope": "project", "project": "alpha"},
				},
				map[string]any{
					"card": "ALPHA-002", "project": "alpha", "title": "Wire the parser", "state": "done", "worker": "",
					"review_attempts": 2, "cost_usd": 0.1, "subtasks": 1, "reason": "", "why": []any{"review_attempts"},
					"rule": "Acceptance criteria", "evidence": "go test passes", "at_fault": false,
				},
			},
		}}, "text", []string{"ALPHA-001", "ALPHA-002", "needs a key", "go test passes", "L3"}, false},
		{"a lesson approved", blueprint.Event{Kind: blueprint.EventLessonDecided, BlueprintID: "S1", Payload: map[string]any{
			"lesson": 2, "state": "active", "scope": "project", "project": "alpha", "text": "Name where the key comes from", "message_id": "m1",
		}}, "system", []string{"L2", "alpha"}, false},
		{"a lesson the merged lists refused", blueprint.Event{Kind: blueprint.EventLessonDecided, BlueprintID: "S1", Payload: map[string]any{
			"lesson": 2, "state": "rejected", "scope": "project", "project": "alpha", "text": "Name where the key comes from",
			"error": "forbidden reference broken: missing closing ]", "message_id": "m1",
		}}, "system", []string{"L2", "forbidden reference broken: missing closing ]"}, false},
		{"a review started", blueprint.Event{Kind: blueprint.EventStageStarted, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "kind": "reviewer", "persona": "reviewer",
		}}, "system", []string{"Review of Shape"}, false},
		{"a review ended", blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "kind": "reviewer", "status": "done", "cost_usd": 0.02,
		}}, "system", []string{"Review of Shape", "0.02"}, false},
		{"a verdict", blueprint.Event{Kind: blueprint.EventReview, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 2, "score": 3, "threshold": 4, "findings": []any{map[string]any{"id": "R1", "severity": "major", "text": "name the owner", "status": "open"}}, "round": 1,
		}}, "text", []string{"Shape", "3", "4", "R1", "major", "name the owner"}, false},
		{"a verdict that re-checked its findings", blueprint.Event{Kind: blueprint.EventReview, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 2, "score": 4, "threshold": 4, "round": 2, "open_critical": []string{"R3"},
			"findings": []any{
				map[string]any{"id": "R1", "severity": "critical", "text": "name the owner", "status": "fixed", "verified_in": 2, "claim": "named in the table"},
				map[string]any{"id": "R2", "severity": "minor", "text": "split it", "status": "still_open", "verified_in": 2},
				map[string]any{"id": "R3", "severity": "critical", "text": "no way back", "status": "open"},
			},
		}}, "text", []string{"R1 (critical, fixed in version 2)", "named in the table", "R2 (minor, still open in version 2)", "R3 (critical, open)"}, false},
		{"a gate with open critical findings", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 3, "actions": []any{},
			"review": map[string]any{
				"score": 4, "threshold": 4, "round": 3, "passed": false, "open_critical": []string{"R2", "R5"},
				"findings": []any{map[string]any{"id": "R2", "severity": "critical", "text": "a", "status": "still_open", "verified_in": 3}},
			},
		}}, "text", []string{"R2 and R5"}, false},
		{"a gate with a verdict", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "shape", "version": 7, "actions": []any{map[string]any{"label": "Approve", "reply": "approve", "feedback": false}},
			"review": map[string]any{"score": 2, "threshold": 4, "findings": []any{map[string]any{"id": "R1", "severity": "major", "text": "split it", "status": "still_open", "verified_in": 7}}, "round": 3, "passed": false},
		}}, "text", []string{"2", "4"}, false},
		{"a gate whose checks accepted problems", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 1, "actions": []any{},
			"review":   map[string]any{"score": 4, "threshold": 4, "findings": []any{map[string]any{"id": "R1", "severity": "minor", "text": "name the owner", "status": "open"}}, "round": 1, "passed": true},
			"problems": []any{"Q1: unused_answer: the question was answered, but no decision rests on it"},
		}}, "text", []string{"- Q1: unused_answer: the question was answered, but no decision rests on it\n\n"}, false},
		{"a note that runs a review again", blueprint.Event{Kind: blueprint.EventNote, BlueprintID: "S1", Payload: map[string]any{
			"text": "go on", "retries": "shape", "critique": true,
		}}, "system", []string{"Shape"}, false},
		{"a fold", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "slicing", "decision": "fold", "feedback": "solution_design", "responder": "auto",
		}}, "system", []string{"Slicing", "Solution design"}, false},
		{"a design superseded by a rejected slicing", blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "decision": "superseded", "feedback": "slicing", "responder": "human",
		}}, "system", []string{"Solution design", "Slicing"}, false},
		{"a folded gate", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 1, "folded": []any{map[string]any{"stage": "slicing", "version": 1}},
			"actions": []any{map[string]any{"label": "Reject slicing", "reply": "reject slicing", "feedback": true}},
		}}, "text", []string{"Slicing", "`reject slicing <feedback>`"}, false},
		{"a folded gate names what it also judges", blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
			"stage": "solution_design", "version": 1, "folded": []any{map[string]any{"stage": "slicing", "version": 3}}, "actions": []any{},
		}}, "text", []string{"Slicing", "3"}, false},
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
	const message = "The blueprint's $1.00 cap is used up ($1.00 spent); Intake stopped."

	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventError, BlueprintID: "S1", Payload: map[string]any{
		"stage": "intake", "message": message, "cap": "blueprint", "cap_usd": 1.0, "blueprint_spent_usd": 1.0,
		"message_id": "m1", "need": "containerise this project",
	}})
	require.True(t, ok)
	assert.Equal(t, message, entry.Content, "the need stays in the audit log, not the panel")
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
			name: "notices", preflight: map[string]any{"blocking": []any{}, "warnings": []any{}, "notices": []any{"project alpha declares no verify command, so each card's agent looks for one"}},
			carries: []string{"Notices (no step needed before the cards run):", "project alpha declares no verify command, so each card's agent looks for one"},
			lacks:   []string{"Manual steps for you", "found nothing", "overrides the warnings"},
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

			assert.Contains(t, entry.Content, "`reject <feedback>` to re-run the blueprint with your feedback.")
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
		{"a sign-off over a critical finding", blueprint.Event{Kind: blueprint.EventSignOff, Payload: map[string]any{
			"version": 4, "cards": []any{}, "order": []any{}, "actions": []any{
				map[string]any{"label": "Approve over critical findings", "reply": "approve", "feedback": true, "ask": "Why approve over the critical findings?"},
				map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
			},
		}}, "text", &protocol.LogPrompt{ID: "gate:signoff:v4", Kind: "gate", Actions: []protocol.LogAction{
			{Label: "Approve over critical findings", Reply: "approve", Feedback: true, Ask: "Why approve over the critical findings?"},
			{Label: "Reject", Reply: "reject", Feedback: true},
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
		{"a slice's gate", blueprint.Event{Kind: blueprint.EventGate, Payload: map[string]any{
			"stage": "solution_design@SL2", "version": 1, "actions": []any{
				map[string]any{"label": "Approve", "reply": "approve", "feedback": false},
			},
		}}, "text", &protocol.LogPrompt{ID: "gate:solution_design:SL2:v1", Kind: "gate", Actions: []protocol.LogAction{{Label: "Approve", Reply: "approve"}}}},
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
		"stage": "solution_design", "version": 1, "score": 3, "threshold": 4, "round": 3,
		"findings": []any{map[string]any{"id": "R1", "severity": "critical", "text": finding, "status": "still_open", "verified_in": 1}},
	}})
	require.True(t, ok)

	gate, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
		"stage": "solution_design", "version": 1, "actions": []any{},
		"review": map[string]any{
			"score": 3, "threshold": 4, "round": 3, "passed": false, "open_critical": []string{"R1"},
			"findings": []any{map[string]any{"id": "R1", "severity": "critical", "text": finding, "status": "still_open", "verified_in": 1}},
		},
	}})
	require.True(t, ok)

	assert.Equal(t, 1, strings.Count(review.Content+"\n"+gate.Content, finding), "the findings are shown once, in the review above the gate")
	assert.Contains(t, review.Content, finding)
	assert.Contains(t, gate.Content, "R1", "the gate names the open critical finding")
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

	// slicedEv is ev on a blueprint with two or more slices.
	slicedEv := func(kind string, p map[string]any) blueprint.Event {
		e := ev(kind, p)
		e.Sliced = true

		return e
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
			"system", &protocol.LogMarker{Kind: "stage", Stage: "solution_design", Persona: "system_designer"}, "Solution design",
		},
		{"a builder round's start is no divider", ev(blueprint.EventStageStarted, map[string]any{"stage": "solution_design", "kind": "stage", "persona": "system_designer", "round": 2}), "system", nil, "Solution design"},
		{"a review's start is no divider", ev(blueprint.EventStageStarted, map[string]any{"stage": "solution_design", "kind": "reviewer", "persona": "senior_developer", "round": 1}), "system", nil, "Review of Solution design"},
		{"a retrospective's start is no divider", ev(blueprint.EventStageStarted, map[string]any{"stage": blueprint.RetroStage, "kind": store.RunKindRetro, "persona": "reviewer", "round": 1}), "system", nil, ""},
		{
			"a first version is drawn by its persona",
			ev(blueprint.EventArtifact, map[string]any{"stage": "solution_design", "version": 1, "body": "# Design", "persona": "system_designer", "round": 1, "revised_for": 0}),
			"text", sheet("solution_design", 1, "system_designer", "Drawn by system designer"), "Solution design rev 1: Drawn by system designer.",
		},
		{
			"a revision says what it was revised for",
			ev(blueprint.EventArtifact, map[string]any{"stage": "solution_design", "version": 2, "body": "# Design", "persona": "system_designer", "round": 2, "revised_for": 3}),
			"text", sheet("solution_design", 2, "system_designer", "Revised for the 3 findings"), "Solution design rev 2",
		},
		{
			"a revision for one finding",
			ev(blueprint.EventArtifact, map[string]any{"stage": "slicing", "version": 2, "body": "# Slices", "persona": "product_owner", "round": 2, "revised_for": 1}),
			"text", sheet("slicing", 2, "product_owner", "Revised for the 1 finding"), "Slicing rev 2",
		},
		{
			"a loop stage says its round",
			ev(blueprint.EventArtifact, map[string]any{"stage": "readiness", "version": 2, "body": "# Ready", "persona": "reviewer", "round": 1, "revised_for": 0, "loop_round": 2, "max_rounds": 3}),
			"text", sheet("readiness", 2, "reviewer", "Round 2 of 3"), "Readiness rev 2",
		},
		{
			"cards accepted with findings say so",
			ev(blueprint.EventArtifact, map[string]any{"stage": "decomposition", "version": 1, "body": "# Cards", "persona": "senior_developer", "findings": []string{"D1: a"}, "accepted_round": 3}),
			"text", sheet("decomposition", 1, "senior_developer", "Refused by checks, accepted on round 3"), "Decomposition rev 1",
		},
		{
			"a review is a review marker",
			ev(blueprint.EventReview, map[string]any{"stage": "solution_design", "version": 1, "score": 4, "threshold": 4, "reviewer": "senior_developer", "findings": []any{map[string]any{"id": "R1", "severity": "minor", "text": "a", "status": "open"}}, "round": 1}),
			"text", &protocol.LogMarker{Kind: "review", Stage: "solution_design", Version: 1, Persona: "senior_developer"}, "4 of 5",
		},
		{
			"a slice's review names its slice apart",
			slicedEv(blueprint.EventReview, map[string]any{"stage": "acceptance@SL2", "version": 3, "score": 4, "threshold": 4, "reviewer": "tester", "findings": []any{}, "round": 1}),
			"text", &protocol.LogMarker{Kind: "review", Stage: "acceptance", Slice: "SL2", Version: 3, Persona: "tester"}, "4 of 5",
		},
		{
			"a gate decision stays a prose row",
			ev(blueprint.EventDecided, map[string]any{"stage": "solution_design", "decision": "approve", "responder": "human", "version": 2}),
			"system", nil, "Approved Solution design",
		},
		{
			"a collapse names the step that settles it, without its slice",
			ev(blueprint.EventDecided, map[string]any{"stage": "shared_decisions", "decision": "fold", "feedback": "solution_design@SL1", "responder": "auto", "version": 0}),
			"system", nil, "Shared decisions is skipped: Solution design settles",
		},
		{
			"a slice's stage divider names its slice",
			slicedEv(blueprint.EventStageStarted, map[string]any{"stage": "solution_design@SL2", "kind": "stage", "persona": "system_designer", "round": 1}),
			"system", &protocol.LogMarker{Kind: "stage", Stage: "solution_design", Slice: "SL2", Persona: "system_designer"}, "Solution design (SL2)",
		},
		{
			"a slice's sheet reference opens its slice",
			slicedEv(blueprint.EventArtifact, map[string]any{"stage": "acceptance@SL1", "version": 1, "body": "# Acceptance", "persona": "senior_developer", "round": 1, "revised_for": 0}),
			"text", &protocol.LogMarker{Kind: "artifact", Stage: "acceptance", Slice: "SL1", Version: 1, Persona: "senior_developer", Note: "Drawn by senior developer"}, "Acceptance (SL1) rev 1",
		},
		{
			"one slice's marker still carries its slice, which its text does not name",
			ev(blueprint.EventStageStarted, map[string]any{"stage": "solution_design@SL1", "kind": "stage", "persona": "system_designer", "round": 1}),
			"system", &protocol.LogMarker{Kind: "stage", Stage: "solution_design", Slice: "SL1", Persona: "system_designer"}, "Stage Solution design started",
		},
		{
			"a typed sign-off rejection names what runs again",
			slicedEv(blueprint.EventDecided, map[string]any{"stage": "signoff", "decision": "reject", "responder": "human", "version": 1, "reruns": "solution_design@SL2"}),
			"system", nil, "Solution design (SL2)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := logEntryFor(tt.event)
			require.True(t, ok)
			assert.Equal(t, tt.wantType, entry.Type)
			assert.Equal(t, tt.want, entry.Marker)
			assert.Contains(t, entry.Content, tt.carries, "the fallback line")

			for _, body := range []string{"# Design", "# Slices", "# Ready", "# Cards", "# Acceptance"} {
				assert.NotContains(t, entry.Content, body, "an artifact's body is no transcript text")
			}
		})
	}
}

func TestLogEntryFor_ASignoffListsItsTypedRejections(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Sliced: true, Payload: map[string]any{
		"version": 1, "cards": []any{}, "order": []any{}, "typed": []any{"reject SL1", "reject SL2"},
	}})
	require.True(t, ok)
	assert.Contains(t, entry.Content, "`reject SL2 <feedback>`")

	// With one slice nothing slice-related shows.
	entry, ok = logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: map[string]any{
		"version": 1, "cards": []any{}, "order": []any{}, "typed": []any{"reject slicing", "reject SL1"},
	}})
	require.True(t, ok)
	assert.NotContains(t, entry.Content, "SL1")
	assert.Contains(t, entry.Content, "`reject slicing <feedback>`", "slicing's typed reject is listed with any number of slices")
}

// With one slice nothing slice-related shows in the text; with two or more,
// a per-slice step's text names its slice. The marker carries the slice
// either way: CM hides it below two.
func TestLogEntryFor_ABlueprintWithOneSliceNamesNoSlice(t *testing.T) {
	for _, e := range []blueprint.Event{
		{Kind: blueprint.EventStageStarted, Payload: map[string]any{"stage": "solution_design@SL1", "kind": "stage", "persona": "system_designer", "round": 1}},
		{Kind: blueprint.EventArtifact, Payload: map[string]any{"stage": "solution_design@SL1", "version": 1, "body": "# Design", "persona": "system_designer", "round": 1, "revised_for": 0}},
		{Kind: blueprint.EventGate, Payload: map[string]any{"stage": "solution_design@SL1", "version": 1, "actions": []any{}}},
		{Kind: blueprint.EventDecided, Payload: map[string]any{"stage": "solution_design@SL1", "decision": "approve", "responder": "human", "version": 1}},
		{Kind: blueprint.EventReview, Payload: map[string]any{"stage": "solution_design@SL1", "version": 1, "score": 5, "threshold": 4, "findings": []any{}, "round": 1}},
	} {
		e.BlueprintID = "S1"

		entry, ok := logEntryFor(e)
		require.True(t, ok)
		assert.Contains(t, entry.Content, "Solution design", e.Kind)
		assert.NotContains(t, entry.Content, "SL1", "%s: with one slice nothing slice-related shows", e.Kind)

		e.Sliced = true

		entry, ok = logEntryFor(e)
		require.True(t, ok)
		assert.Contains(t, entry.Content, "Solution design (SL1)", "%s: with two or more the slice is named", e.Kind)
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
	assert.Equal(t, "Round 3: 2 assumption(s) from the system designer", entry.Prompt.Text)
}

func TestLogEntry_AnAutoRoundEndIsNotAWait(t *testing.T) {
	end := func(extra map[string]any) string {
		p := map[string]any{"stage": "solution_design", "kind": "stage", "status": "asked", "question_round": 2.0, "cost_usd": 0.5}
		for k, v := range extra {
			p[k] = v
		}

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventStageEnded, BlueprintID: "BP1", Payload: p})
		require.True(t, ok)

		return entry.Content
	}

	waiting := end(nil)
	assert.NotEqual(t, waiting, end(map[string]any{"responder": true}), "an auto round is not rendered as a wait")
	assert.NotEqual(t, waiting, end(map[string]any{"chat": true}))
	assert.Equal(t, waiting, end(map[string]any{"responder": false}))
}

func roundPayloadForTest() map[string]any {
	raw := []byte(`{"round":3,"change":"posted","frame":{"persona":"system designer","stage":"solution_design",
		"questions":[{"number":7,"title":"Deploy","text":"Helm or Kustomize?","options":[{"title":"Helm"}]}]}}`)

	var p map[string]any

	_ = json.Unmarshal(raw, &p)

	return p
}

func TestLogEntry_ASliceRoundNamesItsSlice(t *testing.T) {
	p := roundPayloadForTest()
	p["frame"].(map[string]any)["stage"] = "solution_design@SL2"

	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "BP1", Payload: p})
	require.True(t, ok)
	require.NotNil(t, entry.Prompt)
	require.NotNil(t, entry.Prompt.Round)
	assert.Equal(t, "solution_design", entry.Prompt.Round.Stage)
	assert.Equal(t, "SL2", entry.Prompt.Round.Slice)
}

func TestGatePromptID(t *testing.T) {
	assert.Equal(t, "gate:solution_design:SL2:v3", blueprint.GatePromptID("solution_design@SL2", 3))
	assert.Equal(t, "gate:slicing:v1", blueprint.GatePromptID("slicing", 1))
}

func TestLogEntry_ASegmentStartIsNoDivider(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventStageStarted, BlueprintID: "BP1", Payload: map[string]any{
		"stage": "intake", "kind": "stage", "round": 1.0, "segment": blueprint.SegmentAnswers, "question_round": 2.0,
	}})
	require.True(t, ok)
	assert.Nil(t, entry.Marker)
}

func TestAssumptionStatus_AnAcceptedAssumptionNamesItsStageNotATag(t *testing.T) {
	for _, basis := range []string{"round2.A1", "Q5"} {
		got := assumptionStatus(map[string]any{"id": "A1", "status": "confirmed", "basis": basis, "accepted": "solution_design"}, false)

		assert.Contains(t, got, "accepted at Solution design")
		assert.NotContains(t, got, basis)
		assert.NotContains(t, got, "Accepted")
	}

	answered := assumptionStatus(map[string]any{"id": "A1", "status": "confirmed", "basis": "Q1", "answer": "ops"}, false)
	assert.Contains(t, answered, "Q1")
	assert.Contains(t, answered, "ops")
	assert.NotContains(t, answered, "accepted at")
}

func TestLogEntryFor_AGateAndItsReviewNameTheStageNotItsID(t *testing.T) {
	const id, name = "solution_design", "Solution design"

	gate, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventGate, BlueprintID: "S1", Payload: map[string]any{
		"stage": id, "version": 2, "actions": []any{map[string]any{"label": "Approve", "reply": "approve", "feedback": false}},
		"review": map[string]any{
			"score": 4, "threshold": 4, "round": 2, "passed": true, "judged": 2, "open_critical": []string{},
			"findings": []any{map[string]any{"id": "R1", "severity": "minor", "text": "f", "status": "open"}},
		},
		"folded": []any{map[string]any{"stage": "slicing_plan", "version": 1, "body": "# Slices"}},
	}})
	require.True(t, ok)

	review, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventReview, BlueprintID: "S1", Payload: map[string]any{
		"stage": id, "version": 1, "score": 4, "threshold": 4, "findings": []any{}, "round": 1,
	}})
	require.True(t, ok)

	require.NotNil(t, gate.Prompt)
	assert.Equal(t, "gate:solution_design:v2", gate.Prompt.ID, "the prompt id keeps the raw id")

	for what, text := range map[string]string{"gate content": gate.Content, "gate prompt": gate.Prompt.Text, "review": review.Content} {
		assert.NotContains(t, text, id, what)
		assert.NotContains(t, text, "slicing_plan", what)
	}

	assert.Contains(t, gate.Content, name)
	assert.Contains(t, gate.Prompt.Text, name)
	assert.Contains(t, review.Content, name)
}

func TestLogEntryFor_ASignoffShowsItsCrossFindingsAndTheReason(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: map[string]any{
		"version": 1, "cards": []any{}, "order": []any{},
		"cross": []any{map[string]any{"id": "X2", "severity": "critical", "text": "No criterion proves O1.", "refs": []any{"O1"}}},
	}})
	require.True(t, ok)
	assert.Contains(t, entry.Content, "X2")
	assert.Contains(t, entry.Content, "No criterion proves O1.")

	entry, ok = logEntryFor(blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
		"stage": "signoff", "decision": "approve", "responder": "human", "version": 1, "override_reason": "the release check covers it",
	}})
	require.True(t, ok)
	assert.Contains(t, entry.Content, "the release check covers it")
	assert.Contains(t, entry.Content, "Approved over open findings: the release check covers it")
}

func TestLogEntryFor_TheModesApprovalOfAShownSignoffClosesItsPrompt(t *testing.T) {
	cases := []struct {
		name   string
		extra  map[string]any
		marked bool
	}{
		{name: "shown, then approved by the mode", extra: map[string]any{"responder": "auto", "shown": true}, marked: true},
		{name: "approved by the mode before it was shown", extra: map[string]any{"responder": "auto"}},
		{name: "approved by a human", extra: map[string]any{"responder": "human"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := map[string]any{"stage": "signoff", "decision": "approve", "version": 3}
			maps.Copy(p, tc.extra)

			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: p})
			require.True(t, ok)
			assert.Equal(t, "system", entry.Type)

			if !tc.marked {
				assert.Nil(t, entry.Marker)

				return
			}

			require.NotNil(t, entry.Marker)
			assert.Equal(t, protocol.LogMarker{Kind: "decision", Stage: "signoff", Version: 3}, *entry.Marker)
		})
	}
}

func TestLogEntryFor_TheModesApprovalOfAShownGateNamesItsSlice(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: map[string]any{
		"stage": "solution_design@SL2", "decision": "approve", "version": 2, "responder": "auto", "shown": true,
	}})
	require.True(t, ok)
	require.NotNil(t, entry.Marker)
	assert.Equal(t, protocol.LogMarker{Kind: "decision", Stage: "solution_design", Slice: "SL2", Version: 2}, *entry.Marker)
}

func TestLogEntryFor_APlainPromptFoldsTheTechnicalText(t *testing.T) {
	gate := map[string]any{
		"stage": "solution_design", "version": 2,
		"actions": []any{
			map[string]any{"label": "Approve", "reply": "approve", "feedback": false},
			map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
		},
		"review": map[string]any{"score": 2, "threshold": 4, "round": 3, "passed": false, "findings": []any{}, "open_critical": []any{}},
	}
	signoff := map[string]any{
		"version": 1, "order": []any{}, "cards": []any{}, "cross": []any{map[string]any{"id": "X1", "severity": "critical", "text": "two slices write the same table"}},
		"actions": []any{map[string]any{"label": "Approve over critical findings", "reply": "approve", "feedback": true, "ask": "Why approve over the critical findings?"}},
	}
	held := map[string]any{"stage": "signoff", "resume": "held", "version": 1, "prompt": "Push a first commit, then reply approve.", "text": "The run has not started."}

	for _, tc := range []struct {
		name    string
		kind    string
		payload map[string]any
	}{
		{"a gate a verdict did not settle", blueprint.EventGate, gate},
		{"a sign-off over a critical finding", blueprint.EventSignOff, signoff},
		{"a run start held for setup", blueprint.EventAwaitInput, held},
	} {
		t.Run(tc.name, func(t *testing.T) {
			technical, ok := logEntryFor(blueprint.Event{Kind: tc.kind, BlueprintID: "S1", Payload: tc.payload})
			require.True(t, ok)
			require.NotNil(t, technical.Prompt)

			plain, ok := logEntryFor(blueprint.Event{Kind: tc.kind, BlueprintID: "S1", Payload: tc.payload, Plain: true})
			require.True(t, ok)
			require.NotNil(t, plain.Prompt)

			assert.Equal(t, technical.Prompt.Text, plain.Prompt.Detail, "the technical text is folded")
			assert.NotEmpty(t, plain.Prompt.Text)
			assert.NotEqual(t, plain.Prompt.Detail, plain.Prompt.Text)
			assert.Empty(t, technical.Prompt.Detail, "off plain nothing is folded")
			assert.Equal(t, technical.Content, plain.Content, "the fallback stays the record")
			assert.Equal(t, technical.Prompt.ID, plain.Prompt.ID)

			require.Len(t, plain.Prompt.Actions, len(technical.Prompt.Actions))

			for i := range plain.Prompt.Actions {
				assert.Equal(t, technical.Prompt.Actions[i].Reply, plain.Prompt.Actions[i].Reply, "the replies decide as before")
				assert.Equal(t, technical.Prompt.Actions[i].Feedback, plain.Prompt.Actions[i].Feedback)
			}
		})
	}
}

func TestLogEntryFor_APlainPromptWordsEachMomentByItsSituation(t *testing.T) {
	actions := []any{
		map[string]any{"label": "Approve", "reply": "approve", "feedback": false},
		map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
	}
	gate := func(passed bool) map[string]any {
		return map[string]any{
			"stage": "solution_design", "version": 1, "actions": actions,
			"review": map[string]any{"score": 4, "threshold": 4, "round": 1, "passed": passed, "findings": []any{}, "open_critical": []any{}},
		}
	}
	signoff := func(extra map[string]any) map[string]any {
		p := map[string]any{"version": 1, "order": []any{}, "cards": []any{}, "cross": []any{}, "actions": actions}
		maps.Copy(p, extra)

		return p
	}
	render := func(t *testing.T, kind string, p map[string]any, plain bool) protocol.LogEntry {
		t.Helper()

		entry, ok := logEntryFor(blueprint.Event{Kind: kind, BlueprintID: "S1", Payload: p, Plain: plain})
		require.True(t, ok)
		require.NotNil(t, entry.Prompt)

		return entry
	}

	t.Run("a passed gate reads differently from an unsettled one", func(t *testing.T) {
		passed, unsettled := render(t, blueprint.EventGate, gate(true), true), render(t, blueprint.EventGate, gate(false), true)

		assert.NotEqual(t, passed.Prompt.Text, unsettled.Prompt.Text)
		assert.NotEqual(t, passed.Prompt.Actions[0].Label, unsettled.Prompt.Actions[0].Label)
		assert.Equal(t, "approve", passed.Prompt.Actions[0].Reply)
	})

	t.Run("setup steps change a plain sign-off's text", func(t *testing.T) {
		without := render(t, blueprint.EventSignOff, signoff(nil), true)
		with := render(t, blueprint.EventSignOff, signoff(map[string]any{"steps": []any{"Set up the thing"}}), true)

		assert.NotEqual(t, without.Prompt.Text, with.Prompt.Text)

		critical := signoff(map[string]any{"cross": []any{map[string]any{"id": "X1", "severity": "critical", "text": "a clash"}}})
		criticalSteps := signoff(map[string]any{"cross": critical["cross"], "steps": []any{"Set up the thing"}})

		assert.NotEqual(t, render(t, blueprint.EventSignOff, critical, true).Prompt.Text, render(t, blueprint.EventSignOff, criticalSteps, true).Prompt.Text)
	})

	t.Run("notices say the work settles some setup", func(t *testing.T) {
		const (
			settles   = "settle some technical setup on its own"
			technical = "needs someone technical"
		)

		notices := []any{"a notice"}

		only := render(t, blueprint.EventSignOff, signoff(map[string]any{"notices": notices}), true)
		assert.Contains(t, only.Prompt.Text, settles)
		assert.NotContains(t, only.Prompt.Text, technical)

		both := render(t, blueprint.EventSignOff, signoff(map[string]any{"notices": notices, "steps": []any{"Set up the thing"}}), true)
		assert.Contains(t, both.Prompt.Text, settles)
		assert.Contains(t, both.Prompt.Text, technical)
	})

	t.Run("every stop kind folds its text and keeps its reply", func(t *testing.T) {
		for _, resume := range []string{"emission", "run", "held"} {
			p := map[string]any{"stage": "signoff", "resume": resume, "version": 1, "prompt": "p", "text": "technical stop"}

			for _, kind := range []string{blueprint.EventAwaitInput, blueprint.EventError} {
				technical, plain := render(t, kind, p, false), render(t, kind, p, true)

				assert.Equal(t, technical.Prompt.Text, plain.Prompt.Detail)
				assert.NotEqual(t, technical.Prompt.Actions[0].Label, plain.Prompt.Actions[0].Label)
				assert.Equal(t, technical.Prompt.Actions[0].Reply, plain.Prompt.Actions[0].Reply)
			}
		}
	})

	t.Run("a relabelled action differs from its technical label and keeps its reply", func(t *testing.T) {
		for kind, p := range map[string]map[string]any{blueprint.EventGate: gate(false), blueprint.EventSignOff: signoff(map[string]any{
			"cross":   []any{map[string]any{"id": "X1", "severity": "critical", "text": "a clash"}},
			"actions": []any{map[string]any{"label": "Approve over critical", "reply": "approve", "feedback": true}, map[string]any{"label": "Reject", "reply": "reject", "feedback": true}},
		})} {
			technical, plain := render(t, kind, p, false), render(t, kind, p, true)

			for i := range plain.Prompt.Actions {
				assert.NotEqual(t, technical.Prompt.Actions[i].Label, plain.Prompt.Actions[i].Label)
				assert.Equal(t, technical.Prompt.Actions[i].Reply, plain.Prompt.Actions[i].Reply)
			}
		}
	})
}

func TestLogEntryFor_APlainSetupNoteLeadsPlainlyAndKeepsTheSteps(t *testing.T) {
	gaps := map[string]any{"projects": []any{
		map[string]any{"project": "alpha", "has_verify": false, "has_instruction_file": false, "empty": true},
	}}

	technical, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSetupGaps, BlueprintID: "S1", Payload: gaps})
	require.True(t, ok)

	plain, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSetupGaps, BlueprintID: "S1", Payload: gaps, Plain: true})
	require.True(t, ok)

	assert.NotEqual(t, technical.Content, plain.Content)
	assert.Contains(t, plain.Content, technical.Content, "the technical steps follow unchanged")
	assert.Equal(t, technical.Type, plain.Type)
}

func TestLogEntryFor_APlainSetupNoteLeadsWithAPersonOnlyForAStep(t *testing.T) {
	gap := func(verify, file, empty bool) map[string]any {
		return map[string]any{"project": "alpha", "has_verify": verify, "has_instruction_file": file, "empty": empty}
	}

	tests := []struct {
		name   string
		mode   any
		gap    map[string]any
		person bool
	}{
		{name: "instruction file only", mode: protocol.ArchitectModeDefault, gap: gap(true, false, false)},
		{name: "empty repository", mode: protocol.ArchitectModeDefault, gap: gap(true, true, true), person: true},
		{name: "verify gap, default", mode: protocol.ArchitectModeDefault, gap: gap(false, true, false), person: true},
		{name: "verify gap, yolo", mode: protocol.ArchitectModeYolo, gap: gap(false, true, false)},
		{name: "verify gap, no mode", gap: gap(false, true, false), person: true},
		{name: "verify gap and empty repository, yolo", mode: protocol.ArchitectModeYolo, gap: gap(false, true, true), person: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := map[string]any{"projects": []any{tt.gap}}
			if tt.mode != nil {
				p["mode"] = tt.mode
			}

			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSetupGaps, BlueprintID: "S1", Payload: p, Plain: true})
			require.True(t, ok)

			want := plainSetupSettledLead
			if tt.person {
				want = plainSetupLead
			}

			assert.True(t, strings.HasPrefix(entry.Content, want))
		})
	}
}

func TestLogEntryFor_APlainEmittedEntryLeadsPlainlyAndKeepsTheDetails(t *testing.T) {
	payload := func(extra map[string]any) map[string]any {
		p := map[string]any{"cards": []any{map[string]any{"card": "ALPHA-001"}}, "playbook": "", "manual": []any{}}
		maps.Copy(p, extra)

		return p
	}
	render := func(p map[string]any, plain bool) protocol.LogEntry {
		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventEmitted, BlueprintID: "S1", Payload: p, Plain: plain})
		require.True(t, ok)

		return entry
	}

	with := payload(map[string]any{"steps": []any{"push a commit"}, "notices": []any{"a notice"}})
	technical, plain := render(with, false), render(with, true)

	assert.NotEqual(t, technical.Content, plain.Content)
	assert.Contains(t, plain.Content, technical.Content, "the technical text follows unchanged")
	assert.Contains(t, plain.Content, plainStepsSentence)
	assert.Contains(t, plain.Content, plainNoticesSentence)
	assert.Equal(t, technical.Type, plain.Type)

	bare := render(payload(nil), true)
	assert.NotContains(t, bare.Content, plainStepsSentence)
	assert.NotContains(t, bare.Content, plainNoticesSentence)
}

func TestLogEntry_TheOfferIsAGatePrompt(t *testing.T) {
	shown, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventMockupOffer, BlueprintID: "S1", Payload: map[string]any{
		"stage": "mockup", "state": "shown", "actions": []any{
			map[string]any{"label": "Show mockup", "reply": "show mockup", "feedback": false, "recommended": true},
			map[string]any{"label": "Skip mockup", "reply": "skip mockup", "feedback": false},
		},
	}})
	require.True(t, ok)
	assert.Equal(t, "text", shown.Type)
	require.NotNil(t, shown.Prompt)
	assert.Equal(t, "mockup:offer", shown.Prompt.ID)
	assert.Equal(t, "gate", shown.Prompt.Kind)

	var replies []string

	for _, a := range shown.Prompt.Actions {
		replies = append(replies, a.Reply)
	}

	assert.Equal(t, []string{"show mockup", "skip mockup"}, replies)
	assert.True(t, shown.Prompt.Actions[0].Recommended, "showing the mockup is the recommended reply")
	assert.False(t, shown.Prompt.Actions[1].Recommended)

	for name, by := range map[string]string{"a reply": "", "a mode switch": "mode"} {
		t.Run(name, func(t *testing.T) {
			p := map[string]any{"stage": "mockup", "state": "accepted"}
			if by != "" {
				p["by"] = by
			}

			accepted, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventMockupOffer, BlueprintID: "S1", Payload: p})
			require.True(t, ok)
			assert.Equal(t, "system", accepted.Type)
			assert.Nil(t, accepted.Prompt)

			if by == "" {
				assert.Nil(t, accepted.Marker, "the reply itself answered the prompt")

				return
			}

			require.NotNil(t, accepted.Marker)
			assert.Equal(t, "decision", accepted.Marker.Kind)
			assert.Equal(t, "mockup", accepted.Marker.Stage)
		})
	}

	declined, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventMockupOffer, BlueprintID: "S1", Payload: map[string]any{
		"stage": "mockup", "state": "declined", "message_id": "m2",
	}})
	require.True(t, ok)
	assert.Equal(t, "system", declined.Type)
	assert.Nil(t, declined.Marker)
}

func TestLogEntry_AMockupRevisionIsASheetReference(t *testing.T) {
	entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventArtifact, BlueprintID: "S1", Payload: map[string]any{
		"stage": "mockup", "version": 2, "persona": "designer", "revision": 2,
	}})
	require.True(t, ok)
	require.NotNil(t, entry.Marker)
	assert.Equal(t, "artifact", entry.Marker.Kind)
	assert.Equal(t, "mockup", entry.Marker.Stage)
	assert.Equal(t, 2, entry.Marker.Version)
}

// mockupRoundPayload is a mockup round snapshot with change and chat, of
// which the run behind it has seen seen.
func mockupRoundPayload(change string, seen int, chat ...map[string]any) map[string]any {
	entries := make([]any, 0, len(chat))
	for _, m := range chat {
		entries = append(entries, m)
	}

	return map[string]any{
		"round": 3, "change": change, "seen": seen,
		"mockup": map[string]any{"revision": 2, "suggestions": []any{}, "opening": map[string]any{}, "settings": map[string]any{}},
		"frame":  map[string]any{"persona": "designer", "stage": "mockup", "questions": []any{}, "chat": entries},
	}
}

func TestLogEntry_AMockupRoundIsNeverARoundPrompt(t *testing.T) {
	first := map[string]any{"from": "designer", "text": "A list the household shares."}
	person := map[string]any{"from": "user", "text": "Can it group by aisle?"}
	second := map[string]any{"from": "designer", "text": "Grouped by aisle now."}

	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"a revision", mockupRoundPayload("revision", 3, first, person, second), "Grouped by aisle now."},
		{"the first revision", mockupRoundPayload("posted", 1, first), "A list the household shares."},
		{"a reply", mockupRoundPayload("reply", 3, first, person, second), "Grouped by aisle now."},
		{"a revision before a message it has not seen", mockupRoundPayload("revision", 3, first, person, second, person), "Grouped by aisle now."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "S1", Payload: tt.payload})
			require.True(t, ok)
			assert.Equal(t, "text", entry.Type)
			assert.Nil(t, entry.Prompt)
			assert.Equal(t, tt.want, entry.Content)
		})
	}

	t.Run("answers", func(t *testing.T) {
		_, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "S1", Payload: mockupRoundPayload("answers", 1, first)})
		assert.False(t, ok)
	})
}

// A mockup run's start and end never name its round, which the person never
// sees: two rounds read the same, and the revision shown tells two ends apart.
func TestLogEntry_AMockupRunNamesNoRound(t *testing.T) {
	content := func(kind string, p map[string]any) string {
		t.Helper()

		entry, ok := logEntryFor(blueprint.Event{Kind: kind, BlueprintID: "S1", Payload: p})
		require.True(t, ok)

		return entry.Content
	}

	ended := func(round, revision int, chat, mockup bool) map[string]any {
		p := map[string]any{
			"stage": "mockup", "kind": "stage", "status": "asked", "cost_usd": 0.1, "priced": true,
			"question_round": round, "chat": chat, "responder": false,
		}
		if mockup {
			p["mockup"], p["revision"] = true, revision
		}

		return p
	}

	started := func(round int, mockup bool) map[string]any {
		p := map[string]any{"stage": "mockup", "kind": "stage", "persona": "designer", "segment": blueprint.SegmentChat, "question_round": round}
		if mockup {
			p["mockup"] = true
		}

		return p
	}

	ev := blueprint.EventStageEnded
	assert.NotEqual(t, content(ev, ended(3, 0, false, false)), content(ev, ended(7, 0, false, false)), "a question round's end names it")
	assert.Equal(t, content(ev, ended(3, 2, false, true)), content(ev, ended(7, 2, false, true)))
	assert.Equal(t, content(ev, ended(3, 2, true, true)), content(ev, ended(7, 2, true, true)))
	assert.Equal(t, content(ev, ended(3, 0, true, true)), content(ev, ended(7, 0, true, true)), "a reply without a revision")
	assert.NotEqual(t, content(ev, ended(3, 2, false, true)), content(ev, ended(3, 3, false, true)))

	sv := blueprint.EventStageStarted
	assert.NotEqual(t, content(sv, started(3, false)), content(sv, started(7, false)))
	assert.Equal(t, content(sv, started(3, true)), content(sv, started(7, true)))
}

// A use or a skip of a mockup is a system row of its own: neither reads as a
// gate's decision, and the revision and who chose tell two apart.
func TestLogEntry_AMockupDecisionIsItsOwnRow(t *testing.T) {
	content := func(decision, responder string, version int, mockup bool) string {
		t.Helper()

		p := map[string]any{"stage": "mockup", "decision": decision, "feedback": "", "responder": responder, "version": version, "message_id": "m1"}
		if mockup {
			p["mockup"] = true
		}

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: p})
		require.True(t, ok)
		assert.Equal(t, "system", entry.Type)
		assert.Nil(t, entry.Marker)
		assert.Nil(t, entry.Prompt)

		return entry.Content
	}

	use, skip := content("approve", "human", 2, true), content("reject", "human", 2, true)
	assert.NotEqual(t, content("approve", "human", 2, false), use, "a use is no gate's approval")
	assert.NotEqual(t, content("reject", "human", 2, false), skip, "a skip runs nothing again")
	assert.NotEqual(t, use, skip)
	assert.NotEqual(t, use, content("approve", "human", 3, true), "the revision is named")
	assert.NotEqual(t, use, content("approve", "auto", 2, true), "the responder's pick reads as its own")
}

// A use a failed responder left to the architect reads apart from the
// responder's own pick and the person's, and still names its revision.
func TestLogEntry_AFailedRespondersUseIsTheArchitects(t *testing.T) {
	content := func(version int, failed bool) string {
		t.Helper()

		p := map[string]any{"stage": "mockup", "decision": "approve", "feedback": "", "responder": "auto", "version": version, "message_id": "", "mockup": true}
		if failed {
			p["failed"] = true
		}

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventDecided, BlueprintID: "S1", Payload: p})
		require.True(t, ok)
		assert.Equal(t, "system", entry.Type)

		return entry.Content
	}

	assert.NotEqual(t, content(2, false), content(2, true), "no pick of the responder's")
	assert.NotEqual(t, content(2, true), content(3, true), "the revision is named")
}

// The auto responder's change requests and its pick are system rows that
// name it: a request quotes its text, a pick names its revision and what it
// chose. The person's own chat and use add no row: CM shows their message.
func TestLogEntry_TheRespondersTurnsAreSystemRows(t *testing.T) {
	notes := map[string]any{"from": "designer", "text": "A list of the day's free times."}
	request := map[string]any{"from": "responder", "text": "Show the next free time first."}
	person := map[string]any{"from": "user", "text": "Bigger buttons, please."}

	row := func(p map[string]any) (protocol.LogEntry, bool) {
		t.Helper()

		return logEntryFor(blueprint.Event{Kind: blueprint.EventRound, BlueprintID: "S1", Payload: p})
	}

	asked, ok := row(mockupRoundPayload("chat", 1, notes, request))
	require.True(t, ok)
	assert.Equal(t, "system", asked.Type)
	assert.Nil(t, asked.Prompt)
	assert.Contains(t, asked.Content, "Show the next free time first.")

	_, ok = row(mockupRoundPayload("chat", 1, notes, person))
	assert.False(t, ok, "the person's chat is CM's own row")

	chosen := func(by string, revision int) map[string]any {
		p := mockupRoundPayload("chosen", 1, notes)
		m, _ := p["mockup"].(map[string]any)
		m["revision"], m["action"], m["by"] = revision, "use", by
		m["suggestions"] = []any{map[string]any{
			"id": "slots", "title": "Free times as", "kind": "choice", "why": "A grid shows the day.", "default": "list",
			"options": []any{map[string]any{"value": "list", "label": "A list"}, map[string]any{"value": "grid", "label": "A grid"}},
		}}
		m["chosen"] = map[string]any{"slots": "grid"}

		return p
	}

	pick, ok := row(chosen("auto", 2))
	require.True(t, ok)
	assert.Equal(t, "system", pick.Type)
	assert.Nil(t, pick.Prompt)
	assert.Contains(t, pick.Content, "A grid", "the label it chose")

	later, ok := row(chosen("auto", 3))
	require.True(t, ok)
	assert.NotEqual(t, pick.Content, later.Content, "the revision is named")

	_, ok = row(chosen("human", 2))
	assert.False(t, ok, "the person's use is CM's own row")

	failed := chosen("auto", 2)
	failed["mockup"].(map[string]any)["failed"] = true
	_, ok = row(failed)
	assert.False(t, ok, "a failed responder picked nothing; its error row says what became of the revision")
}

// A gate reply while a mockup is open names no round, which the person never
// sees, and reads apart from a reply at an open round and at nothing open.
func TestLogEntry_AGateReplyAtAnOpenMockupNamesNoRound(t *testing.T) {
	content := func(open string) string {
		t.Helper()

		p := map[string]any{"stage": "mockup", "policy": "human", "message_id": "m1", "during_run": true}
		if open != "" {
			p["open"] = open
		}

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventPolicy, BlueprintID: "S1", Payload: p})
		require.True(t, ok)

		return entry.Content
	}

	assert.NotEqual(t, content("round"), content("mockup"))
	assert.NotEqual(t, content(""), content("mockup"), "the open mockup is said")
}

// A mode switch that hands the mockup to the responder names no round, which
// the person never sees, and still says the mockup changed hands.
func TestLogEntry_AModeSwitchHandingOverTheMockupNamesNoRound(t *testing.T) {
	content := func(round int, mockup bool) string {
		t.Helper()

		p := map[string]any{"from": "default", "to": "auto", "during_run": false}
		if round > 0 {
			p["hands_over"] = round
		}

		if mockup {
			p["mockup"] = true
		}

		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventMode, BlueprintID: "S1", Payload: p})
		require.True(t, ok)

		return entry.Content
	}

	assert.NotEqual(t, content(3, false), content(7, false), "a question round is named")
	assert.Equal(t, content(3, true), content(7, true))
	assert.NotEqual(t, content(3, true), content(3, false))
	assert.NotEqual(t, content(0, false), content(3, true), "the hand-over is said")
}

func TestLogEntryFor_ASignoffNamesTheCardsReadinessDidNotPass(t *testing.T) {
	payload := func(extra map[string]any) map[string]any {
		p := map[string]any{
			"version": 3, "cards": []any{}, "order": []any{},
			"cross": []any{map[string]any{"id": "X1", "severity": "major", "text": "Two cards disagree."}},
			"actions": []any{
				map[string]any{"label": "Approve over open findings", "reply": "approve", "feedback": true, "ask": "Why approve over the cards readiness did not pass?"},
				map[string]any{"label": "Reject", "reply": "reject", "feedback": true},
			},
		}
		maps.Copy(p, extra)

		return p
	}
	open := []any{"D3: Solution does not say the settings UI is dropped."}

	t.Run("the open cards come ahead of the cross findings", func(t *testing.T) {
		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: payload(map[string]any{"not_ready": open})})
		require.True(t, ok)
		assert.Contains(t, entry.Content, "**Sign-off needed.** Readiness still found cards not ready after its last round, so a human decides in every mode.")
		assert.NotContains(t, entry.Content, "Readiness passed the cards.")
		assert.Contains(t, entry.Content, "Readiness still finds these cards not ready:\n- D3: Solution does not say the settings UI is dropped.")
		assert.Less(t, strings.Index(entry.Content, "D3: Solution"), strings.Index(entry.Content, "Across the blueprint"))
		assert.Contains(t, entry.Content, "Reply `approve <reason>` to emit the cards over the open findings")
	})

	t.Run("the header wins in an auto mode", func(t *testing.T) {
		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: payload(map[string]any{"not_ready": open, "auto_stopped": true})})
		require.True(t, ok)
		assert.Contains(t, entry.Content, "so a human decides in every mode.")
		assert.NotContains(t, entry.Content, "preflight found something")
	})

	t.Run("a critical finding keeps its reply", func(t *testing.T) {
		cross := []any{map[string]any{"id": "X2", "severity": "critical", "text": "No criterion proves O1."}}
		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: payload(map[string]any{"not_ready": open, "cross": cross})})
		require.True(t, ok)
		assert.Contains(t, entry.Content, "over the critical findings")
		assert.NotContains(t, entry.Content, "over the open findings")
		assert.Contains(t, entry.Content, "so a human decides in every mode.")
	})

	t.Run("a plain sign-off says the architect could not settle it", func(t *testing.T) {
		entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Plain: true, Payload: payload(map[string]any{"not_ready": open})})
		require.True(t, ok)
		require.NotNil(t, entry.Prompt)
		assert.Contains(t, entry.Prompt.Text, "The architect could not settle every piece of the work on its own. A technical helper can read what is open below. Continue anyway and say why, or ask it to try again.")
		assert.NotContains(t, entry.Prompt.Text, "Your plan is ready")
		assert.Equal(t, "Continue anyway", entry.Prompt.Actions[0].Label)
	})

	t.Run("a payload without not_ready reads as before", func(t *testing.T) {
		for _, p := range []map[string]any{payload(nil), payload(map[string]any{"not_ready": []any{}})} {
			entry, ok := logEntryFor(blueprint.Event{Kind: blueprint.EventSignOff, BlueprintID: "S1", Payload: p})
			require.True(t, ok)
			assert.Contains(t, entry.Content, "**Sign-off.** Readiness passed the cards.")
			assert.NotContains(t, entry.Content, "not ready")
		}
	})
}
