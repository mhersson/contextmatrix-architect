// Package pack loads a pipeline pack: the stage list, the persona texts and
// the per-stage instructions. The default pack is embedded in the binary.
package pack

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed default
var defaultFS embed.FS

// Stage is one unit of AI work in the pipeline.
type Stage struct {
	ID       string `yaml:"id"`
	Persona  string `yaml:"persona"`
	Produces string `yaml:"produces"`
	// Gate is the default responder policy after the stage: "questions"
	// (no gate, questions only), "human" or "auto".
	Gate string `yaml:"gate"`
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
		Name: m.Name, Version: m.Version, Stages: m.Stages,
		personas: map[string]string{}, instructions: map[string]string{},
	}

	for _, s := range m.Stages {
		if s.ID == "" || s.Persona == "" {
			return nil, fmt.Errorf("pack: stage needs id and persona: %+v", s)
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

	return p, nil
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
