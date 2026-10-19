package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// pipelineStep is one place in a blueprint's pipeline: a pack stage and,
// for a per-slice stage once slicing has stored a version, the slice it
// runs for. ID shadows the stage's id with the step's record key
// (pack.StepKey), so whatever reads or writes the record names the step;
// Stage.ID is the pack id, for the pack's own lookups.
type pipelineStep struct {
	pack.Stage
	ID    string
	Slice string
	// Collapsed marks a shared stage that does not run: fewer than two
	// slices, or a record from before slices. It is recorded, never run.
	Collapsed bool
}

// piece is one slice of slicing's newest version: SL<n> by its position,
// and its title.
type piece struct {
	id, title string
}

// sliceID names the n-th slice, from 1.
func sliceID(n int) string { return fmt.Sprintf("SL%d", n) }

// piecesOf reads a slicing artifact's slices in order. The n-th is SL<n>
// whatever id its data gives it; the slices check makes a new submit agree.
func piecesOf(art store.Artifact) []piece {
	var d struct {
		Slices []struct {
			Title string `json:"title"`
		} `json:"slices"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &d) != nil {
		return nil
	}

	out := make([]piece, 0, len(d.Slices))
	for i, s := range d.Slices {
		out = append(out, piece{id: sliceID(i + 1), title: strings.TrimSpace(s.Title)})
	}

	return out
}

// unsliced reports a record written before stages ran per slice: a run of
// a per-slice stage under its plain id. Such a blueprint keeps running its
// per-slice stages once, for the whole blueprint.
func unsliced(pk *pack.Pack, runs []store.Run) bool {
	return slices.ContainsFunc(runs, func(r store.Run) bool {
		st, ok := pk.Stage(r.Stage)

		return ok && st.PerSlice
	})
}

// stepsOf is the pipeline the pack gives pieces: its stages in order, the
// per-slice ones repeated for every piece, all of one slice's steps before
// the next slice's. Without pieces the per-slice stages are one step each.
// collapse marks the shared stages collapsed.
func stepsOf(pk *pack.Pack, pieces []piece, collapse bool) []pipelineStep {
	var (
		out   []pipelineStep
		group []pack.Stage
	)

	// The pack loader keeps the per-slice stages contiguous, so the group is
	// complete at the first stage after it.
	flush := func() {
		for _, p := range pieces {
			for _, s := range group {
				out = append(out, pipelineStep{Stage: s, ID: pack.StepKey(s.ID, p.id), Slice: p.id})
			}
		}

		group = nil
	}

	for _, st := range pk.Stages {
		if st.PerSlice && len(pieces) > 0 {
			group = append(group, st)

			continue
		}

		flush()

		out = append(out, pipelineStep{Stage: st, ID: st.ID, Collapsed: collapse && st.Shared})
	}

	flush()

	return out
}

// blueprintSteps is the pipeline a blueprint's record gives, and the pieces
// it repeats per-slice stages for: slicing's newest version's, or none
// before slicing stores one and on a record from before stages ran per
// slice. slicing is the newest artifact of the pack's slicing stage, nil
// when there is none. A shared stage collapses with fewer than two pieces,
// and on a record from before slices.
func blueprintSteps(pk *pack.Pack, runs []store.Run, slicing *store.Artifact) ([]pipelineStep, []piece) {
	switch {
	case slicing == nil:
		return stepsOf(pk, nil, false), nil
	case unsliced(pk, runs):
		return stepsOf(pk, nil, true), nil
	}

	pieces := piecesOf(*slicing)

	return stepsOf(pk, pieces, len(pieces) < 2), pieces
}

// steps reads blueprint id's pipeline from the store.
func (c *Controller) steps(ctx context.Context, id string) ([]pipelineStep, []piece, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("blueprint: list runs: %w", err)
	}

	var slicing *store.Artifact

	if sid := c.opts.Pack.SlicingStage(); sid != "" {
		art, err := c.opts.Store.LatestArtifact(ctx, id, sid)

		switch {
		case err == nil:
			slicing = &art
		case !errors.Is(err, store.ErrNotFound):
			return nil, nil, fmt.Errorf("blueprint: read %s: %w", pack.StageName(sid, false), err)
		}
	}

	steps, pieces := blueprintSteps(c.opts.Pack, runs, slicing)

	return steps, pieces, nil
}

// shownPieces is the slices steps repeat per-slice stages for: pieces, or
// none when no step carries a slice (before slicing stores a version, on a
// record from before slices and on a pack with no per-slice stage). It is
// the one answer to which slices show, in the view and in the names.
func shownPieces(steps []pipelineStep, pieces []piece) []piece {
	if !slices.ContainsFunc(steps, func(s pipelineStep) bool { return s.Slice != "" }) {
		return nil
	}

	return pieces
}

// sliced reports whether blueprint id runs per slice with two or more
// slices (shownPieces), the only case in which text a person reads names a
// step's slice; false when the record cannot be read.
func (c *Controller) sliced(ctx context.Context, id string) bool {
	steps, pieces, err := c.steps(ctx, id)

	return err == nil && len(shownPieces(steps, pieces)) >= 2
}

// stepName is how key reads to a person on blueprint id: with its slice
// only when the blueprint has two or more.
func (c *Controller) stepName(ctx context.Context, id, key string) string {
	return pack.StageName(key, c.sliced(ctx, id))
}

// stepAt is the step a record key names: its pack stage and the slice the
// key carries. false when the key names no pack stage, or a slice for a
// stage that does not repeat per slice.
func (c *Controller) stepAt(key string) (pipelineStep, bool) {
	stageID, slice := pack.SplitStep(key)

	st, ok := c.opts.Pack.Stage(stageID)
	if !ok || (slice != "" && !st.PerSlice) {
		return pipelineStep{}, false
	}

	return pipelineStep{Stage: st, ID: key, Slice: slice}, true
}

// stepIndex is key's position in steps; -1 when it is not one of them.
func stepIndex(steps []pipelineStep, key string) int {
	return slices.IndexFunc(steps, func(s pipelineStep) bool { return s.ID == key })
}

// earlierSteps lists the steps of st's stage for every slice before st's,
// in slice order; none for a step that is no slice's.
func earlierSteps(steps []pipelineStep, st pipelineStep) []pipelineStep {
	if st.Slice == "" {
		return nil
	}

	var out []pipelineStep

	// Steps run in slice order, so the earlier slices' come first; stop at st's own.
	for _, s := range steps {
		if s.Slice == st.Slice {
			break
		}

		if s.Stage.ID == st.Stage.ID {
			out = append(out, s)
		}
	}

	return out
}

// consumedSteps lists the steps whose latest artifacts st reads, in the
// order its pack entry consumes them: a whole-blueprint stage's one step; a
// per-slice stage's step for st's own slice, or for every slice in order
// when st runs for the whole blueprint; then, for a per-slice st, each
// earlier slice's steps of the stages its earlier_slices names, slice by
// slice.
func consumedSteps(steps []pipelineStep, st pipelineStep) []pipelineStep {
	var out []pipelineStep

	for _, id := range st.Consumes {
		for _, s := range steps {
			if s.Stage.ID == id && (st.Slice == "" || s.Slice == "" || s.Slice == st.Slice) {
				out = append(out, s)
			}
		}
	}

	if st.Slice == "" {
		return out
	}

	// Steps run in slice order, so the earlier slices' come first; stop at st's own.
	for _, s := range steps {
		if s.Slice == st.Slice {
			break
		}

		if s.Slice != "" && slices.Contains(st.EarlierSlices, s.Stage.ID) {
			out = append(out, s)
		}
	}

	return out
}

// loopTargets are the steps of stageID in order, one per slice or the one
// step of an unsliced blueprint: a loop stage's targets when stageID is the
// stage it loops to, and the cards stage's steps for the draft.
func loopTargets(steps []pipelineStep, stageID string) []pipelineStep {
	var out []pipelineStep

	for _, s := range steps {
		if s.Stage.ID == stageID {
			out = append(out, s)
		}
	}

	return out
}

// owningTarget is where a not-ready verdict sends the loop: the earliest
// target whose cards include a failing one, or the first target when none
// does, as for a verdict that judged no card. drafts holds each target's
// card ids by its key.
func owningTarget(targets []pipelineStep, drafts map[string][]string, failing []string) string {
	for _, t := range targets {
		if slices.ContainsFunc(drafts[t.ID], func(card string) bool { return slices.Contains(failing, card) }) {
			return t.ID
		}
	}

	return targets[0].ID
}

// inChain reports whether key's latest done stage run, a run of a loop
// target, belongs to a chain loopStage's verdict or the sign-off started,
// which goes on through every later slice's target. Walking back over the
// done stage runs before it, every run of the target's stage is passed
// over, whatever its slice: an earlier slice's run in the chain, or this
// slice's own earlier run, as a gated target's re-run leaves. The first
// run of another stage decides, and only loopStage's makes a chain. A
// first pass, and one a gate or a rejection of a design started, reach the
// target from its own slice's earlier steps instead.
func inChain(runs []store.Run, key, loopStage string) bool {
	target, _ := pack.SplitStep(key)

	var done []store.Run

	for _, r := range runs {
		if r.Kind == store.RunKindStage && r.Status == store.RunDone {
			done = append(done, r)
		}
	}

	i := -1

	for j, r := range done {
		if r.Stage == key {
			i = j
		}
	}

	for ; i > 0; i-- {
		switch before, _ := pack.SplitStep(done[i-1].Stage); before {
		case loopStage:
			return true
		case target:
			// A run of the target's stage: the run before it decides.
		default:
			return false
		}
	}

	return false
}

// nextSliceStep is the step of st's stage for the slice after st's; "" for
// the last slice.
func nextSliceStep(steps []pipelineStep, st pipelineStep) string {
	seen := false

	for _, s := range steps {
		switch {
		case s.ID == st.ID:
			seen = true
		case seen && s.Stage.ID == st.Stage.ID:
			return s.ID
		}
	}

	return ""
}

// chained reports whether st, a loop target's step, runs in a chain its
// loop stage or the sign-off started (inChain). A step no loop targets is
// never chained, and reads no runs.
func (c *Controller) chained(ctx context.Context, id string, st pipelineStep) (bool, error) {
	loop := c.loopOf(st.Stage.ID)
	if loop == "" {
		return false, nil
	}

	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return false, fmt.Errorf("blueprint: list runs: %w", err)
	}

	return inChain(runs, st.ID, loop), nil
}

// loopOf is the loop stage that loops to stageID; "" when none does.
func (c *Controller) loopOf(stageID string) string {
	for _, s := range c.opts.Pack.Stages {
		if s.LoopsTo == stageID {
			return s.ID
		}
	}

	return ""
}

// writeSlice opens a per-slice task: the slice it works on, its title and
// its place among the slices.
func writeSlice(b *strings.Builder, pieces []piece, slice string) {
	for i, p := range pieces {
		if p.id != slice {
			continue
		}

		fmt.Fprintf(b, "## The slice\n\nThis run works on slice %s, %q, number %d of %d in slicing's order. "+
			"Earlier slices are settled: build on what they hold, given below whole or as digests whose whole artifact read_artifact returns, and never redo or duplicate it.\n\n",
			p.id, p.title, i+1, len(pieces))

		return
	}
}
