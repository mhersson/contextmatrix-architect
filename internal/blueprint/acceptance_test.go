package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func goodAcceptance() map[string]any {
	return map[string]any{"slices": []any{map[string]any{
		"id":       "SL1",
		"criteria": []any{map[string]any{"id": "AC1", "project": "alpha", "text": "GET /version answers 200 with the commit."}},
		"manual":   []any{},
	}}}
}

func TestAcceptance_AnAutoGateAndCardsThatCarryItsCriteria(t *testing.T) {
	script := dialogScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()})
		if err == nil || !strings.Contains(err.Error(), "AC1: criterion_unmapped") {
			return stage.Output{}, fmt.Errorf("cards that carry no criterion are refused: %v", err)
		}

		carried := goodCards()
		carried["cards"].([]any)[0].(map[string]any)["criteria"] = []any{"AC1"}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: carried}); err != nil {
			return stage.Output{}, fmt.Errorf("cards that carry every criterion pass: %w", err)
		}

		return done("# Cards", carried), nil
	}

	c, st, runner, rec, _ := newDialog(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "acceptance.review"), 1)

	gates, err := st.ListGates(ctx, "BP1", "acceptance")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder, "acceptance's gate is auto by default")

	cards, err := st.LatestArtifact(ctx, "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(cards))
	assert.True(t, rec.has(EventSignOff))
}

func TestCriteriaValidator(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	slicing, err := json.Marshal(oneSlice())
	require.NoError(t, err)

	_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "slicing", Body: "# Slices", Data: slicing})
	require.NoError(t, err)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	slice := func(d map[string]any) map[string]any { return d["slices"].([]any)[0].(map[string]any) }

	tests := []struct {
		name   string
		change func(d map[string]any)
		want   string
	}{
		{name: "it stands", change: func(map[string]any) {}},
		{name: "no slices", change: func(d map[string]any) { d["slices"] = []any{} }, want: "no_slices"},
		{name: "a slice slicing did not cut", change: func(d map[string]any) { slice(d)["id"] = "SL9" }, want: "slice_missing"},
		{name: "a repeated criterion id", change: func(d map[string]any) {
			slice(d)["criteria"] = append(slice(d)["criteria"].([]any), map[string]any{"id": "AC1", "project": "alpha", "text": "again"})
		}, want: "criterion_id"},
		{name: "a project the blueprint does not target", change: func(d map[string]any) {
			slice(d)["criteria"].([]any)[0].(map[string]any)["project"] = "beta"
		}, want: "criterion_project"},
		{name: "a slice with no criterion", change: func(d map[string]any) { slice(d)["criteria"] = []any{} }, want: "slice_empty"},
		{name: "a slice with only manual checks", change: func(d map[string]any) {
			slice(d)["criteria"] = []any{}
			slice(d)["manual"] = []any{map[string]any{"id": "MC1", "text": "look"}}
		}, want: "slice_empty"},
		{name: "data that is not slices", change: func(d map[string]any) { d["slices"] = "none" }, want: "shape"},
		{name: "a repeated manual id", change: func(d map[string]any) {
			slice(d)["manual"] = []any{map[string]any{"id": "MC1", "text": "a"}, map[string]any{"id": "MC1", "text": "b"}}
		}, want: "manual_id"},
		{name: "an empty manual id", change: func(d map[string]any) {
			slice(d)["manual"] = []any{map[string]any{"id": "", "text": "a"}}
		}, want: "manual_id"},
		{name: "a criterion without text", change: func(d map[string]any) {
			slice(d)["criteria"].([]any)[0].(map[string]any)["text"] = " "
		}, want: "criterion_text"},
		{name: "a manual check without text", change: func(d map[string]any) {
			slice(d)["manual"] = []any{map[string]any{"id": "MC1", "text": ""}}
		}, want: "manual_text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodAcceptance()
			tt.change(d)

			validate, _ := c.criteriaValidator(bp, "acceptance", nil)

			err := validate(ctx, stage.Artifact{Body: "# Acceptance", Data: d})
			if tt.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), ": "+tt.want+": ")
		})
	}
}

func TestAcceptance_UnreadableDataIsAFindingNotAStall(t *testing.T) {
	script := dialogScript()
	script["acceptance"] = returns("# Acceptance", map[string]any{"slices": "none"})

	var got error

	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		got = in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()})

		return done("# Cards", goodCards()), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), "acceptance: criteria_unreadable: ")
	assert.NotContains(t, got.Error(), "were not judged")
}

func TestAcceptance_TheSubmitIsRefusedItsFaults(t *testing.T) {
	script := dialogScript()

	var got error

	script["acceptance"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		bad := goodAcceptance()
		bad["slices"].([]any)[0].(map[string]any)["criteria"] = []any{}
		got = in.Validate(ctx, stage.Artifact{Body: "# Acceptance", Data: bad})

		return done("# Acceptance", goodAcceptance()), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), ": slice_empty: ")
}

func TestAcceptance_AManualCheckNeedsAnEntryThatCarriesIt(t *testing.T) {
	acceptance := goodAcceptance()
	acceptance["slices"].([]any)[0].(map[string]any)["manual"] = []any{map[string]any{"id": "MC1", "text": "check the dashboard"}}

	script := dialogScript()
	script["acceptance"] = returns("# Acceptance", acceptance)

	var got error

	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		carried := goodCards()
		carried["cards"].([]any)[0].(map[string]any)["criteria"] = []any{"AC1"}
		got = in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: carried})

		return done("# Cards", carried), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), "MC1: manual_unmapped: ")
}

func TestAcceptance_NoCriterionAtAllIsAFinding(t *testing.T) {
	script := dialogScript()
	script["acceptance"] = returns("# Acceptance", map[string]any{"slices": []any{}})

	var got error

	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		got = in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()})

		return done("# Cards", goodCards()), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), "acceptance: criteria_empty: ")
}

// tracedPack is the dialog pack, copied to a temporary directory, with
// intake stating outcomes and lists.yaml listing two edge cases.
func tracedPack(t *testing.T) *pack.Pack {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/dialog")))

	packYAML := filepath.Join(dir, "pack.yaml")
	raw, err := os.ReadFile(packYAML) //nolint:gosec // the test's own temp dir
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(packYAML, []byte(strings.Replace(string(raw), "checks: [assumptions]\n", "checks: [assumptions, outcomes]\n", 1)), 0o600)) //nolint:gosec // the test's own temp dir

	lists := filepath.Join(dir, "lists.yaml")
	raw, err = os.ReadFile(lists) //nolint:gosec // the test's own temp dir
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(lists, append(raw, []byte("\nedge_cases:\n  - Empty or missing input\n  - Limits\n")...), 0o600)) //nolint:gosec // the test's own temp dir

	pk, err := pack.LoadDir(dir)
	require.NoError(t, err)

	return pk
}

// tracedBlueprint starts BP1 on the traced pack with one slice, and intake
// stating outcomes.
func tracedBlueprint(t *testing.T, outcomes ...any) (*Controller, store.Blueprint) {
	t.Helper()

	c, st, _, _, _ := newDialog(t, dialogScript())
	c.opts.Pack = tracedPack(t)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	putData(t, st, "intake", map[string]any{"outcomes": outcomes})
	putData(t, st, "slicing", oneSlice())

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	return c, bp
}

// tracedAcceptance is goodAcceptance with its criterion proving O1, a manual
// check proving O2, and a sweep of both edge cases.
func tracedAcceptance() map[string]any {
	d := goodAcceptance()
	s := d["slices"].([]any)[0].(map[string]any)
	s["criteria"].([]any)[0].(map[string]any)["outcomes"] = []any{"O1"}
	s["manual"] = []any{map[string]any{"id": "MC1", "text": "time an export by hand", "outcomes": []any{"O2"}}}
	s["sweep"] = []any{
		map[string]any{"condition": " empty or missing INPUT", "settled_by": []any{"AC1"}},
		map[string]any{"condition": "Limits", "not_applicable": "The version endpoint takes no input that could grow."},
	}

	return d
}

func TestCriteriaValidator_Traces(t *testing.T) {
	c, bp := tracedBlueprint(t, outcomeItem("O1", "Callers read the version."), outcomeItem("O2", "It answers within 100 ms."))

	slice := func(d map[string]any) map[string]any { return d["slices"].([]any)[0].(map[string]any) }
	criterion := func(d map[string]any) map[string]any { return slice(d)["criteria"].([]any)[0].(map[string]any) }
	manual := func(d map[string]any) map[string]any { return slice(d)["manual"].([]any)[0].(map[string]any) }
	sweep := func(d map[string]any, i int) map[string]any { return slice(d)["sweep"].([]any)[i].(map[string]any) }

	tests := []struct {
		name   string
		change func(d map[string]any)
		want   string
	}{
		{name: "it stands", change: func(map[string]any) {}},
		{name: "a criterion citing no outcome", change: func(d map[string]any) { delete(criterion(d), "outcomes") }, want: "criterion_outcomes"},
		{name: "a criterion citing an unknown outcome", change: func(d map[string]any) { criterion(d)["outcomes"] = []any{"O9"} }, want: "criterion_outcomes"},
		{name: "outcomes that are no list", change: func(d map[string]any) { criterion(d)["outcomes"] = "O1" }, want: "criterion_outcomes"},
		{name: "a manual check citing an unknown outcome", change: func(d map[string]any) { manual(d)["outcomes"] = []any{"O7"} }, want: "criterion_outcomes"},
		{name: "an outcome nothing cites", change: func(d map[string]any) { delete(manual(d), "outcomes") }, want: "outcome_uncovered"},
		{name: "no sweep", change: func(d map[string]any) { delete(slice(d), "sweep") }, want: "sweep_missing"},
		{name: "a sweep that is no list", change: func(d map[string]any) { slice(d)["sweep"] = "done" }, want: "sweep_missing"},
		{name: "an edge case left out", change: func(d map[string]any) { slice(d)["sweep"] = slice(d)["sweep"].([]any)[:1] }, want: "sweep_missing"},
		{name: "a condition the list lacks", change: func(d map[string]any) { sweep(d, 1)["condition"] = "Weather" }, want: "sweep_unknown"},
		{name: "a condition that is no text", change: func(d map[string]any) { sweep(d, 1)["condition"] = 3 }, want: "sweep_unknown"},
		{name: "a condition swept twice", change: func(d map[string]any) {
			slice(d)["sweep"] = append(slice(d)["sweep"].([]any), map[string]any{"condition": "limits", "not_applicable": "No input grows."})
		}, want: "sweep_duplicate"},
		{name: "settled and not applicable", change: func(d map[string]any) { sweep(d, 0)["not_applicable"] = "no" }, want: "sweep_settled"},
		{name: "neither", change: func(d map[string]any) { delete(sweep(d, 0), "settled_by") }, want: "sweep_settled"},
		{name: "settled by an id the slice lacks", change: func(d map[string]any) { sweep(d, 0)["settled_by"] = []any{"AC7"} }, want: "sweep_settled"},
		{name: "a reason that is blank", change: func(d map[string]any) { sweep(d, 1)["not_applicable"] = " " }, want: "sweep_settled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tracedAcceptance()
			tt.change(d)

			validate, _ := c.criteriaValidator(bp, "acceptance", nil)
			refusedFor(t, validate, d, tt.want)
		})
	}

	t.Run("a malformed trace is never fatal", func(t *testing.T) {
		d := tracedAcceptance()
		criterion(d)["outcomes"] = "O1"
		slice(d)["sweep"] = 7

		validate, _ := c.criteriaValidator(bp, "acceptance", nil)
		require.NoError(t, thirdSubmit(t, validate, d), "stored with its problems, as any non-fatal check")
	})
}

// tracedSlicedBlueprint is slicedBlueprint with intake checking outcomes and
// stating O1 and O2.
func tracedSlicedBlueprint(t *testing.T) (*Controller, *store.Store, store.Blueprint) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/sliced")))

	packYAML := filepath.Join(dir, "pack.yaml")
	raw, err := os.ReadFile(packYAML) //nolint:gosec // the test's own temp dir
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(packYAML, []byte(strings.Replace(string(raw), "    policy: human\n", "    policy: human\n    checks: [outcomes]\n", 1)), 0o600)) //nolint:gosec // the test's own temp dir

	c, st, bp := slicedBlueprint(t)

	c.opts.Pack, err = pack.LoadDir(dir)
	require.NoError(t, err)

	putData(t, st, "intake", map[string]any{"outcomes": []any{outcomeItem("O1", "Exports are complete."), outcomeItem("O2", "An export takes under a minute.")}})

	return c, st, bp
}

func TestCriteriaValidator_OnlyTheLastSlicesAcceptanceLeavesNoOutcomeUncited(t *testing.T) {
	c, st, bp := tracedSlicedBlueprint(t)

	cites := func(slice, criterion, manual string, criterionCites, manualCites []any) map[string]any {
		d := sliceAcceptance(slice, criterion, manual)
		s := d["slices"].([]any)[0].(map[string]any)
		s["criteria"].([]any)[0].(map[string]any)["outcomes"] = criterionCites

		if manualCites != nil {
			s["manual"].([]any)[0].(map[string]any)["outcomes"] = manualCites
		}

		return d
	}

	// O1 is cited by a criterion, O2 by nothing yet: an earlier slice may leave it.
	first := cites("SL1", "AC1", "MC1", []any{"O1"}, nil)
	validate, _ := c.criteriaValidator(bp, "acceptance@SL1", nil)
	refusedFor(t, validate, first, "")

	putData(t, st, "acceptance@SL1", first)

	last, _ := c.criteriaValidator(bp, "acceptance@SL2", nil)
	refusedFor(t, last, cites("SL2", "AC2", "MC2", []any{"O1"}, nil), "outcome_uncovered")

	// An earlier slice's manual check counts toward the last slice's coverage.
	putData(t, st, "acceptance@SL1", cites("SL1", "AC1", "MC1", []any{"O1"}, []any{"O2"}))

	last, _ = c.criteriaValidator(bp, "acceptance@SL2", nil)
	refusedFor(t, last, cites("SL2", "AC2", "MC2", []any{"O1"}, nil), "")
}

func TestCriteriaValidator_ANeedWithoutOutcomesAsksNoTraces(t *testing.T) {
	c, bp := tracedBlueprint(t)

	d := tracedAcceptance()
	s := d["slices"].([]any)[0].(map[string]any)
	delete(s["criteria"].([]any)[0].(map[string]any), "outcomes")
	delete(s["manual"].([]any)[0].(map[string]any), "outcomes")

	validate, _ := c.criteriaValidator(bp, "acceptance", nil)
	refusedFor(t, validate, d, "")

	delete(s, "sweep")

	validate, _ = c.criteriaValidator(bp, "acceptance", nil)
	refusedFor(t, validate, d, "sweep_missing")
}

func TestAcceptance_TheTaskAndItsReviewListTheEdgeCases(t *testing.T) {
	c, _, runner, _, _ := newDialog(t, dialogScript())
	c.opts.Pack = tracedPack(t)
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	for _, key := range []string{"acceptance", "acceptance.review"} {
		runs := inputsOf(runner, key)
		require.NotEmpty(t, runs, key)

		for _, edge := range c.opts.Pack.Lists().EdgeCases {
			assert.Contains(t, runs[0].Task, edge, "%s is told the edge cases", key)
		}
	}
}

// touching is oneSlice with SL1 touching projects; none leaves the list out.
func touching(projects ...any) map[string]any {
	d := oneSlice()
	s := d["slices"].([]any)[0].(map[string]any)

	if projects == nil {
		delete(s, "projects")
	} else {
		s["projects"] = projects
	}

	return d
}

// pocBlueprint starts BP1 on the traced pack, targeting alpha and beta,
// with slicing stored as given; a nil slicing stores none.
func pocBlueprint(t *testing.T, slicing map[string]any) (*Controller, store.Blueprint) {
	t.Helper()

	c, st, _, _, _ := newDialog(t, dialogScript())
	c.opts.Pack = tracedPack(t)
	ctx := context.Background()

	in := startInput("BP1")
	in.Projects = []string{"alpha", "beta"}
	require.NoError(t, c.Start(ctx, in))

	if slicing != nil {
		putData(t, st, "slicing", slicing)
	}

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	return c, bp
}

// mixedAcceptance is SL1 with criterion AC1 on alpha, AC2 on beta and
// manual check MC1; its sweep settles one case by AC2 and records the other
// not applicable as a POC project's.
func mixedAcceptance() map[string]any {
	return map[string]any{"slices": []any{map[string]any{
		"id": "SL1",
		"criteria": []any{
			map[string]any{"id": "AC1", "project": "alpha", "text": "the prototype page lists the uploaded files by name"},
			map[string]any{"id": "AC2", "project": "beta", "text": "GET /files answers 200 with the stored file names"},
		},
		"manual": []any{map[string]any{"id": "MC1", "text": "upload a file from a phone and see it listed"}},
		"sweep": []any{
			map[string]any{"condition": "Empty or missing input", "settled_by": []any{"AC2"}},
			map[string]any{"condition": "Limits", "not_applicable": "POC project"},
		},
	}}}
}

func TestCriteriaValidator_POCSweeps(t *testing.T) {
	alpha := []string{"alpha"}

	slice := func(d map[string]any) map[string]any { return d["slices"].([]any)[0].(map[string]any) }
	entry := func(d map[string]any, i int) map[string]any { return slice(d)["sweep"].([]any)[i].(map[string]any) }
	// pocOnly keeps AC1, on alpha, as the slice's one criterion.
	pocOnly := func(d map[string]any) { slice(d)["criteria"] = slice(d)["criteria"].([]any)[:1] }

	tests := []struct {
		name    string
		slicing map[string]any
		poc     []string
		change  func(d map[string]any)
		want    string
	}{
		{name: "a mixed slice settled by its production criterion", slicing: touching("alpha", "beta"), poc: alpha, change: func(map[string]any) {}},
		{name: "a mixed slice settled by a POC criterion", slicing: touching("alpha", "beta"), poc: alpha, change: func(d map[string]any) {
			entry(d, 0)["settled_by"] = []any{"AC1"}
		}, want: "sweep_poc"},
		{name: "a POC criterion beside a production one", slicing: touching("alpha", "beta"), poc: alpha, change: func(d map[string]any) {
			entry(d, 0)["settled_by"] = []any{"AC2", " AC1 "}
		}, want: "sweep_poc"},
		{name: "a mixed slice settled by a manual check", slicing: touching("alpha", "beta"), poc: alpha, change: func(d map[string]any) {
			entry(d, 0)["settled_by"] = []any{"MC1"}
		}},
		{name: "a mixed slice without a sweep", slicing: touching("alpha", "beta"), poc: alpha, change: func(d map[string]any) {
			delete(slice(d), "sweep")
		}, want: "sweep_missing"},
		{name: "a mixed slice short of a case", slicing: touching("alpha", "beta"), poc: alpha, change: func(d map[string]any) {
			slice(d)["sweep"] = slice(d)["sweep"].([]any)[:1]
		}, want: "sweep_missing"},
		{name: "an all-POC slice leaves its sweep out", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			delete(slice(d), "sweep")
		}},
		{name: "an all-POC slice with a null sweep", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			slice(d)["sweep"] = nil
		}},
		{name: "an all-POC slice with an empty sweep", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			slice(d)["sweep"] = []any{}
		}},
		{name: "an all-POC slice short of a case", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			slice(d)["sweep"] = slice(d)["sweep"].([]any)[1:]
		}},
		{name: "an all-POC slice settling a case with its criterion", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			entry(d, 0)["settled_by"] = []any{"AC1"}
		}, want: "sweep_poc"},
		{name: "an all-POC slice's entry is still judged", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			slice(d)["sweep"] = []any{map[string]any{"condition": "Weather", "not_applicable": "No weather here."}}
		}, want: "sweep_unknown"},
		{name: "slicing says all POC but a criterion targets production", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			delete(slice(d), "sweep")
		}, want: "sweep_missing"},
		{name: "the criteria are all POC but slicing names production", slicing: touching("alpha", "beta"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			delete(slice(d), "sweep")
		}, want: "sweep_missing"},
		{name: "slicing names no project, so the criteria decide", slicing: touching(), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			delete(slice(d), "sweep")
		}},
		{name: "an all-POC slice whose sweep is no list", slicing: touching("alpha"), poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			slice(d)["sweep"] = "none"
		}, want: "sweep_missing"},
		{name: "no slicing version yet, so the criteria decide", poc: alpha, change: func(d map[string]any) {
			pocOnly(d)
			delete(slice(d), "sweep")
		}},
		{name: "no POC target: every slice sweeps", slicing: touching("alpha"), change: func(d map[string]any) {
			pocOnly(d)
			delete(slice(d), "sweep")
		}, want: "sweep_missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, bp := pocBlueprint(t, tt.slicing)

			d := mixedAcceptance()
			tt.change(d)

			validate, _ := c.criteriaValidator(bp, "acceptance", tt.poc)
			refusedFor(t, validate, d, tt.want)
		})
	}

	t.Run("a POC refusal is never fatal", func(t *testing.T) {
		c, bp := pocBlueprint(t, touching("alpha", "beta"))

		d := mixedAcceptance()
		entry(d, 0)["settled_by"] = []any{"AC1"}

		validate, _ := c.criteriaValidator(bp, "acceptance", alpha)
		require.NoError(t, thirdSubmit(t, validate, d), "stored with its problems, as any sweep problem")
	})
}

func TestCriteriaValidator_EachSliceSweepsByItsOwnProjects(t *testing.T) {
	slicing := twoSlices()
	slicing["slices"].([]any)[1].(map[string]any)["projects"] = []any{"alpha", "beta"}

	c, bp := pocBlueprint(t, slicing)

	second := mixedAcceptance()["slices"].([]any)[0].(map[string]any)
	second["id"] = "SL2"
	second["criteria"].([]any)[0].(map[string]any)["id"] = "AC2"
	second["criteria"].([]any)[1].(map[string]any)["id"] = "AC3"
	second["manual"].([]any)[0].(map[string]any)["id"] = "MC2"
	second["sweep"].([]any)[0].(map[string]any)["settled_by"] = []any{"AC3"}

	both := map[string]any{"slices": []any{
		map[string]any{"id": "SL1", "criteria": []any{map[string]any{"id": "AC1", "project": "alpha", "text": "the CLI prints the parsed config as JSON"}}, "manual": []any{}},
		second,
	}}

	validate, _ := c.criteriaValidator(bp, "acceptance", []string{"alpha"})
	refusedFor(t, validate, both, "")

	delete(second, "sweep")

	validate, _ = c.criteriaValidator(bp, "acceptance", []string{"alpha"})
	err := validate(context.Background(), stage.Artifact{Body: "# x", Data: both})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SL2: sweep_missing: ", "SL2 touches beta")
	assert.NotContains(t, err.Error(), "SL1: sweep_missing: ", "SL1 touches alpha alone")
}

func TestCriteriaValidator_APerSliceStepFindsItsSliceByPosition(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/sliced")))

	lists := filepath.Join(dir, "lists.yaml")
	raw, err := os.ReadFile(lists) //nolint:gosec // the test's own temp dir
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(lists, append(raw, []byte("\nedge_cases:\n  - Empty or missing input\n")...), 0o600)) //nolint:gosec // the test's own temp dir

	pk, err := pack.LoadDir(dir)
	require.NoError(t, err)

	tests := []struct {
		name     string
		projects []any
		want     string
	}{
		{name: "a second slice on a POC project alone sweeps nothing", projects: []any{"alpha"}},
		{name: "a second slice that also touches another project sweeps", projects: []any{"alpha", "beta"}, want: "sweep_missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, bp := slicedBlueprint(t)
			c.opts.Pack = pk

			slicing := twoSlices()
			slicing["slices"].([]any)[0].(map[string]any)["projects"] = []any{"alpha", "beta"}
			slicing["slices"].([]any)[1].(map[string]any)["projects"] = tt.projects
			putData(t, st, "slicing", slicing)

			// SL1's acceptance is stored first, as the run before this one left it.
			putData(t, st, "acceptance@SL1", sliceAcceptance("SL1", "AC1", "MC1"))

			validate, _ := c.criteriaValidator(bp, "acceptance@SL2", []string{"alpha"})
			refusedFor(t, validate, sliceAcceptance("SL2", "AC2", "MC2"), tt.want)
		})
	}
}
