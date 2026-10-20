package contract

import (
	"encoding/json"
	"fmt"
	"strings"
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
	Priority  string   `json:"priority"`
	Labels    []string `json:"labels"`
	Skills    []string `json:"skills"`
	DependsOn []string `json:"depends_on"`
	Body      string   `json:"body"`
	Where     []Where  `json:"where"`
	// Criteria names the acceptance criteria (AC ids) the card carries.
	Criteria []string `json:"criteria,omitempty"`
	// Mockup names the screens of the approved mockup the card builds or
	// changes, by screen id; a card that builds no screen names none.
	Mockup []string `json:"mockup,omitempty"`
}

// Manual is a playbook step for a human: a check no card can carry, such as
// a live check or a cross-project gate. Its id (M1, M2, ...) sits in Order
// with the card ids.
type Manual struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Note string `json:"note,omitempty"`
	// Criteria names the manual checks (MC ids) of acceptance the entry
	// carries.
	Criteria []string `json:"criteria,omitempty"`
}

// Setup is what decomposition plans for a target project an agent run is
// not set up for yet: the verify command it proposes when ContextMatrix
// declares none, and the draft id of the card that also writes AGENTS.md
// when the repository has no instruction file.
type Setup struct {
	VerifyCommand   string `json:"verify_command,omitempty"`
	InstructionCard string `json:"instruction_card,omitempty"`
}

// Draft is decomposition's structured block: the cards, the manual steps,
// the playbook title, the playbook order over both, and the setup per
// target project.
type Draft struct {
	Title  string           `json:"title"`
	Cards  []Card           `json:"cards"`
	Manual []Manual         `json:"manual"`
	Order  []string         `json:"order"`
	Setup  map[string]Setup `json:"setup,omitempty"`
}

// ParseDraft decodes an artifact's data block into a Draft. Ids must be
// unique across cards and manual entries; every card needs an id, a project
// and a title, every manual entry an id and text.
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
			return Draft{}, fmt.Errorf("contract: duplicate id %q", c.ID)
		}

		seen[c.ID] = true
	}

	for _, m := range d.Manual {
		if m.ID == "" || strings.TrimSpace(m.Text) == "" {
			return Draft{}, fmt.Errorf("contract: manual entry %q needs an id and text", m.ID)
		}

		if seen[m.ID] {
			return Draft{}, fmt.Errorf("contract: duplicate id %q", m.ID)
		}

		seen[m.ID] = true
	}

	for name, s := range d.Setup {
		d.Setup[name] = Setup{VerifyCommand: strings.TrimSpace(s.VerifyCommand), InstructionCard: strings.TrimSpace(s.InstructionCard)}
	}

	return d, nil
}
