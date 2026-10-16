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
	// BugTitleVerbs open the titles of bug work. The checks route a card by
	// its type alone; this list is data for the pack, not a check input.
	BugTitleVerbs []string `yaml:"bug_title_verbs"`
	// MaintenanceLabels and MaintenanceVerbs together route a card to the
	// maintenance path; a maintenance verb needs a maintenance label.
	MaintenanceLabels []string `yaml:"maintenance_labels"`
	MaintenanceVerbs  []string `yaml:"maintenance_verbs"`
	// ForbiddenReferences are regular expressions no card body may match:
	// local paths, parent directories, other checkouts.
	ForbiddenReferences []Pattern `yaml:"forbidden_references"`
}

// Pattern is one named regular expression.
type Pattern struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
}
