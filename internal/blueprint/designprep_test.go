package blueprint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	designpkg "github.com/mhersson/contextmatrix-architect/internal/design"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

const (
	// designTokenValue is the fake material's one token value: a run whose
	// task carries it was given the brief.
	designTokenValue = "#0a7f5a"
	designPkgDir     = "_npm+@acme+ui@1.2.3"
	designPkgFile    = designPkgDir + "/style/core.css"
)

// fakeDesigner returns material and err, writing the material's package
// files under the tool root as a gather keeps them, and records each request.
type fakeDesigner struct {
	material designpkg.Material
	err      error

	mu   sync.Mutex
	reqs []designpkg.Request
}

func (f *fakeDesigner) Gather(_ context.Context, req designpkg.Request) (designpkg.Material, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()

	for _, p := range f.material.Packages {
		for _, file := range p.Files {
			path := filepath.Join(req.Root, filepath.FromSlash(file))
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return designpkg.Material{}, err
			}

			if err := os.WriteFile(path, []byte(".acme-button {\ncolor: var(--acme-brand);\n}\n"), 0o600); err != nil {
				return designpkg.Material{}, err
			}
		}
	}

	return f.material, f.err
}

func (f *fakeDesigner) requests() []designpkg.Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]designpkg.Request(nil), f.reqs...)
}

// designMaterial is one frontend root in alpha with one token, reaching one
// kept package.
func designMaterial() designpkg.Material {
	return designpkg.Material{
		Roots: []designpkg.Root{{
			Project: "alpha", Path: "alpha",
			Tokens:   []designpkg.Token{{Name: "--acme-brand", Value: designTokenValue}},
			Packages: []string{designPkgDir},
		}},
		Packages: []designpkg.Package{{
			Name: "@acme/ui", Version: "1.2.3", Dir: designPkgDir, Registry: "registry.example.test", Verified: true,
			Files: []string{designPkgFile},
		}},
	}
}

// designScript has a screen, a designer that shows a revision on every run
// and the rest of the dialog pipeline.
func designScript() stageScript {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())

	return script
}

// newDesign is newMockup with d as the controller's designer; nil leaves
// the designer out.
func newDesign(t *testing.T, d Designer) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeWorkspace) {
	t.Helper()

	c, st, runner, rec, ws := newMockup(t, designScript())
	if d != nil {
		c.opts.Design = d
	}

	return c, st, runner, rec, ws
}

// packageSources are BP1's package sources.
func packageSources(t *testing.T, st *store.Store) []store.Source {
	t.Helper()

	all, err := st.ListSources(context.Background(), "BP1")
	require.NoError(t, err)

	var out []store.Source

	for _, s := range all {
		if s.Kind == "package" {
			out = append(out, s)
		}
	}

	return out
}

// mockupTask is the task of the mockup stage's first run.
func mockupTask(t *testing.T, runner *fakeRunner) string {
	t.Helper()

	runs := inputsOf(runner, "mockup")
	require.NotEmpty(t, runs)

	return runs[0].Task
}

func TestDesign_TheDesignerGetsTheBrief(t *testing.T) {
	d := &fakeDesigner{material: designMaterial()}
	c, st, runner, _, ws := newDesign(t, d)
	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, nil)

	srcs := packageSources(t, st)
	require.Len(t, srcs, 1)

	task := mockupTask(t, runner)
	assert.True(t, strings.HasPrefix(task, designpkg.Brief(designMaterial(), map[string]string{designPkgDir: srcs[0].Tag()}, stage.WindowBytes(0))),
		"the brief opens the task, before the checkout lines:\n%s", task)
	assert.Contains(t, task, designTokenValue)
	assert.Contains(t, task, "sha-alpha", "the checkout lines follow it")

	assert.Equal(t, []designpkg.Request{{Root: ws.TreeRoot("BP1"), Trees: []designpkg.Tree{{Project: "alpha", Dir: "alpha"}}}}, d.requests())

	require.Contains(t, stagesRun(runner), "solution_design")

	for i := range runner.inputs {
		if in := runner.input(i); in.Stage != "mockup" {
			assert.NotContains(t, in.Task, designTokenValue, "%s is given the brief", in.Stage)
		}
	}
}

func TestDesign_TheBriefFitsTheRunsWindow(t *testing.T) {
	// Four 10 KiB frame files: a 128k window carries three of them, a 64k one two.
	material := designMaterial()
	for _, letter := range []string{"a", "b", "c", "d"} {
		material.Roots[0].Frame = append(material.Roots[0].Frame, designpkg.Asset{
			Path: "alpha/src/" + letter + ".tsx", Size: 10 << 10, Text: strings.Repeat(letter, 10<<10),
		})
	}

	tests := []struct {
		name     string
		window   int
		override string
		third    bool
	}{
		{"an unknown window is the default", 0, "", true},
		{"a 64k window", 64_000, "", false},
		{"an overridden model's window is unknown", 64_000, "other/model", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, _, _ := newDesign(t, &fakeDesigner{material: material})
			if tt.override != "" {
				c.opts.Models = map[string]string{"designer": tt.override}
			}

			in := startInput("BP1")
			in.ContextWindow = tt.window
			driveIn(t, c, in)
			takeOffer(t, c)

			srcs := packageSources(t, st)
			require.Len(t, srcs, 1)

			window := tt.window
			if tt.override != "" {
				window = 0
			}

			task := mockupTask(t, runner)
			assert.True(t, strings.HasPrefix(task, designpkg.Brief(material, map[string]string{designPkgDir: srcs[0].Tag()}, stage.WindowBytes(window))),
				"the brief is sized by the run's window")
			assert.Contains(t, task, strings.Repeat("b", 10<<10))

			if tt.third {
				assert.Contains(t, task, strings.Repeat("c", 10<<10))
			} else {
				assert.NotContains(t, task, strings.Repeat("c", 100))
			}
		})
	}
}

func TestDesign_AnEmptyRepositoryIsNotGatheredFrom(t *testing.T) {
	d := &fakeDesigner{}
	c, _, _, _, ws := newDesign(t, d)
	ws.empty = map[string]bool{"alpha": true}

	drive(t, c, "BP1")
	takeOffer(t, c)

	reqs := d.requests()
	require.Len(t, reqs, 1)
	assert.Empty(t, reqs[0].Trees)
}

func TestDesign_PackagesAreSources(t *testing.T) {
	c, st, _, rec, _ := newDesign(t, &fakeDesigner{material: designMaterial()})
	drive(t, c, "BP1")
	takeOffer(t, c)

	srcs := packageSources(t, st)
	require.Len(t, srcs, 1)
	assert.Equal(t, "npm:@acme/ui", srcs[0].Identity)
	assert.Equal(t, "1.2.3", srcs[0].Version)
	assert.Equal(t, "mockup", srcs[0].FirstUsedBy)

	fetched := rec.all(EventFetched)
	require.Len(t, fetched, 1)
	assert.Equal(t, "mockup", fetched[0].Payload["stage"])

	pkgs, _ := fetched[0].Payload["packages"].([]any)
	require.Len(t, pkgs, 1)

	got, _ := pkgs[0].(map[string]any)
	assert.Equal(t, map[string]any{
		"tag": srcs[0].Tag(), "name": "@acme/ui", "version": "1.2.3", "registry": "registry.example.test", "verified": true,
	}, got)
}

func TestDesign_ARevisionReusesTheSource(t *testing.T) {
	d := &fakeDesigner{material: designMaterial()}
	c, st, runner, rec, _ := newDesign(t, d)
	drive(t, c, "BP1")
	takeOffer(t, c)
	chatMockup(t, c, "Make the key bar darker.", nil)

	require.Len(t, inputsOf(runner, "mockup"), 2, "the change request ran the stage again")
	assert.Len(t, d.requests(), 2, "the run gathered again")
	assert.Len(t, packageSources(t, st), 1)
	assert.Len(t, rec.all(EventFetched), 1)
}

// The run that kept the package, and every later code stage, which greps
// the whole tool root where the kept package still sits, tag its results
// there with the package's source.
func TestDesign_CodeStagesTagThePackage(t *testing.T) {
	c, st, runner, _, _ := newDesign(t, &fakeDesigner{material: designMaterial()})
	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, nil)

	srcs := packageSources(t, st)
	require.Len(t, srcs, 1)

	for _, stageID := range []string{"mockup", "solution_design"} {
		runs := inputsOf(runner, stageID)
		require.NotEmpty(t, runs, stageID)

		res, err := toolNamed(t, runs[0], "read").Execute(context.Background(), map[string]any{"path": designPkgFile})
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(res.Text, "["+srcs[0].Tag()+"] "), "%s: %s", stageID, res.Text)
		assert.Contains(t, res.Text, "acme-button")
	}
}

// The package dir sits under the tool root beside the checkouts, so a later
// code stage is told what it is.
func TestDesign_LaterCodeStagesAreToldThePackageDir(t *testing.T) {
	c, st, runner, _, _ := newDesign(t, &fakeDesigner{material: designMaterial()})
	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, nil)

	srcs := packageSources(t, st)
	require.Len(t, srcs, 1)

	runs := inputsOf(runner, "solution_design")
	require.NotEmpty(t, runs)

	assert.True(t, hasLineWith(runs[0].Task, designPkgDir+"/", "@acme/ui 1.2.3", "["+srcs[0].Tag()+"]"), runs[0].Task)
}

func TestDesign_ACodeReviewerTagsThePackage(t *testing.T) {
	c, st, runner, _, _ := newDesign(t, &fakeDesigner{material: designMaterial()})

	for i := range c.opts.Pack.Stages {
		if c.opts.Pack.Stages[i].ID == "solution_design" {
			c.opts.Pack.Stages[i].Reviewer.Tools = []string{pack.ToolCode}
		}
	}

	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, nil)

	srcs := packageSources(t, st)
	require.Len(t, srcs, 1)

	runs := inputsOf(runner, "solution_design.review")
	require.NotEmpty(t, runs)

	res, err := toolNamed(t, runs[0], "read").Execute(context.Background(), map[string]any{"path": designPkgFile})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Text, "["+srcs[0].Tag()+"] "), res.Text)
	assert.True(t, hasLineWith(runs[0].Task, designPkgDir+"/", "@acme/ui 1.2.3", "["+srcs[0].Tag()+"]"), runs[0].Task)
}

// hasLineWith reports whether one line of s holds every part.
func hasLineWith(s string, parts ...string) bool {
	for _, line := range strings.Split(s, "\n") {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			return true
		}
	}

	return false
}

func TestDesign_GatherFailureNeverFailsTheRun(t *testing.T) {
	// The material returned with the error is never used.
	d := &fakeDesigner{material: designMaterial(), err: errors.New("design: write package: disk full")}
	c, st, runner, rec, _ := newDesign(t, d)
	drive(t, c, "BP1")
	takeOffer(t, c)

	assert.Equal(t, photoRevision().Source, storedRevision(t, st, 1).Source, "the revision is shown")
	assert.NotContains(t, mockupTask(t, runner), designTokenValue)
	assert.Empty(t, packageSources(t, st))
	assert.Empty(t, rec.all(EventFetched))

	var failures []Event

	for _, e := range rec.all(EventError) {
		if e.Payload["reason"] == "design" {
			failures = append(failures, e)
		}
	}

	require.Len(t, failures, 1)
	assert.Equal(t, "mockup", failures[0].Payload["stage"])
	assert.NotEmpty(t, failures[0].Payload["message"])
}

// Without a designer, or on a stage without the design key, the task is the
// plain code stage's.
func TestDesign_NilDesignerLeavesTheTask(t *testing.T) {
	c, _, runner, _, _ := newDesign(t, nil)
	drive(t, c, "BP1")
	takeOffer(t, c)

	withoutDesigner := mockupTask(t, runner)

	d := &fakeDesigner{material: designMaterial()}
	c, _, runner, _, _ = newDesign(t, d)

	for i := range c.opts.Pack.Stages {
		if c.opts.Pack.Stages[i].ID == "mockup" {
			c.opts.Pack.Stages[i].Design = false
		}
	}

	drive(t, c, "BP1")
	takeOffer(t, c)

	withoutKey := mockupTask(t, runner)

	assert.NotContains(t, withoutDesigner, designTokenValue)
	assert.Equal(t, withoutKey, withoutDesigner)
	assert.Empty(t, d.requests(), "a stage without the design key gathers nothing")
}
