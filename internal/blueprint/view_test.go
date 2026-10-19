package blueprint

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

const viewID = "BP1"

// newViewController builds a controller on the dialog pack over a store the
// test fills itself, with BP1 in mode under capUSD. Nothing runs and no
// session is attached, as after End or a restart.
func newViewController(t *testing.T, mode string, capUSD float64) (*Controller, *store.Store) {
	t.Helper()

	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	pk, err := pack.LoadDir("testdata/dialog")
	require.NoError(t, err)

	now := time.Now().UTC()
	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
		ID: viewID, Projects: []string{"alpha"}, Mode: mode, CostCapUSD: capUSD, Model: "session/model",
		State: string(StateIdle), CreatedAt: now, UpdatedAt: now,
	}))

	return New(Options{Store: st, Pack: pk}), st
}

// ran records a completed run and returns its id.
func ran(t *testing.T, st *store.Store, stageID, kind string, attempt, round int, cost float64) int64 {
	t.Helper()

	ctx := context.Background()

	id, err := st.StartRun(ctx, store.Run{
		BlueprintID: viewID, Stage: stageID, Kind: kind, Attempt: attempt, Round: round,
		Status: store.RunRunning, Model: "session/model", StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{CostUSD: cost}, time.Now().UTC()))

	return id
}

// flying records a run of stageID still running and its flight, which lands
// when the test ends.
func flying(t *testing.T, c *Controller, st *store.Store, stageID, kind string, round int) *flight {
	t.Helper()

	id, err := st.StartRun(context.Background(), store.Run{
		BlueprintID: viewID, Stage: stageID, Kind: kind, Attempt: 1, Round: round,
		Status: store.RunRunning, Model: "session/model", StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	persona := ""

	if s, ok := c.opts.Pack.Stage(stageID); ok {
		persona = s.Persona
		if kind == store.RunKindReviewer && s.Reviewer != nil {
			persona = s.Reviewer.Persona
		}
	}

	f := &flight{runID: id, stage: stageID, kind: kind, persona: persona, model: "session/model", round: round, startedAt: time.Now().UTC()}
	t.Cleanup(c.fly(viewID, f))

	return f
}

// put stores the next version of stageID's artifact and returns it.
func put(t *testing.T, st *store.Store, stageID string, data map[string]any) int {
	t.Helper()

	var raw json.RawMessage

	if data != nil {
		b, err := json.Marshal(data)
		require.NoError(t, err)

		raw = b
	}

	v, err := st.PutArtifact(context.Background(), store.Artifact{BlueprintID: viewID, Stage: stageID, Body: "# " + stageID, Data: raw})
	require.NoError(t, err)

	return v
}

func gateRow(t *testing.T, st *store.Store, stageID string, version int, decision, responder, feedback string) {
	t.Helper()

	_, err := st.AddGate(context.Background(), store.Gate{
		BlueprintID: viewID, Stage: stageID, Version: version, Decision: decision,
		Feedback: feedback, Responder: responder, DecidedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
}

func appendEvent(t *testing.T, st *store.Store, kind string, payload map[string]any) {
	t.Helper()

	b, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, st.AppendEvent(context.Background(), store.Event{Time: time.Now().UTC(), BlueprintID: viewID, Kind: kind, Payload: b}))
}

// stageData is a stage's minimal data: slicing with two slices, so it never
// folds, and ready cards.
func stageData(stageID string) map[string]any {
	switch stageID {
	case "slicing":
		return map[string]any{"slices": []any{map[string]any{"id": "SL1"}, map[string]any{"id": "SL2"}}}
	case "decomposition":
		return map[string]any{"cards": []any{map[string]any{"id": "D1"}}}
	case "readiness":
		return map[string]any{"cards": []any{map[string]any{"id": "D1", "ready": true}}}
	default:
		return nil
	}
}

// through completes every pack stage up to and including last: a done run
// costing 0.1, an artifact and, at a gate, the human's approval.
func through(t *testing.T, c *Controller, st *store.Store, last string) {
	t.Helper()

	for _, s := range c.opts.Pack.Stages {
		ran(t, st, s.ID, store.RunKindStage, 1, 1, 0.1)
		v := put(t, st, s.ID, stageData(s.ID))

		if s.Gate {
			gateRow(t, st, s.ID, v, store.GateApprove, store.ResponderHuman, "")
		}

		if s.ID == last {
			return
		}
	}
}

func view(t *testing.T, c *Controller) protocol.BlueprintView {
	t.Helper()

	v, err := c.View(context.Background(), viewID)
	require.NoError(t, err)

	return v
}

func stepOf(t *testing.T, v protocol.BlueprintView, stageID string) protocol.PipelineStep {
	t.Helper()

	for _, s := range v.Pipeline {
		if s.Stage == stageID {
			return s
		}
	}

	t.Fatalf("no step %s in %+v", stageID, v.Pipeline)

	return protocol.PipelineStep{}
}

func refOf(t *testing.T, v protocol.BlueprintView, stageID string, version int) protocol.ArtifactRef {
	t.Helper()

	for _, r := range v.Artifacts {
		if r.Stage == stageID && r.Version == version {
			return r
		}
	}

	t.Fatalf("no artifact %s v%d in %+v", stageID, version, v.Artifacts)

	return protocol.ArtifactRef{}
}

// newSlicedView is newViewController on the sliced test pack, in default
// mode with no cap.
func newSlicedView(t *testing.T) (*Controller, *store.Store) {
	t.Helper()

	c, st := newViewController(t, "default", 0)
	c.opts.Pack = slicedPack(t)

	return c, st
}

// gatedCards gives pk's decomposition, the loop's target, a reviewed gate a
// human decides, in memory only: a pack may gate it, the sliced pack does
// not.
func gatedCards(pk *pack.Pack) *pack.Pack {
	for i := range pk.Stages {
		if pk.Stages[i].ID == "decomposition" {
			pk.Stages[i].Gate, pk.Stages[i].GatePolicy = true, pack.PolicyHuman
			pk.Stages[i].Reviewer = &pack.Reviewer{Persona: "senior_developer", Threshold: 4}
		}
	}

	return pk
}

// chainAtGate leaves BP1 on the gated-cards pack mid-chain: readiness sent
// SL1's cards back, and their new version, reviewed, waits at its gate.
func chainAtGate(t *testing.T, c *Controller, st *store.Store) {
	t.Helper()

	gatedCards(c.opts.Pack)
	throughSliced(t, st, "decomposition@SL2")
	gateRow(t, st, "decomposition@SL1", 1, store.GateApprove, store.ResponderHuman, "")
	gateRow(t, st, "decomposition@SL2", 1, store.GateApprove, store.ResponderHuman, "")
	ran(t, st, "readiness", store.RunKindStage, 1, 1, 0.1)
	put(t, st, "readiness", readinessFor(false, "D1"))
	ran(t, st, "decomposition@SL1", store.RunKindStage, 2, 1, 0.1)
	v := put(t, st, "decomposition@SL1", draftOf("", cardFor("D1", nil)))
	put(t, st, reviewStage("decomposition@SL1"), map[string]any{"score": 5, "findings": []any{}, "version": v, "attempt": 2, "round": 1})
	appendEvent(t, st, EventGate, map[string]any{"stage": "decomposition@SL1", "version": v})
}

// throughSliced completes BP1's steps on the sliced pack with two slices,
// up to and including last: a done run, an artifact and, at a gate, the
// human's approval.
func throughSliced(t *testing.T, st *store.Store, last string) {
	t.Helper()

	data := map[string]map[string]any{
		"slicing":           twoSlices(),
		"decomposition@SL1": draftOf("", cardFor("D1", nil)),
		"decomposition@SL2": draftOf("Export the reports", cardFor("D2", nil, "D1")),
		"readiness":         readinessFor(true, "D1", "D2"),
	}

	gated := map[string]bool{
		"slicing": true, "shared_decisions": true, "solution_design@SL1": true, "acceptance@SL1": true,
		"solution_design@SL2": true, "acceptance@SL2": true,
	}

	for _, key := range []string{
		"intake", "slicing", "shared_decisions",
		"solution_design@SL1", "grounding@SL1", "acceptance@SL1", "decomposition@SL1",
		"solution_design@SL2", "grounding@SL2", "acceptance@SL2", "decomposition@SL2", "readiness",
	} {
		ran(t, st, key, store.RunKindStage, 1, 1, 0.1)
		v := put(t, st, key, data[key])

		if gated[key] {
			gateRow(t, st, key, v, store.GateApprove, store.ResponderHuman, "")
		}

		if key == last {
			return
		}
	}
}

// sliceStep is the view's step of stage for slice.
func sliceStep(t *testing.T, v protocol.BlueprintView, stage, slice string) protocol.PipelineStep {
	t.Helper()

	for _, s := range v.Pipeline {
		if s.Stage == stage && s.Slice == slice {
			return s
		}
	}

	t.Fatalf("no step %s for %q in %+v", stage, slice, v.Pipeline)

	return protocol.PipelineStep{}
}

// sliceRef is the view's ref of stage for slice at version.
func sliceRef(t *testing.T, v protocol.BlueprintView, stage, slice string, version int) protocol.ArtifactRef {
	t.Helper()

	for _, r := range v.Artifacts {
		if r.Stage == stage && r.Slice == slice && r.Version == version {
			return r
		}
	}

	t.Fatalf("no ref %s %q v%d in %+v", stage, slice, version, v.Artifacts)

	return protocol.ArtifactRef{}
}

func TestView_AnUnknownBlueprintIsNotFound(t *testing.T) {
	c, _ := newViewController(t, "default", 0)

	_, err := c.View(context.Background(), "nosuch")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestView_AFreshBlueprintListsEveryStepAndNoNulls(t *testing.T) {
	c, _ := newViewController(t, "default", 0)

	v := view(t, c)

	assert.Equal(t, viewID, v.ID)
	assert.Equal(t, "default", v.Mode)

	var ids []string
	for _, s := range v.Pipeline {
		ids = append(ids, s.Stage)
	}

	assert.Equal(t, []string{"intake", "slicing", "solution_design", "grounding", "acceptance", "decomposition", "readiness", "signoff", "emission"}, ids)

	for _, s := range v.Pipeline {
		assert.Equal(t, stepPending, s.Status, s.Stage)
	}

	slicing := stepOf(t, v, "slicing")
	assert.True(t, slicing.Gate)
	assert.Equal(t, "solution_design", slicing.FoldsInto)
	assert.Equal(t, "human", slicing.Policy)
	assert.Equal(t, "product_owner", slicing.Persona)
	assert.Equal(t, "auto", stepOf(t, v, "acceptance").Policy)
	assert.Equal(t, 3, stepOf(t, v, "readiness").MaxRounds)

	b, err := json.Marshal(v)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"artifacts":[]`)
	assert.NotContains(t, string(b), `null`)
	assert.Nil(t, v.Current)
	assert.Nil(t, v.Open)
}

func TestView_APackWithoutCardsHasNoSignoffOrBoard(t *testing.T) {
	c, st, _, _ := newGated(t, gatedScript())

	now := time.Now().UTC()
	require.NoError(t, st.CreateBlueprint(context.Background(), store.Blueprint{ID: viewID, Mode: "default", State: "idle", CreatedAt: now, UpdatedAt: now}))

	var ids []string
	for _, s := range view(t, c).Pipeline {
		ids = append(ids, s.Stage)
	}

	assert.Equal(t, []string{"intake", "shape", "plan"}, ids)
}

func TestView_States(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, c *Controller, st *store.Store)
		check func(t *testing.T, v protocol.BlueprintView)
	}{
		{
			name: "at gate",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 5, "findings": []any{}, "version": 1, "attempt": 1, "round": 1})
				appendEvent(t, st, EventGate, map[string]any{"stage": "slicing", "version": 1})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepDone, stepOf(t, v, "intake").Status)
				assert.Equal(t, stepWaiting, stepOf(t, v, "slicing").Status)
				assert.Equal(t, &protocol.OpenPrompt{PromptID: "gate:slicing:v1", Kind: promptGate}, v.Open)

				r := refOf(t, v, "slicing", 1)
				assert.Equal(t, artAtGate, r.Status)
				require.NotNil(t, r.Review)
				assert.Equal(t, "system_designer", r.Review.Reviewer)
				assert.Equal(t, 5, r.Review.Score)
				assert.Equal(t, 4, r.Review.Threshold)
				assert.NotNil(t, r.Review.Findings, "findings are [] not null")
				assert.Empty(t, r.Review.Findings)
				assert.InDelta(t, 0.15, stepOf(t, v, "slicing").CostUSD, 1e-9, "the reviewer's run counts for its stage")
			},
		},
		{
			name: "reviewing",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				flying(t, c, st, "slicing", store.RunKindReviewer, 1)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepReviewing, stepOf(t, v, "slicing").Status)
				assert.Equal(t, artReviewing, refOf(t, v, "slicing", 1).Status)
				require.NotNil(t, v.Current)
				assert.Equal(t, store.RunKindReviewer, v.Current.Kind)
				assert.Equal(t, "system_designer", v.Current.Persona)
				assert.Nil(t, v.Open)
			},
		},
		{
			name: "revising",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 4, "findings": []any{
					map[string]any{"id": "R1", "severity": "major", "text": "name the owner", "status": "open"},
					map[string]any{"id": "R2", "severity": "minor", "text": "split it", "status": "open"},
				}, "version": 1, "attempt": 1, "round": 1})
				flying(t, c, st, "slicing", store.RunKindStage, 2)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepRevising, stepOf(t, v, "slicing").Status)

				r := refOf(t, v, "slicing", 1)
				require.NotNil(t, r.Review)
				require.Len(t, r.Review.Findings, 2)
				assert.True(t, r.Review.Passed)
				assert.Equal(t, 1, r.Review.Round)
				assert.Equal(t, "R1", r.Review.Findings[0].ID)
				assert.Equal(t, "minor", r.Review.Findings[1].Severity)
				assert.Equal(t, "open", r.Review.Findings[1].Status)

				for _, f := range r.Review.Findings {
					assert.Equal(t, 2, f.SentTo, "every finding went to the revision")
					assert.Zero(t, f.AddressedIn, "nothing is addressed before the revision says so")
				}
			},
		},
		{
			name: "revised with what it addressed",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 4, "findings": []any{
					map[string]any{"id": "R1", "severity": "major", "text": "name the owner", "status": "open"},
					map[string]any{"id": "R2", "severity": "minor", "text": "split it", "status": "open"},
				}, "version": 1, "attempt": 1, "round": 1})
				ran(t, st, "slicing", store.RunKindStage, 1, 2, 0.1)

				data := stageData("slicing")
				data["addressed"] = []any{map[string]any{"finding": "R1", "note": "named"}, map[string]any{"finding": "R9", "note": "bogus"}}
				put(t, st, "slicing", data)
				appendEvent(t, st, EventGate, map[string]any{"stage": "slicing", "version": 2})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				r := refOf(t, v, "slicing", 1)
				assert.Equal(t, artSuperseded, r.Status)
				require.NotNil(t, r.Review)
				assert.Equal(t, protocol.ReviewFinding{ID: "R1", Severity: "major", Text: "name the owner", Status: "open", SentTo: 2, AddressedIn: 2, AddressedBy: "product_owner", AddressedNote: "named"}, r.Review.Findings[0])
				assert.Equal(t, protocol.ReviewFinding{ID: "R2", Severity: "minor", Text: "split it", Status: "open", SentTo: 2}, r.Review.Findings[1], "a finding the revision did not list is only sent")

				revised := refOf(t, v, "slicing", 2)
				assert.Equal(t, artAtGate, revised.Status)
				assert.Nil(t, revised.Review, "the revision has no verdict yet")
				assert.Equal(t, 2, stepOf(t, v, "slicing").LatestVersion)
			},
		},
		{
			name: "a re-checked finding keeps its id in the later review",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 4, "findings": []any{
					map[string]any{"id": "R1", "severity": "critical", "text": "no owner", "status": "open"},
				}, "version": 1, "attempt": 1, "round": 1})
				ran(t, st, "slicing", store.RunKindStage, 1, 2, 0.1)

				data := stageData("slicing")
				data["addressed"] = []any{map[string]any{"finding": "R1", "note": "named the owner"}}
				put(t, st, "slicing", data)
				ran(t, st, "slicing", store.RunKindReviewer, 1, 2, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 5, "findings": []any{
					map[string]any{"id": "R1", "severity": "critical", "text": "no owner", "status": "fixed", "verified_in": 2},
				}, "version": 2, "attempt": 1, "round": 2})
				appendEvent(t, st, EventGate, map[string]any{"stage": "slicing", "version": 2})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				first := refOf(t, v, "slicing", 1).Review
				require.NotNil(t, first)
				assert.False(t, first.Passed, "a critical finding left open fails a review at the threshold")
				assert.Equal(t, 1, first.Round)
				assert.Equal(t, []protocol.ReviewFinding{{
					ID: "R1", Severity: "critical", Text: "no owner", Status: "open",
					SentTo: 2, AddressedIn: 2, AddressedBy: "product_owner", AddressedNote: "named the owner",
				}}, first.Findings, "the claim sits where the finding was sent")

				second := refOf(t, v, "slicing", 2).Review
				require.NotNil(t, second)
				assert.True(t, second.Passed)
				assert.Equal(t, 2, second.Round)
				assert.Equal(t, []protocol.ReviewFinding{{ID: "R1", Severity: "critical", Text: "no owner", Status: "fixed", VerifiedIn: 2}},
					second.Findings, "the later review keeps the id with its verdict")
			},
		},
		{
			name: "a verdict stored before the ledger",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 4, "findings": []any{"name the owner"}, "version": 1, "attempt": 1, "round": 1})
				appendEvent(t, st, EventGate, map[string]any{"stage": "slicing", "version": 1})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				r := refOf(t, v, "slicing", 1).Review
				require.NotNil(t, r)
				assert.True(t, r.Passed)
				assert.Equal(t, 1, r.Round)
				assert.False(t, r.SentBack, "its gate was shown, so nothing was sent back")
				assert.Equal(t, []protocol.ReviewFinding{{ID: "R1", Severity: "major", Text: "name the owner", Status: "open"}}, r.Findings)
			},
		},
		{
			name: "a verdict stored without its round",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 4, "findings": []any{"name the owner"}, "version": 1, "attempt": 1})
				appendEvent(t, st, EventGate, map[string]any{"stage": "slicing", "version": 1})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				r := refOf(t, v, "slicing", 1).Review
				require.NotNil(t, r)
				assert.True(t, r.Passed)
				assert.Equal(t, 1, r.Round)
			},
		},
		{
			name: "folded",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", map[string]any{"slices": []any{map[string]any{"id": "SL1"}}})
				gateRow(t, st, "slicing", 1, store.GateFold, store.ResponderAuto, "solution_design")
				flying(t, c, st, "solution_design", store.RunKindStage, 1)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepFolded, stepOf(t, v, "slicing").Status)
				assert.Equal(t, artFolded, refOf(t, v, "slicing", 1).Status)
				assert.Equal(t, stepRunning, stepOf(t, v, "solution_design").Status)
			},
		},
		{
			name: "rejected and redrawn",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				put(t, st, reviewStage("slicing"), map[string]any{"score": 5, "findings": []any{}, "version": 1, "attempt": 1, "round": 1})
				gateRow(t, st, "slicing", 1, store.GateReject, store.ResponderHuman, "smaller")
				flying(t, c, st, "slicing", store.RunKindStage, 1)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				r := refOf(t, v, "slicing", 1)
				assert.Equal(t, artRejected, r.Status)
				assert.Equal(t, byYou, r.DecidedBy)
				require.NotNil(t, r.DecidedAt)
				assert.Equal(t, stepRunning, stepOf(t, v, "slicing").Status)
			},
		},
		{
			name: "failed",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "solution_design")

				id, err := st.StartRun(context.Background(), store.Run{BlueprintID: viewID, Stage: "grounding", Kind: store.RunKindStage, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: time.Now().UTC()})
				require.NoError(t, err)
				require.NoError(t, st.EndRun(context.Background(), id, store.RunFailed, "error", store.RunUsage{}, time.Now().UTC()))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepFailed, stepOf(t, v, "grounding").Status)
				assert.Equal(t, stepDone, stepOf(t, v, "solution_design").Status)
				assert.Equal(t, artApproved, refOf(t, v, "solution_design", 1).Status)
			},
		},
		{
			name: "superseded by a rejection of the stage folded into its gate",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", map[string]any{"slices": []any{map[string]any{"id": "SL1"}}})
				gateRow(t, st, "slicing", 1, store.GateFold, store.ResponderAuto, "solution_design")
				ran(t, st, "solution_design", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "solution_design", nil)
				appendEvent(t, st, EventGate, map[string]any{"stage": "solution_design", "version": 1})
				gateRow(t, st, "slicing", 1, store.GateReject, store.ResponderHuman, "merge them")
				gateRow(t, st, "solution_design", 1, store.GateSuperseded, store.ResponderHuman, "slicing")
				flying(t, c, st, "slicing", store.RunKindStage, 1)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				d := refOf(t, v, "solution_design", 1)
				assert.Equal(t, artSuperseded, d.Status)
				assert.Empty(t, d.DecidedBy)
				assert.Nil(t, d.DecidedAt)
				assert.Equal(t, artRejected, refOf(t, v, "slicing", 1).Status)
				assert.Equal(t, stepRunning, stepOf(t, v, "slicing").Status)
				assert.Equal(t, stepPending, stepOf(t, v, "solution_design").Status)
				assert.Nil(t, v.Open)
			},
		},
		{
			name: "a failed review leaves the version current",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))

				id, err := st.StartRun(context.Background(), store.Run{BlueprintID: viewID, Stage: "slicing", Kind: store.RunKindReviewer, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: time.Now().UTC()})
				require.NoError(t, err)
				require.NoError(t, st.EndRun(context.Background(), id, store.RunFailed, "error", store.RunUsage{}, time.Now().UTC()))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepFailed, stepOf(t, v, "slicing").Status)
				assert.Equal(t, artCurrent, refOf(t, v, "slicing", 1).Status)
				assert.Nil(t, v.Open)
			},
		},
		{
			name: "a failed revision leaves the reviewed version current",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				put(t, st, reviewStage("slicing"), map[string]any{"score": 4, "findings": []any{"name the owner"}, "version": 1, "attempt": 1, "round": 1})

				id, err := st.StartRun(context.Background(), store.Run{BlueprintID: viewID, Stage: "slicing", Kind: store.RunKindStage, Attempt: 1, Round: 2, Status: store.RunRunning, StartedAt: time.Now().UTC()})
				require.NoError(t, err)
				require.NoError(t, st.EndRun(context.Background(), id, store.RunFailed, "error", store.RunUsage{}, time.Now().UTC()))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepFailed, stepOf(t, v, "slicing").Status)
				assert.Equal(t, artCurrent, refOf(t, v, "slicing", 1).Status)
			},
		},
		{
			name: "a review that ended without a verdict leaves the version current",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", stageData("slicing"))
				ran(t, st, "slicing", store.RunKindReviewer, 1, 1, 0.05)
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateFailed)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepFailed, stepOf(t, v, "slicing").Status)
				assert.Equal(t, artCurrent, refOf(t, v, "slicing", 1).Status)
			},
		},
		{
			name: "readiness round 2",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "decomposition")
				ran(t, st, "readiness", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "readiness", map[string]any{"cards": []any{map[string]any{"id": "D1", "ready": false, "findings": []any{"too big"}}}})
				ran(t, st, "decomposition", store.RunKindStage, 2, 1, 0.1)
				put(t, st, "decomposition", stageData("decomposition"))
				flying(t, c, st, "readiness", store.RunKindStage, 1)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				r := stepOf(t, v, "readiness")
				assert.Equal(t, stepRunning, r.Status)
				assert.Equal(t, 2, r.Round)
				assert.Equal(t, 3, r.MaxRounds)
				assert.Equal(t, 1, r.LatestVersion)
				assert.Equal(t, 2, stepOf(t, v, "decomposition").Attempt)
				assert.Equal(t, stepPending, stepOf(t, v, "signoff").Status)
			},
		},
		{
			name: "at the sign-off",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				appendEvent(t, st, EventSignOff, map[string]any{"version": 1, "cards": []any{}})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepWaiting, stepOf(t, v, "signoff").Status)
				assert.Equal(t, 1, stepOf(t, v, "signoff").LatestVersion)
				assert.Equal(t, &protocol.OpenPrompt{PromptID: "gate:signoff:v1", Kind: promptGate}, v.Open)
				assert.Equal(t, artAtGate, refOf(t, v, "signoff", 1).Status)
				assert.Equal(t, stepPending, stepOf(t, v, "emission").Status)
			},
		},
		{
			name: "emitted",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				appendEvent(t, st, EventSignOff, map[string]any{"version": 1, "cards": []any{}})
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderHuman, "")
				appendEvent(t, st, EventEmitted, map[string]any{"cards": []any{map[string]any{"draft": "D1", "project": "alpha", "card": "ALPHA-1", "title": "T"}}})
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateEmitted)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, string(StateEmitted), v.State)
				assert.Equal(t, stepDone, stepOf(t, v, "signoff").Status)
				assert.Equal(t, stepDone, stepOf(t, v, "emission").Status)
				assert.Equal(t, 1, stepOf(t, v, "emission").LatestVersion)

				so := refOf(t, v, "signoff", 1)
				assert.Equal(t, artApproved, so.Status)
				assert.Equal(t, byYou, so.DecidedBy)
				assert.Equal(t, artCurrent, refOf(t, v, "emission", 1).Status)
				assert.Nil(t, v.Open)
			},
		},
		{
			name: "an automatic approval recorded before sign-offs were",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderAuto, "")
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateEmitting)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepDone, stepOf(t, v, "signoff").Status)
				assert.Equal(t, stepRunning, stepOf(t, v, "emission").Status)

				for _, r := range v.Artifacts {
					assert.NotEqual(t, signoffStage, r.Stage, "a sign-off with no record is no artifact")
				}
			},
		},
		{
			name: "a stopped emission waits for approve",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				appendEvent(t, st, EventSignOff, map[string]any{"version": 1})
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderHuman, "")
				appendEvent(t, st, EventError, map[string]any{"stage": "emission", "message": "blocked", "resume": "emission", "version": 1})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, &protocol.OpenPrompt{PromptID: "emission:v1", Kind: promptGate}, v.Open)
				assert.Equal(t, stepWaiting, stepOf(t, v, "emission").Status)
			},
		},
		{
			name: "an approved resume closes the prompt",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderHuman, "")
				appendEvent(t, st, EventError, map[string]any{"stage": "emission", "message": "blocked", "resume": "emission", "version": 1})
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateEmitting)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Nil(t, v.Open)
				assert.Equal(t, stepRunning, stepOf(t, v, "emission").Status)
			},
		},
		{
			name: "a held run start waits after the cards exist",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderAuto, "")
				appendEvent(t, st, EventEmitted, map[string]any{"cards": []any{}})
				appendEvent(t, st, EventAwaitInput, map[string]any{"stage": "signoff", "prompt": "commit first", "resume": "held", "version": 1})
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateEmitted)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, &protocol.OpenPrompt{PromptID: "emission:v1", Kind: promptGate}, v.Open)
				assert.Equal(t, stepDone, stepOf(t, v, "emission").Status)
			},
		},
		{
			name: "a run start after the prompt closes it",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderAuto, "")
				appendEvent(t, st, EventEmitted, map[string]any{"cards": []any{}})
				appendEvent(t, st, EventAwaitInput, map[string]any{"stage": "signoff", "resume": "held", "version": 1})
				appendEvent(t, st, EventRunStarted, map[string]any{"kind": "card", "target": "ALPHA-1"})
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateEmitted)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Nil(t, v.Open)
			},
		},
		{
			name: "a stage gate the auto policy decided",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "intake")
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				v := put(t, st, "slicing", stageData("slicing"))
				gateRow(t, st, "slicing", v, store.GateApprove, store.ResponderAuto, "")
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				r := refOf(t, v, "slicing", 1)
				assert.Equal(t, artApproved, r.Status)
				assert.Equal(t, byAutomatic, r.DecidedBy, "the reviewer's verdict decided it")
				assert.Equal(t, []protocol.BlueprintSlice{}, v.Slices, "a pack with no per-slice stage repeats no step")
			},
		},
		{
			name: "a sign-off an emitting mode approved",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				appendEvent(t, st, EventSignOff, map[string]any{"version": 1, "cards": []any{}})
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderAuto, "")
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				r := refOf(t, v, "signoff", 1)
				assert.Equal(t, artApproved, r.Status)
				assert.Equal(t, byAutomatic, r.DecidedBy)
			},
		},
		{
			name: "a failed sign-off",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateFailed)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepFailed, stepOf(t, v, "signoff").Status)
				assert.Equal(t, stepPending, stepOf(t, v, "emission").Status)
			},
		},
		{
			name: "a failed emission",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "readiness")
				appendEvent(t, st, EventSignOff, map[string]any{"version": 1, "cards": []any{}})
				gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderHuman, "")
				require.NoError(t, st.SetBlueprintState(context.Background(), viewID, string(StateFailed)))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepDone, stepOf(t, v, "signoff").Status)
				assert.Equal(t, stepFailed, stepOf(t, v, "emission").Status)
			},
		},
		{
			name: "ended not ready",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				through(t, c, st, "decomposition")

				for round := 1; round <= 3; round++ {
					ran(t, st, "readiness", store.RunKindStage, round, 1, 0.1)
					put(t, st, "readiness", map[string]any{"cards": []any{map[string]any{"id": "D1", "ready": false, "findings": []any{"no"}}}})

					if round < 3 {
						ran(t, st, "decomposition", store.RunKindStage, round+1, 1, 0.1)
						put(t, st, "decomposition", stageData("decomposition"))
					}
				}
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepDone, stepOf(t, v, "readiness").Status)
				assert.Equal(t, stepSkipped, stepOf(t, v, "signoff").Status)
				assert.Equal(t, stepSkipped, stepOf(t, v, "emission").Status)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st := newViewController(t, "default", 0)
			tt.setup(t, c, st)
			tt.check(t, view(t, c))
		})
	}
}

func TestView_Cost(t *testing.T) {
	tests := []struct {
		name   string
		capUSD float64
		setup  func(t *testing.T, c *Controller, st *store.Store)
		want   protocol.BlueprintCost
		steps  map[string]float64
	}{
		{
			name:   "the ledger plus the run in flight, under a cap",
			capUSD: 2,
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				ran(t, st, "intake", store.RunKindStage, 1, 1, 0.25)
				put(t, st, "intake", nil)

				f := flying(t, c, st, "slicing", store.RunKindStage, 1)
				f.price, f.capUSD, f.spent = testPrice(), 1, 0.1
			},
			want:  protocol.BlueprintCost{SpentUSD: 0.35, RunSpentUSD: 0.1, CapUSD: 2, RunCapUSD: 1, Priced: true},
			steps: map[string]float64{"intake": 0.25, "slicing": 0.1},
		},
		{
			name: "no cap",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				ran(t, st, "intake", store.RunKindStage, 1, 1, 0.25)
			},
			want: protocol.BlueprintCost{SpentUSD: 0.25, Priced: true},
		},
		{
			name: "an unpriced run that ended",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				ran(t, st, "intake", store.RunKindStage, 1, 1, 0.25)
				appendEvent(t, st, EventStageEnded, map[string]any{"stage": "intake", "kind": "stage", "status": "done", "cost_usd": 0.25, "priced": false})
			},
			want: protocol.BlueprintCost{SpentUSD: 0.25, Priced: false},
		},
		{
			name: "an unpriced run in flight",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				flying(t, c, st, "intake", store.RunKindStage, 1).spent = 0.05
			},
			want: protocol.BlueprintCost{SpentUSD: 0.05, RunSpentUSD: 0.05, Priced: false},
		},
		{
			name: "a responder in flight spends against the asking run's cap",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				f := flying(t, c, st, "intake", store.RunKindStage, 1)
				f.price, f.capUSD, f.spent, f.charged = testPrice(), 1, 0.2, 0.1

				r := flying(t, c, st, "intake", store.RunKindResponder, 1)
				r.price, r.spent = testPrice(), 0.05
			},
			want: protocol.BlueprintCost{SpentUSD: 0.25, RunSpentUSD: 0.35, RunCapUSD: 1, Priced: true},
		},
		{
			name: "a run whose end is recorded counts once",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				f := flying(t, c, st, "intake", store.RunKindStage, 1)
				f.price, f.spent = testPrice(), 0.3
				require.NoError(t, st.EndRun(context.Background(), f.runID, store.RunDone, "done", store.RunUsage{CostUSD: 0.3}, time.Now().UTC()))
			},
			want: protocol.BlueprintCost{SpentUSD: 0.3, Priced: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st := newViewController(t, "default", tt.capUSD)
			tt.setup(t, c, st)

			v := view(t, c)
			assert.InDelta(t, tt.want.SpentUSD, v.Cost.SpentUSD, 1e-9)
			assert.InDelta(t, tt.want.RunSpentUSD, v.Cost.RunSpentUSD, 1e-9)
			assert.InDelta(t, tt.want.CapUSD, v.Cost.CapUSD, 1e-9)
			assert.InDelta(t, tt.want.RunCapUSD, v.Cost.RunCapUSD, 1e-9)
			assert.Equal(t, tt.want.Priced, v.Cost.Priced)

			for stageID, cost := range tt.steps {
				assert.InDelta(t, cost, stepOf(t, v, stageID).CostUSD, 1e-9, stageID)
			}
		})
	}
}

func TestView_SeqRisesWithEveryChange(t *testing.T) {
	c, st := newViewController(t, "default", 0)
	ctx := context.Background()

	before := view(t, c)

	last, err := st.LastEventID(ctx, viewID)
	require.NoError(t, err)
	assert.Equal(t, uint64(last), before.Seq)

	c.setState(ctx, viewID, StateRunning)

	after := view(t, c)
	assert.Greater(t, after.Seq, before.Seq, "a state change alone raises the sequence")
	assert.Equal(t, string(StateRunning), after.State)
}

func TestView_AnUnplaceableRecordStillHasAView(t *testing.T) {
	c, st := newViewController(t, "default", 0)

	// A stage the pack does not have completed last: the position cannot be
	// read, so every step is pending, but the artifacts are there.
	ran(t, st, "vanished", store.RunKindStage, 1, 1, 0.1)
	put(t, st, "intake", nil)

	v := view(t, c)

	for _, s := range v.Pipeline {
		assert.Equal(t, stepPending, s.Status, s.Stage)
	}

	assert.Equal(t, artCurrent, refOf(t, v, "intake", 1).Status)
}

func TestView_Slices(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, c *Controller, st *store.Store)
		check func(t *testing.T, v protocol.BlueprintView)
	}{
		{
			name: "two slices tag every per-slice step and list the slices",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				throughSliced(t, st, "shared_decisions")
				ran(t, st, "solution_design@SL1", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "solution_design@SL1", nil)
				put(t, st, reviewStage("solution_design@SL1"), map[string]any{"score": 5, "findings": []any{}, "version": 1, "attempt": 1, "round": 1})
				appendEvent(t, st, EventGate, map[string]any{"stage": "solution_design@SL1", "version": 1})
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, []protocol.BlueprintSlice{{ID: "SL1", Title: "Ship the endpoint"}, {ID: "SL2", Title: "Show it in the UI"}}, v.Slices)
				assert.Equal(t, stepDone, sliceStep(t, v, "shared_decisions", "").Status)
				assert.Equal(t, stepWaiting, sliceStep(t, v, "solution_design", "SL1").Status)
				assert.Equal(t, stepPending, sliceStep(t, v, "solution_design", "SL2").Status)
				require.NotNil(t, v.Open)
				assert.Equal(t, "gate:solution_design:SL1:v1", v.Open.PromptID)
				assert.Equal(t, artAtGate, sliceRef(t, v, "solution_design", "SL1", 1).Status)
				assert.Equal(t, 5, sliceRef(t, v, "solution_design", "SL1", 1).Review.Score, "a slice's review is its own")
			},
		},
		{
			name: "one slice skips the shared stage and still tags SL1",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				ran(t, st, "intake", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "intake", nil)
				ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "slicing", oneSlice())
				gateRow(t, st, "slicing", 1, store.GateFold, store.ResponderAuto, "solution_design@SL1")
				gateRow(t, st, "shared_decisions", 0, store.GateFold, store.ResponderAuto, "solution_design@SL1")
				ran(t, st, "solution_design@SL1", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "solution_design@SL1", nil)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Len(t, v.Slices, 1, "one slice is sent; consumers show nothing slice-related below two")
				assert.Equal(t, stepSkipped, sliceStep(t, v, "shared_decisions", "").Status, "its collapse row at version 0 is not a fold")
				assert.Equal(t, stepFolded, sliceStep(t, v, "slicing", "").Status)
				assert.Equal(t, "solution_design", sliceStep(t, v, "slicing", "").FoldsInto)
				sliceStep(t, v, "solution_design", "SL1")
			},
		},
		{
			name: "a record from before slices",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				for _, s := range []string{"intake", "slicing", "solution_design"} {
					ran(t, st, s, store.RunKindStage, 1, 1, 0.1)
					put(t, st, s, stageData(s))
				}
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, []protocol.BlueprintSlice{}, v.Slices, "no slices, though its slicing holds two")
				assert.Empty(t, sliceStep(t, v, "solution_design", "").Slice)
				assert.Equal(t, stepSkipped, sliceStep(t, v, "shared_decisions", "").Status)
			},
		},
		{
			name: "before slicing stores a version",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				ran(t, st, "intake", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "intake", nil)
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, []protocol.BlueprintSlice{}, v.Slices)
				assert.Equal(t, stepPending, sliceStep(t, v, "solution_design", "").Status)
				assert.Equal(t, stepPending, sliceStep(t, v, "shared_decisions", "").Status, "nothing is skipped before slicing decides")
			},
		},
		{
			name: "a slice the newest slicing dropped",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				throughSliced(t, st, "solution_design@SL2")
				ran(t, st, "slicing", store.RunKindStage, 2, 1, 0.1)
				put(t, st, "slicing", oneSlice())
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Len(t, v.Slices, 1)
				dropped := sliceRef(t, v, "solution_design", "SL2", 1)
				assert.Equal(t, artSuperseded, dropped.Status)
				assert.Equal(t, artSuperseded, sliceRef(t, v, "shared_decisions", "", 1).Status, "a collapsed stage's approved version")

				last := v.Artifacts[len(v.Artifacts)-1]
				assert.Equal(t, "SL2", last.Slice, "a dropped slice's artifacts follow the steps'")
			},
		},
		{
			name: "the chain shows the later slices' steps done",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				throughSliced(t, st, "decomposition@SL2")
				ran(t, st, "readiness", store.RunKindStage, 1, 1, 0.1)
				put(t, st, "readiness", readinessFor(false, "D1"))
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepPending, sliceStep(t, v, "decomposition", "SL1").Status, "the chain starts at the slice that owns the card")
				assert.Equal(t, stepDone, sliceStep(t, v, "solution_design", "SL2").Status, "the chain runs no design")
				assert.Equal(t, stepPending, sliceStep(t, v, "decomposition", "SL2").Status)
				assert.Equal(t, stepPending, sliceStep(t, v, "readiness", "").Status)
			},
		},
		{
			name:  "the chain at a gated target's gate shows the later slices' steps done",
			setup: chainAtGate,
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepWaiting, sliceStep(t, v, "decomposition", "SL1").Status)
				require.NotNil(t, v.Open)
				assert.Equal(t, "gate:decomposition:SL1:v2", v.Open.PromptID)
				assert.Equal(t, stepDone, sliceStep(t, v, "solution_design", "SL2").Status, "the chain runs no design")
				assert.Equal(t, stepPending, sliceStep(t, v, "decomposition", "SL2").Status)
			},
		},
		{
			name: "a rejection at the chain's gate keeps the chain",
			setup: func(t *testing.T, c *Controller, st *store.Store) {
				chainAtGate(t, c, st)
				gateRow(t, st, "decomposition@SL1", 2, store.GateReject, store.ResponderHuman, "split D1")
			},
			check: func(t *testing.T, v protocol.BlueprintView) {
				assert.Equal(t, stepPending, sliceStep(t, v, "decomposition", "SL1").Status)
				assert.Equal(t, stepDone, sliceStep(t, v, "solution_design", "SL2").Status)
				assert.Equal(t, stepPending, sliceStep(t, v, "decomposition", "SL2").Status)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st := newSlicedView(t)
			tt.setup(t, c, st)
			tt.check(t, view(t, c))
		})
	}
}

func TestView_TheRunInFlightNamesItsSlice(t *testing.T) {
	c, st := newSlicedView(t)
	throughSliced(t, st, "solution_design@SL2")
	flying(t, c, st, "grounding@SL2", store.RunKindStage, 1)

	v := view(t, c)
	require.NotNil(t, v.Current)
	assert.Equal(t, "grounding", v.Current.Stage)
	assert.Equal(t, "SL2", v.Current.Slice)
	assert.Equal(t, stepRunning, sliceStep(t, v, "grounding", "SL2").Status)
}
