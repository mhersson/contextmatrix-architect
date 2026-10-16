package stage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// RepoFacts is what repo_info reports about one checked-out repository: the
// directory under the tool root, the commit, the instruction file the agent
// injects, the project's verify command, the source tag to cite, and
// whether the repository has no commits yet.
type RepoFacts struct {
	Project         string `json:"project"`
	Dir             string `json:"dir"`
	DefaultBranch   string `json:"default_branch,omitempty"`
	SHA             string `json:"sha"`
	InstructionFile string `json:"instruction_file"`
	VerifyCommand   string `json:"verify_command"`
	SourceTag       string `json:"source_tag"`
	Empty           bool   `json:"empty,omitempty"`
}

// CodeTools is the read-only tool set of a code stage: the harness read, grep
// and glob tools jailed to root, each result prefixed with the source tag of
// the project its path starts with, plus repo_info. All three harness tools
// take their location in the "path" argument; grep and glob default it to the
// whole root, which is tagged with every source.
func CodeTools(root string, repos []RepoFacts) []tools.Tool {
	tags := map[string]string{}
	for _, r := range repos {
		tags[r.Dir] = r.SourceTag
	}

	tagFor := func(args map[string]any) string {
		path := strings.TrimSpace(stringArg(args, "path"))
		// The jail accepts an absolute path inside root; tag it by its
		// root-relative form.
		if filepath.IsAbs(path) {
			if rel, err := filepath.Rel(root, path); err == nil {
				path = filepath.ToSlash(rel)
			}
		}

		// Clean resolves "./" and ".." the way the jail does, so the tag names
		// the tree the path really lands in.
		path = filepath.ToSlash(filepath.Clean(path))

		if dir, _, _ := strings.Cut(path, "/"); dir != "" {
			if tag, ok := tags[dir]; ok {
				return "[" + tag + "] "
			}
		}

		all := make([]string, 0, len(repos))
		for _, r := range repos {
			all = append(all, r.SourceTag)
		}

		return "[" + strings.Join(all, ",") + "] "
	}

	return []tools.Tool{
		tagged{Tool: tools.NewReadTool(root), tagFor: tagFor},
		tagged{Tool: tools.NewGrepTool(root), tagFor: tagFor},
		tagged{Tool: tools.NewGlobTool(root), tagFor: tagFor},
		repoInfoTool{repos: repos},
	}
}

// tagged prefixes a read-only tool's result with its source tag. ReadOnly is
// re-declared because the harness asserts it on the wrapper.
type tagged struct {
	tools.Tool
	tagFor func(args map[string]any) string
}

func (t tagged) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	res, err := t.Tool.Execute(ctx, args)
	if err != nil {
		return res, err
	}

	res.Text = t.tagFor(args) + res.Text

	return res, nil
}

func (tagged) ReadOnly() bool { return true }

type repoInfoTool struct {
	repos []RepoFacts
}

func (repoInfoTool) Name() string   { return "repo_info" }
func (repoInfoTool) ReadOnly() bool { return true }

func (repoInfoTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: "repo_info",
		Description: "Facts about every checked-out target project: its directory under the workspace root, " +
			"the commit, the instruction file at its root, the verify command and the source tag to cite.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}}
}

func (t repoInfoTool) Execute(_ context.Context, _ map[string]any) (tools.Result, error) {
	b, err := json.MarshalIndent(t.repos, "", "  ")
	if err != nil {
		return tools.Result{}, err
	}

	return tools.Result{Text: string(b)}, nil
}
