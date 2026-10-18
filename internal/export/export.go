// Package export renders a blueprint's record as a markdown bundle. The
// store is the record; this is a rendering of what it already holds, so
// everything written here has passed the session's redactor.
package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Bundle is everything the store holds for one blueprint.
type Bundle struct {
	Blueprint   store.Blueprint
	Runs        []store.Run
	Artifacts   []store.Artifact
	Events      []store.Event
	Sources     []store.Source
	Questions   []store.Question
	Gates       []store.Gate
	Emissions   []store.Emission
	Transcripts map[int64]json.RawMessage
}

// Write renders b into dir, creating it, and returns the relative paths
// written.
func Write(dir string, b Bundle) ([]string, error) {
	files := map[string][]byte{
		"blueprint.md": []byte(blueprintMD(b)),
		"questions.md": []byte(questionsMD(b.Questions)),
		"sources.md":   []byte(sourcesMD(b.Sources)),
		"events.jsonl": eventsJSONL(b.Events),
	}

	for _, a := range b.Artifacts {
		files[fmt.Sprintf("artifacts/%s-v%d.md", a.Stage, a.Version)] = []byte(artifactMD(a))
	}

	for runID, t := range b.Transcripts {
		files[fmt.Sprintf("transcripts/%d.json", runID)] = append([]byte(nil), t...)
	}

	written := make([]string, 0, len(files))

	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return nil, fmt.Errorf("export: mkdir: %w", err)
		}

		if err := os.WriteFile(full, content, 0o640); err != nil { //nolint:gosec // bundles are meant to be read by the operator's group
			return nil, fmt.Errorf("export: write %s: %w", rel, err)
		}

		written = append(written, rel)
	}

	return written, nil
}

func blueprintMD(b Bundle) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "# Blueprint %s\n\n", b.Blueprint.ID)
	fmt.Fprintf(&sb, "- Projects: %s\n- Mode: %s\n- State: %s\n- Cost cap: %.2f USD\n- Created: %s\n\n",
		strings.Join(b.Blueprint.Projects, ", "), b.Blueprint.Mode, b.Blueprint.State, b.Blueprint.CostCapUSD, b.Blueprint.CreatedAt.Format("2006-01-02 15:04 UTC"))

	sb.WriteString("## Runs\n\n| Run | Stage | Kind | Attempt | Round | Status | Model | Cost USD | Prompt | Completion |\n|---|---|---|---|---|---|---|---|---|---|\n")

	total := 0.0

	for _, r := range b.Runs {
		total += r.Usage.CostUSD
		fmt.Fprintf(&sb, "| %d | %s | %s | %d | %d | %s | %s | %.4f | %d | %d |\n",
			r.ID, pack.StageName(r.Stage, false), r.Kind, r.Attempt, r.Round, r.Status, r.Model, r.Usage.CostUSD, r.Usage.PromptTokens, r.Usage.CompletionTokens)
	}

	fmt.Fprintf(&sb, "\nTotal recorded cost: %.4f USD\n", total)

	if len(b.Gates) > 0 {
		sb.WriteString("\n## Gates\n\n| Decided | Stage | Version | Decision | Responder | Feedback |\n|---|---|---|---|---|---|\n")

		for _, g := range b.Gates {
			fmt.Fprintf(&sb, "| %s | %s | %d | %s | %s | %s |\n",
				g.DecidedAt.Format("2006-01-02 15:04 UTC"), pack.StageName(g.Stage, false), g.Version, g.Decision, g.Responder, cell(g.Feedback))
		}
	}

	if len(b.Emissions) > 0 {
		sb.WriteString("\n## Emitted\n\n| Created | Kind | Draft | Project | ContextMatrix id |\n|---|---|---|---|---|\n")

		for _, e := range b.Emissions {
			fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n",
				e.CreatedAt.Format("2006-01-02 15:04 UTC"), e.Kind, cell(e.DraftID), cell(e.Project), cell(e.CMID))
		}
	}

	return sb.String()
}

// cell makes free text safe inside a markdown table cell. A backslash is
// escaped too, so text ending in one cannot unescape the pipe after it.
func cell(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", `\`, `\\`, "|", `\|`).Replace(s)
}

func artifactMD(a store.Artifact) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "<!-- stage: %s, version: %d, created: %s -->\n\n", a.Stage, a.Version, a.CreatedAt.Format("2006-01-02 15:04 UTC"))
	sb.WriteString(a.Body)

	if len(a.Data) > 0 {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, a.Data, "", "  "); err == nil {
			sb.WriteString("\n\n```json\n" + pretty.String() + "\n```\n")
		}
	}

	return sb.String()
}

func questionsMD(questions []store.Question) string {
	var sb strings.Builder

	sb.WriteString("# Questions and answers\n\n")

	for _, q := range questions {
		fmt.Fprintf(&sb, "## %s (%s, answered by %s, %s)\n\n%s\n", q.Tag(), pack.StageName(q.Stage, false), q.Responder,
			q.AnsweredAt.Format("2006-01-02 15:04:05 UTC"), q.Text)

		if q.Context != "" {
			fmt.Fprintf(&sb, "\n%s\n", q.Context)
		}

		for i, o := range q.Options {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, o)
		}

		fmt.Fprintf(&sb, "\n**Answer:** %s\n\n", q.Answer)
	}

	return sb.String()
}

func sourcesMD(sources []store.Source) string {
	var sb strings.Builder

	sb.WriteString("# Sources\n\n| Tag | Kind | Identity | Version | Fetched | First used by |\n|---|---|---|---|---|---|\n")

	for _, s := range sources {
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s |\n", s.Tag(), s.Kind, s.Identity, s.Version, s.FetchedAt.Format("2006-01-02 15:04 UTC"), s.FirstUsedBy)
	}

	return sb.String()
}

func eventsJSONL(events []store.Event) []byte {
	var sb strings.Builder

	for _, e := range events {
		line, err := json.Marshal(map[string]any{
			"time": e.Time, "blueprint_id": e.BlueprintID, "run_id": e.RunID, "kind": e.Kind, "payload": e.Payload,
		})
		if err == nil {
			sb.Write(line)
			sb.WriteByte('\n')
		}
	}

	return []byte(sb.String())
}
