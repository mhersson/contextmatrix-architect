package cm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// CardInput is the create_card argument set; empty Skills inherit the project default.
type CardInput struct {
	Project   string   `json:"project"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Priority  string   `json:"priority"`
	Labels    []string `json:"labels,omitempty"`
	Skills    []string `json:"skills,omitempty"`
	Body      string   `json:"body"`
	DependsOn []string `json:"depends_on,omitempty"`
	AgentID   string   `json:"agent_id,omitempty"`
}

// CreatedCard is the part of create_card's reply the architect reads.
type CreatedCard struct {
	ID       string   `json:"id"`
	Warnings []string `json:"warnings"`
}

// Card is the part of a card's summary the run watch reads.
type Card struct {
	ID             string `json:"id"`
	Project        string `json:"project"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Phase          string `json:"phase"`
	WorkerStatus   string `json:"worker_status"`
	AssignedAgent  string `json:"assigned_agent"`
	PRURL          string `json:"pr_url"`
	ReviewAttempts int    `json:"review_attempts"`
}

// TokenUsage is the card's estimated spend as CM reports it.
type TokenUsage struct {
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

// ActivityEntry is one line of a card's activity log.
type ActivityEntry struct {
	Action  string `json:"action"`
	Message string `json:"message"`
}

// EndedCard is a card as its run left it: the watch's fields and what a
// retrospective judges.
type EndedCard struct {
	Card
	Body           string          `json:"body"`
	Subtasks       []string        `json:"subtasks"`
	TokenUsage     *TokenUsage     `json:"token_usage"`
	SubtaskCostUSD float64         `json:"subtask_cost_usd"`
	ActivityLog    []ActivityEntry `json:"activity_log"`
}

// PlaybookEntryInput is one entry of a playbook being created.
type PlaybookEntryInput struct {
	Type    string `json:"type"`
	Project string `json:"project,omitempty"`
	Card    string `json:"card,omitempty"`
	Text    string `json:"text,omitempty"`
	Note    string `json:"note,omitempty"`
}

// PlaybookCardRef names a card of a playbook by project and id.
type PlaybookCardRef struct {
	Project string `json:"project"`
	Card    string `json:"card"`
}

// PlaybookFollowupInput is one follow-up of a playbook being created: a
// check a person makes after the run, which the runner never reads.
type PlaybookFollowupInput struct {
	Text  string            `json:"text"`
	Cards []PlaybookCardRef `json:"cards,omitempty"`
}

// PlaybookInput is the create_playbook argument set. Followups is omitted
// when empty: an older ContextMatrix's schema refuses the key.
type PlaybookInput struct {
	Title       string                  `json:"title"`
	Description string                  `json:"description,omitempty"`
	Entries     []PlaybookEntryInput    `json:"entries"`
	Followups   []PlaybookFollowupInput `json:"followups,omitempty"`
	AgentID     string                  `json:"agent_id"`
}

// PlaybookRun is a playbook's current run state.
type PlaybookRun struct {
	Status  string     `json:"status"`
	Entry   string     `json:"entry"`
	Reason  string     `json:"reason"`
	EndedAt *time.Time `json:"ended_at"`
}

// PlaybookRepo is a compare link for one project's repo in a playbook.
type PlaybookRepo struct {
	Project    string `json:"project"`
	CompareURL string `json:"compare_url"`
}

// PlaybookEntry is one playbook entry with the live state of its card.
// Missing marks a card entry whose card or project was deleted: it stays in
// the playbook, counts in the total and never completes.
type PlaybookEntry struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Project   string `json:"project"`
	Card      string `json:"card"`
	Text      string `json:"text"`
	Done      bool   `json:"done"`
	CardTitle string `json:"card_title"`
	CardState string `json:"card_state"`
	Missing   bool   `json:"missing"`
	Complete  bool   `json:"complete"`
}

// PlaybookFollowup is one follow-up get_playbook returns: a check a person
// makes after the run, which people may add, change or remove in
// ContextMatrix while the run goes on.
type PlaybookFollowup struct {
	ID    string            `json:"id"`
	Text  string            `json:"text"`
	Cards []PlaybookCardRef `json:"cards"`
	Done  bool              `json:"done"`
}

// Playbook is the part of get_playbook's reply the run watch reads. An older
// ContextMatrix returns no followups.
type Playbook struct {
	ID        string             `json:"id"`
	Title     string             `json:"title"`
	Runnable  bool               `json:"runnable"`
	Run       *PlaybookRun       `json:"run"`
	Repos     []PlaybookRepo     `json:"repos"`
	Complete  int                `json:"complete"`
	Total     int                `json:"total"`
	Entries   []PlaybookEntry    `json:"entries"`
	Followups []PlaybookFollowup `json:"followups"`
}

// PlaybookSummary is one row of list_playbooks.
type PlaybookSummary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Runnable bool   `json:"runnable"`
}

// callJSON calls a tool and decodes its JSON text into out.
func (c *Client) callJSON(ctx context.Context, tool string, args, out any) error {
	text, err := c.callArgs(ctx, tool, args)
	if err != nil {
		return err
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("cm: decode %s: %w", tool, err)
	}

	return nil
}

// CreateCard creates one card. CM creates the card before it lints it, so
// warnings come back with a card that exists.
func (c *Client) CreateCard(ctx context.Context, in CardInput) (CreatedCard, error) {
	var out CreatedCard

	return out, c.callJSON(ctx, "create_card", in, &out)
}

// CreatePlaybook creates a playbook with its entries, all or nothing, and
// returns its id.
func (c *Client) CreatePlaybook(ctx context.Context, in PlaybookInput) (string, error) {
	var out PlaybookSummary
	if err := c.callJSON(ctx, "create_playbook", in, &out); err != nil {
		return "", err
	}

	return out.ID, nil
}

// GetCard reads a card without its activity log or images.
func (c *Client) GetCard(ctx context.Context, project, id string) (Card, error) {
	var out Card

	return out, c.callJSON(ctx, "get_card", map[string]any{
		"project": project, "card_id": id, "include_activity_log": false, "include_images": false,
	}, &out)
}

// GetEndedCard reads a card with its body, subtasks, costs and activity log.
func (c *Client) GetEndedCard(ctx context.Context, project, id string) (EndedCard, error) {
	var out EndedCard

	return out, c.callJSON(ctx, "get_card", map[string]any{
		"project": project, "card_id": id, "include_activity_log": true, "include_images": false,
	}, &out)
}

// GetPlaybook reads a playbook with its run and the live state of its entries.
func (c *Client) GetPlaybook(ctx context.Context, id string) (Playbook, error) {
	var out Playbook

	return out, c.callJSON(ctx, "get_playbook", map[string]any{"id": id}, &out)
}

// ListPlaybooks lists every playbook's summary.
func (c *Client) ListPlaybooks(ctx context.Context) ([]PlaybookSummary, error) {
	var out struct {
		Playbooks []PlaybookSummary `json:"playbooks"`
	}

	if err := c.callJSON(ctx, "list_playbooks", map[string]any{}, &out); err != nil {
		return nil, err
	}

	return out.Playbooks, nil
}

// RunCard starts a card's autonomous run under its own settings. agentID
// must be a human: identity, which CM's run_card gate requires.
func (c *Client) RunCard(ctx context.Context, project, id, agentID string) (Card, error) {
	var out Card

	return out, c.callJSON(ctx, "run_card", map[string]any{"project": project, "card_id": id, "agent_id": agentID}, &out)
}

// RunPlaybook makes a playbook runnable if it is not and plays it. A CM
// without the run_playbook tool answers with an error naming it.
func (c *Client) RunPlaybook(ctx context.Context, id, agentID string) error {
	return c.callJSON(ctx, "run_playbook", map[string]any{"id": id, "agent_id": agentID}, nil)
}
