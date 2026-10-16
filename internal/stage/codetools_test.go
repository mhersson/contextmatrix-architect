package stage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func codeRoot(t *testing.T) (string, []RepoFacts) {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "alpha", "cmd"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "alpha", "cmd", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "beta"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "beta", "README.md"), []byte("# beta\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte("name: ci\n"), 0o600))

	repos := []RepoFacts{
		{Project: "alpha", Dir: "alpha", DefaultBranch: "main", SHA: "aaa", InstructionFile: "AGENTS.md", VerifyCommand: "make test", SourceTag: "S1"},
		{Project: "beta", Dir: "beta", DefaultBranch: "main", SHA: "bbb", SourceTag: "S2"},
	}

	return root, repos
}

func toolByName(t *testing.T, ts []tools.Tool, name string) tools.Tool {
	t.Helper()

	for _, tool := range ts {
		if tool.Name() == name {
			return tool
		}
	}

	t.Fatalf("no tool %q", name)

	return nil
}

func TestCodeTools_ReadIsTaggedWithTheProjectSource(t *testing.T) {
	root, repos := codeRoot(t)
	ts := CodeTools(root, repos)

	res, err := toolByName(t, ts, "read").Execute(context.Background(), map[string]any{"path": "alpha/cmd/main.go"})
	require.NoError(t, err)
	assert.True(t, len(res.Text) > 0 && res.Text[:4] == "[S1]", res.Text)
	assert.Contains(t, res.Text, "func main()")

	res, err = toolByName(t, ts, "read").Execute(context.Background(), map[string]any{"path": "beta/README.md"})
	require.NoError(t, err)
	assert.Equal(t, "[S2]", res.Text[:4], res.Text)

	ro, ok := toolByName(t, ts, "read").(tools.ReadOnly)
	require.True(t, ok)
	assert.True(t, ro.ReadOnly())
}

func TestCodeTools_ReadOutsideTheRootIsRefused(t *testing.T) {
	root, repos := codeRoot(t)
	ts := CodeTools(root, repos)

	_, err := toolByName(t, ts, "read").Execute(context.Background(), map[string]any{"path": "../../etc/passwd"})
	require.Error(t, err)
}

func TestCodeTools_TagFollowsThePathArgument(t *testing.T) {
	root, repos := codeRoot(t)
	// A dot-directory under a project keeps its name when the path is trimmed.
	repos = append(repos, RepoFacts{Project: "gamma", Dir: ".github", SourceTag: "S3"})
	ts := CodeTools(root, repos)

	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"read dot-slash prefix", "read", map[string]any{"path": "./alpha/cmd/main.go"}, "[S1] "},
		{"read absolute path inside the root", "read", map[string]any{"path": filepath.Join(root, "beta", "README.md")}, "[S2] "},
		{"read under a dot-directory", "read", map[string]any{"path": ".github/workflows/ci.yml"}, "[S3] "},
		{"grep scoped to a project", "grep", map[string]any{"pattern": "main", "path": "alpha"}, "[S1] "},
		{"grep without a path", "grep", map[string]any{"pattern": "main"}, "[S1,S2,S3] "},
		{"glob scoped to a project", "glob", map[string]any{"pattern": "*.md", "path": "beta"}, "[S2] "},
		{"glob without a path", "glob", map[string]any{"pattern": "*.md"}, "[S1,S2,S3] "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := toolByName(t, ts, tc.tool).Execute(context.Background(), tc.args)
			require.NoError(t, err)
			assert.True(t, len(res.Text) >= len(tc.want) && res.Text[:len(tc.want)] == tc.want, res.Text)
		})
	}
}

func TestCodeTools_RepoInfo(t *testing.T) {
	root, repos := codeRoot(t)
	ts := CodeTools(root, repos)

	res, err := toolByName(t, ts, "repo_info").Execute(context.Background(), map[string]any{})
	require.NoError(t, err)

	var got []RepoFacts
	require.NoError(t, json.Unmarshal([]byte(res.Text), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "alpha", got[0].Project)
	assert.Equal(t, "make test", got[0].VerifyCommand)
	assert.Equal(t, "S2", got[1].SourceTag)
}
