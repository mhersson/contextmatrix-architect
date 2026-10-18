package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// parkUntil blocks a scripted run until ch closes or the run is cancelled.
func parkUntil(ctx context.Context, ch <-chan struct{}) {
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// signalled waits for a scripted run to close ch.
func signalled(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the scripted run never started")
	}
}

// reviews scripts a reviewer that gives scores in turn, the last one again
// once they run out. A score below 4 raises one major finding, "finding
// <call>", and every call re-checks every finding raised before it: still
// open below 4, fixed at 4 or more. It serves one attempt.
func reviews(scores ...int) func(context.Context, stage.Input) (stage.Output, error) {
	var (
		mu     sync.Mutex
		calls  int
		raised int
	)

	return func(context.Context, stage.Input) (stage.Output, error) {
		mu.Lock()
		defer mu.Unlock()

		score := scores[min(calls, len(scores)-1)]
		calls++

		status := findingFixed
		if score < 4 {
			status = findingStillOpen
		}

		checks := make([]map[string]any, 0, raised)
		for n := 1; n <= raised; n++ {
			checks = append(checks, check(findingID(n), status, ""))
		}

		var found []map[string]any

		if score < 4 {
			raised++

			found = append(found, newFinding(severityMajor, fmt.Sprintf("finding %d", calls)))
		}

		return done("# Review", rechecked(score, checks, found...)), nil
	}
}

// newFinding is a new finding in a reviewer's submit.
func newFinding(severity, text string) map[string]any {
	return map[string]any{"severity": severity, "text": text}
}

// check is a later round's verdict on finding id.
func check(id, status, note string) map[string]any {
	return map[string]any{"id": id, "status": status, "note": note}
}

// rechecked is a reviewer's submit: a score, a verdict on every earlier
// finding of the attempt and the new findings.
func rechecked(score int, checks []map[string]any, findings ...map[string]any) map[string]any {
	cs := make([]any, 0, len(checks))
	for _, c := range checks {
		cs = append(cs, c)
	}

	fs := make([]any, 0, len(findings))
	for _, f := range findings {
		fs = append(fs, f)
	}

	return map[string]any{"score": float64(score), "verdicts": cs, "findings": fs}
}

// fixed is a later round's submit that finds every id fixed and nothing new.
func fixed(score int, ids ...string) map[string]any {
	checks := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		checks = append(checks, check(id, findingFixed, ""))
	}

	return rechecked(score, checks)
}

// scored is a first round's submit: a score and new major findings.
func scored(score int, findings ...string) map[string]any {
	fs := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		fs = append(fs, newFinding(severityMajor, f))
	}

	return rechecked(score, nil, fs...)
}

// verdicts scripts a reviewer that submits vs in turn, the last one again
// once they run out.
func verdicts(vs ...map[string]any) func(context.Context, stage.Input) (stage.Output, error) {
	var n atomic.Int32

	return func(context.Context, stage.Input) (stage.Output, error) {
		i := int(n.Add(1)) - 1

		return done("# Review", vs[min(i, len(vs)-1)]), nil
	}
}

// shapeRounds lists shape's stage runs as "attempt A round R status".
func shapeRounds(t *testing.T, st *store.Store) []string {
	t.Helper()

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	var out []string

	for _, r := range runs {
		if r.Stage == "shape" && r.Kind == store.RunKindStage {
			out = append(out, fmt.Sprintf("attempt %d round %d %s", r.Attempt, r.Round, r.Status))
		}
	}

	return out
}

// reviewOf is the review map a gate event carries.
func reviewOf(t *testing.T, gate Event) map[string]any {
	t.Helper()

	v, ok := gate.Payload["review"].(map[string]any)
	require.True(t, ok, "the gate carries its verdict")

	return v
}

func TestParseReview(t *testing.T) {
	prior := findingList{
		{ID: "R1", Severity: severityCritical, Text: "name the owner", Status: findingOpen},
		{ID: "R3", Severity: severityMinor, Text: "split it", Status: findingStillOpen, VerifiedIn: 2},
	}

	both := func(r1, r3 string) []any {
		return []any{check("R1", r1, ""), check("R3", r3, "")}
	}

	tests := []struct {
		name    string
		data    map[string]any
		prior   findingList
		want    review
		wantErr string
	}{
		{
			name: "a first round's findings take ids in order",
			data: map[string]any{"score": float64(3), "findings": []any{newFinding(severityMajor, " name the owner "), newFinding(severityMinor, "split it")}},
			want: review{Score: 3, Findings: findingList{
				{ID: "R1", Severity: severityMajor, Text: "name the owner", Status: findingOpen},
				{ID: "R2", Severity: severityMinor, Text: "split it", Status: findingOpen},
			}},
		},
		{name: "no findings", data: map[string]any{"score": float64(5)}, want: review{Score: 5, Findings: findingList{}}},
		{
			name:  "a later round re-checks every finding and numbers new ones after the highest",
			prior: prior,
			data: map[string]any{
				"score":    float64(4),
				"verdicts": []any{check("R1", findingFixed, " the owners table names one "), check("R3", findingStillOpen, "")},
				"findings": []any{newFinding(severityCritical, "no way back")},
			},
			want: review{Score: 4, Findings: findingList{
				{ID: "R1", Severity: severityCritical, Text: "name the owner", Status: findingFixed, VerifiedIn: 3, Note: "the owners table names one"},
				{ID: "R3", Severity: severityMinor, Text: "split it", Status: findingStillOpen, VerifiedIn: 3},
				{ID: "R4", Severity: severityCritical, Text: "no way back", Status: findingOpen},
			}},
		},
		{name: "a missing verdict", prior: prior, data: map[string]any{"score": float64(4), "verdicts": []any{check("R1", findingFixed, "")}}, wantErr: "missing: R3"},
		{name: "no verdicts at all", prior: prior, data: map[string]any{"score": float64(4)}, wantErr: "missing: R1, R3"},
		{name: "a verdict on an unknown finding", prior: prior, data: map[string]any{"score": float64(4), "verdicts": append(both(findingFixed, findingFixed), check("R9", findingFixed, ""))}, wantErr: "R9"},
		{name: "two verdicts on one finding", prior: prior, data: map[string]any{"score": float64(4), "verdicts": append(both(findingFixed, findingFixed), check("R1", findingStillOpen, ""))}, wantErr: "two verdicts"},
		{name: "a verdict that is neither fixed nor still open", prior: prior, data: map[string]any{"score": float64(4), "verdicts": both(findingOpen, findingFixed)}, wantErr: "fixed or still_open"},
		{name: "a verdict without an id", prior: prior, data: map[string]any{"score": float64(4), "verdicts": []any{map[string]any{"status": findingFixed}}}, wantErr: "id"},
		{name: "verdicts that are not a list", prior: prior, data: map[string]any{"score": float64(4), "verdicts": "all fixed"}, wantErr: "verdicts"},
		{name: "verdicts in a first round", data: map[string]any{"score": float64(4), "verdicts": []any{check("R1", findingFixed, "")}}, wantErr: "first round"},
		{name: "a finding without a severity", data: map[string]any{"score": float64(3), "findings": []any{map[string]any{"text": "name the owner"}}}, wantErr: "severity"},
		{name: "a finding as a bare sentence", data: map[string]any{"score": float64(3), "findings": []any{"name the owner"}}, wantErr: "finding 1"},
		{name: "a blank finding", data: map[string]any{"score": float64(2), "findings": []any{newFinding(severityMajor, " ")}}, wantErr: "sentence"},
		{name: "a low score with nothing left open", prior: prior, data: map[string]any{"score": float64(2), "verdicts": both(findingFixed, findingFixed)}, wantErr: "at least one finding"},
		{name: "no score", data: map[string]any{"findings": []any{}}, wantErr: "score"},
		{name: "a score above five", data: map[string]any{"score": float64(6)}, wantErr: "score"},
		{name: "a score of zero", data: map[string]any{"score": float64(0)}, wantErr: "score"},
		{name: "a fraction", data: map[string]any{"score": 3.5}, wantErr: "score"},
		{name: "a score as text", data: map[string]any{"score": "4"}, wantErr: "score"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseReview(tt.data, tt.prior, 4, 3)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReview_Passes(t *testing.T) {
	tests := []struct {
		name string
		v    review
		want bool
	}{
		{name: "at the threshold", v: review{Score: 4}, want: true},
		{name: "below it", v: review{Score: 3}},
		{name: "an open critical finding fails a top score", v: review{Score: 5, Findings: findingList{{ID: "R1", Severity: severityCritical, Status: findingOpen}}}},
		{name: "so does one still open", v: review{Score: 5, Findings: findingList{{ID: "R1", Severity: severityCritical, Status: findingStillOpen}}}},
		{name: "a fixed critical finding does not", v: review{Score: 4, Findings: findingList{{ID: "R1", Severity: severityCritical, Status: findingFixed}}}, want: true},
		{name: "open major and minor findings do not", v: review{Score: 4, Findings: findingList{
			{ID: "R1", Severity: severityMajor, Status: findingOpen}, {ID: "R2", Severity: severityMinor, Status: findingStillOpen},
		}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.v.passes(4))
		})
	}
}

func TestReview_AVerdictStoredBeforeTheLedgerReadsAsOne(t *testing.T) {
	var v review
	require.NoError(t, json.Unmarshal([]byte(`{"score":4,"findings":["name the owner","split it"],"version":1,"attempt":1,"round":1}`), &v))

	assert.Equal(t, findingList{
		{ID: "R1", Severity: severityMajor, Text: "name the owner", Status: findingOpen},
		{ID: "R2", Severity: severityMajor, Text: "split it", Status: findingOpen},
	}, v.Findings, "each sentence is an open major finding, numbered as its builder round numbered it")
	assert.True(t, v.passes(4))

	require.NoError(t, json.Unmarshal([]byte(`{"score":5,"findings":null}`), &v))
	assert.Empty(t, v.Findings)
}

func TestReview_ALaterRoundReChecksTheLedger(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		switch shapes.Add(1) {
		case 1:
			return done("# Shape", nil), nil
		case 2:
			return done("# Shape 2", map[string]any{"addressed": []any{entry("R1", "named the owner")}}), nil
		default:
			return done("# Shape 3", nil), nil
		}
	}
	script["shape.review"] = verdicts(
		scored(3, "name the owner"),
		rechecked(5, []map[string]any{check("R1", findingFixed, "the owners table names one")}, newFinding(severityMinor, "spell out the acronym")),
		fixed(5, "R1", "R2"),
	)

	c, st, runner, rec := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeDefault)

	judged := inputsOf(runner, "shape.review")
	require.GreaterOrEqual(t, len(judged), 2)
	assert.Contains(t, judged[1].Task, "R1", "the second round gets the finding to re-check")
	assert.Contains(t, judged[1].Task, "name the owner")
	assert.Contains(t, judged[1].Task, "named the owner", "with the builder's claim beside it")

	arts, err := st.ListArtifacts(context.Background(), "BP1")
	require.NoError(t, err)

	var stored []store.Artifact

	for _, a := range arts {
		if a.Stage == reviewStage("shape") {
			stored = append(stored, a)
		}
	}

	require.GreaterOrEqual(t, len(stored), 2)
	assert.JSONEq(t, `{"score":5,"passed":true,"findings":[`+
		`{"id":"R1","severity":"major","text":"name the owner","status":"fixed","verified_in":2,"note":"the owners table names one"},`+
		`{"id":"R2","severity":"minor","text":"spell out the acronym","status":"open"}],`+
		`"version":2,"attempt":1,"round":2}`, string(stored[1].Data))

	events := rec.all(EventReview)
	require.GreaterOrEqual(t, len(events), 2)

	second, _ := events[1].Payload["findings"].([]any)
	require.Len(t, second, 2)
	assert.Equal(t, map[string]any{
		"id": "R1", "severity": severityMajor, "text": "name the owner", "status": findingFixed, "verified_in": 2,
		"note": "the owners table names one", "claim": "named the owner",
	}, second[0], "the review entry carries the claim the round checked")
	assert.Equal(t, false, events[0].Payload["passed"])
	assert.Equal(t, true, events[1].Payload["passed"])
	assert.Equal(t, "reviewer", events[1].Payload["reviewer"])
	assert.Equal(t, []string{}, events[1].Payload["open_critical"])
}

func TestReview_AFindingTheRoundRaisedCarriesNoClaim(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		switch shapes.Add(1) {
		case 1:
			return done("# Shape", nil), nil
		case 2:
			// As a third failing submit is accepted: the round was given R1 alone.
			return done("# Shape 2", map[string]any{"addressed": []any{entry("R1", "named the owner"), entry("R2", "spelled it out")}}), nil
		default:
			return done("# Shape 3", nil), nil
		}
	}
	script["shape.review"] = verdicts(
		scored(3, "name the owner"),
		rechecked(5, []map[string]any{check("R1", findingFixed, "")}, newFinding(severityMinor, "spell out the acronym")),
		fixed(5, "R1", "R2"),
	)

	c, _, _, rec := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeDefault)

	events := rec.all(EventReview)
	require.GreaterOrEqual(t, len(events), 2)

	second, _ := events[1].Payload["findings"].([]any)
	require.Len(t, second, 2)

	r1, _ := second[0].(map[string]any)
	r2, _ := second[1].(map[string]any)

	assert.Equal(t, "named the owner", r1["claim"], "the re-checked finding carries the builder's claim")
	assert.Equal(t, "R2", r2["id"])
	assert.NotContains(t, r2, "claim", "a finding the round raised was never the builder's to address")
}

func TestReview_AnOpenCriticalFailsAPassingScore(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(
		rechecked(5, nil, newFinding(severityCritical, "there is no way back from a bad release")),
		rechecked(5, []map[string]any{check("R1", findingStillOpen, "still none")}),
	)

	c, st, runner, rec := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeAuto)

	assert.Len(t, inputsOf(runner, "shape.review"), 3, "a critical finding left open fails every round")

	shapes := inputsOf(runner, "shape")
	require.Len(t, shapes, 3)
	assert.Contains(t, shapes[2].Task, "still none", "the builder round gets the reviewer's reason")
	assert.Equal(t, []string{"R1"}, rec.all(EventReview)[0].Payload["open_critical"])

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the auto policy never approves over an open critical finding")

	verdict := reviewOf(t, gate)
	assert.Equal(t, 5, verdict["score"])
	assert.Equal(t, false, verdict["passed"])
	assert.Equal(t, []string{"R1"}, verdict["open_critical"])

	gates, err := st.ListGates(context.Background(), "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestReview_ANewAttemptStartsAFreshLedger(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(3, "name the owner"), fixed(5, "R1"), scored(5))

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.True(t, rec.has(EventGate))

	require.NoError(t, c.Message(ctx, "BP1", "reject make it smaller", "m2"))
	c.Wait("BP1")

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 3)
	assert.NotContains(t, judged[2].Task, "name the owner", "the earlier attempt's findings stay behind")

	gates := rec.all(EventGate)
	require.Len(t, gates, 2, "a first round with no verdicts is accepted, so no earlier ledger is asked for")
	assert.Equal(t, 1, reviewOf(t, gates[1])["round"], "the new attempt's first round")
	assert.Equal(t, []any{}, reviewOf(t, gates[1])["findings"])
}

func TestReview_AHumanGateShowsTheVerdict(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(4)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	gate, ok := rec.first(EventGate)
	require.True(t, ok)

	assert.Equal(t, 1, gate.Payload["version"])

	verdict := reviewOf(t, gate)
	assert.Equal(t, 4, verdict["score"])
	assert.Equal(t, true, verdict["passed"])
	assert.NotContains(t, verdict, "revised")
	assert.Equal(t, 1, verdict["judged"])

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 1)
	assert.Contains(t, judged[0].Task, "# Shape", "the reviewer judges the artifact")
	assert.Contains(t, judged[0].Task, "# Need", "with the stage's inputs")
	assert.Nil(t, judged[0].History, "never the builder's transcript")
	assert.Nil(t, judged[0].Interviewer)
	assert.Equal(t, c.opts.Pack.Persona("reviewer"), judged[0].Persona)
	assert.Equal(t, c.opts.Pack.ReviewInstructions(), judged[0].Instructions)
	assert.Empty(t, judged[0].Tools, "a reviewer the pack gives no tools gets none")

	stored, err := st.LatestArtifact(ctx, "BP1", "shape.review")
	require.NoError(t, err)
	assert.JSONEq(t, `{"score":4,"passed":true,"findings":[],"version":1,"attempt":1,"round":1}`, string(stored.Data))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var kinds []string
	for _, r := range runs {
		kinds = append(kinds, r.Stage+":"+r.Kind)
	}

	assert.Equal(t, []string{"intake:stage", "shape:stage", "shape:reviewer"}, kinds)
}

func TestReview_ALowScoreResumesTheBuilderWithTheFindings(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(2, 5)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	shapes := inputsOf(runner, "shape")
	require.Len(t, shapes, 2)
	assert.NotEmpty(t, shapes[1].History, "the builder's conversation resumes")
	assert.Contains(t, shapes[1].Task, "finding 1", "with the findings")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var rounds []string

	for _, r := range runs {
		if r.Stage == "shape" && r.Kind == store.RunKindStage {
			rounds = append(rounds, fmt.Sprintf("attempt %d round %d", r.Attempt, r.Round))
		}
	}

	assert.Equal(t, []string{"attempt 1 round 1", "attempt 1 round 2"}, rounds)

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"], "the gate judges the second round's artifact")
	assert.Equal(t, 2, rec.count(EventReview))
}

func TestReview_ThreeLowRoundsGoToAHumanWhateverThePolicy(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(2)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	assert.Len(t, inputsOf(runner, "shape"), 3)
	assert.Len(t, inputsOf(runner, "shape.review"), 3)

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the auto policy does not decide a gate its reviewer did not pass")

	verdict, _ := gate.Payload["review"].(map[string]any)
	assert.Equal(t, false, verdict["passed"])
	assert.Equal(t, 3, verdict["round"])

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)
	assert.Equal(t, []string{"intake", "shape", "shape", "shape"}, stagesRun(runner), "plan never runs")
}

func TestReview_AnAutoGateApprovesAtTheThreshold(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(4)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestReview_APassWithFindingsIsReviewedAgainBeforeTheGate(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"), fixed(4, "R1"))

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 2, "the revision is reviewed")
	assert.Contains(t, judged[1].Task, "name the owner", "the verification gets the finding to re-check")

	shapes := inputsOf(runner, "shape")
	require.Len(t, shapes, 2)
	assert.Equal(t, []llm.Message{{Role: "user", Content: "x"}}, shapes[1].History, "the builder's conversation resumes")
	assert.Contains(t, shapes[1].Task, "R1", "the builder round names the finding by id")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 done"}, shapeRounds(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 1, rec.count(EventGate))
	assert.Equal(t, 2, gate.Payload["version"])

	verdict := reviewOf(t, gate)
	assert.NotContains(t, verdict, "revised", "no version reaches a gate unreviewed")
	assert.Equal(t, 2, verdict["judged"])
	assert.Equal(t, 2, verdict["round"])
	assert.Equal(t, true, verdict["passed"])
	assert.Equal(t, []any{map[string]any{"id": "R1", "severity": severityMajor, "text": "name the owner", "status": findingFixed, "verified_in": 2}}, verdict["findings"])

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates, "the gate waits for its human")
}

func TestReview_AFalseAddressedClaimIsCaught(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		n := shapes.Add(1)
		if n == 1 {
			return done("# Shape", nil), nil
		}

		return done(fmt.Sprintf("# Shape %d", n), map[string]any{"addressed": []any{entry("R1", "named the owner")}}), nil
	}
	script["shape.review"] = verdicts(
		scored(3, "name the owner"),
		rechecked(4, []map[string]any{check("R1", findingStillOpen, "the owners table is still empty")}),
		fixed(4, "R1"),
	)

	c, _, runner, rec := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeDefault)

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 3)
	assert.Contains(t, judged[1].Task, "named the owner", "the reviewer sees the claim it must check")

	rounds := inputsOf(runner, "shape")
	require.Len(t, rounds, 3, "a claim the reviewer did not find in the artifact goes back to the builder")
	assert.Contains(t, rounds[2].Task, "the owners table is still empty", "with the reviewer's reason")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 3, gate.Payload["version"])

	verdict := reviewOf(t, gate)
	assert.Equal(t, 3, verdict["round"])
	assert.Equal(t, []any{map[string]any{
		"id": "R1", "severity": severityMajor, "text": "name the owner", "status": findingFixed, "verified_in": 3, "claim": "named the owner",
	}}, verdict["findings"])
}

func TestReview_TheThirdVerdictGoesToTheGateWhateverItSays(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(
		scored(4, "name the owner"),
		rechecked(4, []map[string]any{check("R1", findingStillOpen, "still unnamed")}),
	)

	c, st, runner, rec := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeDefault)

	assert.Len(t, inputsOf(runner, "shape.review"), 3, "three reviewer rounds per attempt")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 done", "attempt 1 round 3 done"}, shapeRounds(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 3, gate.Payload["version"])
	assert.Equal(t, 3, reviewOf(t, gate)["round"])
}

func TestReview_ARestartDuringTheVerificationRunsItAgain(t *testing.T) {
	var calls atomic.Int32

	script := gatedScript()
	script["shape.review"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		switch calls.Add(1) {
		case 1:
			return done("# Review", scored(4, "name the owner")), nil
		case 2:
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		default:
			return done("# Review", fixed(5, "R1")), nil
		}
	}

	c, st, _, _ := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	c2, _, runner2, rec2 := newGatedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	assert.Equal(t, int32(3), calls.Load(), "the verification runs once more")
	assert.Empty(t, inputsOf(runner2, "shape"), "the revision it verifies is not built again")

	gate, ok := rec2.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, 2, reviewOf(t, gate)["round"])
}

func TestReview_AnAutoGateApprovesTheVerifiedRevision(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"), fixed(4, "R1"))

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "shape", "plan"}, stagesRun(runner))
	assert.Len(t, inputsOf(runner, "shape.review"), 2, "the revision is reviewed before the auto policy approves it")

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, 2, gates[0].Version, "the revised version is approved")
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestReview_ALowScoreThenAPassWithFindingsIsReviewedAgain(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(2, "split it"), rechecked(4, []map[string]any{check("R1", findingFixed, "")}, newFinding(severityMajor, "name the owner")), fixed(4, "R1", "R2"))

	c, st, runner, rec := newGated(t, script)

	startGated(t, c, protocol.ArchitectModeDefault)

	assert.Len(t, inputsOf(runner, "shape.review"), 3)

	shapes := inputsOf(runner, "shape")
	require.Len(t, shapes, 3)
	assert.Contains(t, shapes[1].Task, "split it", "the builder round")
	assert.Contains(t, shapes[2].Task, "name the owner", "the revision round")
	assert.NotContains(t, shapes[2].Task, "split it", "R1 was fixed in the round the revision follows")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 done", "attempt 1 round 3 done"}, shapeRounds(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 3, gate.Payload["version"])

	verdict := reviewOf(t, gate)
	assert.NotContains(t, verdict, "revised")
	assert.Equal(t, 3, verdict["judged"])
	assert.Equal(t, 3, verdict["round"])
}

func TestReview_ARestartDuringTheRevisionRunsItAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"), fixed(4, "R1"))
	script["shape"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Shape", nil), nil
	}

	c, st, runner, _ := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return shapes.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))
	require.Len(t, inputsOf(runner, "shape.review"), 1)

	c2, _, runner2, rec2 := newGatedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	again := inputsOf(runner2, "shape")
	require.Len(t, again, 1)
	assert.Contains(t, again[0].Task, "name the owner", "the revision runs again with its findings")
	assert.NotEmpty(t, again[0].History, "on the builder's conversation")
	assert.Len(t, inputsOf(runner2, "shape.review"), 1, "the revision is then reviewed")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 interrupted", "attempt 1 round 2 done"}, shapeRounds(t, st))

	gate, ok := rec2.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, 2, reviewOf(t, gate)["round"])
}

func TestReview_AFailedRevisionWaitsAndANoteRunsItAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"), fixed(4, "R1"))
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Shape", nil), nil
	}

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.False(t, rec.has(EventGate), "the failed revision stops the pipeline")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m2"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "shape", note.Payload["retries"])
	assert.Equal(t, true, note.Payload["critique"])

	shapesRun := inputsOf(runner, "shape")
	require.Len(t, shapesRun, 3, "the revision runs again")
	assert.Contains(t, shapesRun[2].Task, "name the owner")
	assert.Len(t, inputsOf(runner, "shape.review"), 2, "the first verdict is not asked for again, and the revision is reviewed")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 failed", "attempt 1 round 2 done"}, shapeRounds(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, 2, reviewOf(t, gate)["round"])
}

// seedLegacy stores shape versions 2 to upTo after seedGated's first, each
// built by its own round of attempt 1, with a legacy verdict on each of the
// first versions.
func seedLegacy(t *testing.T, st *store.Store, upTo int, verdictData []string) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	for n := 1; n <= upTo; n++ {
		if n > 1 {
			run, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "shape", Attempt: 1, Round: n, Status: store.RunRunning, StartedAt: now})
			require.NoError(t, err)
			require.NoError(t, st.EndRun(ctx, run, store.RunDone, "", store.RunUsage{}, now))

			_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "shape", Body: fmt.Sprintf("# Shape %d", n)})
			require.NoError(t, err)
		}

		if n <= len(verdictData) {
			run, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "shape", Kind: store.RunKindReviewer, Attempt: 1, Round: n, Status: store.RunRunning, StartedAt: now})
			require.NoError(t, err)
			require.NoError(t, st.EndRun(ctx, run, store.RunDone, "", store.RunUsage{}, now))

			_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: reviewStage("shape"), Body: "# Review", Data: []byte(verdictData[n-1])})
			require.NoError(t, err)
		}
	}
}

func TestReview_AnUnreviewedRevisionStoredEarlierGetsAVerificationRound(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(fixed(5, "R1"))

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	seedGated(t, st, protocol.ArchitectModeDefault)
	seedLegacy(t, st, 2, []string{`{"score":4,"passed":true,"findings":["name the owner"],"version":1,"attempt":1,"round":1}`})

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "shape.review"), 1, "the revision is reviewed")
	assert.Empty(t, inputsOf(runner, "shape"), "no builder round")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, 2, reviewOf(t, gate)["round"])
	assert.Equal(t, 2, reviewOf(t, gate)["judged"])
}

func TestReview_AnUnreviewedRevisionAfterTheThirdVerdictIsVerifiedPastTheCap(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(rechecked(4, []map[string]any{check("R1", findingStillOpen, "still")}))

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	seedGated(t, st, protocol.ArchitectModeDefault)
	seedLegacy(t, st, 4, []string{
		`{"score":2,"passed":false,"findings":["name the owner"],"version":1,"attempt":1,"round":1}`,
		`{"score":2,"passed":false,"findings":["name the owner"],"version":2,"attempt":1,"round":2}`,
		`{"score":4,"passed":true,"findings":["name the owner"],"version":3,"attempt":1,"round":3}`,
	})

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "shape.review"), 1, "one verification")
	assert.Empty(t, inputsOf(runner, "shape"), "and no builder round after it")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 4, gate.Payload["version"])
	assert.Equal(t, 4, reviewOf(t, gate)["round"])
	assert.Equal(t, 4, reviewOf(t, gate)["judged"])
}

func TestReview_AGateShownOnAnUnreviewedRevisionStaysWithItsHuman(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	seedGated(t, st, protocol.ArchitectModeDefault)
	seedLegacy(t, st, 2, []string{`{"score":4,"passed":true,"findings":["name the owner"],"version":1,"attempt":1,"round":1}`})
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventGate,
		Payload: []byte(`{"stage":"shape","version":2,"review":{"score":4,"threshold":4,"round":1,"passed":true,"revised":true,"judged":1}}`),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "shape.review"), "a shown gate is not reviewed")
	assert.Empty(t, stagesRun(runner))

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the gate is shown again")
	assert.Equal(t, 2, gate.Payload["version"])
}

func TestReview_AGateShownOnAPassWithFindingsStaysWithItsHuman(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	// A gate shown on a verdict that passed with findings still open, as a
	// legacy record may hold.
	seedGated(t, st, protocol.ArchitectModeDefault)

	_, err := st.PutArtifact(ctx, store.Artifact{
		BlueprintID: "BP1", Stage: reviewStage("shape"), Body: "# Review",
		Data: []byte(`{"score":4,"findings":["name the owner"],"version":1,"attempt":1,"round":1}`),
	})
	require.NoError(t, err)
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventGate, Payload: []byte(`{"stage":"shape","version":1}`),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the gate is shown again")
	assert.Equal(t, 1, gate.Payload["version"])
	assert.NotContains(t, reviewOf(t, gate), "revised")
	assert.Empty(t, stagesRun(runner), "no revision runs")

	// The rejection starts a new attempt.
	require.NoError(t, c.Message(ctx, "BP1", "reject make it smaller", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "shape.review"), 1, "the new attempt's version is reviewed")

	gates := rec.all(EventGate)
	require.Len(t, gates, 2)
	assert.Equal(t, 2, gates[1].Payload["version"])
	assert.NotContains(t, reviewOf(t, gates[1]), "revised")
	assert.Equal(t, 2, reviewOf(t, gates[1])["judged"])
}

func TestReview_AFailedRoundWaitsAndANoteRunsItAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = reviews(2, 5)
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Shape", nil), nil
	}

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.False(t, rec.has(EventGate), "the failed builder round stops the pipeline")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m2"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "shape", note.Payload["retries"])

	assert.Len(t, inputsOf(runner, "shape"), 3, "the round runs again")
	assert.Len(t, inputsOf(runner, "shape.review"), 2, "its earlier verdict is not asked for again")
	assert.True(t, rec.has(EventGate))
}

func TestReview_ARestartDuringTheReviewRunsItAgain(t *testing.T) {
	var calls atomic.Int32

	script := gatedScript()
	script["shape.review"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, st, _, _ := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	c2, _, _, rec2 := newGatedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	assert.Equal(t, int32(2), calls.Load())
	assert.True(t, rec2.has(EventGate))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var reviewers []string

	for _, r := range runs {
		if r.Kind == store.RunKindReviewer {
			reviewers = append(reviewers, r.Status)
		}
	}

	assert.Equal(t, []string{store.RunInterrupted, store.RunDone}, reviewers)
}

func TestReview_ARestartDuringABuilderRoundRunsTheRoundAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = reviews(2, 5)
	script["shape"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Shape", nil), nil
	}

	c, st, _, _ := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return shapes.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	c2, _, runner2, rec2 := newGatedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	again := inputsOf(runner2, "shape")
	require.Len(t, again, 1)
	assert.Contains(t, again[0].Task, "finding 1", "the round runs again with its findings")
	assert.NotEmpty(t, again[0].History, "on the builder's conversation")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var rounds []string

	for _, r := range runs {
		if r.Stage == "shape" && r.Kind == store.RunKindStage {
			rounds = append(rounds, fmt.Sprintf("attempt %d round %d %s", r.Attempt, r.Round, r.Status))
		}
	}

	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 interrupted", "attempt 1 round 2 done"}, rounds)
	assert.True(t, rec2.has(EventGate))
}

func TestReview_ARejectionWhoseRunNeverStartedRunsTheStageOnReopen(t *testing.T) {
	c, st, _, _ := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	// A crash after the reject row, before the stage's run started.
	_, err := st.AddGate(ctx, store.Gate{
		BlueprintID: "BP1", Stage: "shape", Version: 1, Decision: store.GateReject,
		Feedback: "make it smaller", Responder: store.ResponderHuman, DecidedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, c.Shutdown(ctx))

	c2, _, runner2, rec2 := newGatedOn(t, st, gatedScript())
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	shapes := inputsOf(runner2, "shape")
	require.Len(t, shapes, 1, "the rejected stage runs again")
	assert.Contains(t, shapes[0].Task, "make it smaller")

	gate, ok := rec2.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"], "its new version waits at the gate")
}

func TestReview_AGateReplyWhileItsReviewFailedFindsNoOpenGate(t *testing.T) {
	var calls atomic.Int32

	script := gatedScript()
	script["shape.review"] = func(context.Context, stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.False(t, rec.has(EventGate), "the failed review stops the pipeline")

	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", "auto", "m2"))

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.NotContains(t, change.Payload, "open", "the gate was never shown")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m3"))
	c.Wait("BP1")

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder, "the gate opens under the new policy")
}

func TestReview_AShownGateWithoutAVerdictStaysWithItsHuman(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	// A gate shown before its stage had a reviewer run, so with no verdict.
	seedGated(t, st, protocol.ArchitectModeDefault)
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventGate, Payload: []byte(`{"stage":"shape","version":1}`),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")
	require.True(t, rec.has(EventGate), "the gate is shown again")

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "shape.review"), "a shown gate is never reviewed")
	assert.Equal(t, []string{"plan"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderHuman, gates[0].Responder, "the approve decides it")
}

func TestReview_ANoteReachesTheReviewer(t *testing.T) {
	var calls atomic.Int32

	script := gatedScript()
	script["shape.review"] = func(context.Context, stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.NoError(t, c.Message(ctx, "BP1", "keep the owner named", "m2"))
	c.Wait("BP1")

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 2)
	assert.Contains(t, judged[1].Task, "keep the owner named")
	assert.True(t, rec.has(EventGate))
}

func TestReview_AReviewRunsAsItsStage(t *testing.T) {
	var calls atomic.Int32

	started, release := make(chan struct{}), make(chan struct{})

	script := gatedScript()
	script["shape.review"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			return stage.Output{}, errors.New("provider down")
		}

		close(started)
		parkUntil(ctx, release)

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, st, _, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.Equal(t, string(StateFailed), stateOf(t, st, "BP1"))

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m2"))
	signalled(t, started)
	assert.Equal(t, string(StateRunning), stateOf(t, st, "BP1"), "the review runs")

	require.NoError(t, c.Message(ctx, "BP1", "keep it short", "m3"))
	close(release)
	c.Wait("BP1")

	var during Event

	for _, n := range rec.all(EventNote) {
		if n.Payload["message_id"] == "m3" {
			during = n
		}
	}

	assert.Equal(t, "shape", during.Payload["stage"], "a note during the review names its stage")
}

func TestReview_TheBuilderRoundsConversation(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
	}{
		{name: "resumed without its system message", messages: []llm.Message{
			{Role: "system", Content: "persona"}, {Role: "user", Content: "task"}, {Role: "assistant", Content: "submitted"},
		}},
		{name: "started over without a transcript"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var shapes atomic.Int32

			script := gatedScript()
			script["shape.review"] = reviews(2, 5)
			script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
				return stage.Output{
					Artifact: stage.Artifact{Body: fmt.Sprintf("# Shape %d", shapes.Add(1))},
					Result:   harness.Result{Completed: true, Reason: "done", Messages: tt.messages},
				}, nil
			}

			c, _, runner, _ := newGated(t, script)

			startGated(t, c, protocol.ArchitectModeDefault)

			rounds := inputsOf(runner, "shape")
			require.Len(t, rounds, 2)

			if tt.messages == nil {
				assert.Nil(t, rounds[1].History)
				assert.Contains(t, rounds[1].Task, "# Shape 1", "its own artifact")
				assert.Contains(t, rounds[1].Task, "finding 1")

				return
			}

			require.NotEmpty(t, rounds[1].History)

			for _, m := range rounds[1].History {
				assert.NotEqual(t, "system", m.Role, "the round's own system message comes first")
			}
		})
	}
}

func TestReview_ABuilderRoundGetsEachNoteOnce(t *testing.T) {
	for _, resumes := range []bool{true, false} {
		t.Run(fmt.Sprintf("resumes %v", resumes), func(t *testing.T) {
			intakeRuns, intakeGoes := make(chan struct{}), make(chan struct{})
			shapeRuns, shapeGoes := make(chan struct{}), make(chan struct{})

			var shapes atomic.Int32

			script := gatedScript()
			script["shape.review"] = reviews(2, 5)
			script["intake"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
				close(intakeRuns)
				parkUntil(ctx, intakeGoes)

				return done("# Need", nil), nil
			}
			script["shape"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
				if shapes.Add(1) == 1 {
					close(shapeRuns)
					parkUntil(ctx, shapeGoes)
				}

				out := done("# Shape", nil)
				if !resumes {
					out.Result.Messages = nil
				}

				return out, nil
			}

			c, _, runner, _ := newGated(t, script)
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

			signalled(t, intakeRuns)
			require.NoError(t, c.Message(ctx, "BP1", "an early note", "m2"))
			close(intakeGoes)

			signalled(t, shapeRuns)
			require.NoError(t, c.Message(ctx, "BP1", "a late note", "m3"))
			close(shapeGoes)
			c.Wait("BP1")

			rounds := inputsOf(runner, "shape")
			require.Len(t, rounds, 2)
			assert.Contains(t, rounds[0].Task, "an early note")

			if resumes {
				assert.Contains(t, rounds[1].Task, "a late note", "the note its conversation has not seen")
				assert.NotContains(t, rounds[1].Task, "an early note", "the conversation holds it")

				return
			}

			assert.Equal(t, 1, strings.Count(rounds[1].Task, "an early note"))
			assert.Equal(t, 1, strings.Count(rounds[1].Task, "a late note"))
		})
	}
}

func TestReview_AReopenAfterACompletedReviewGoesOn(t *testing.T) {
	tests := []struct {
		name    string
		verdict string // "" stores none
		shapes  int
		script  func(context.Context, stage.Input) (stage.Output, error)
	}{
		{name: "a low verdict runs the builder round", verdict: `{"score":2,"findings":["name the owner"],"version":1,"attempt":1,"round":1}`, shapes: 1, script: verdicts(fixed(5, "R1"))},
		{name: "no verdict runs the review again", script: reviews(5)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := gatedScript()
			script["shape.review"] = tt.script

			c, st, runner, rec := newGated(t, script)
			ctx := context.Background()
			now := time.Now().UTC()

			// A crash after the review completed, before what follows it.
			seedGated(t, st, protocol.ArchitectModeDefault)

			id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "shape", Kind: store.RunKindReviewer, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
			require.NoError(t, err)
			require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{}, now))

			if tt.verdict != "" {
				_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: reviewStage("shape"), Body: "# Review", Data: []byte(tt.verdict)})
				require.NoError(t, err)
			}

			require.NoError(t, c.Start(ctx, startInput("BP1")))
			c.Wait("BP1")

			rounds := inputsOf(runner, "shape")
			require.Len(t, rounds, tt.shapes)

			if tt.shapes > 0 {
				assert.Contains(t, rounds[0].Task, "name the owner", "the stored verdict's findings")
			}

			assert.Len(t, inputsOf(runner, "shape.review"), 1)
			assert.True(t, rec.has(EventGate))
		})
	}
}

func TestReview_ACritiqueWithoutAReviewerFailsTheBlueprint(t *testing.T) {
	// Both stages have an artifact and a completed run in the record, so
	// only the missing reviewer stops the critique.
	for _, stageID := range []string{"nosuch", "intake"} {
		t.Run(stageID, func(t *testing.T) {
			c, st, runner, rec := newGated(t, gatedScript())
			ctx := context.Background()
			now := time.Now().UTC()

			seedGated(t, st, protocol.ArchitectModeDefault)

			_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "nosuch", Body: "# nosuch"})
			require.NoError(t, err)

			id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "nosuch", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
			require.NoError(t, err)
			require.NoError(t, st.EndRun(ctx, id, store.RunDone, "", store.RunUsage{}, now))

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			assert.Equal(t, stageStopped, c.critique(ctx, &run{}, &session{audit: c.newAudit()}, bp, stageID))
			assert.True(t, rec.has(EventError))
			assert.Equal(t, string(StateFailed), stateOf(t, st, "BP1"))
			assert.Zero(t, runner.calls())
		})
	}
}

func TestReview_TheReviewerGetsTheAnswersAndTheGateFeedback(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 1 {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Store", "Which store?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return done("# Shape", nil), nil
	}

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "keep it in Postgres"))
	c.Wait("BP1")
	require.True(t, rec.has(EventGate))

	require.NoError(t, c.Message(ctx, "BP1", "reject name the owner", "m3"))
	c.Wait("BP1")

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 2)
	assert.Contains(t, judged[0].Task, "keep it in Postgres", "the answer the decision rests on")
	assert.Contains(t, judged[1].Task, "keep it in Postgres")
	assert.Contains(t, judged[1].Task, "name the owner", "the feedback the attempt addresses")
}

func TestReview_ACompletedReviewLeavesTheBlueprintIdle(t *testing.T) {
	c, st, _, _ := newGated(t, gatedScript())
	ctx := context.Background()

	seedGated(t, st, protocol.ArchitectModeDefault)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	require.Equal(t, stageStored, c.critique(ctx, &run{}, &session{audit: c.newAudit()}, bp, "shape"))
	assert.Equal(t, string(StateIdle), stateOf(t, st, "BP1"), "the step after the review sets its own state")
}

func TestReview_TheBlueprintCapStopsTheReview(t *testing.T) {
	maxCost := func(costUSD float64) func(context.Context, stage.Input) (stage.Output, error) {
		return func(context.Context, stage.Input) (stage.Output, error) {
			return stage.Output{Result: harness.Result{Reason: "max_cost", TotalCostUSD: costUSD}}, errors.New("stage shape.review: no artifact")
		}
	}

	tests := []struct {
		name    string
		capUSD  float64
		review  func(context.Context, stage.Input) (stage.Output, error)
		reviews int
		capStop bool
	}{
		{name: "less than the floor left refuses the review", capUSD: 1, review: reviews(5), capStop: true},
		{name: "what the blueprint had left bound the review", capUSD: 2, review: maxCost(1.1), reviews: 1, capStop: true},
		{name: "the per-run cap bound the review", capUSD: 10, review: maxCost(2.1), reviews: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := gatedScript()
			// Shape spends 0.95: of a 1 USD cap that leaves 0.05, below a
			// review's 0.10 floor.
			script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
				out := done("# Shape", nil)
				out.Result.TotalCostUSD = 0.95

				return out, nil
			}
			script["shape.review"] = tt.review

			c, st, _, rec := newGated(t, script)
			ctx := context.Background()
			in := startInput("BP1")
			in.CostCapUSD = tt.capUSD
			in.Price = testPrice()
			require.NoError(t, c.Start(ctx, in))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)

			var reviewers int

			for _, r := range runs {
				if r.Kind == store.RunKindReviewer {
					reviewers++
				}
			}

			assert.Equal(t, tt.reviews, reviewers)
			assert.False(t, rec.has(EventGate), "the gate waits for its review")
			assert.Equal(t, string(StateIdle), stateOf(t, st, "BP1"))

			ev, ok := rec.first(EventError)
			require.True(t, ok)
			assert.Equal(t, "shape", ev.Payload["stage"])

			if !tt.capStop {
				assert.Empty(t, capStops(t, st), "a per-run cap stop is an ordinary failed run")
				assert.Equal(t, "max_cost", ev.Payload["reason"], "reported as a stage run's per-run cap is")

				return
			}

			assert.Equal(t, []string{"shape"}, capStops(t, st))
			assert.InDelta(t, tt.capUSD, ev.Payload["cap_usd"], 1e-9)
		})
	}
}
