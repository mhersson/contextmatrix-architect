package contract

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithoutMockups(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"no block", "## Context\nwhy\n", "## Context\nwhy\n"},
		{"a block", "a\n```mockup\nerror: x\n```\nb\n", "a\nb\n"},
		{"trailing spaces on the fences", "a\n```mockup  \nx\n```\t\nb\n", "a\nb\n"},
		{"a longer fence holds a shorter backtick line", "a\n````mockup\n```\nx\n````\nb\n", "a\nb\n"},
		{"an unclosed block runs to the end", "a\n```mockup\nx\npanic\n", "a\n"},
		{"two blocks", "a\n```mockup\nx\n```\nb\n```mockup\ny\n```\nc\n", "a\nb\nc\n"},
		{"a tilde fence is no block", "a\n~~~mockup\nx\n~~~\n", "a\n~~~mockup\nx\n~~~\n"},
		{"an indented fence is no block", "a\n  ```mockup\nx\n  ```\n", "a\n  ```mockup\nx\n  ```\n"},
		{"another language is no block", "a\n```html\nx\n```\n", "a\n```html\nx\n```\n"},
		{"mockup plus words is no block", "a\n```mockup two\nx\n```\n", "a\n```mockup two\nx\n```\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, WithoutMockups(tt.in)) })
	}
}

func TestCheck_AMockupBlockHidesItsSourceFromThePhraseScans(t *testing.T) {
	inside := "\n## Mockup\n\n```mockup\n<p>error: the card was declined</p><p>/home/anna/Photos</p><p>see above</p>\n```\n"
	c := goodCard("D1")
	c.Body = goodBody + inside

	assert.Empty(t, checkNames(Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t))))

	c.Body = goodBody + strings.NewReplacer("```mockup\n", "", "\n```\n", "\n").Replace(inside)
	names := checkNames(Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t)))
	assert.Contains(t, names, "D1:defect_phrase")
	assert.Contains(t, names, "D1:dangling_reference")
}

func TestCheck_AMockupBlockHidesItsSourceFromTheReferenceScans(t *testing.T) {
	inside := "\n## Mockup\n\n```mockup\n<p>Photos saved in /home/anna/Photos</p>\n<p>Imported from club/archive</p>\n```\n"
	projects := withRoot(t)
	alpha := projects["alpha"]
	alpha.ForeignRepos = []string{"https://example.com/club/archive.git"}
	projects["alpha"] = alpha

	c := goodCard("D1")
	c.Body = goodBody + inside

	assert.Empty(t, checkNames(Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), projects)))

	c.Body = goodBody + strings.NewReplacer("```mockup\n", "", "\n```\n", "\n").Replace(inside)
	names := checkNames(Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), projects))
	assert.Contains(t, names, "D1:forbidden_reference")
	assert.Contains(t, names, "D1:foreign_repo")
}

func TestCheck_TheStructuralScansReadAMockupBlock(t *testing.T) {
	c := goodCard("D1")
	c.Body = goodBody + "\n## Mockup\n\n```mockup\n## Verify Command\n```\n"

	assert.Contains(t, checkNames(Check(Draft{Cards: []Card{c}, Order: []string{"D1"}}, testLists(), withRoot(t))), "D1:stripped_heading",
		"the agent's section parser does not track fences")
}

func TestCheckMockups(t *testing.T) {
	card := func(screens ...string) Draft {
		c := goodCard("D1")
		c.Mockup = screens

		return Draft{Cards: []Card{c}, Order: []string{"D1"}}
	}

	assert.Empty(t, CheckMockups(card("members"), []string{"members", "edit"}, true))
	assert.Empty(t, CheckMockups(card(), nil, false))
	assert.Equal(t, []string{"D1:mockup_screen"}, checkNames(CheckMockups(card("calendar"), []string{"members"}, true)))
	assert.Equal(t, []string{"D1:mockup_none"}, checkNames(CheckMockups(card("members"), nil, false)))
}
