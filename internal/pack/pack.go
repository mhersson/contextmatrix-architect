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
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
)

//go:embed default
var defaultFS embed.FS

// Stage sources, tools and checks a pack.yaml may name.
const (
	SourceModel = "model"
	SourceHuman = "human"
	ToolCode    = "code"
	// ToolCM is the read-only ContextMatrix tool group.
	ToolCM     = "cm"
	CheckCards = "cards"
)

// Stage is one unit of work in the pipeline: a model run, or an artifact a
// human pastes.
type Stage struct {
	ID       string `yaml:"id"`
	Persona  string `yaml:"persona"`
	Produces string `yaml:"produces"`
	// Gate is the default responder policy after the stage: "questions"
	// (no gate, questions only), "human", "auto" or "none".
	Gate string `yaml:"gate"`
	// Source is "model" (default) or "human": a human-sourced stage waits
	// for the user's next message and stores it as the artifact.
	Source string `yaml:"source"`
	// Consumes lists earlier stages whose latest artifacts are this stage's
	// input, in the order they are presented.
	Consumes []string `yaml:"consumes"`
	// Tools names the tool groups the run gets: "code" is the read-only
	// checkout tools plus repo_info, "cm" the read-only ContextMatrix tools.
	Tools []string `yaml:"tools"`
	// Checks names the mechanical checks run on every submit; "cards" is the
	// card contract.
	Checks []string `yaml:"checks"`
	// LoopsTo names the earlier stage re-run when this stage's artifact
	// reports failures, at most MaxRounds times.
	LoopsTo   string `yaml:"loops_to"`
	MaxRounds int    `yaml:"max_rounds"`
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

		if s.Source == "" {
			s.Source = SourceModel
		}

		if err := validateStage(*s, seen); err != nil {
			return nil, err
		}

		// The controller launches the first stage on the need and runs every
		// other stage through the model unless its source is human, so a
		// human-sourced first stage would be run as a model stage.
		if i == 0 && s.Source == SourceHuman {
			return nil, fmt.Errorf("pack: stage %q: the first stage cannot be human-sourced", s.ID)
		}

		seen[s.ID] = *s

		if s.Source == SourceHuman {
			// A human-sourced stage has no persona; its instructions file,
			// when present, is the prompt the panel shows while waiting.
			text, err := fs.ReadFile(fsys, path.Join("stages", s.ID+".md"))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("pack: stage %q: instructions: %w", s.ID, err)
			}

			p.instructions[s.ID] = string(text)

			continue
		}

		if _, ok := p.personas[s.Persona]; !ok {
			text, err := fs.ReadFile(fsys, path.Join("personas", s.Persona+".md"))
			if err != nil {
				return nil, fmt.Errorf("pack: stage %q: persona %q: %w", s.ID, s.Persona, err)
			}

			p.personas[s.Persona] = string(text)
		}

		text, err := fs.ReadFile(fsys, path.Join("stages", s.ID+".md"))
		if err != nil {
			return nil, fmt.Errorf("pack: stage %q: instructions: %w", s.ID, err)
		}

		p.instructions[s.ID] = string(text)
	}

	p.Stages = m.Stages

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

	raw, err = optionalFile(fsys, "lists.yaml", checker)
	if err != nil {
		return nil, err
	}

	if p.lists, err = parseLists(raw); err != nil {
		return nil, err
	}

	card, err := optionalFile(fsys, "card.md", checker)
	if err != nil {
		return nil, err
	}

	p.card = string(card)

	return p, nil
}

// optionalFile reads name from the pack; a missing file is empty unless
// checker names a stage that checks cards, which needs it.
func optionalFile(fsys fs.FS, name, checker string) ([]byte, error) {
	raw, err := fs.ReadFile(fsys, name)

	switch {
	case err == nil:
		return raw, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("pack: read %s: %w", name, err)
	case checker != "":
		return nil, fmt.Errorf("pack: stage %q checks cards, so the pack needs %s", checker, name)
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

	return lists, nil
}

func validateStage(s Stage, seen map[string]Stage) error {
	if s.ID == "" {
		return fmt.Errorf("pack: stage needs an id: %+v", s)
	}

	if _, dup := seen[s.ID]; dup {
		return fmt.Errorf("pack: duplicate stage %q", s.ID)
	}

	switch s.Source {
	case SourceModel:
		if s.Persona == "" {
			return fmt.Errorf("pack: stage %q needs a persona", s.ID)
		}
	case SourceHuman:
		// The panel waits for the user at a human-sourced stage, so nothing
		// a model run would use applies; refuse rather than ignore it.
		if len(s.Tools) > 0 || len(s.Checks) > 0 || s.LoopsTo != "" {
			return fmt.Errorf("pack: stage %q is human-sourced, so it takes no tools, checks or loops_to", s.ID)
		}
	default:
		return fmt.Errorf("pack: stage %q: unknown source %q", s.ID, s.Source)
	}

	switch s.Gate {
	case "", "questions", "human", "auto", "none":
	default:
		return fmt.Errorf("pack: stage %q: unknown gate %q", s.ID, s.Gate)
	}

	for _, c := range s.Consumes {
		if _, ok := seen[c]; !ok {
			return fmt.Errorf("pack: stage %q consumes %q, which is not an earlier stage", s.ID, c)
		}
	}

	for _, tool := range s.Tools {
		if tool != ToolCode && tool != ToolCM {
			return fmt.Errorf("pack: stage %q: unknown tool group %q", s.ID, tool)
		}
	}

	for _, check := range s.Checks {
		if check != CheckCards {
			return fmt.Errorf("pack: stage %q: unknown check %q", s.ID, check)
		}
	}

	// The card contract reads the target projects and their checkouts, which
	// only a stage with the code tools has.
	if slices.Contains(s.Checks, CheckCards) && !slices.Contains(s.Tools, ToolCode) {
		return fmt.Errorf("pack: stage %q checks cards, so it needs tools: [code]", s.ID)
	}

	if s.LoopsTo != "" {
		target, ok := seen[s.LoopsTo]
		if !ok {
			return fmt.Errorf("pack: stage %q loops to %q, which is not an earlier stage", s.ID, s.LoopsTo)
		}

		if target.Source == SourceHuman {
			return fmt.Errorf("pack: stage %q loops to %q, which is human-sourced and would run through the model", s.ID, s.LoopsTo)
		}

		if s.MaxRounds <= 0 {
			return fmt.Errorf("pack: stage %q: loops_to needs a positive max_rounds", s.ID)
		}
	}

	return nil
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

// Instructions returns a stage's instructions; empty when unknown.
func (p *Pack) Instructions(stageID string) string { return p.instructions[stageID] }

// Lists returns the data behind the mechanical checks.
func (p *Pack) Lists() contract.Lists { return p.lists }

// CardTemplate returns the card body template handed to decomposition.
func (p *Pack) CardTemplate() string { return p.card }
