package contract

import (
	"fmt"
	"strings"
)

// The matching below mirrors CM's self-containment lint (internal/board/lint.go
// in contextmatrix): CM warns only after it has created the card, so the
// architect refuses the same text before emission.

// checkForeignRepos flags a card that names another CM project's repository
// by URL or owner/name outside its mockup blocks: the agent's container
// holds only its own project's repository.
func checkForeignRepos(c Card, foreign []string) []Finding {
	text := strings.ToLower(c.Title + "\n" + WithoutMockups(c.Body))

	var out []Finding

	for _, repo := range foreign {
		for _, form := range repoMatchForms(repo) {
			if form != "" && hasMatchWithBoundary(text, strings.ToLower(form)) {
				out = append(out, Finding{c.ID, "foreign_repo", fmt.Sprintf("names another project's repository %q; the agent's container holds only this project's repository", form)})

				break
			}
		}
	}

	return out
}

// repoMatchForms returns the forms of a repository url CM matches: the url
// without a .git suffix and its owner/name tail, for https and scp-like urls.
func repoMatchForms(repoURL string) []string {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return nil
	}

	bare := strings.TrimSuffix(repoURL, ".git")
	forms := []string{bare}

	trimmed := strings.Trim(bare, "/")
	if idx := strings.LastIndexAny(trimmed, "/:"); idx >= 0 {
		rest := trimmed[idx+1:]
		if idx2 := strings.LastIndexAny(trimmed[:idx], "/:"); idx2 >= 0 {
			owner := trimmed[idx2+1 : idx]
			if owner != "" && rest != "" {
				forms = append(forms, owner+"/"+rest)
			}
		}
	}

	return forms
}

// hasMatchWithBoundary reports whether needle occurs in haystack with no
// word or path character ([A-Za-z0-9_-]) directly before or after it.
func hasMatchWithBoundary(haystack, needle string) bool {
	idx := 0

	for {
		pos := strings.Index(haystack[idx:], needle)
		if pos < 0 {
			return false
		}

		at := idx + pos

		if at > 0 && isWordPathChar(haystack[at-1]) {
			idx = at + 1

			continue
		}

		if end := at + len(needle); end < len(haystack) && isWordPathChar(haystack[end]) {
			idx = at + 1

			continue
		}

		return true
	}
}

func isWordPathChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') || b == '_' || b == '-'
}
