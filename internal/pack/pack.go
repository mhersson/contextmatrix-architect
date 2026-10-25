// Package pack loads a pipeline pack: the stage list, the persona texts and
// the per-stage instructions. The default pack is embedded in the binary.
package pack

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
)

//go:embed default
var defaultFS embed.FS

// Tool groups and checks a pack.yaml may name.
const (
	ToolCode = "code"
	// ToolCM is the read-only ContextMatrix tool group.
	ToolCM = "cm"
	// ToolResearch is web_search and web_fetch, offered when the session's
	// research setting is on.
	ToolResearch = "research"
	CheckCards   = "cards"
	// CheckAssumptions checks a submit's assumptions: standalone sentences,
	// each basis an answered question, and no sentence naming one.
	CheckAssumptions = "assumptions"
	// CheckDecisions checks a design's decisions: each made, with a
	// rejected option and a basis, and every answer used.
	CheckDecisions = "decisions"
	// CheckSlices checks a slicing's slices: at least one, unique ids, a
	// title each, target projects only.
	CheckSlices = "slices"
	// CheckCriteria checks acceptance's criteria and manual checks, and
	// makes decomposition map every criterion to exactly one card.
	CheckCriteria = "criteria"
	// CheckOutcomes checks a submit's measurable outcomes: ids O<n> unique
	// across the blueprint and after the highest an earlier step holds, each
	// with its sentence; the first stage that checks them states at least
	// one.
	CheckOutcomes = "outcomes"
	// CheckReadiness checks a readiness verdict: one well-formed entry per
	// card, and its findings across the blueprint, each X<n> with a
	// severity and its text.
	CheckReadiness = "readiness"
	// CheckScreen marks the stage whose data states screen: true or false,
	// whether the need has a screen to draw.
	CheckScreen = "screen"
	// CheckMockup marks the stage that draws the mockups.
	CheckMockup = "mockup"
)

// Responder policies: who answers a stage's questions and decides its gate.
const (
	PolicyHuman = "human"
	PolicyAuto  = "auto"
)

// The instructions every reviewer, responder or plain-language run reads,
// and every stage run of an unattended blueprint, under stages/. No stage
// may take these ids.
const (
	reviewInstructions     = "review"
	responderInstructions  = "responder"
	plainInstructions      = "plain"
	unattendedInstructions = "unattended"
	// mockupResponderInstructions is what every mockup responder run reads.
	mockupResponderInstructions = "mockup_responder"
	// retrospectiveInstructions is what every retrospective run reads.
	retrospectiveInstructions = "retrospective"
)

// RetroPersona is the persona a retrospective run plays.
const RetroPersona = "reviewer"

// stepSeparator joins a per-slice stage's id and its slice in a step key;
// no stage id holds it.
const stepSeparator = "@"

// StepKey names one step of a blueprint's pipeline in its record: a
// whole-blueprint stage by its id, a per-slice stage's step for one slice
// as <stage>@<slice>.
func StepKey(stageID, slice string) string {
	if slice == "" {
		return stageID
	}

	return stageID + stepSeparator + slice
}

// SplitStep is StepKey's inverse; slice is "" for a whole-blueprint key.
func SplitStep(key string) (stageID, slice string) {
	stageID, slice, _ = strings.Cut(key, stepSeparator)

	return stageID, slice
}

// Fold lets a gate fold into the next gated stage's gate: when the stage's
// data array Field holds at most Max entries and both gates have the same
// gate policy, the next gate and its reviewer judge both artifacts.
type Fold struct {
	Field string `yaml:"field"`
	Max   int    `yaml:"max"`
}

// Reviewer is the run that judges a gated stage's artifact before its gate:
// Persona plays it, and a score below Threshold, 1 to 5, or a critical or
// major finding left open sends the stage back for another round; open
// minors go to the gate. Tools names its tool groups: "code", and
// "research", which gives a reviewer web_fetch alone, to open what the
// artifact cites.
type Reviewer struct {
	Persona   string   `yaml:"persona"`
	Threshold int      `yaml:"threshold"`
	Tools     []string `yaml:"tools"`
}

// Stage is one unit of work in the pipeline: one model run, and the gate
// after it when it has one.
type Stage struct {
	ID       string `yaml:"id"`
	Persona  string `yaml:"persona"`
	Produces string `yaml:"produces"`
	// Policy is who answers the stage's questions, "human" or "auto", and
	// decides its gate unless GatePolicy says otherwise; a model stage
	// without one is auto. The mode and the blueprint's gate replies
	// override both.
	Policy string `yaml:"policy"`
	// Gate stops the pipeline after the stage for approve or reject. A gated
	// stage declares a Reviewer and may fold into the next gate.
	Gate bool `yaml:"gate"`
	// GatePolicy is who decides the gate, "human" or "auto". It defaults to
	// Policy, so only a stage whose gate differs from its questions sets it.
	GatePolicy string    `yaml:"gate_policy"`
	Fold       *Fold     `yaml:"fold"`
	Reviewer   *Reviewer `yaml:"reviewer"`
	// MaxQuestions caps the questions one attempt of the stage may ask, its
	// builder rounds included; 0 is no cap.
	MaxQuestions int `yaml:"max_questions"`
	// Consumes lists earlier stages whose latest artifacts are this stage's
	// input, in the order they are presented.
	Consumes []string `yaml:"consumes"`
	// Tools names the tool groups the run gets: "code" is the read-only
	// checkout tools plus repo_info, "cm" the read-only ContextMatrix tools,
	// "research" web_search and web_fetch when the session has research on.
	Tools []string `yaml:"tools"`
	// Checks names the mechanical checks run on every submit; "cards" is the
	// card contract.
	Checks []string `yaml:"checks"`
	// LoopsTo names the earlier stage re-run when this stage's artifact
	// reports failures, at most MaxRounds times.
	LoopsTo   string `yaml:"loops_to"`
	MaxRounds int    `yaml:"max_rounds"`
	// PerSlice repeats the stage once per slice of the newest version of the
	// stage that checks slices, in its order: every per-slice stage for SL1,
	// then for SL2. Per-slice stages are contiguous and follow that stage.
	PerSlice bool `yaml:"per_slice"`
	// EarlierSlices names per-slice stages whose latest artifact for every
	// earlier slice a per-slice stage also reads, so later slices build on
	// what earlier ones settled.
	EarlierSlices []string `yaml:"earlier_slices"`
	// Shared marks the stage that settles what every slice shares. It sits
	// between the stage that checks slices and the per-slice stages, and with
	// fewer than two slices it collapses: recorded, never run, its decisions
	// settled by the first slice's design.
	Shared bool `yaml:"shared"`
}

type manifest struct {
	Name    string  `yaml:"name"`
	Version string  `yaml:"version"`
	Stages  []Stage `yaml:"stages"`
}

// Pack is a loaded pipeline pack.
type Pack struct {
	Name    string
	Version string
	Stages  []Stage

	personas     map[string]string
	instructions map[string]string
	lists        contract.Lists
	card         string
	review       string
	responder    string
	mockupResp   string
	plain        string
	unattended   string
	retro        string
}

// LoadEmbedded loads the pack compiled into the binary.
func LoadEmbedded() (*Pack, error) {
	sub, err := fs.Sub(defaultFS, "default")
	if err != nil {
		return nil, fmt.Errorf("pack: embedded root: %w", err)
	}

	return load(sub)
}

// LoadDir loads a pack from a directory on disk.
func LoadDir(dir string) (*Pack, error) {
	return load(os.DirFS(dir))
}

func load(fsys fs.FS) (*Pack, error) {
	raw, err := fs.ReadFile(fsys, "pack.yaml")
	if err != nil {
		return nil, fmt.Errorf("pack: read pack.yaml: %w", err)
	}

	var m manifest

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)

	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("pack: parse pack.yaml: %w", err)
	}

	if m.Name == "" || m.Version == "" || len(m.Stages) == 0 {
		return nil, fmt.Errorf("pack: pack.yaml needs name, version and at least one stage")
	}

	p := &Pack{
		Name: m.Name, Version: m.Version,
		personas: map[string]string{}, instructions: map[string]string{},
	}

	seen := map[string]Stage{}

	for i := range m.Stages {
		s := &m.Stages[i]

		if err := validateStage(*s, seen); err != nil {
			return nil, err
		}

		if s.Policy == "" {
			s.Policy = PolicyAuto
		}

		if s.Gate && s.GatePolicy == "" {
			s.GatePolicy = s.Policy
		}

		seen[s.ID] = *s

		if err := p.loadPersona(fsys, s.ID, s.Persona); err != nil {
			return nil, err
		}

		if s.Reviewer != nil {
			if err := p.loadPersona(fsys, s.ID, s.Reviewer.Persona); err != nil {
				return nil, err
			}
		}

		text, err := fs.ReadFile(fsys, path.Join("stages", s.ID+".md"))
		if err != nil {
			return nil, fmt.Errorf("pack: stage %q: instructions: %w", s.ID, err)
		}

		p.instructions[s.ID] = string(text)
	}

	p.Stages = m.Stages

	if err := validateSlices(p.Stages); err != nil {
		return nil, err
	}

	if err := validateMockup(p.Stages); err != nil {
		return nil, err
	}

	for i, s := range p.Stages {
		if s.Fold != nil && !slices.ContainsFunc(p.Stages[i+1:], func(t Stage) bool { return t.Gate && !t.Shared }) {
			return nil, fmt.Errorf("pack: stage %q folds, but no later stage has a gate to fold into (a shared stage's gate takes no fold)", s.ID)
		}
	}

	// A stage that checks cards needs the data the checks read and the
	// template the cards follow; without them the contract would silently
	// pass every card.
	checker := ""

	for _, s := range p.Stages {
		if slices.Contains(s.Checks, CheckCards) {
			checker = s.ID

			break
		}
	}

	raw, err = optionalFile(fsys, "lists.yaml", checker, "checks cards")
	if err != nil {
		return nil, err
	}

	if p.lists, err = parseLists(raw); err != nil {
		return nil, err
	}

	card, err := optionalFile(fsys, "card.md", checker, "checks cards")
	if err != nil {
		return nil, err
	}

	p.card = string(card)

	reviewed := ""

	for _, s := range p.Stages {
		if s.Reviewer != nil {
			reviewed = s.ID

			break
		}
	}

	review, err := optionalFile(fsys, path.Join("stages", reviewInstructions+".md"), reviewed, "has a reviewer")
	if err != nil {
		return nil, err
	}

	p.review = string(review)

	responder, err := optionalFile(fsys, path.Join("stages", responderInstructions+".md"), "", "")
	if err != nil {
		return nil, err
	}

	p.responder = string(responder)

	mockupResp, err := optionalFile(fsys, path.Join("stages", mockupResponderInstructions+".md"), p.MockupStage(), "draws mockups")
	if err != nil {
		return nil, err
	}

	p.mockupResp = string(mockupResp)

	plain, err := optionalFile(fsys, path.Join("stages", plainInstructions+".md"), "", "")
	if err != nil {
		return nil, err
	}

	p.plain = string(plain)

	unattended, err := optionalFile(fsys, path.Join("stages", unattendedInstructions+".md"), "", "")
	if err != nil {
		return nil, err
	}

	p.unattended = string(unattended)

	retro, err := optionalFile(fsys, path.Join("stages", retrospectiveInstructions+".md"), "", "")
	if err != nil {
		return nil, err
	}

	p.retro = string(retro)

	if p.retro != "" {
		if err := p.loadPersona(fsys, retrospectiveInstructions, RetroPersona); err != nil {
			return nil, err
		}
	}

	return p, nil
}

// loadPersona reads a persona's text once, for the stage that names it.
func (p *Pack) loadPersona(fsys fs.FS, stageID, name string) error {
	if _, ok := p.personas[name]; ok {
		return nil
	}

	text, err := fs.ReadFile(fsys, path.Join("personas", name+".md"))
	if err != nil {
		return fmt.Errorf("pack: stage %q: persona %q: %w", stageID, name, err)
	}

	p.personas[name] = string(text)

	return nil
}

// optionalFile reads name from the pack; a missing file is empty unless
// stageID names a stage that needs it, for the reason why.
func optionalFile(fsys fs.FS, name, stageID, why string) ([]byte, error) {
	raw, err := fs.ReadFile(fsys, name)

	switch {
	case err == nil:
		return raw, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("pack: read %s: %w", name, err)
	case stageID != "":
		return nil, fmt.Errorf("pack: stage %q %s, so the pack needs %s", stageID, why, name)
	default:
		return nil, nil
	}
}

// parseLists decodes lists.yaml; an empty file is empty lists.
func parseLists(raw []byte) (contract.Lists, error) {
	var lists contract.Lists

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&lists); err != nil && !errors.Is(err, io.EOF) {
		return contract.Lists{}, fmt.Errorf("pack: parse lists.yaml: %w", err)
	}

	for _, pat := range lists.ForbiddenReferences {
		if _, err := regexp.Compile(pat.Pattern); err != nil {
			return contract.Lists{}, fmt.Errorf("pack: lists.yaml: forbidden reference %q: %w", pat.Name, err)
		}
	}

	for _, phrase := range lists.DanglingReferences {
		if strings.TrimSpace(phrase) == "" {
			return contract.Lists{}, errors.New("pack: lists.yaml: a dangling_references entry is blank")
		}
	}

	seen := map[string]bool{}

	for _, edge := range lists.EdgeCases {
		key := strings.ToLower(strings.TrimSpace(edge))

		switch {
		case key == "":
			return contract.Lists{}, errors.New("pack: lists.yaml: an edge_cases entry is blank")
		case seen[key]:
			return contract.Lists{}, fmt.Errorf("pack: lists.yaml: edge_cases lists %q twice", edge)
		}

		seen[key] = true
	}

	return lists, nil
}

func validateStage(s Stage, seen map[string]Stage) error {
	if s.ID == "" {
		return fmt.Errorf("pack: stage needs an id: %+v", s)
	}

	if slices.Contains([]string{reviewInstructions, responderInstructions, plainInstructions, unattendedInstructions, mockupResponderInstructions, retrospectiveInstructions}, s.ID) {
		return fmt.Errorf("pack: stage id %q is reserved for the %s instructions", s.ID, s.ID)
	}

	// A reviewer's verdicts are artifacts of <stage>.review.
	if strings.Contains(s.ID, ".") {
		return fmt.Errorf("pack: stage id %q has a dot, which names a stage's review", s.ID)
	}

	if strings.Contains(s.ID, stepSeparator) {
		return fmt.Errorf("pack: stage id %q has an %s, which joins a per-slice stage's id and its slice", s.ID, stepSeparator)
	}

	if _, dup := seen[s.ID]; dup {
		return fmt.Errorf("pack: duplicate stage %q", s.ID)
	}

	if s.Persona == "" {
		return fmt.Errorf("pack: stage %q needs a persona", s.ID)
	}

	if err := validateDialog(s); err != nil {
		return err
	}

	for _, c := range s.Consumes {
		if _, ok := seen[c]; !ok {
			return fmt.Errorf("pack: stage %q consumes %q, which is not an earlier stage", s.ID, c)
		}
	}

	for _, tool := range s.Tools {
		if tool != ToolCode && tool != ToolCM && tool != ToolResearch {
			return fmt.Errorf("pack: stage %q: unknown tool group %q", s.ID, tool)
		}
	}

	for _, check := range s.Checks {
		if !slices.Contains([]string{CheckCards, CheckAssumptions, CheckDecisions, CheckSlices, CheckCriteria, CheckOutcomes, CheckReadiness, CheckScreen, CheckMockup}, check) {
			return fmt.Errorf("pack: stage %q: unknown check %q", s.ID, check)
		}
	}

	// The card contract reads the target projects and their checkouts, which
	// only a stage with the code tools has.
	if slices.Contains(s.Checks, CheckCards) && !slices.Contains(s.Tools, ToolCode) {
		return fmt.Errorf("pack: stage %q checks cards, so it needs tools: [code]", s.ID)
	}

	if s.LoopsTo != "" {
		_, ok := seen[s.LoopsTo]
		if !ok {
			return fmt.Errorf("pack: stage %q loops to %q, which is not an earlier stage", s.ID, s.LoopsTo)
		}

		if s.MaxRounds <= 0 {
			return fmt.Errorf("pack: stage %q: loops_to needs a positive max_rounds", s.ID)
		}
	}

	return nil
}

// validateSlices checks the keys that repeat stages per slice: per-slice
// stages are contiguous, follow the stage that checks slices, and neither
// fold nor loop; earlier_slices names per-slice stages only; a shared stage
// has a gate and sits between the slicing stage and the per-slice stages;
// the stages that check cards and criteria both repeat per slice or neither
// does.
func validateSlices(stages []Stage) error {
	slicing, first, last := -1, -1, -1

	for i, s := range stages {
		if slicing < 0 && slices.Contains(s.Checks, CheckSlices) {
			slicing = i
		}

		if !s.PerSlice {
			continue
		}

		if last >= 0 && last != i-1 {
			return fmt.Errorf("pack: per-slice stage %q is apart from the other per-slice stages; they run together, once per slice", s.ID)
		}

		if first < 0 {
			first = i
		}

		last = i
	}

	for i, s := range stages {
		switch {
		case s.PerSlice && s.Shared:
			return fmt.Errorf("pack: stage %q is both per_slice and shared", s.ID)
		case s.PerSlice && (slicing < 0 || i <= slicing):
			return fmt.Errorf("pack: per-slice stage %q needs an earlier stage that checks slices", s.ID)
		case s.PerSlice && s.Fold != nil:
			return fmt.Errorf("pack: per-slice stage %q cannot fold", s.ID)
		case s.PerSlice && s.LoopsTo != "":
			return fmt.Errorf("pack: per-slice stage %q cannot loop; a stage after the per-slice stages loops to one", s.ID)
		case len(s.EarlierSlices) > 0 && !s.PerSlice:
			return fmt.Errorf("pack: stage %q reads earlier slices but is not per_slice", s.ID)
		case s.Shared && !s.Gate:
			return fmt.Errorf("pack: shared stage %q needs a gate", s.ID)
		case s.Shared && (slicing < 0 || i <= slicing || first < 0 || i > first):
			return fmt.Errorf("pack: shared stage %q sits between the stage that checks slices and the per-slice stages", s.ID)
		}

		for _, id := range s.EarlierSlices {
			if !slices.ContainsFunc(stages, func(t Stage) bool { return t.ID == id && t.PerSlice }) {
				return fmt.Errorf("pack: stage %q reads %q of earlier slices, which is not a per-slice stage", s.ID, id)
			}
		}
	}

	// The cards check reads the criteria of the cards' own scope, one slice's
	// or the whole blueprint's, so both stages share it.
	cards := slices.IndexFunc(stages, func(s Stage) bool { return slices.Contains(s.Checks, CheckCards) })
	criteria := slices.IndexFunc(stages, func(s Stage) bool { return slices.Contains(s.Checks, CheckCriteria) })

	if cards >= 0 && criteria >= 0 && stages[cards].PerSlice != stages[criteria].PerSlice {
		return fmt.Errorf("pack: stage %q checks cards and stage %q checks criteria; both repeat per slice or neither does",
			stages[cards].ID, stages[criteria].ID)
	}

	return nil
}

// validateDialog checks the keys that decide who answers a stage and how its
// gate is decided.
func validateDialog(s Stage) error {
	switch s.Policy {
	case "", PolicyHuman, PolicyAuto:
	default:
		return fmt.Errorf("pack: stage %q: unknown policy %q", s.ID, s.Policy)
	}

	switch s.GatePolicy {
	case "", PolicyHuman, PolicyAuto:
	default:
		return fmt.Errorf("pack: stage %q: unknown gate_policy %q", s.ID, s.GatePolicy)
	}

	if s.GatePolicy != "" && !s.Gate {
		return fmt.Errorf("pack: stage %q has a gate_policy but no gate", s.ID)
	}

	if s.MaxQuestions < 0 {
		return fmt.Errorf("pack: stage %q: max_questions cannot be negative", s.ID)
	}

	if s.Gate && s.LoopsTo != "" {
		return fmt.Errorf("pack: stage %q loops to %q, so its verdict decides what follows and it takes no gate", s.ID, s.LoopsTo)
	}

	if s.Gate && s.Reviewer == nil {
		return fmt.Errorf("pack: stage %q has a gate, so it needs a reviewer to decide it under the auto policy", s.ID)
	}

	if r := s.Reviewer; r != nil {
		switch {
		case !s.Gate:
			return fmt.Errorf("pack: stage %q has a reviewer but no gate for it to judge", s.ID)
		case r.Persona == "":
			return fmt.Errorf("pack: stage %q: the reviewer needs a persona", s.ID)
		case r.Threshold < 1 || r.Threshold > 5:
			return fmt.Errorf("pack: stage %q: the reviewer's threshold must be 1 to 5", s.ID)
		}

		for _, tool := range r.Tools {
			if tool != ToolCode && tool != ToolResearch {
				return fmt.Errorf("pack: stage %q: the reviewer's tool group %q is not code or research", s.ID, tool)
			}
		}
	}

	if f := s.Fold; f != nil {
		switch {
		case !s.Gate:
			return fmt.Errorf("pack: stage %q folds a gate it does not have", s.ID)
		case strings.TrimSpace(f.Field) == "":
			return fmt.Errorf("pack: stage %q: fold needs the data field it counts", s.ID)
		case f.Max < 1:
			return fmt.Errorf("pack: stage %q: fold max must be at least 1", s.ID)
		}
	}

	return nil
}

// validateMockup holds the mockup stage to one plain run: no gate, reviewer,
// fold, loop or slices, after the one stage that states the screen and before
// the stage that checks slices.
func validateMockup(stages []Stage) error {
	has := func(check string) []int {
		var at []int

		for i, s := range stages {
			if slices.Contains(s.Checks, check) {
				at = append(at, i)
			}
		}

		return at
	}

	mockups, screens := has(CheckMockup), has(CheckScreen)

	if len(screens) > 1 {
		return fmt.Errorf("pack: only one stage may check %s, not %q and %q", CheckScreen, stages[screens[0]].ID, stages[screens[1]].ID)
	}

	if len(mockups) > 1 {
		return fmt.Errorf("pack: only one stage may check %s, not %q and %q", CheckMockup, stages[mockups[0]].ID, stages[mockups[1]].ID)
	}

	if len(mockups) == 0 {
		return nil
	}

	i := mockups[0]
	s := stages[i]

	var key string

	switch {
	case s.Gate:
		key = "gate"
	case s.Reviewer != nil:
		key = "reviewer"
	case s.Fold != nil:
		key = "fold"
	case s.LoopsTo != "":
		key = "loops_to"
	case s.PerSlice:
		key = "per_slice"
	case s.Shared:
		key = "shared"
	}

	if key != "" {
		return fmt.Errorf("pack: mockup stage %q has no gate, reviewer, fold, loop or slices, but sets %s", s.ID, key)
	}

	if len(screens) == 0 || screens[0] >= i {
		return fmt.Errorf("pack: mockup stage %q needs an earlier stage that checks %s", s.ID, CheckScreen)
	}

	if slicing := slices.IndexFunc(stages, func(t Stage) bool { return slices.Contains(t.Checks, CheckSlices) }); slicing >= 0 && i >= slicing {
		return fmt.Errorf("pack: mockup stage %q must sit before the stage that checks slices", s.ID)
	}

	return nil
}

// SlicingStage is the id of the first stage that checks slices, whose newest
// version decides how often the per-slice stages run; "" when the pack has
// none.
func (p *Pack) SlicingStage() string {
	for _, s := range p.Stages {
		if slices.Contains(s.Checks, CheckSlices) {
			return s.ID
		}
	}

	return ""
}

// Stage returns the stage with id.
func (p *Pack) Stage(id string) (Stage, bool) {
	for _, s := range p.Stages {
		if s.ID == id {
			return s, true
		}
	}

	return Stage{}, false
}

// Persona returns a persona's text; empty when unknown.
func (p *Pack) Persona(name string) string { return p.personas[name] }

// Personas returns the names of every persona a run can use, sorted: each
// stage's, each reviewer's and the retrospective's.
func (p *Pack) Personas() []string { return slices.Sorted(maps.Keys(p.personas)) }

// Instructions returns a stage's instructions; empty when unknown.
func (p *Pack) Instructions(stageID string) string { return p.instructions[stageID] }

// Lists returns the data behind the mechanical checks.
func (p *Pack) Lists() contract.Lists { return p.lists }

// RetroInstructions returns what every retrospective run reads; empty when
// the pack has none.
func (p *Pack) RetroInstructions() string { return p.retro }

// MergeLists returns the pack's lists with each overlay's additions after
// the pack's entries, in the order given. An edge case already held, in
// other case or spacing, is left out. The result is checked by the loader's
// own list parser, whose error is returned unchanged.
func (p *Pack) MergeLists(overlays []contract.Overlay) (contract.Lists, error) {
	merged := p.lists
	merged.EdgeCases = slices.Clone(p.lists.EdgeCases)
	merged.DanglingReferences = slices.Clone(p.lists.DanglingReferences)
	merged.ForbiddenReferences = slices.Clone(p.lists.ForbiddenReferences)

	seen := map[string]bool{}
	for _, e := range merged.EdgeCases {
		seen[strings.ToLower(strings.TrimSpace(e))] = true
	}

	for _, o := range overlays {
		merged.DanglingReferences = append(merged.DanglingReferences, o.DanglingReferences...)
		merged.ForbiddenReferences = append(merged.ForbiddenReferences, o.ForbiddenReferences...)

		for _, e := range o.EdgeCases {
			k := strings.ToLower(strings.TrimSpace(e))
			if seen[k] {
				continue
			}

			seen[k] = true

			merged.EdgeCases = append(merged.EdgeCases, e)
		}
	}

	raw, err := yaml.Marshal(merged)
	if err != nil {
		return contract.Lists{}, fmt.Errorf("pack: encode merged lists: %w", err)
	}

	return parseLists(raw)
}

// CardTemplate returns the card body template handed to decomposition.
func (p *Pack) CardTemplate() string { return p.card }

// ReviewInstructions returns what every reviewer run reads; empty when no
// stage declares a reviewer and the pack carries none.
func (p *Pack) ReviewInstructions() string { return p.review }

// ResponderInstructions returns what every responder run reads; empty when
// the pack has none, and then an auto question returns to its stage.
func (p *Pack) ResponderInstructions() string { return p.responder }

// MockupStage is the id of the stage that checks mockups; "" when the pack
// has none.
func (p *Pack) MockupStage() string { return p.checking(CheckMockup) }

// ScreenStage is the id of the stage that states whether the need has a
// screen; "" when the pack has none.
func (p *Pack) ScreenStage() string { return p.checking(CheckScreen) }

func (p *Pack) checking(check string) string {
	for _, s := range p.Stages {
		if slices.Contains(s.Checks, check) {
			return s.ID
		}
	}

	return ""
}

// MockupResponderInstructions returns what every mockup responder run reads;
// empty when the pack has no mockup stage.
func (p *Pack) MockupResponderInstructions() string { return p.mockupResp }

// PlainInstructions are added to every stage, responder and reviewer run of a
// plain-language blueprint; empty when the pack has none.
func (p *Pack) PlainInstructions() string { return p.plain }

// UnattendedInstructions are added to every stage run of an unattended
// blueprint; empty when the pack has none.
func (p *Pack) UnattendedInstructions() string { return p.unattended }

// StageName is how a stage id or a step key reads to a person: underscores
// become spaces and the first letter is upper case. A per-slice step names
// its slice, as in "Solution design (SL2)", only when sliced says the
// blueprint has two or more slices; with one, nothing slice-related shows.
// A pack has no title field, and a raw id is what a typed command, the
// record and the models keep.
func StageName(id string, sliced bool) string {
	stageID, slice := SplitStep(id)

	words := strings.ReplaceAll(stageID, "_", " ")
	if words == "" {
		return ""
	}

	r, size := utf8.DecodeRuneInString(words)
	name := string(unicode.ToUpper(r)) + words[size:]

	if sliced && slice != "" {
		name += " (" + slice + ")"
	}

	return name
}
