package buildinfo

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-protocol/changelog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInfo_History(t *testing.T) {
	tests := []struct {
		name string
		fsys fstest.MapFS
		want []protocol.Change
	}{
		{name: "no history file", fsys: fstest.MapFS{"history/.gitkeep": {}}, want: []protocol.Change{}},
		// A build without git: the shell creates the redirect target before git fails.
		{name: "empty history file", fsys: fstest.MapFS{"history/merges.log": {}}, want: []protocol.Change{}},
		{
			name: "recorded merges",
			fsys: fstest.MapFS{"history/merges.log": {Data: []byte(
				"2026-10-20T16:00:00Z\x1fMerge pull request #33 from mhersson/fix/readiness-settle\x1f" +
					"fix(cli): name the cards readiness did not pass on the sign-off\x1e\n" +
					"2026-10-19T09:00:00Z\x1fMerge pull request #32 from mhersson/dependabot/go_modules/x\x1f" +
					"chore(deps): bump x\x1e\n")}},
			want: []protocol.Change{{
				Number: 33, Kind: protocol.ChangeKindFix,
				Title: "name the cards readiness did not pass on the sign-off", MergedAt: "2026-10-20T16:00:00Z",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := info(tt.fsys)
			assert.Equal(t, tt.want, got.Changes) // DeepEqual: a nil slice fails the [] row

			raw, err := json.Marshal(got)
			require.NoError(t, err)
			assert.Contains(t, string(raw), `"changes":[`)
		})
	}
}

func TestInfo_CapsAtLimit(t *testing.T) {
	var log strings.Builder
	for n := changelog.Limit + 1; n > 0; n-- {
		fmt.Fprintf(&log, "2026-10-20T16:00:00Z\x1fMerge pull request #%d from mhersson/feat/x%d\x1ffeat: change %d\x1e\n", n, n, n)
	}

	got := info(fstest.MapFS{"history/merges.log": {Data: []byte(log.String())}}).Changes
	require.Len(t, got, changelog.Limit)
	assert.Equal(t, changelog.Limit+1, got[0].Number, "newest first")
}

func TestHistoryCommands_MatchGitLogArgs(t *testing.T) {
	root := os.DirFS("../..")

	for _, file := range []string{"Makefile", ".github/workflows/ci.yaml"} {
		t.Run(file, func(t *testing.T) {
			assert.Equal(t, changelog.GitLogArgs, historyCommand(t, root, file))
		})
	}
}

// historyCommand returns the arguments after "git" on the file's one line
// that runs git log, quotes dropped, up to the output redirect.
func historyCommand(t *testing.T, root fs.FS, file string) []string {
	t.Helper()

	raw, err := fs.ReadFile(root, file)
	require.NoError(t, err)

	var lines []string

	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.Contains(line, "git log --") {
			lines = append(lines, line)
		}
	}

	require.Len(t, lines, 1, "exactly one git log line in %s", file)

	fields := strings.Fields(strings.ReplaceAll(lines[0], "'", ""))
	i := slices.Index(fields, "git")
	require.GreaterOrEqual(t, i, 0)

	args := fields[i+1:]
	if end := slices.IndexFunc(args, func(f string) bool { return strings.HasPrefix(f, ">") }); end >= 0 {
		args = args[:end]
	}

	return args
}
