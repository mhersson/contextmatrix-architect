package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLists() Lists {
	return Lists{
		RequiredSections:        []string{"Context", "Where", "Solution", "Acceptance criteria", "Tests expected", "Out of scope"},
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
		DanglingReferences: []string{"see above"},
	}
}

const goodBody = `## Context
Why.
## Where
internal/api/router.go
## Solution
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

	return map[string]Project{"alpha": {HasRepo: true, VerifyCommand: "make test", HasInstructionFile: true, Root: root}}
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
		{name: "new path that exists", mutate: func(c *Card) { c.Where = []Where{{Path: "internal/api/router.go", New: true}} }, want: "where_exists"},
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

func TestCheck_ProjectWithoutARepository(t *testing.T) {
	projects := map[string]Project{"alpha": {VerifyCommand: "make test"}}

	names := checkNames(Check(Draft{Cards: []Card{goodCard("D1")}}, testLists(), projects))
	assert.Equal(t, []string{"D1:project_repo"}, names)
}

func TestCheck_NoRootSkipsWhere(t *testing.T) {
	projects := map[string]Project{"alpha": {HasRepo: true, VerifyCommand: "make test"}}

	findings := Check(Draft{Cards: []Card{goodCard("D1")}}, testLists(), projects)
	assert.Empty(t, findings, "without a checkout neither the where check nor the instruction check runs")
}

func TestCheck_WhereRejectsPathsOutsideRoot(t *testing.T) {
	tests := []string{"../other-repo/x.go", "/etc/passwd"}

	for _, path := range tests {
		for _, isNew := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s new=%t", path, isNew), func(t *testing.T) {
				c := goodCard("D1")
				c.Where = []Where{{Path: path, New: isNew}}

				findings := Check(Draft{Cards: []Card{c}}, testLists(), withRoot(t))
				assert.Contains(t, checkNames(findings), "D1:where_exists")
			})
		}
	}
}

func TestCheck_WhereAcceptsNewPaths(t *testing.T) {
	c := goodCard("D1")
	c.Where = []Where{{Path: "web/app.js", New: true}, {Path: "cmd/new/main.go", Symbol: "main", New: true}}

	findings := Check(Draft{Cards: []Card{c}}, testLists(), withRoot(t))
	assert.NotContains(t, checkNames(findings), "D1:where_exists")
}

func TestParseDraft(t *testing.T) {
	data := map[string]any{
		"cards": []any{map[string]any{
			"id": "D1", "project": "alpha", "title": "Add x", "type": "feature",
			"depends_on": []any{}, "body": "b", "where": []any{map[string]any{"path": "a.go", "symbol": "A"}, map[string]any{"path": "b.go", "new": true}},
		}},
		"order": []any{"D1"},
	}

	d, err := ParseDraft(data)
	require.NoError(t, err)
	require.Len(t, d.Cards, 1)
	assert.Equal(t, "alpha", d.Cards[0].Project)
	assert.Equal(t, []Where{{Path: "a.go", Symbol: "A"}, {Path: "b.go", New: true}}, d.Cards[0].Where)
	assert.Equal(t, []string{"D1"}, d.Order)

	_, err = ParseDraft(map[string]any{"cards": "nope"})
	require.Error(t, err)

	_, err = ParseDraft(map[string]any{"cards": []any{map[string]any{"id": "D1", "project": "alpha", "title": "t"}, map[string]any{"id": "D1", "project": "alpha", "title": "u"}}})
	require.Error(t, err, "duplicate ids are rejected")
}

func TestParseDraft_ManualEntries(t *testing.T) {
	tests := []struct {
		name    string
		data    map[string]any
		wantErr string
	}{
		{
			name: "card and manual entry",
			data: map[string]any{
				"title":  "Ship the thing",
				"cards":  []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing", "priority": "medium"}},
				"manual": []any{map[string]any{"id": "M1", "text": "Check the dashboard on staging"}},
				"order":  []any{"D1", "M1"},
			},
		},
		{
			name: "manual entry without text",
			data: map[string]any{
				"cards":  []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing"}},
				"manual": []any{map[string]any{"id": "M1", "text": "  "}},
			},
			wantErr: `manual entry "M1" needs an id and text`,
		},
		{
			name: "manual entry without an id",
			data: map[string]any{
				"cards":  []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing"}},
				"manual": []any{map[string]any{"id": "", "text": "Check the dashboard on staging"}},
			},
			wantErr: `manual entry "" needs an id and text`,
		},
		{
			name: "manual id reuses a card id",
			data: map[string]any{
				"cards":  []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add the thing"}},
				"manual": []any{map[string]any{"id": "D1", "text": "Check it"}},
			},
			wantErr: `duplicate id "D1"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseDraft(tt.data)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, "Ship the thing", d.Title)
			assert.Equal(t, "medium", d.Cards[0].Priority)
			require.Len(t, d.Manual, 1)
			assert.Equal(t, "Check the dashboard on staging", d.Manual[0].Text)
			assert.Equal(t, []string{"D1", "M1"}, d.Order)
		})
	}
}

func validBody() string {
	return "## Context\nwhy\n## Where\nx.go\n## Solution\nadd\n## Acceptance criteria\n1. go test ./... passes.\n## Tests expected\nTestX.\n## Out of scope\nrest.\n"
}

func TestCheck_ProjectFieldsAndSelfContainment(t *testing.T) {
	lists := Lists{ForbiddenReferences: []Pattern{
		{Name: "home path", Pattern: `(?:^|[\s"'` + "`" + `(\[])~/[^\s"'` + "`" + `)\]]*`},
		{Name: "file url", Pattern: `file://[^\s"'` + "`" + `)\]]*`},
	}}
	projects := map[string]Project{"alpha": {
		HasRepo: true, VerifyCommand: "make test",
		Types: []string{"feature", "bug"}, Priorities: []string{"low", "medium", "high"},
		ForeignRepos: []string{"https://github.com/acme/beta.git"},
	}}

	card := func(mut func(*Card)) Draft {
		c := Card{ID: "D1", Project: "alpha", Title: "Add the thing", Type: "feature", Priority: "medium", Body: validBody()}
		mut(&c)

		return Draft{Cards: []Card{c}, Order: []string{"D1"}}
	}

	tests := []struct {
		name  string
		draft Draft
		want  string // check name expected among the findings; empty means none
	}{
		{"clean", card(func(*Card) {}), ""},
		{"unknown type", card(func(c *Card) { c.Type = "epic" }), "type"},
		{"type in another case", card(func(c *Card) { c.Type = "Feature" }), "type"},
		{"missing priority", card(func(c *Card) { c.Priority = "" }), "priority"},
		{"unknown priority", card(func(c *Card) { c.Priority = "urgent" }), "priority"},
		{"priority in another case", card(func(c *Card) { c.Priority = "Medium" }), "priority"},
		{"sibling repo url", card(func(c *Card) { c.Body += "\nSee https://github.com/acme/beta for the API.\n" }), "foreign_repo"},
		{"sibling owner/name", card(func(c *Card) { c.Body += "\nMatches acme/beta.\n" }), "foreign_repo"},
		{"sibling repo in the title only", card(func(c *Card) { c.Title = "Add the acme/beta client" }), "foreign_repo"},
		{"longer name is not the sibling", card(func(c *Card) { c.Body += "\nUses acme/beta-api.\n" }), ""},
		{"home path in the title", card(func(c *Card) { c.Title = "Add ~/notes support" }), "forbidden_reference"},
		{"file url", card(func(c *Card) { c.Body += "\nfile:///tmp/x\n" }), "forbidden_reference"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var checks []string
			for _, f := range Check(tt.draft, lists, projects) {
				checks = append(checks, f.Check)
			}

			if tt.want == "" {
				assert.Empty(t, checks)

				return
			}

			assert.Contains(t, checks, tt.want)
		})
	}
}

func TestCheck_ProjectWithoutTypesOrPrioritiesSkipsThoseChecks(t *testing.T) {
	projects := map[string]Project{"alpha": {HasRepo: true, VerifyCommand: "make test"}}
	d := Draft{Cards: []Card{{ID: "D1", Project: "alpha", Title: "Add the thing", Type: "anything", Priority: "whatever", Body: validBody()}}}

	assert.Empty(t, Check(d, Lists{}, projects))
}

// changeOnlyBody names "make check" in its Solution section but not in its
// criteria.
const changeOnlyBody = `## Context
Why.
## Where
internal/api/router.go
## Solution
Run make check before pushing.
## Acceptance criteria
1. The route answers 200.
## Tests expected
TestRoute_Answers200.
## Out of scope
Auth.
`

func TestCheck_VerifySetup(t *testing.T) {
	tests := []struct {
		name    string
		project Project
		setup   map[string]Setup
		body    string // the cards' body; "" is goodBody
		want    []string
		message string // substring of the first finding
	}{
		{name: "the project declares one", project: Project{HasRepo: true, VerifyCommand: "make test"}},
		{
			name: "no command and no proposal", project: Project{HasRepo: true},
			want: []string{"D1:setup_verify", "D2:setup_verify"}, message: "setup.alpha.verify_command",
		},
		{
			name: "proposed and run by every card", project: Project{HasRepo: true},
			setup: map[string]Setup{"alpha": {VerifyCommand: "go test ./internal/api/"}},
		},
		{
			name: "proposed but run by no card", project: Project{HasRepo: true},
			setup: map[string]Setup{"alpha": {VerifyCommand: "make check"}},
			want:  []string{"D1:setup_verify", "D2:setup_verify"}, message: `"make check"`,
		},
		{
			name: "named outside the criteria", project: Project{HasRepo: true},
			setup: map[string]Setup{"alpha": {VerifyCommand: "make check"}}, body: changeOnlyBody,
			want: []string{"D1:setup_verify", "D2:setup_verify"}, message: `"make check"`,
		},
		{
			name: "a proposal equal to the declared command is ignored", project: Project{HasRepo: true, VerifyCommand: "make check"},
			setup: map[string]Setup{"alpha": {VerifyCommand: "make check"}},
		},
		{
			name: "a replacement run by every card", project: Project{HasRepo: true, VerifyCommand: "make all"},
			setup: map[string]Setup{"alpha": {VerifyCommand: "go test ./internal/api/", Replaces: "make all"}},
		},
		{
			name: "a replacement run by no card", project: Project{HasRepo: true, VerifyCommand: "make all"},
			setup: map[string]Setup{"alpha": {VerifyCommand: "make check", Replaces: "make all"}},
			want:  []string{"D1:setup_verify", "D2:setup_verify"}, message: `"make check"`,
		},
		{
			name: "a differing proposal without replaces is ignored", project: Project{HasRepo: true, VerifyCommand: "make all"},
			setup: map[string]Setup{"alpha": {VerifyCommand: "make check"}},
		},
		{
			name: "a replacement for a command no longer declared is ignored", project: Project{HasRepo: true, VerifyCommand: "make lint"},
			setup: map[string]Setup{"alpha": {VerifyCommand: "make check", Replaces: "make all"}},
		},
		{
			name: "replaces on a project that declares none", project: Project{HasRepo: true},
			setup: map[string]Setup{"alpha": {VerifyCommand: "go test ./internal/api/", Replaces: "make all"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := goodCard("D1"), goodCard("D2")
			b.DependsOn = []string{"D1"}

			if tt.body != "" {
				a.Body, b.Body = tt.body, tt.body
			}

			findings := Check(Draft{Cards: []Card{a, b}, Setup: tt.setup}, testLists(), map[string]Project{"alpha": tt.project})
			assert.Equal(t, tt.want, nilIfEmpty(checkNames(findings)))

			if tt.message != "" {
				require.NotEmpty(t, findings)
				assert.Contains(t, findings[0].Message, tt.message)
			}
		})
	}
}

func TestCheck_InstructionSetup(t *testing.T) {
	three := func(depsOfD2, depsOfD3 []string) []Card {
		a, b, c := goodCard("D1"), goodCard("D2"), goodCard("D3")
		b.DependsOn, c.DependsOn = depsOfD2, depsOfD3

		return []Card{a, b, c}
	}

	chain := three([]string{"D1"}, []string{"D2"})

	tests := []struct {
		name  string
		has   bool // the checkout has an instruction file
		cards []Card
		setup map[string]Setup
		want  []string
	}{
		{name: "the checkout has one", has: true, cards: three(nil, nil)},
		{
			name: "the checkout has one and a card would overwrite it", has: true, cards: three(nil, nil),
			setup: map[string]Setup{"alpha": {InstructionCard: "D1"}}, want: []string{"D1:setup_instruction"},
		},
		{name: "none and no card named", cards: chain, want: []string{"D1:setup_instruction"}},
		{name: "every other card depends on it", cards: chain, setup: map[string]Setup{"alpha": {InstructionCard: "D1"}}},
		{
			name: "a card that does not depend on it", cards: three([]string{"D1"}, nil),
			setup: map[string]Setup{"alpha": {InstructionCard: "D1"}}, want: []string{"D3:setup_instruction"},
		},
		{
			name: "a later card named", cards: chain,
			setup: map[string]Setup{"alpha": {InstructionCard: "D3"}}, want: []string{"D1:setup_instruction", "D2:setup_instruction"},
		},
		{
			name: "an unknown card named", cards: chain,
			setup: map[string]Setup{"alpha": {InstructionCard: "D9"}}, want: []string{"D1:setup_instruction"},
		},
		{
			name: "a single card writes it", cards: []Card{goodCard("D1")},
			setup: map[string]Setup{"alpha": {InstructionCard: "D1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projects := withRoot(t)
			p := projects["alpha"]
			p.HasInstructionFile = tt.has
			projects["alpha"] = p

			var got []string

			for _, name := range checkNames(Check(Draft{Cards: tt.cards, Setup: tt.setup}, testLists(), projects)) {
				if strings.HasSuffix(name, ":setup_instruction") {
					got = append(got, name)
				}
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheck_InstructionSetupIsPerProject(t *testing.T) {
	projects := withRoot(t)
	alpha := projects["alpha"]
	alpha.HasInstructionFile = false
	beta := projects["alpha"]
	projects["alpha"], projects["beta"] = alpha, beta

	a1, a2, b1 := goodCard("D1"), goodCard("D2"), goodCard("D3")
	a2.DependsOn = []string{"D1"}
	b1.Project = "beta"

	findings := Check(Draft{Cards: []Card{a1, a2, b1}, Setup: map[string]Setup{"alpha": {InstructionCard: "D1"}}}, testLists(), projects)
	assert.Empty(t, findings, "beta has its instruction file, and its card depends on nothing in alpha")
}

func TestParseDraft_ReadsTheSetupTrimmed(t *testing.T) {
	d, err := ParseDraft(map[string]any{
		"cards": []any{map[string]any{"id": "D1", "project": "alpha", "title": "Add x"}},
		"setup": map[string]any{"alpha": map[string]any{"verify_command": "  go test ./...\n", "replaces": " make all ", "instruction_card": " D1 "}},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]Setup{"alpha": {VerifyCommand: "go test ./...", Replaces: "make all", InstructionCard: "D1"}}, d.Setup)
}

// nilIfEmpty lets a table expect nil for no findings.
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}

func TestCheck_DanglingReference(t *testing.T) {
	lists := testLists()
	lists.DanglingReferences = []string{"as noted above", "as above", "as mentioned"}
	projects := map[string]Project{"alpha": {HasRepo: true, VerifyCommand: "make test"}}

	card := goodCard("D1")
	assert.Empty(t, Check(Draft{Cards: []Card{card}}, lists, projects))

	card.Body += "The limits stay As noted above.\n"
	assert.Equal(t, []string{"D1:dangling_reference"}, checkNames(Check(Draft{Cards: []Card{card}}, lists, projects)))

	for _, tc := range []struct{ name, text, want string }{
		{"matches case-insensitively", "The limits stay As noted above.", "as noted above"},
		{"no phrase", "The limits stay at 100.", ""},
		{"inside another word", "p99 was above 300 ms", ""},
		{"inside another word at its end", "it was mentioned in passing", ""},
		{"at the start of a sentence", "As above, the API keeps its routes.", "as above"},
		{"inside a hyphenated word", "such as above-the-fold", ""},
		{"at the end of the text", "The routes stay as above", "as above"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DanglingReference(tc.text, lists))
		})
	}
}

func TestCheckCriteria(t *testing.T) {
	criteria := []Criterion{
		{ID: "AC1", Slice: "SL1", Project: "alpha"},
		{ID: "AC2", Slice: "SL1", Project: "beta"},
	}

	card := func(id, project string, criteria ...string) Card {
		c := goodCard(id)
		c.Project, c.Criteria = project, criteria

		return c
	}

	tests := []struct {
		name   string
		cards  []Card
		manual []Manual
		checks []string // the manual checks acceptance defines
		want   []string
	}{
		{name: "each carried once", cards: []Card{card("D1", "alpha", "AC1"), card("D2", "beta", "AC2")}},
		{
			name: "one carried by no card", cards: []Card{card("D1", "alpha", "AC1")},
			want: []string{"AC2:criterion_unmapped"},
		},
		{name: "a criterion listed twice on one card", cards: []Card{card("D1", "alpha", "AC1", "AC1"), card("D2", "beta", "AC2")}},
		{
			name: "one carried twice", cards: []Card{card("D1", "alpha", "AC1"), card("D2", "alpha", "AC1"), card("D3", "beta", "AC2")},
			want: []string{"AC1:criterion_shared"},
		},
		{
			name: "another project's criterion", cards: []Card{card("D1", "alpha", "AC1", "AC2")},
			want: []string{"D1:criterion_project", "AC2:criterion_unmapped"},
		},
		{
			name: "an unknown criterion", cards: []Card{card("D1", "alpha", "AC1", "AC9"), card("D2", "beta", "AC2")},
			want: []string{"D1:criterion_unknown"},
		},
		{
			name: "a manual check carried", cards: []Card{card("D1", "alpha", "AC1"), card("D2", "beta", "AC2")},
			manual: []Manual{{ID: "M1", Text: "check the dashboard", Criteria: []string{"MC1"}}}, checks: []string{"MC1"},
		},
		{
			name: "a manual check carried by no entry", cards: []Card{card("D1", "alpha", "AC1"), card("D2", "beta", "AC2")},
			checks: []string{"MC1"}, want: []string{"MC1:manual_unmapped"},
		},
		{
			name: "an entry naming an unknown check", cards: []Card{card("D1", "alpha", "AC1"), card("D2", "beta", "AC2")},
			manual: []Manual{{ID: "M1", Text: "check it", Criteria: []string{"MC9"}}}, want: []string{"M1:manual_unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkNames(CheckCriteria(Draft{Cards: tt.cards, Manual: tt.manual}, criteria, tt.checks))
			assert.Equal(t, tt.want, nilIfEmpty(got))
		})
	}
}
