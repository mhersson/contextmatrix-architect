// Package contract holds the card contract: the data the default pack ships
// in lists.yaml and the mechanical checks that read it. The agent's prompts
// are the ground truth the lists are copied from.
package contract

// Lists is the data behind the mechanical checks. The word lists are matched
// case-insensitively; ForbiddenReferences are regular expressions matched as
// written.
type Lists struct {
	// RequiredSections are the "## " headings every card body must carry.
	RequiredSections []string `yaml:"required_sections"`
	// StrippedHeadings are headings the agent removes before prompting; a
	// card must not use them.
	StrippedHeadings []string `yaml:"stripped_headings"`
	// StrippedHeadingPrefixes are stripped when a heading starts with them.
	StrippedHeadingPrefixes []string `yaml:"stripped_heading_prefixes"`
	// DefectPhrases route a card to the bug path; a feature card must not
	// contain one, negated or not.
	DefectPhrases []string `yaml:"defect_phrases"`
	// BugTitleVerbs route a card to the bug path when they are its title's
	// first word, whatever its type: the agent routes on that word. The
	// checks flag a card that is not bug-typed and would be misrouted.
	BugTitleVerbs []string `yaml:"bug_title_verbs"`
	// MaintenanceLabels and MaintenanceVerbs together route a card to the
	// maintenance path; a maintenance verb needs a maintenance label.
	MaintenanceLabels []string `yaml:"maintenance_labels"`
	MaintenanceVerbs  []string `yaml:"maintenance_verbs"`
	// ForbiddenReferences are regular expressions no card body may match:
	// local paths, parent directories, other checkouts.
	ForbiddenReferences []Pattern `yaml:"forbidden_references"`
	// DanglingReferences point at text the reader does not have, such as
	// "as noted above": a card body or an assumption states the fact
	// itself, since each stands alone once it leaves the blueprint.
	DanglingReferences []string `yaml:"dangling_references"`
	// EdgeCases are the conditions acceptance sweeps for every slice: each
	// settled by criteria or manual checks of the slice, or recorded not
	// applicable with a reason. Matched case-insensitively after trimming. A
	// slice whose projects are all proofs of concept may skip the sweep, and
	// a criterion on such a project settles no case.
	EdgeCases []string `yaml:"edge_cases"`
}

// Pattern is one named regular expression.
type Pattern struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
}
