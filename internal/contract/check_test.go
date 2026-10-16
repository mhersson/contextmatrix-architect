package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLists() Lists {
	return Lists{
		RequiredSections:        []string{"Context", "Where", "Change", "Acceptance criteria", "Tests expected", "Out of scope"},
		StrippedHeadings:        []string{"Diagnosis", "Design", "Verify Command"},
		StrippedHeadingPrefixes: []string{"Review Findings"},
		DefectPhrases:           []string{"does not work", "panic", "error:"},
		BugTitleVerbs:           []string{"fix"},
		MaintenanceLabels:       []string{"chore", "dependencies"},
		MaintenanceVerbs:        []string{"bump", "update"},
		ForbiddenReferences: []Pattern{
			{Name: "local path", Pattern: `(^|[\s"` + "`" + `(])(/(home|Users)/|C:\\Users\\)`},
			{Name: "parent directory", Pattern: `(^|\s)\.\./`},
		},
	}
}

const goodBody = `## Context
Why.
## Where
internal/api/router.go
## Change
Add the route.
## Acceptance criteria
1. go test ./internal/api/ passes.
2. The route answers 200.
## Tests expected
TestRoute_Answers200.
## Out of scope
Auth.
`

func goodCard(id string) Card {
	return Card{
		ID: id, Project: "alpha", Title: "Add the health route", Type: "feature", Body: goodBody,
		Where: []Where{{Path: "internal/api/router.go", Symbol: "NewRouter"}},
	}
}

func withRoot(t *testing.T) map[string]Project {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "api"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "internal", "api", "router.go"), []byte("package api\n\nfunc NewRouter() {}\n"), 0o600))

	return map[string]Project{"alpha": {HasRepo: true, HasVerify: true, Root: root}}
}

func checkNames(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.CardID+":"+f.Check)
	}

	return out
}

func TestCheck_GoodCardPasses(t *testing.T) {
	findings := Check(Draft{Cards: []Card{goodCard("D1")}, Order: []string{"D1"}}, testLists(), withRoot(t))
	assert.Empty(t, findings)
}

func TestCheck_Findings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Card)
		want   string
	}{
		{name: "missing section", mutate: func(c *Card) { c.Body = "## Context\nx\n" }, want: "required_section"},
		{name: "criteria not numbered", mutate: func(c *Card) {
			c.Body = strings.Replace(goodBody, "1. go test ./internal/api/ passes.\n2. The route answers 200.\n", "- go test passes\n", 1)
		}, want: "criteria_numbered"},
		{name: "stripped heading", mutate: func(c *Card) { c.Body += "## Verify Command\nmake test\n" }, want: "stripped_heading"},
		{name: "stripped heading by prefix", mutate: func(c *Card) { c.Body += "## Review Findings round 2\nx\n" }, want: "stripped_heading"},
		{name: "defect phrase in a feature", mutate: func(c *Card) { c.Body += "\nIt must not panic.\n" }, want: "defect_phrase"},
		{name: "no numbered criterion", mutate: func(c *Card) {
			c.Body = strings.Replace(goodBody, "1. go test ./internal/api/ passes.\n2. The route answers 200.\n", "", 1)
		}, want: "criteria_numbered"},
		{name: "indented stripped heading inside the criteria", mutate: func(c *Card) {
			c.Body = strings.Replace(goodBody, "2. The route answers 200.\n", "2. The route answers 200.\n   ## Verify Command\n3. The agent never sees this.\n", 1)
		}, want: "stripped_heading"},
		{name: "feature titled with a bug verb", mutate: func(c *Card) { c.Title = "Fix the missing health route" }, want: "bug_title_verb"},
		{name: "bug verb with trailing punctuation", mutate: func(c *Card) { c.Title = "Fix: the health route" }, want: "bug_title_verb"},
		{name: "local path", mutate: func(c *Card) { c.Body += "\nSee /home/dev/notes.md\n" }, want: "forbidden_reference"},
		{name: "windows local path", mutate: func(c *Card) { c.Body += "\nSee C:\\Users\\dev\\notes.md\n" }, want: "forbidden_reference"},
		{name: "parent directory", mutate: func(c *Card) { c.Body += "\nCopy from ../other-repo/x.go\n" }, want: "forbidden_reference"},
		{name: "parent directory at line start", mutate: func(c *Card) { c.Body += "\n../other-repo/x.go\n" }, want: "forbidden_reference"},
		{name: "maintenance verb without label", mutate: func(c *Card) { c.Title = "Bump the router dependency" }, want: "maintenance_label"},
		{name: "unknown where path", mutate: func(c *Card) { c.Where = []Where{{Path: "internal/api/missing.go"}} }, want: "where_exists"},
		{name: "unknown where symbol", mutate: func(c *Card) { c.Where = []Where{{Path: "internal/api/router.go", Symbol: "OldRouter"}} }, want: "where_exists"},
		{name: "unknown project", mutate: func(c *Card) { c.Project = "zeta" }, want: "project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := goodCard("D1")
			tt.mutate(&c)

			findings := Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t))
			require.NotEmpty(t, findings, "expected a finding")
			assert.Contains(t, checkNames(findings), "D1:"+tt.want)
		})
	}
}

func TestCheck_InRepoPathsAreNotForbidden(t *testing.T) {
	c := goodCard("D1")
	c.Body += "\nMount the config at /etc/app/config.yaml, data under /var/lib/app, scratch in /tmp/x.\n" +
		"Keep `import { x } from \"../lib/x\"` as it is.\n"

	findings := Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t))
	assert.Empty(t, findings)
}

func TestCheck_BugCardMayNameTheDefect(t *testing.T) {
	c := goodCard("D1")
	c.Type = "bug"
	c.Title = "Fix the panic on an empty route"
	c.Body += "\nThe handler panics when the path is empty.\n"

	findings := Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t))
	assert.Empty(t, findings)
}

func TestCheck_FeatureTitledFixMayNotNameTheDefect(t *testing.T) {
	c := goodCard("D1")
	c.Title = "Fix the empty route"
	c.Body += "\nThe handler panics when the path is empty.\n"

	findings := Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t))
	assert.Contains(t, checkNames(findings), "D1:defect_phrase", "only a bug-typed card takes the bug path")
}

func TestCheck_StrippedHeadingReportedOnce(t *testing.T) {
	c := goodCard("D1")
	c.Body += "## Verify Command\nmake test\n"

	lists := testLists()
	lists.StrippedHeadingPrefixes = []string{"Verify"}

	findings := Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, lists, withRoot(t))

	var n int

	for _, name := range checkNames(findings) {
		if name == "D1:stripped_heading" {
			n++
		}
	}

	assert.Equal(t, 1, n, "a heading matching both a name and a prefix is one finding")
}

func TestFinding_StringNamesCardAndCheck(t *testing.T) {
	f := Finding{CardID: "D7", Check: "depends_on_acyclic", Message: "depends_on forms a cycle"}
	assert.Equal(t, "D7: depends_on_acyclic: depends_on forms a cycle", f.String())
}

func TestCheck_MaintenanceVerbWithLabelPasses(t *testing.T) {
	c := goodCard("D1")
	c.Title = "Bump go-git to v5.19.3"
	c.Labels = []string{"dependencies"}

	findings := Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t))
	assert.Empty(t, findings)
}

func TestCheck_DependsOn(t *testing.T) {
	projects := withRoot(t)
	projects["beta"] = projects["alpha"]

	tests := []struct {
		name  string
		cards []Card
		want  string
	}{
		{name: "self dependency", cards: func() []Card {
			a := goodCard("D1")
			a.DependsOn = []string{"D1"}

			return []Card{a}
		}(), want: "depends_on_acyclic"},
		{name: "cycle", cards: func() []Card {
			a, b := goodCard("D1"), goodCard("D2")
			a.DependsOn, b.DependsOn = []string{"D2"}, []string{"D1"}

			return []Card{a, b}
		}(), want: "depends_on_acyclic"},
		{name: "unknown id", cards: func() []Card {
			a := goodCard("D1")
			a.DependsOn = []string{"D9"}

			return []Card{a}
		}(), want: "depends_on_known"},
		{name: "across projects", cards: func() []Card {
			a, b := goodCard("D1"), goodCard("D2")
			b.Project = "beta"
			a.DependsOn = []string{"D2"}

			return []Card{a, b}
		}(), want: "depends_on_same_project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := Check(Draft{Cards: tt.cards}, testLists(), projects)
			assert.Contains(t, checkNames(findings), "D1:"+tt.want)
		})
	}
}

func TestCheck_CycleReportsOnlyCardsOnIt(t *testing.T) {
	projects := withRoot(t)

	a, b, c := goodCard("D1"), goodCard("D2"), goodCard("D3")
	a.DependsOn, b.DependsOn = []string{"D2"}, []string{"D1"}
	c.DependsOn = []string{"D1"}

	findings := Check(Draft{Cards: []Card{a, b, c}}, testLists(), projects)
	names := checkNames(findings)
	assert.Contains(t, names, "D1:depends_on_acyclic")
	assert.Contains(t, names, "D2:depends_on_acyclic")
	assert.NotContains(t, names, "D3:depends_on_acyclic", "D3 depends on a cycle but is not on one")
}

func TestCheck_ProjectPrerequisites(t *testing.T) {
	projects := withRoot(t)
	projects["alpha"] = Project{HasRepo: false, HasVerify: false, Root: projects["alpha"].Root}

	findings := Check(Draft{Cards: []Card{goodCard("D1")}}, testLists(), projects)
	names := checkNames(findings)
	assert.Contains(t, names, "D1:project_repo")
	assert.Contains(t, names, "D1:project_verify")
}

func TestCheck_NoRootSkipsWhere(t *testing.T) {
	projects := map[string]Project{"alpha": {HasRepo: true, HasVerify: true}}

	findings := Check(Draft{Cards: []Card{goodCard("D1")}}, testLists(), projects)
	assert.Empty(t, findings, "without a checkout the where check is not run")
}

func TestCheck_WhereRejectsPathsOutsideRoot(t *testing.T) {
	tests := []string{"../other-repo/x.go", "/etc/passwd"}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			c := goodCard("D1")
			c.Where = []Where{{Path: path}}

			findings := Check(Draft{Cards: []Card{c}}, testLists(), withRoot(t))
			assert.Contains(t, checkNames(findings), "D1:where_exists")
		})
	}
}

func TestParseDraft(t *testing.T) {
	data := map[string]any{
		"cards": []any{map[string]any{
			"id": "D1", "project": "alpha", "title": "Add x", "type": "feature",
			"depends_on": []any{}, "body": "b", "where": []any{map[string]any{"path": "a.go", "symbol": "A"}},
		}},
		"order": []any{"D1"},
	}

	d, err := ParseDraft(data)
	require.NoError(t, err)
	require.Len(t, d.Cards, 1)
	assert.Equal(t, "alpha", d.Cards[0].Project)
	assert.Equal(t, []Where{{Path: "a.go", Symbol: "A"}}, d.Cards[0].Where)
	assert.Equal(t, []string{"D1"}, d.Order)

	_, err = ParseDraft(map[string]any{"cards": "nope"})
	require.Error(t, err)

	_, err = ParseDraft(map[string]any{"cards": []any{map[string]any{"id": "D1", "project": "alpha", "title": "t"}, map[string]any{"id": "D1", "project": "alpha", "title": "u"}}})
	require.Error(t, err, "duplicate ids are rejected")
}
