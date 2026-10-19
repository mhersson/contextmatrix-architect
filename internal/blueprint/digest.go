package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// digest is what a step reads of another slice's artifact in place of the
// whole, by the shape the producing stage's checks give its data: the ids a
// later slice must not repeat and depends on, the contracts it lines up with,
// and one line of text each. read_artifact returns the whole.
type digest struct {
	Decisions []digestDecision          `json:"decisions,omitempty"`
	Contracts []string                  `json:"contracts,omitempty"`
	Criteria  []digestLine              `json:"criteria,omitempty"`
	Cards     []digestCard              `json:"cards,omitempty"`
	Manual    []digestLine              `json:"manual,omitempty"`
	Setup     map[string]contract.Setup `json:"setup,omitempty"`
}

type digestDecision struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Chosen string `json:"chosen"`
}

type digestLine struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type digestCard struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Project   string   `json:"project"`
	Type      string   `json:"type"`
	Where     []string `json:"where,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Criteria  []string `json:"criteria,omitempty"`
}

func (d digest) empty() bool {
	return len(d.Decisions)+len(d.Contracts)+len(d.Criteria)+len(d.Cards)+len(d.Manual) == 0
}

// misfit counts the entries of another shape that a digest would drop; any
// makes digestOf return false, so a digest only ever holds data that fits.
type misfit int

// appendLine adds a line that carries an id or text.
func (m *misfit) appendLine(dst []digestLine, id, text string) []digestLine {
	if id == "" && strings.TrimSpace(text) == "" {
		*m++

		return dst
	}

	return append(dst, digestLine{ID: id, Text: oneLine(text)})
}

// appendContracts adds the contracts; a blank one is a misfit.
func (m *misfit) appendContracts(dst, cs []string) []string {
	for _, c := range cs {
		if strings.TrimSpace(c) == "" {
			*m++

			continue
		}

		dst = append(dst, c)
	}

	return dst
}

// appendDecisions adds the decisions; one with neither text nor chosen is a
// misfit, as the digest would hold an empty line.
func (m *misfit) appendDecisions(dst, ds []digestDecision) []digestDecision {
	for _, dec := range ds {
		if dec.Text == "" && dec.Chosen == "" {
			*m++

			continue
		}

		dst = append(dst, dec)
	}

	return dst
}

// oneLineMax caps a digest's line of text.
const oneLineMax = 200

// oneLine is s's first line, cut at oneLineMax runes.
func oneLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.TrimSpace(line)

	if r := []rune(line); len(r) > oneLineMax {
		return string(r[:oneLineMax]) + "..."
	}

	return line
}

// digestOf digests art, an artifact of a stage that declares checks. false
// when the checks give its data no digest shape, the data does not decode
// or holds an item of another shape, or nothing is left: the step then
// reads the whole artifact.
func digestOf(checks []string, art store.Artifact) (digest, bool) {
	var (
		d   digest
		bad misfit
	)

	switch {
	case slices.Contains(checks, pack.CheckCards):
		var draft contract.Draft
		if json.Unmarshal(art.Data, &draft) != nil {
			return digest{}, false
		}

		for _, card := range draft.Cards {
			if card.ID == "" && card.Title == "" {
				bad++

				continue
			}

			dc := digestCard{ID: card.ID, Title: card.Title, Project: card.Project, Type: card.Type, DependsOn: card.DependsOn, Criteria: card.Criteria}
			for _, w := range card.Where {
				dc.Where = append(dc.Where, w.Path)
			}

			d.Cards = append(d.Cards, dc)
		}

		for _, m := range draft.Manual {
			d.Manual = bad.appendLine(d.Manual, m.ID, m.Text)
		}

		d.Setup = draft.Setup
	case slices.Contains(checks, pack.CheckDecisions), slices.Contains(checks, pack.CheckCriteria):
		var data struct {
			// Decisions and contracts at the top level are the shared stage's
			// shape; a per-slice design's checks do not rule on where they sit.
			Decisions []digestDecision `json:"decisions"`
			Contracts []string         `json:"contracts"`
			Slices    []struct {
				Decisions []digestDecision `json:"decisions"`
				Contracts []string         `json:"contracts"`
				Criteria  []digestLine     `json:"criteria"`
				Manual    []digestLine     `json:"manual"`
			} `json:"slices"`
		}

		if json.Unmarshal(art.Data, &data) != nil {
			return digest{}, false
		}

		d.Decisions = bad.appendDecisions(d.Decisions, data.Decisions)
		d.Contracts = bad.appendContracts(d.Contracts, data.Contracts)

		for _, s := range data.Slices {
			d.Decisions = bad.appendDecisions(d.Decisions, s.Decisions)

			d.Contracts = bad.appendContracts(d.Contracts, s.Contracts)

			for _, c := range s.Criteria {
				d.Criteria = bad.appendLine(d.Criteria, c.ID, c.Text)
			}

			for _, m := range s.Manual {
				d.Manual = bad.appendLine(d.Manual, m.ID, m.Text)
			}
		}
	default:
		return digest{}, false
	}

	if bad > 0 || d.empty() {
		return digest{}, false
	}

	return d, true
}

// digested reports whether st reads in, a step it consumes, as a digest:
// another slice's, on a blueprint with two or more slices. A whole-blueprint
// step reads the stage it loops to whole: its verdict judges those items one
// by one.
func digested(st, in pipelineStep, sliced bool) bool {
	switch {
	case !sliced || in.Slice == "":
		return false
	case st.Slice != "":
		return in.Slice != st.Slice
	default:
		return in.Stage.ID != st.LoopsTo
	}
}

// readsDigests reports whether st's task can hold a digest, so its run is
// offered read_artifact.
func readsDigests(steps []pipelineStep, pieces []piece, st pipelineStep) bool {
	return slices.ContainsFunc(consumedSteps(steps, st), func(in pipelineStep) bool {
		return !in.Collapsed && digested(st, in, len(pieces) > 1)
	})
}

// writeDigest gives a step another slice's artifact as its digest, with the
// read_artifact call that returns the whole.
func writeDigest(b *strings.Builder, in pipelineStep, art store.Artifact, d digest) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode the digest of %s: %w", in.ID, err)
	}

	stageID, slice := pack.SplitStep(in.ID)

	fmt.Fprintf(b, "## %s\n\nDigest of version %d: the ids, contracts and one line of text each that later slices build on. "+
		"read_artifact with stage %q and slice %q returns the whole artifact.\n\n```json\n%s\n```\n\n", in.ID, art.Version, stageID, slice, raw)

	return nil
}

// Tool names and the line every read_artifact result opens with.
const (
	readArtifactName = "read_artifact"
	readMarker       = "read_artifact: "
)

// A read's result must stay out of the stored transcript but its first line.
var _ stage.Transient = artifactReader{}

// artifactReader is read_artifact for one run of st: any stored version of
// a step st consumes, as a task gives it whole.
type artifactReader struct {
	c  *Controller
	id string
	st pipelineStep
}

func (artifactReader) Name() string   { return readArtifactName }
func (artifactReader) ReadOnly() bool { return true }

func (artifactReader) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: readArtifactName,
		Description: "Read the whole of an artifact this task gives as a digest, or of any other artifact it reads: " +
			"its markdown body and its data.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"stage": {"type": "string", "description": "the stage id, such as solution_design"},
				"slice": {"type": "string", "description": "the slice id, such as SL1, for a stage that runs per slice"},
				"version": {"type": "integer", "description": "a stored version; the latest when left out"}
			},
			"required": ["stage"]
		}`),
	}}
}

func (r artifactReader) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	stageID, _ := args["stage"].(string)
	slice, _ := args["slice"].(string)
	key := pack.StepKey(strings.TrimSpace(stageID), strings.ToUpper(strings.TrimSpace(slice)))

	steps, _, err := r.c.steps(ctx, r.id)
	if err != nil {
		return tools.Result{}, fmt.Errorf("read_artifact: %w", err)
	}

	var readable []string

	for _, in := range consumedSteps(steps, r.st) {
		if !in.Collapsed {
			readable = append(readable, in.ID)
		}
	}

	if !slices.Contains(readable, key) {
		return tools.Result{}, fmt.Errorf("read_artifact: this run reads %s, not %s", strings.Join(readable, ", "), key)
	}

	art, err := r.version(ctx, key, args["version"])
	if err != nil {
		return tools.Result{}, err
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s%s version %d\n\n", readMarker, key, art.Version)
	writeArtifact(&b, key, art)

	return tools.Result{Text: b.String()}, nil
}

// version is key's stored version v, the latest when v is absent.
func (r artifactReader) version(ctx context.Context, key string, v any) (store.Artifact, error) {
	if v == nil {
		art, err := r.c.opts.Store.LatestArtifact(ctx, r.id, key)
		if err != nil {
			return store.Artifact{}, fmt.Errorf("read_artifact: %s: %w", key, err)
		}

		return art, nil
	}

	n, ok := v.(float64)
	if !ok || n != float64(int(n)) || n < 1 {
		return store.Artifact{}, fmt.Errorf("read_artifact: version is a whole number from 1")
	}

	arts, err := r.c.opts.Store.ListArtifacts(ctx, r.id)
	if err != nil {
		return store.Artifact{}, fmt.Errorf("read_artifact: %w", err)
	}

	for _, a := range arts {
		if a.Stage == key && a.Version == int(n) {
			return a, nil
		}
	}

	return store.Artifact{}, fmt.Errorf("read_artifact: %s has no version %d", key, int(n))
}

// Stored keeps a read's first line: the record holds the artifact itself.
func (artifactReader) Stored(result string) string {
	head, _, _ := strings.Cut(result, "\n")
	if !strings.HasPrefix(head, readMarker) {
		return result
	}

	return head
}
