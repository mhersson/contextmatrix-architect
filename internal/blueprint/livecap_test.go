package blueprint

import (
	"context"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

var errBoom = errors.New("boom")

// fifthLine is a usage line that costs 0.20 USD at testPrice.
const fifthLine = `{"seq":1,"kind":"usage","data":{"prompt_tokens":100000,"completion_tokens":10000,"cost_usd":0}}` + "\n"

// spendFifths writes up to n fifthLines to in's sink, stopping at the first
// that cuts the run, and returns that line's number, 0 when none did.
func spendFifths(ctx context.Context, in stage.Input, n int) int {
	for i := 1; i <= n; i++ {
		_, _ = in.Events.Write([]byte(fifthLine))

		if ctx.Err() != nil {
			return i
		}
	}

	return 0
}

// fifths is the harness result of k fifthLines, so the ledger agrees.
func fifths(k int) harness.Result {
	return harness.Result{PromptTokens: int64(k) * 100_000, CompletionTokens: int64(k) * 10_000}
}

func okWrite() error { return nil }

// seededCap is a cell seeded with usd.
func seededCap(usd float64) *liveCap {
	l := &liveCap{}
	l.seed(usd)

	return l
}

// runCapOf is the run cap in force for the run given in, 0 when none binds.
func runCapOf(t *testing.T, in stage.Input) float64 {
	t.Helper()

	s, ok := in.Events.(*harnessSink)
	require.True(t, ok)

	return s.limit.read().usd
}

// assertRead checks a limit read field by field.
func assertRead(t *testing.T, want, got capRead) {
	t.Helper()

	assert.InDelta(t, want.usd, got.usd, 1e-9, "usd")
	assert.Equal(t, want.capped, got.capped, "capped")
	assert.InDelta(t, want.blueprintUSD, got.blueprintUSD, 1e-9, "blueprintUSD")
	assert.Equal(t, want.bound, got.bound, "bound")
}

// pricedSink is a priced sink under l, and the context its stop cancels.
func pricedSink(t *testing.T, l runLimit) (*harnessSink, context.Context) {
	t.Helper()

	ctx, stop := context.WithCancelCause(context.Background())

	t.Cleanup(func() { stop(nil) })

	return &harnessSink{price: testPrice(), limit: l, stop: stop, canceled: ctx.Done()}, ctx
}

// feedFifths feeds n fifthLines to s.
func feedFifths(s *harnessSink, n int) {
	for range n {
		s.account([]byte(fifthLine))
	}
}

func TestRunLimit_IsTheSmallestBoundInForce(t *testing.T) {
	askingRun, _ := pricedSink(t, runLimit{perRunUSD: 2})
	askingRun.charge(1.5)

	tests := []struct {
		name   string
		perRun float64
		cell   *liveCap
		base   float64
		parent *harnessSink
		want   capRead
	}{
		{name: "nothing bounds the run", want: capRead{}},
		{name: "the per-run cap alone", perRun: 2, want: capRead{usd: 2, capped: true}},
		{
			name: "the blueprint's remainder below the per-run cap", perRun: 2, cell: seededCap(1), base: 0.25,
			want: capRead{usd: 0.75, capped: true, bound: true, blueprintUSD: 1},
		},
		{
			name: "the per-run cap below the remainder", perRun: 0.5, cell: seededCap(10),
			want: capRead{usd: 0.5, capped: true, blueprintUSD: 10},
		},
		{
			name: "a tie goes to the per-run cap", perRun: 1, cell: seededCap(1.5), base: 0.5,
			want: capRead{usd: 1, capped: true, blueprintUSD: 1.5},
		},
		{
			name: "no per-run cap", cell: seededCap(1), base: 0.4,
			want: capRead{usd: 0.6, capped: true, bound: true, blueprintUSD: 1},
		},
		{
			name: "a cap lowered past the spend", perRun: 2, cell: seededCap(1), base: 1.2,
			want: capRead{usd: -0.2, capped: true, bound: true, blueprintUSD: 1},
		},
		{
			name: "the asking run's remainder", perRun: 2, cell: seededCap(10), parent: askingRun,
			want: capRead{usd: 0.5, capped: true, blueprintUSD: 10},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := runLimit{perRunUSD: tt.perRun, blueprint: tt.cell, baseUSD: tt.base, parent: tt.parent}
			assertRead(t, tt.want, l.read())
		})
	}
}

func TestRunLimit_FollowsTheCell(t *testing.T) {
	cell := seededCap(1)
	l := runLimit{perRunUSD: 2, blueprint: cell}

	assertRead(t, capRead{usd: 1, capped: true, blueprintUSD: 1, bound: true}, l.read())

	require.NoError(t, cell.set(1.5, okWrite))
	assertRead(t, capRead{usd: 1.5, capped: true, blueprintUSD: 1.5, bound: true}, l.read())

	require.NoError(t, cell.set(0, okWrite))
	assertRead(t, capRead{usd: 2, capped: true}, l.read())
}

func TestRunLimit_TheHarnessGetsAFixedCap(t *testing.T) {
	asking, _ := pricedSink(t, runLimit{perRunUSD: 2})
	asking.charge(1.5)

	tests := []struct {
		name   string
		limit  runLimit
		priced bool
		want   float64
	}{
		{name: "a priced run gets the per-run cap alone", limit: runLimit{perRunUSD: 2, blueprint: seededCap(1)}, priced: true, want: 2},
		{name: "a priced run without a per-run cap gets none", limit: runLimit{blueprint: seededCap(1)}, priced: true, want: 0},
		{name: "an unpriced run gets the limit as it stands", limit: runLimit{perRunUSD: 2, parent: asking}, want: 0.5},
		{name: "an unpriced run under the per-run cap alone gets it", limit: runLimit{perRunUSD: 2}, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, tt.limit.fixedUSD(tt.priced), 1e-9)
		})
	}
}

func TestRunLimit_KeptByAnUnpricedRunIsFixedAtStart(t *testing.T) {
	asking, _ := pricedSink(t, runLimit{perRunUSD: 2})
	asking.charge(1.5)

	cell := seededCap(1)
	limit := runLimit{perRunUSD: 2, blueprint: cell, parent: asking}

	assert.Equal(t, limit, limit.kept(true), "a priced run keeps the live limit")

	kept := limit.kept(false)
	assert.InDelta(t, 0.5, kept.read().usd, 1e-9, "the asking run's remainder at start")
	assert.Nil(t, kept.blueprint)
	assert.Nil(t, kept.parent)

	asking.charge(0.25)
	assert.InDelta(t, 0.5, kept.read().usd, 1e-9, "it does not follow the asking run")
}

func TestLiveCap_SeedsOnceAndSetsOnlyAWrittenCap(t *testing.T) {
	l := &liveCap{}
	l.seed(1)
	l.seed(3)
	assert.InDelta(t, 1.0, l.get(), 1e-9, "only the first seed counts")

	require.NoError(t, l.set(4, okWrite))
	assert.InDelta(t, 4.0, l.get(), 1e-9)

	require.ErrorIs(t, l.set(5, func() error { return errBoom }), errBoom)
	assert.InDelta(t, 4.0, l.get(), 1e-9, "a cap the store refused is not taken")

	fresh := &liveCap{}
	require.NoError(t, fresh.set(2, okWrite))
	fresh.seed(1)
	assert.InDelta(t, 2.0, fresh.get(), 1e-9, "a seed after a set changes nothing")
}

func TestHarnessSink_ACapChangeReachesTheRunInFlight(t *testing.T) {
	cell := seededCap(1)
	s, ctx := pricedSink(t, runLimit{perRunUSD: 2, blueprint: cell})

	feedFifths(s, 4)
	require.NoError(t, ctx.Err())

	require.NoError(t, cell.set(1.5, okWrite))
	feedFifths(s, 3)
	require.NoError(t, ctx.Err(), "1.40 is under the raised cap")

	feedFifths(s, 1)
	require.ErrorIs(t, context.Cause(ctx), errCostCap, "1.60 passes it")

	cut := capRead{usd: 1.5, capped: true, blueprintUSD: 1.5, bound: true}
	assertRead(t, cut, s.capAtEnd())

	// A raise after the cut leaves the end naming the cap it was cut under.
	require.NoError(t, cell.set(4, okWrite))
	assertRead(t, cut, s.capAtEnd())
}

func TestHarnessSink_CountsTheHigherOfReportedAndPricedCost(t *testing.T) {
	s, _ := pricedSink(t, runLimit{})

	s.account([]byte(`{"seq":1,"kind":"usage","data":{"prompt_tokens":100000,"completion_tokens":10000,"cost_usd":0.5}}`))

	spent, _ := s.tally()
	assert.InDelta(t, 0.5, spent, 1e-9, "the reported cost is above the priced 0.20")

	s.account([]byte(`{"seq":2,"kind":"usage","data":{"prompt_tokens":100000,"completion_tokens":10000,"cost_usd":0.1}}`))

	spent, _ = s.tally()
	assert.InDelta(t, 0.7, spent, 1e-9, "the priced 0.20 is above the reported cost")
}

func TestHarnessSink_ACapSetMidRunCountsWhatWasSpentBefore(t *testing.T) {
	cell := seededCap(0)
	s, ctx := pricedSink(t, runLimit{blueprint: cell})

	feedFifths(s, 3)
	require.NoError(t, ctx.Err(), "nothing bounds the run")

	require.NoError(t, cell.set(0.5, okWrite))
	feedFifths(s, 1)
	require.ErrorIs(t, context.Cause(ctx), errCostCap, "0.80 spent passes the 0.50 set mid-run")
}

func TestHarnessSink_AResponderFollowsTheAskingRunsRaisedCap(t *testing.T) {
	cell := seededCap(1)

	parent, _ := pricedSink(t, runLimit{blueprint: cell})
	parent.charge(0.8)

	child, ctx := pricedSink(t, runLimit{blueprint: cell, parent: parent})
	assert.InDelta(t, 0.2, child.limit.read().usd, 1e-9, "what the asking run has left")

	require.NoError(t, cell.set(2, okWrite))
	assert.InDelta(t, 1.2, child.limit.read().usd, 1e-9, "what the asking run has left under the raise")

	feedFifths(child, 2)
	require.NoError(t, ctx.Err())
}

func TestHarnessSink_AnUnpricedSinkCountsNoCharge(t *testing.T) {
	_, stop := context.WithCancelCause(context.Background())

	t.Cleanup(func() { stop(nil) })

	f := &flight{}
	s := &harnessSink{limit: runLimit{perRunUSD: 2}, stop: stop, flight: f}

	s.charge(0.5)

	spent, charged := s.tally()
	assert.Zero(t, spent, "an unpriced run counts only its own reported cost")
	assert.Zero(t, charged)
	assert.InDelta(t, 0.5, f.view().charged, 1e-9, "the view still shows what its responders cost")
}
