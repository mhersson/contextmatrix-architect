package contract

import (
	"encoding/json"
	"fmt"
)

// Where is one code location a card names: a repo-relative path and,
// optionally, a symbol expected inside it.
type Where struct {
	Path   string `json:"path"`
	Symbol string `json:"symbol,omitempty"`
}

// Card is a draft card as decomposition submits it. IDs are draft ids
// (D1, D2, ...); emission maps them to CM ids later.
type Card struct {
	ID        string   `json:"id"`
	Project   string   `json:"project"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Labels    []string `json:"labels"`
	Skills    []string `json:"skills"`
	DependsOn []string `json:"depends_on"`
	Body      string   `json:"body"`
	Where     []Where  `json:"where"`
}

// Draft is decomposition's structured block: the cards and the playbook order.
type Draft struct {
	Cards []Card   `json:"cards"`
	Order []string `json:"order"`
}

// ParseDraft decodes an artifact's data block into a Draft. Ids must be
// unique and every card needs an id, a project and a title.
func ParseDraft(data map[string]any) (Draft, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Draft{}, fmt.Errorf("contract: encode draft: %w", err)
	}

	var d Draft
	if err := json.Unmarshal(raw, &d); err != nil {
		return Draft{}, fmt.Errorf("contract: decode draft: %w", err)
	}

	seen := map[string]bool{}

	for _, c := range d.Cards {
		if c.ID == "" || c.Project == "" || c.Title == "" {
			return Draft{}, fmt.Errorf("contract: card %q needs id, project and title", c.ID)
		}

		if seen[c.ID] {
			return Draft{}, fmt.Errorf("contract: duplicate card id %q", c.ID)
		}

		seen[c.ID] = true
	}

	return d, nil
}
