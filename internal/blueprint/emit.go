package blueprint

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

var errNoCMClient = errors.New("blueprint: emission needs the ContextMatrix client")

// cmWriteTimeout bounds one ContextMatrix write of emission. It covers CM's
// own bound on run_playbook, three minutes, as a Run can wait behind a Stop
// of the same playbook.
const cmWriteTimeout = 4 * time.Minute

// writeContext is the context of one ContextMatrix write: the MCP client
// gives up at once on a cancel while CM may still finish the write, which
// the record would then miss and the next attempt would make again. So
// abort, End and Shutdown stop emission between two calls, never inside one.
func writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cmWriteTimeout)
}

// connectCM opens a ContextMatrix session with the session's MCP key.
func (c *Controller) connectCM(ctx context.Context, sess *session) (CMClient, error) {
	if c.opts.ConnectCM == nil {
		return nil, errNoCMClient
	}

	client, err := c.opts.ConnectCM(ctx, sess.mcpKey)
	if err != nil {
		return nil, fmt.Errorf("blueprint: connect to ContextMatrix: %w", err)
	}

	return client, nil
}

// cardsStage is the stage that checks cards; "" when the pack has none, and
// then nothing is emitted.
func (c *Controller) cardsStage() string {
	for _, st := range c.opts.Pack.Stages {
		if slices.Contains(st.Checks, pack.CheckCards) {
			return st.ID
		}
	}

	return ""
}

// draft reads the cards the pipeline ended on: the latest draft of every
// step of the stage that checks cards, merged into one playbook when that
// stage ran per slice.
func (c *Controller) draft(ctx context.Context, id string) (contract.Draft, error) {
	drafts, err := c.stepDrafts(ctx, id)
	if err != nil {
		return contract.Draft{}, err
	}

	switch len(drafts) {
	case 0:
		return contract.Draft{}, fmt.Errorf("blueprint: load the cards: %w", store.ErrNotFound)
	case 1:
		return drafts[0], nil
	}

	return mergeDrafts(drafts), nil
}

// stepDrafts reads the latest draft of every step of the stage that checks
// cards: one per slice in slice order when that stage runs per slice, else
// the one draft.
func (c *Controller) stepDrafts(ctx context.Context, id string) ([]contract.Draft, error) {
	stageID := c.cardsStage()
	if stageID == "" {
		return nil, errors.New("blueprint: the pack has no stage that checks cards")
	}

	steps, _, err := c.steps(ctx, id)
	if err != nil {
		return nil, err
	}

	var drafts []contract.Draft

	for _, s := range loopTargets(steps, stageID) {
		d, err := c.stepDraft(ctx, id, s.ID)
		if err != nil {
			return nil, err
		}

		drafts = append(drafts, d)
	}

	return drafts, nil
}

// emitted reports what emission has recorded: cards when every draft card
// and, for two or more cards, the playbook exist, and complete when in yolo
// mode the run start is recorded too.
func (c *Controller) emitted(ctx context.Context, bp store.Blueprint) (cards, complete bool, err error) {
	draft, err := c.draft(ctx, bp.ID)
	if err != nil {
		return false, false, err
	}

	recs, err := c.opts.Store.ListEmissions(ctx, bp.ID)
	if err != nil {
		return false, false, fmt.Errorf("blueprint: list emissions: %w", err)
	}

	n, playbook, run := 0, false, false

	for _, e := range recs {
		switch e.Kind {
		case store.EmitCard:
			n++
		case store.EmitPlaybook:
			playbook = true
		case store.EmitRun:
			run = true
		}
	}

	cards = n == len(draft.Cards) && (len(draft.Cards) < 2 || playbook)

	return cards, cards && (bp.Mode != protocol.ArchitectModeYolo || run), nil
}

// emit creates an approved blueprint's cards and playbook in ContextMatrix.
// It runs on the blueprint's run slot, so a message meanwhile is a note and
// abort stops it between two calls. Every created id is recorded at once,
// and the next approve resumes from what is recorded. The state is emitting
// until the cards exist and are announced and, in yolo mode, the run start
// was attempted or held for a repository with no commits.
func (c *Controller) emit(ctx context.Context, sess *session, bp store.Blueprint) {
	bg := context.WithoutCancel(ctx)
	au := sess.audit

	c.setState(bg, bp.ID, StateEmitting)

	// The stored mode, not bp's: a chain of runs holds bp from its start, and
	// a switch before the approval decides whether the run starts.
	cur, err := c.opts.Store.GetBlueprint(bg, bp.ID)
	if err != nil {
		c.emitError(ctx, au, bp.ID, err)

		return
	}

	bp = cur

	draft, err := c.draft(bg, bp.ID)
	if err != nil {
		c.emitError(ctx, au, bp.ID, err)

		return
	}

	client, err := c.connectCM(ctx, sess)
	if err != nil {
		c.emitError(ctx, au, bp.ID, err)

		return
	}

	defer func() { _ = client.Close() }()

	// The blocking checks run again: ContextMatrix may have changed since
	// the sign-off. Warnings were shown at the sign-off, and the approval
	// overrode them. The re-run also reads the steps and notices announced
	// with the cards.
	pf, err := c.preflight(ctx, sess, bp, draft, client)
	if err != nil {
		c.emitError(ctx, au, bp.ID, fmt.Errorf("preflight: %w", err))

		return
	}

	if len(pf.Blocking) > 0 {
		if c.closingDown() {
			return
		}

		c.setState(bg, bp.ID, StateIdle)
		au.event(bg, Event{Kind: EventError, BlueprintID: bp.ID, Payload: map[string]any{
			"stage":   "emission",
			"message": "Emission is blocked until this is fixed in ContextMatrix; then reply approve:\n- " + strings.Join(pf.Blocking, "\n- "),
			"text":    "Emission is blocked until this is fixed in ContextMatrix:\n- " + strings.Join(pf.Blocking, "\n- "),
			"resume":  "emission", "version": c.signedOff(bg, bp.ID),
		}})

		return
	}

	if err := c.emitCards(ctx, au, bp, client, draft, pf); err != nil {
		c.emitError(ctx, au, bp.ID, err)

		return
	}

	if bp.Mode == protocol.ArchitectModeYolo && len(pf.EmptyRepos) > 0 {
		// Nothing is recorded, so the blueprint stays incomplete: the watch
		// starts once an approve records the run start.
		text, prompt := emptyReposPrompt(pf.EmptyRepos)

		au.event(bg, Event{Kind: EventAwaitInput, BlueprintID: bp.ID, Payload: map[string]any{
			"stage":  signoffStage,
			"prompt": prompt,
			"text":   text,
			"resume": "held", "version": c.signedOff(bg, bp.ID),
		}})
	} else if bp.Mode == protocol.ArchitectModeYolo {
		err := c.startRun(ctx, au, bp, client, draft)
		if err != nil && c.closingDown() {
			c.opts.Logger.Info("blueprint: run start interrupted by shutdown", "blueprint", bp.ID, "error", au.text(err.Error()))

			return
		}

		if err != nil {
			c.opts.Logger.Error("blueprint: run start", "blueprint", bp.ID, "error", au.text(err.Error()))

			// A cancel here is an abort or End; the error stays in the log.
			stopped := "the run did not start: " + err.Error() + "."
			if ctx.Err() != nil {
				stopped = "the run start was stopped by abort."
			}

			text := "The cards are emitted, but " + stopped

			au.event(bg, Event{Kind: EventError, BlueprintID: bp.ID, Payload: map[string]any{
				"stage":   "run",
				"message": text + " Reply approve to retry the start, or start it in ContextMatrix and reply approve.",
				"text":    text + " Starting it in ContextMatrix first works too; the retry then records that run.",
				"resume":  "run", "version": c.signedOff(bg, bp.ID),
			}})
		}
	}

	// Set last, so a restart before it finds emitting and finishes the rest.
	c.setState(bg, bp.ID, StateEmitted)

	_, complete, err := c.emitted(bg, bp)
	if err != nil {
		c.opts.Logger.Warn("blueprint: watch not started", "blueprint", bp.ID, "error", au.text(err.Error()))

		return
	}

	if complete {
		c.startWatch(sess, bp.ID)
	}
}

// startRun starts the emitted cards once: run_card for one card, which
// opens one pull request to the default branch under the card's own
// settings, and run_playbook for several. The start is recorded, so a
// retry or a reopen never starts a run twice. A run ContextMatrix already
// has, started there by a human or by a start whose record was lost, is
// recorded without starting another, and so is a run found after a start
// failed: the MCP client re-sends a call after a dropped connection, and
// ContextMatrix refuses the re-send of a start it made.
func (c *Controller) startRun(ctx context.Context, au audit, bp store.Blueprint, client CMClient, draft contract.Draft) error {
	bg := context.WithoutCancel(ctx)

	recs, err := c.opts.Store.ListEmissions(bg, bp.ID)
	if err != nil {
		return fmt.Errorf("list emissions: %w", err)
	}

	cards := map[string]store.Emission{}
	playbook := ""

	for _, e := range recs {
		switch e.Kind {
		case store.EmitRun:
			return nil
		case store.EmitCard:
			cards[e.DraftID] = e
		case store.EmitPlaybook:
			playbook = e.CMID
		}
	}

	// A cancel after the cards exist stops before the run start.
	if err := ctx.Err(); err != nil {
		return err
	}

	rec := store.Emission{BlueprintID: bp.ID, Kind: store.EmitRun, CreatedAt: c.opts.Now().UTC()}
	kind, name := "playbook", "playbook "+playbook

	// running reads whether ContextMatrix has the run; start starts it.
	var (
		running func(context.Context) (bool, error)
		start   func(context.Context) error
	)

	if len(draft.Cards) == 1 {
		e := cards[draft.Cards[0].ID]
		rec.Project, rec.CMID, kind, name = e.Project, e.CMID, "card", e.CMID

		running = func(ctx context.Context) (bool, error) {
			card, err := client.GetCard(ctx, e.Project, e.CMID)

			return err == nil && hasRun(card), err
		}
		start = func(ctx context.Context) error {
			_, err := client.RunCard(ctx, e.Project, e.CMID, c.opts.CallerIdentity)

			return err
		}
	} else {
		rec.CMID = playbook

		running = func(ctx context.Context) (bool, error) {
			pb, err := client.GetPlaybook(ctx, playbook)

			return err == nil && playedRun(pb), err
		}
		start = func(ctx context.Context) error {
			return client.RunPlaybook(ctx, playbook, c.opts.CallerIdentity)
		}
	}

	existing, err := running(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}

	if !existing {
		wctx, cancel := writeContext(ctx)
		err = start(wctx)

		cancel()

		// A read after a failed start decides the write's outcome, so it
		// outlives a cancel as the write does. It gets a context of its own:
		// the write's may have run out, which may be why it failed.
		if err != nil {
			rctx, rcancel := writeContext(ctx)
			ran, rerr := running(rctx)

			rcancel()

			switch {
			case rerr != nil:
				c.opts.Logger.Warn("blueprint: read the run start again", "blueprint", bp.ID, "error", au.text(rerr.Error()))
			case ran:
				c.opts.Logger.Warn("blueprint: the run start failed, but the run is there", "blueprint", bp.ID, "error", au.text(err.Error()))

				existing, err = true, nil
			}
		}

		if err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
	}

	if err := c.opts.Store.AddEmission(bg, rec); err != nil {
		return fmt.Errorf("record the start of %s, which runs in ContextMatrix: %w", rec.CMID, err)
	}

	au.event(bg, Event{Kind: EventRunStarted, BlueprintID: bp.ID, Payload: map[string]any{
		"kind": kind, "target": rec.CMID, "existing": existing,
	}})

	return nil
}

// signedOff is the version of the cards the latest sign-off approval
// judged, which names the emission in the panel's prompt; 0 when none is
// recorded or the record cannot be read, which is logged.
func (c *Controller) signedOff(ctx context.Context, id string) int {
	gates, err := c.opts.Store.ListGates(ctx, id, signoffStage)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the sign-off approvals", "blueprint", id, "error", err)

		return 0
	}

	v := 0

	for _, g := range gates {
		if g.Decision == store.GateApprove {
			v = g.Version
		}
	}

	return v
}

// emitError reports a stopped emission and leaves the blueprint idle. One
// Shutdown cut short is only logged and stays emitting. ctx is the run's: a
// cancel there is an abort or End, which the session is told plainly, the
// error going to the log only.
func (c *Controller) emitError(ctx context.Context, au audit, id string, err error) {
	if c.closingDown() {
		c.opts.Logger.Info("blueprint: emission interrupted by shutdown", "blueprint", id, "error", au.text(err.Error()))

		return
	}

	c.opts.Logger.Error("blueprint: emission stopped", "blueprint", id, "error", au.text(err.Error()))

	stopped := "Emission stopped: " + err.Error() + "."
	if ctx.Err() != nil {
		stopped = "Emission stopped by abort."
	}

	bg := context.WithoutCancel(ctx)

	c.setState(bg, id, StateIdle)
	au.event(bg, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
		"stage":   "emission",
		"message": stopped + " Cards already created are kept; reply approve to resume.",
		"text":    stopped + " Cards already created are kept.",
		"resume":  "emission", "version": c.signedOff(bg, id),
	}})
}

// emitCards creates the draft's missing cards in playbook order, which the
// contract keeps dependency-first, then the playbook for two or more cards,
// recording each CM id as soon as it exists. A card CM warns about is kept and
// its warnings reported: readiness mirrors CM's lint, and the outcomes stage
// learns from what slips through. The steps and notices preflight found at this
// attempt, by the stored mode, are announced with the cards, and the run start
// is said to follow unless a repository has no commits. The trees go once
// everything exists. Each card of a slice that builds a screen of a used mockup
// carries it, and each body says what the source tags it cites stand for.
func (c *Controller) emitCards(ctx context.Context, au audit, bp store.Blueprint, client CMClient, draft contract.Draft, pf preflightResult) error {
	bg := context.WithoutCancel(ctx)

	recs, err := c.opts.Store.ListEmissions(bg, bp.ID)
	if err != nil {
		return fmt.Errorf("list emissions: %w", err)
	}

	cmIDs := map[string]string{}
	playbookID := ""
	started := false

	for _, e := range recs {
		switch e.Kind {
		case store.EmitCard:
			cmIDs[e.DraftID] = e.CMID
		case store.EmitPlaybook:
			playbookID = e.CMID
		case store.EmitRun:
			started = true
		}
	}

	summary, err := c.summary(bg, bp.ID)
	if err != nil {
		return err
	}

	sources, err := c.opts.Store.ListSources(bg, bp.ID)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}

	projects, err := client.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}

	byID := map[string]contract.Card{}
	for _, card := range draft.Cards {
		byID[card.ID] = card
	}

	mockup, design, used, err := c.approvedMockup(bg, bp.ID)
	if err != nil {
		return err
	}

	var carriers map[string]bool

	if used {
		if carriers, err = c.mockupCarriersOf(bg, bp.ID); err != nil {
			return err
		}
	}

	created := false

	for _, id := range draft.Order {
		card, isCard := byID[id]
		if !isCard {
			continue
		}

		if _, done := cmIDs[card.ID]; done {
			continue
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		deps := make([]string, 0, len(card.DependsOn))

		for _, dep := range card.DependsOn {
			cmID, ok := cmIDs[dep]
			if !ok {
				return fmt.Errorf("%s depends on %s, which is not created", card.ID, dep)
			}

			deps = append(deps, cmID)
		}

		body := card.Body
		if len(draft.Cards) == 1 {
			body = withContext(body, summary.contextText())
		}

		if carriers[card.ID] {
			body = withMockup(body, mockup, design, card.Mockup)
		}

		body = withSources(body, sources, ownRepos(projects, card.Project))

		wctx, cancel := writeContext(ctx)
		out, err := client.CreateCard(wctx, cm.CardInput{
			Project: card.Project, Title: card.Title, Type: card.Type, Priority: card.Priority,
			Labels: card.Labels, Skills: card.Skills, Body: body, DependsOn: deps, AgentID: c.opts.CallerIdentity,
		})

		cancel()

		if err != nil {
			return fmt.Errorf("create %s: %w", card.ID, err)
		}

		if err := c.opts.Store.AddEmission(bg, store.Emission{
			BlueprintID: bp.ID, Kind: store.EmitCard, DraftID: card.ID, Project: card.Project,
			CMID: out.ID, CreatedAt: c.opts.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("record %s as %s, which exists in ContextMatrix: %w", card.ID, out.ID, err)
		}

		cmIDs[card.ID] = out.ID
		created = true

		au.event(bg, Event{Kind: EventCardCreated, BlueprintID: bp.ID, Payload: map[string]any{
			"draft": card.ID, "project": card.Project, "card": out.ID, "title": card.Title, "warnings": out.Warnings,
		}})
	}

	if len(draft.Cards) > 1 && playbookID == "" {
		if err := ctx.Err(); err != nil {
			return err
		}

		entries := make([]cm.PlaybookEntryInput, 0, len(draft.Order))
		manual := map[string]contract.Manual{}

		for _, m := range draft.Manual {
			manual[m.ID] = m
		}

		for _, id := range draft.Order {
			if card, ok := byID[id]; ok {
				entries = append(entries, cm.PlaybookEntryInput{Type: "card", Project: card.Project, Card: cmIDs[id]})

				continue
			}

			if m, ok := manual[id]; ok {
				entries = append(entries, cm.PlaybookEntryInput{Type: "manual", Text: m.Text, Note: m.Note})
			}
		}

		// A follow-up's card list stays empty: a manual entry names no cards,
		// so nothing attributes it reliably.
		var followups []cm.PlaybookFollowupInput

		for _, id := range draft.Followups {
			if m, ok := manual[id]; ok {
				text := m.Text
				if note := strings.TrimSpace(m.Note); note != "" {
					text += " - " + note
				}

				followups = append(followups, cm.PlaybookFollowupInput{Text: text})
			}
		}

		var own []string
		for _, card := range draft.Cards {
			own = append(own, ownRepos(projects, card.Project)...)
		}

		wctx, cancel := writeContext(ctx)
		id, err := client.CreatePlaybook(wctx, cm.PlaybookInput{
			Title: draft.Title, Description: withSources(summary.description(bp.ID), sources, own), Entries: entries,
			Followups: followups, AgentID: c.opts.CallerIdentity,
		})

		cancel()

		if err != nil {
			return fmt.Errorf("create the playbook: %w", err)
		}

		if err := c.opts.Store.AddEmission(bg, store.Emission{
			BlueprintID: bp.ID, Kind: store.EmitPlaybook, CMID: id, CreatedAt: c.opts.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("record the playbook %s, which exists in ContextMatrix: %w", id, err)
		}

		playbookID = id
		created = true

		au.event(bg, Event{Kind: EventPlaybookCreated, BlueprintID: bp.ID, Payload: map[string]any{
			"playbook": id, "title": draft.Title, "entries": len(entries),
		}})
	}

	if c.opts.Workspace != nil {
		if err := c.opts.Workspace.Remove(bg, bp.ID); err != nil {
			c.opts.Logger.Warn("blueprint: remove trees after emission", "blueprint", bp.ID, "error", err)
		}
	}

	// A resumed emission that created nothing (a retried run start) does not
	// announce the cards again, unless a crash cut the announcement short.
	if !created {
		_, err := c.opts.Store.LatestEvent(bg, bp.ID, EventEmitted)
		if err == nil {
			return nil
		}

		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("read the announcement: %w", err)
		}
	}

	cards := make([]any, 0, len(draft.Cards))

	for _, id := range draft.Order {
		if card, ok := byID[id]; ok {
			cards = append(cards, map[string]any{
				"draft": id, "project": card.Project, "card": cmIDs[id], "title": card.Title,
				"type": card.Type, "priority": card.Priority,
			})
		}
	}

	var manual []string

	if len(draft.Cards) == 1 {
		for _, m := range draft.Manual {
			manual = append(manual, m.Text)
		}
	}

	steps, notices := pf.setup(pf.runsAtOnce(bp.Mode))

	// run_follows tells the panel that the run start comes next.
	au.event(bg, Event{Kind: EventEmitted, BlueprintID: bp.ID, Payload: map[string]any{
		"cards": cards, "playbook": playbookID, "manual": manual, "steps": steps, "notices": notices,
		"run_follows": !started && pf.runsAtOnce(bp.Mode),
	}})

	return nil
}

// blueprintSummary is what the cards and the playbook carry about the
// blueprint: intake's problem and value, and every assumption with its
// latest status.
type blueprintSummary struct {
	problem, value string
	assumptions    []assumption
	// prose is the text of intake's Assumptions section, read only when no
	// stage stated assumptions as data.
	prose string
	// sliced says the blueprint has two or more slices, so a step's name
	// names its slice.
	sliced bool
}

// summary reads the first stage's latest artifact, the need, and the
// blueprint's assumptions.
func (c *Controller) summary(ctx context.Context, id string) (blueprintSummary, error) {
	art, err := c.opts.Store.LatestArtifact(ctx, id, c.opts.Pack.Stages[0].ID)
	if err != nil {
		return blueprintSummary{}, fmt.Errorf("load the need: %w", err)
	}

	list, err := c.assumptions(ctx, id)
	if err != nil {
		return blueprintSummary{}, fmt.Errorf("load the assumptions: %w", err)
	}

	sum := blueprintSummary{
		problem: sectionText(art.Body, "Problem"), value: sectionText(art.Body, "Value"), assumptions: list,
		sliced: c.sliced(ctx, id),
	}

	// An intake stored before assumptions were data keeps them in its body's
	// Assumptions section; without it, a blueprint in flight across that
	// change would tell CM that none were recorded.
	if len(list) == 0 {
		sum.prose = sectionText(art.Body, "Assumptions")
	}

	return sum, nil
}

// description is the playbook's description.
func (s blueprintSummary) description(id string) string {
	var b strings.Builder

	for _, part := range []struct{ head, text string }{{"Problem", s.problem}, {"Value", s.value}} {
		if part.text != "" {
			fmt.Fprintf(&b, "%s:\n%s\n\n", part.head, part.text)
		}
	}

	switch {
	case len(s.assumptions) > 0:
		fmt.Fprintf(&b, "Assumptions, each with its latest status:\n%s\n\n", assumptionLines(s.assumptions, s.sliced))
	case s.prose != "":
		fmt.Fprintf(&b, "Assumptions:\n%s\n\n", s.prose)
	default:
		b.WriteString("Assumptions: none recorded.\n\n")
	}

	fmt.Fprintf(&b, "Emitted by contextmatrix-architect from blueprint %s.", id)

	return b.String()
}

// contextText is what a single card's Context section gains.
func (s blueprintSummary) contextText() string {
	var b strings.Builder

	if s.problem != "" {
		fmt.Fprintf(&b, "Blueprint problem: %s\n\n", s.problem)
	}

	switch {
	case len(s.assumptions) > 0:
		fmt.Fprintf(&b, "Blueprint assumptions, each with its latest status:\n%s", assumptionLines(s.assumptions, s.sliced))
	case s.prose != "":
		fmt.Fprintf(&b, "Blueprint assumptions:\n%s", s.prose)
	default:
		b.WriteString("Blueprint assumptions: none recorded.")
	}

	return b.String()
}

// sectionText returns the text under the first "## <name>" heading of body,
// up to the next "## " heading, trimmed; "" when there is none.
func sectionText(body, name string) string {
	var (
		out []string
		in  bool
	)

	for _, line := range strings.Split(body, "\n") {
		if title, ok := strings.CutPrefix(line, "## "); ok {
			if in {
				break
			}

			in = strings.EqualFold(strings.TrimSpace(title), name)

			continue
		}

		if in {
			out = append(out, line)
		}
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}

// withContext appends text to the end of body's Context section, or opens
// the body with one when it has none.
func withContext(body, text string) string {
	lines := strings.Split(body, "\n")

	start := slices.IndexFunc(lines, func(l string) bool {
		title, ok := strings.CutPrefix(l, "## ")

		return ok && strings.EqualFold(strings.TrimSpace(title), "Context")
	})
	if start < 0 {
		return "## Context\n\n" + text + "\n\n" + body
	}

	end := len(lines)

	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i

			break
		}
	}

	out := slices.Concat(lines[:end], []string{"", text, ""}, lines[end:])

	return strings.Join(out, "\n")
}

// hasRun reports whether a card has a run, in flight or over. run_card
// starts only a todo card with no run queued or running, and marks it
// queued. Claimed, the card leaves todo, and a run that ends clears the
// worker status or parks it. A todo card whose worker failed or was killed
// is a start that never ran, such as a trigger CM could not send.
func hasRun(card cm.Card) bool {
	switch card.WorkerStatus {
	case "queued", "running", "parked":
		return true
	}

	return card.State != "todo"
}

// playedRun reports whether a playbook has a run, in flight or over: it is
// running, waiting or completed. Only a stopped run is played again, which
// resumes it; a completed one is not started over.
func playedRun(pb cm.Playbook) bool {
	return pb.Run != nil && pb.Run.Status != "stopped"
}

// citation is one bracketed citation, such as [S1], [S1 internal/x.go] or
// [S1, S2]; sourceTag is one source tag inside it.
var (
	citation  = regexp.MustCompile(`\[[^\[\]]*\]`)
	sourceTag = regexp.MustCompile(`\bS(\d+)\b`)
)

// withSources appends a Sources section that says what every source tag
// the body cites stands for, so the tags still mean something once the card
// has left the blueprint. A tag the body does not cite, or one that names
// no source, is left out, and a body citing none is returned as it is. A
// mockup block cites nothing: its brackets are the source's own.
func withSources(body string, sources []store.Source, own []string) string {
	byNumber := map[int]store.Source{}
	for _, s := range sources {
		byNumber[s.Number] = s
	}

	cited := map[int]bool{}

	for _, c := range citation.FindAllString(contract.WithoutMockups(body), -1) {
		for _, m := range sourceTag.FindAllStringSubmatch(c, -1) {
			n, err := strconv.Atoi(m[1])
			if _, known := byNumber[n]; err == nil && known {
				cited[n] = true
			}
		}
	}

	if len(cited) == 0 {
		return body
	}

	var b strings.Builder

	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n\n## Sources\n\n")

	for _, n := range slices.Sorted(maps.Keys(cited)) {
		b.WriteString("- " + sourceLine(byNumber[n], own) + "\n")
	}

	return b.String()
}

// sourceLine says what one source is. A repository is named with the
// commit the checkout recorded, unless it is not one of own, the card's
// project's repositories: the contract keeps other projects' repositories
// out of a card, and CM's lint warns on them.
func sourceLine(s store.Source, own []string) string {
	switch {
	case s.Kind == "cm":
		return s.Tag() + ": the ContextMatrix board, as read while the cards were written"
	case s.Kind == "repo" && !slices.Contains(own, s.Identity):
		return s.Tag() + ": another project's repository " + atCommit(s.Version)
	case s.Kind == "repo":
		return s.Tag() + ": " + s.Identity + " " + atCommit(s.Version)
	default:
		return s.Tag() + ": " + s.Identity
	}
}

// atCommit names the commit a repository was read at by its first seven
// characters, or says it had none.
func atCommit(sha string) string {
	if sha == "" {
		return "with no commits"
	}

	return "at " + sha[:min(7, len(sha))]
}

// ownRepos is the clean url of every repository of the named project, the
// form sources are recorded under.
func ownRepos(projects []cm.Project, name string) []string {
	var out []string

	for _, p := range projects {
		if p.Name != name {
			continue
		}

		for _, u := range p.RepoURLs() {
			out = append(out, code.CleanURL(u))
		}
	}

	return out
}

// emptyReposPrompt tells the session why yolo did not start the run and how
// to go on: the prompt's text, and the message with its reply instruction.
// The approve is what records the run start, which starts the watch.
func emptyReposPrompt(repos []string) (text, prompt string) {
	if len(repos) == 1 {
		return repos[0] + " has no commits, so the run was not started. Push an initial commit, or start the run in ContextMatrix, before starting it here.",
			repos[0] + " has no commits, so the run was not started; push an initial commit, then reply approve to start it, or start it in ContextMatrix and reply approve."
	}

	return strings.Join(repos, ", ") + " have no commits, so the run was not started. Push an initial commit to each, or start the run in ContextMatrix, before starting it here.",
		strings.Join(repos, ", ") + " have no commits, so the run was not started; push an initial commit to each, then reply approve to start it, or start it in ContextMatrix and reply approve."
}
