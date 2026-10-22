package blueprint

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
)

// fakeCM is ContextMatrix for the controller. Created cards are numbered
// ALPHA-001, ALPHA-002, ... per project; every call is recorded; tests set
// the card and playbook views GetCard and GetPlaybook serve, optionally as a
// sequence (the last one sticks).
type fakeCM struct {
	mu sync.Mutex

	projects   []cm.Project
	created    []cm.CardInput
	playbooks  []cm.PlaybookInput
	warnings   map[string][]string // by card title
	failCreate map[string]int      // by card title: fail this many times
	runCards   []string            // "project/id|agent"
	runBooks   []string            // "id|agent"
	runErr     error
	list       []cm.PlaybookSummary
	cardSeq    map[string][]cm.Card
	ended      map[string]cm.EndedCard // by card id, as GetEndedCard serves it
	failEnded  int                     // fail this many GetEndedCard calls
	endedGets  int
	bookSeq    map[string][]cm.Playbook
	seq        map[string]int
	gets       int
	failGets   int // fail this many GetCard calls
	connects   int
	closes     int
}

func newFakeCM() *fakeCM {
	return &fakeCM{
		projects:   []cm.Project{alphaProject()},
		warnings:   map[string][]string{},
		failCreate: map[string]int{},
		cardSeq:    map[string][]cm.Card{},
		ended:      map[string]cm.EndedCard{},
		bookSeq:    map[string][]cm.Playbook{},
		seq:        map[string]int{},
	}
}

func alphaProject() cm.Project {
	return cm.Project{Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make test"}}
}

func (f *fakeCM) connect(context.Context, string) (CMClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.connects++

	return f, nil
}

func (f *fakeCM) ListProjects(context.Context) ([]cm.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]cm.Project(nil), f.projects...), nil
}

func (f *fakeCM) ListPlaybooks(context.Context) ([]cm.PlaybookSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]cm.PlaybookSummary(nil), f.list...), nil
}

func (f *fakeCM) GetPlaybook(_ context.Context, id string) (cm.Playbook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gets++

	seq := f.bookSeq[id]
	if len(seq) == 0 {
		return cm.Playbook{}, fmt.Errorf("cm: get_playbook: %s not found", id)
	}

	if len(seq) > 1 {
		f.bookSeq[id] = seq[1:]
	}

	return seq[0], nil
}

func (f *fakeCM) GetCard(_ context.Context, _, id string) (cm.Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gets++

	if f.failGets > 0 {
		f.failGets--

		return cm.Card{}, errors.New("cm: get_card: remote unreachable")
	}

	seq := f.cardSeq[id]
	if len(seq) == 0 {
		return cm.Card{}, fmt.Errorf("cm: get_card: %s not found", id)
	}

	if len(seq) > 1 {
		f.cardSeq[id] = seq[1:]
	}

	return seq[0], nil
}

func (f *fakeCM) GetEndedCard(_ context.Context, _, id string) (cm.EndedCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gets++
	f.endedGets++

	if f.failEnded > 0 {
		f.failEnded--

		return cm.EndedCard{}, errors.New("cm: get_card: remote unreachable")
	}

	card, ok := f.ended[id]
	if !ok {
		return cm.EndedCard{}, fmt.Errorf("cm: get_card: %s not found", id)
	}

	return card, nil
}

func (f *fakeCM) CreateCard(_ context.Context, in cm.CardInput) (cm.CreatedCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if n := f.failCreate[in.Title]; n > 0 {
		f.failCreate[in.Title] = n - 1

		return cm.CreatedCard{}, errors.New("cm: create_card: remote unreachable")
	}

	f.seq[in.Project]++
	id := fmt.Sprintf("%s-%03d", strings.ToUpper(in.Project), f.seq[in.Project])
	f.created = append(f.created, in)
	f.cardSeq[id] = []cm.Card{{ID: id, Project: in.Project, Title: in.Title, State: "todo"}}

	return cm.CreatedCard{ID: id, Warnings: f.warnings[in.Title]}, nil
}

func (f *fakeCM) CreatePlaybook(_ context.Context, in cm.PlaybookInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := strings.ReplaceAll(strings.ToLower(in.Title), " ", "-")
	f.playbooks = append(f.playbooks, in)
	f.bookSeq[id] = []cm.Playbook{{ID: id, Title: in.Title, Total: len(in.Entries)}}

	return id, nil
}

func (f *fakeCM) RunCard(_ context.Context, project, id, agentID string) (cm.Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runCards = append(f.runCards, project+"/"+id+"|"+agentID)

	if f.runErr != nil {
		return cm.Card{}, f.runErr
	}

	return cm.Card{ID: id, Project: project, State: "todo", WorkerStatus: "queued"}, nil
}

func (f *fakeCM) RunPlaybook(_ context.Context, id, agentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runBooks = append(f.runBooks, id+"|"+agentID)

	return f.runErr
}

func (f *fakeCM) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closes++

	return nil
}

// holdCM holds CreateCard of one title, or every run start, until free or
// the call's cancel. The held write is made either way, as ContextMatrix
// finishes a write whose caller gave up, and a call cancelled by then
// returns the cancel.
type holdCM struct {
	CMClient

	title   string
	run     bool
	entered chan struct{}
	release chan struct{}
	free    func()
}

// newHoldCM wraps inner; the hold is freed when the test ends at the latest.
func newHoldCM(t *testing.T, inner CMClient) *holdCM {
	t.Helper()

	h := &holdCM{CMClient: inner, entered: make(chan struct{}, 1), release: make(chan struct{})}
	h.free = closer(t, h.release)

	return h
}

func (h *holdCM) connect(context.Context, string) (CMClient, error) {
	return h, nil
}

// hold waits for free or the cancel and returns the cancel, if any.
func (h *holdCM) hold(ctx context.Context) error {
	signal(h.entered)

	select {
	case <-h.release:
	case <-ctx.Done():
	}

	return ctx.Err()
}

func (h *holdCM) CreateCard(ctx context.Context, in cm.CardInput) (cm.CreatedCard, error) {
	if in.Title != h.title {
		return h.CMClient.CreateCard(ctx, in)
	}

	canceled := h.hold(ctx)
	out, err := h.CMClient.CreateCard(ctx, in)

	if canceled != nil {
		return cm.CreatedCard{}, canceled
	}

	return out, err
}

func (h *holdCM) RunCard(ctx context.Context, project, id, agentID string) (cm.Card, error) {
	if !h.run {
		return h.CMClient.RunCard(ctx, project, id, agentID)
	}

	canceled := h.hold(ctx)
	out, err := h.CMClient.RunCard(ctx, project, id, agentID)

	if canceled != nil {
		return cm.Card{}, canceled
	}

	return out, err
}

func (h *holdCM) RunPlaybook(ctx context.Context, id, agentID string) error {
	if !h.run {
		return h.CMClient.RunPlaybook(ctx, id, agentID)
	}

	canceled := h.hold(ctx)

	return cmp.Or(canceled, h.CMClient.RunPlaybook(ctx, id, agentID))
}

// titles returns the titles of the cards created, in creation order.
func (f *fakeCM) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.created))
	for _, in := range f.created {
		out = append(out, in.Title)
	}

	return out
}

func (f *fakeCM) inputs() ([]cm.CardInput, []cm.PlaybookInput) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]cm.CardInput(nil), f.created...), append([]cm.PlaybookInput(nil), f.playbooks...)
}

// set replaces what the fake serves for a card or a playbook.
func (f *fakeCM) setCards(id string, seq ...cm.Card) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.cardSeq[id] = seq
}

// setEnded replaces what the fake serves for a card read as it ended.
func (f *fakeCM) setEnded(id string, card cm.EndedCard) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ended[id] = card
}

func (f *fakeCM) setBooks(id string, seq ...cm.Playbook) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.bookSeq[id] = seq
}

func (f *fakeCM) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.gets
}

// endedReads is how many GetEndedCard calls were made.
func (f *fakeCM) endedReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.endedGets
}

func (f *fakeCM) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closes
}

func (f *fakeCM) connectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.connects
}

// cardRuns returns the run_card calls, in call order.
func (f *fakeCM) cardRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.runCards...)
}
