package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func mockupPack(t *testing.T) *pack.Pack {
	t.Helper()

	pk, err := pack.LoadDir("testdata/mockup")
	require.NoError(t, err)

	return pk
}

// newMockup is newDialog on the mockup test pack.
func newMockup(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeWorkspace) {
	t.Helper()

	c, st, runner, rec, ws := newPipeline(t, script)
	c.opts.Pack = mockupPack(t)

	return c, st, runner, rec, ws
}

// withScreen has intake state screen, and no re-design with a screen; nil
// leaves the keys out, as an intake stored before mockups did.
func withScreen(script stageScript, screen any) stageScript {
	data := map[string]any{}
	if screen != nil {
		data["screen"] = screen
	}

	if screen == true {
		data["redesign"] = false
	}

	script["intake"] = returns("# Need", data)

	return script
}

func TestMockup_ANeedWithoutAScreenNeverShowsTheStep(t *testing.T) {
	for name, screen := range map[string]any{"no screen": false, "an intake from before screens": nil} {
		t.Run(name, func(t *testing.T) {
			c, st, runner, rec, _ := newMockup(t, withScreen(dialogScript(), screen))
			drive(t, c, "BP1")

			assert.NotContains(t, stagesRun(runner), "mockup")
			assert.Contains(t, stagesRun(runner), "slicing")
			assert.Equal(t, "skipped", stepOf(t, view(t, c), "mockup").Status)

			gates, err := st.ListGates(context.Background(), "BP1", "mockup")
			require.NoError(t, err)
			assert.Empty(t, gates, "the collapse writes no fold row")

			for _, e := range rec.events() {
				assert.NotEqual(t, "mockup", e.Payload["stage"], "without a word: %s", e.Kind)
			}

			plans := rec.all(EventGatePlan)
			require.NotEmpty(t, plans)

			for _, plan := range plans {
				entries, _ := plan.Payload["stages"].([]any)
				require.NotEmpty(t, entries)

				for _, e := range entries {
					assert.NotEqual(t, "mockup", e.(map[string]any)["stage"], "the plan lists the collapsed step")
				}
			}

			for i := range runner.inputs {
				assert.NotContains(t, runner.input(i).Task, "## mockup", "%s is told of a mockup", runner.input(i).Stage)
			}
		})
	}
}

func TestMockup_AScreenPutsTheStepInThePipeline(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{}, errors.New("model down")
	}

	// auto mode takes no offer, so the step runs at once.
	c, _, runner, _, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Contains(t, stagesRun(runner), "mockup")
	assert.NotContains(t, stagesRun(runner), "slicing")
	assert.Equal(t, "failed", stepOf(t, view(t, c), "mockup").Status)
}

func TestMockup_IntakeMustSayWhetherThereIsAScreen(t *testing.T) {
	script := withScreen(dialogScript(), false)
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if err := in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"screen": "yes"}}); err == nil || !strings.Contains(err.Error(), "screen_shape") {
			return stage.Output{}, fmt.Errorf("a screen that is not a boolean passed: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"screen": true}}); err == nil || !strings.Contains(err.Error(), "redesign_shape") {
			return stage.Output{}, fmt.Errorf("a screen without the re-design flag passed: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"screen": false}}); err != nil {
			return stage.Output{}, fmt.Errorf("no screen needs no re-design flag: %w", err)
		}

		return done("# Need", map[string]any{"screen": false}), nil
	}

	c, _, runner, _, _ := newMockup(t, script)
	drive(t, c, "BP1")

	assert.Contains(t, stagesRun(runner), "slicing", "intake's run went on after the refusal")
}

// showing is a designer whose n-th run shows revs[n-1], its last one again
// after that, and ends asked.
func showing(revs ...stage.MockupRevision) func(context.Context, stage.Input) (stage.Output, error) {
	var n atomic.Int32

	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		i := min(int(n.Add(1)), len(revs)) - 1
		if _, err := in.Designer.Show(ctx, revs[i]); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
}

// photoSource imitates a full-screen terminal interface for renaming
// photos: a file list and a key bar.
const photoSource = `<!doctype html><html><head><style>body{margin:0;background:#111;color:#ddd;font-family:monospace}
#bar{position:fixed;bottom:0;width:100%;background:#335;color:#fff}</style></head>
<body><pre id="out"></pre><div id="bar">F2 rename  Space mark  q quit</div><img src="data:image/png;base64,iVBORw0KGgo=" alt=""><svg width="8" height="8"></svg><script>
function draw(s){document.getElementById("out").textContent = s.preview === "on" ? "[x] IMG_0001.jpg -> beach-001.jpg" : "[x] IMG_0001.jpg"}
draw(window.mockupSettings || {}); window.addEventListener("mockup:settings", function(e){ draw(e.detail) });
</script></body></html>`

// photoRevision is a full-screen terminal interface for renaming photos.
func photoRevision() stage.MockupRevision {
	return stage.MockupRevision{
		Notes:   "A dry run lists every new name first.",
		Source:  photoSource,
		Screens: []stage.MockupScreen{{ID: "run", Title: "The run"}},
		Suggestions: []stage.MockupSuggestion{
			{ID: "preview", Title: "Show a dry run", Kind: stage.MockupSwitch, Why: "See the names before anything moves.", Default: "off"},
			{
				ID: "order", Title: "Number by", Kind: stage.MockupChoice, Why: "Dates sort as taken.", Default: "date",
				Options: []stage.MockupOption{{Value: "date", Label: "Date taken"}, {Value: "name", Label: "Old name"}},
			},
		},
	}
}

// listSource imitates a shared shopping list on a phone.
const listSource = `<!doctype html><html><head><style>body{margin:0;font-family:system-ui;max-width:390px}
.row{padding:12px;border-bottom:1px solid #ddd}.card{margin:8px;padding:16px;border-radius:12px;box-shadow:0 1px 3px #0003}</style></head>
<body><h1>Groceries</h1><div id="list"></div><script>
function draw(s){var c = s.layout === "cards" ? "card" : "row"; var items = s.group === "on" ? ["Dairy: milk", "Bakery: bread"] : ["milk", "bread"];
document.getElementById("list").innerHTML = items.map(function(i){ return "<div class=" + c + ">" + i + "</div>" }).join("")}
draw(window.mockupSettings || {}); window.addEventListener("mockup:settings", function(e){ draw(e.detail) });
</script></body></html>`

// listRevision is a shared shopping list on a phone.
func listRevision() stage.MockupRevision {
	return stage.MockupRevision{
		Notes:   "One list the household shares, ticked off in the shop.",
		Source:  listSource,
		Screens: []stage.MockupScreen{{ID: "list", Title: "The list"}},
		Suggestions: []stage.MockupSuggestion{
			{
				ID: "layout", Title: "List style", Kind: stage.MockupChoice, Why: "Rows fit more on screen; cards are easier to tap.", Default: "rows",
				Options: []stage.MockupOption{{Value: "rows", Label: "Rows"}, {Value: "cards", Label: "Cards"}},
			},
			{ID: "group", Title: "Group by aisle", Kind: stage.MockupSwitch, Why: "Walk the shop once.", Default: "off"},
		},
	}
}

// storedRevision decodes version v of BP1's mockup.
func storedRevision(t *testing.T, st *store.Store, v int) mockupData {
	t.Helper()

	art, err := st.GetArtifact(context.Background(), "BP1", "mockup", v)
	require.NoError(t, err)

	var data mockupData
	require.NoError(t, json.Unmarshal(art.Data, &data))

	return data
}

// mockupArtifactEvents are the recorded artifact events of the mockup stage.
func mockupArtifactEvents(rec *recorder) []Event {
	var out []Event

	for _, e := range rec.all(EventArtifact) {
		if e.Payload["stage"] == "mockup" {
			out = append(out, e)
		}
	}

	return out
}

func TestMockup_TheDesignerShowsARevision(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())

	c, st, runner, rec, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)

	got := storedRevision(t, st, 1)
	assert.Equal(t, 1, got.Revision)
	assert.Equal(t, photoRevision().Source, got.Source)
	assert.Equal(t, photoRevision().Screens, got.Screens)
	assert.Equal(t, map[string]string{"preview": "off", "order": "date"}, got.Settings)
	assert.Nil(t, got.Chosen)

	art, err := st.LatestArtifact(context.Background(), "BP1", "mockup")
	require.NoError(t, err)
	assert.Equal(t, photoRevision().Notes, art.Body)

	v := view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, promptMockup, v.Open.Kind)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)
	assert.Equal(t, "current", refOf(t, v, "mockup", 1).Status)
	assert.NotContains(t, stagesRun(runner), "slicing")

	stored := mockupArtifactEvents(rec)
	require.Len(t, stored, 1)
	assert.Equal(t, "mockup", stored[0].Payload["stage"])
	assert.NotContains(t, stored[0].Payload, "data", "the source stays in the artifact row")
}

func TestMockup_TheRevisionIsNotDrafted(t *testing.T) {
	var drafted atomic.Bool

	script := withScreen(dialogScript(), true)
	show := showing(photoRevision())
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		drafted.Store(in.Draft != nil)

		return show(ctx, in)
	}

	c, _, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)

	assert.False(t, drafted.Load())
}

// A target project's checkout is what the designer reads the existing UI
// and its design material from.
func TestMockup_TheDesignerReadsTheProjectsCode(t *testing.T) {
	var tools atomic.Value

	script := withScreen(dialogScript(), true)
	show := showing(photoRevision())
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		tools.Store(toolNames(in))

		return show(ctx, in)
	}

	c, _, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)

	got, _ := tools.Load().([]string)
	assert.Contains(t, got, "repo_info")
	assert.Contains(t, got, "read")
	assert.Contains(t, got, "grep")
}

func TestCarriedSettings(t *testing.T) {
	sugs := listRevision().Suggestions // layout (rows|cards), group (switch)
	sugs = append(sugs, stage.MockupSuggestion{ID: "tabs", Kind: stage.MockupSwitch, Default: "on"})

	assert.Equal(t, map[string]string{"layout": "cards", "group": "off", "tabs": "on"},
		carriedSettings(sugs, map[string]string{"layout": "cards", "group": "off", "gone": "x"}))
	assert.Equal(t, map[string]string{"layout": "rows", "group": "off", "tabs": "on"},
		carriedSettings(sugs, map[string]string{"layout": "tiles"}), "a value that no longer exists takes the default")
}

// A message while the revision waits is chat about it; the designer's chat
// run answers with the next revision in the same mockup round.
func TestMockup_AChatRunShowsTheNextRevisionInTheSameRound(t *testing.T) {
	two := listRevision()
	two.Notes = "Each shop gets its own tab."
	two.Suggestions = append(two.Suggestions, stage.MockupSuggestion{ID: "shops", Title: "A tab per shop", Kind: stage.MockupSwitch, Default: "on"})

	var chats atomic.Int32

	script := withScreen(dialogScript(), true)
	show := showing(listRevision(), two)
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.Chat {
			chats.Add(1)
		}

		return show(ctx, in)
	}

	c, st, runner, rec, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)

	first := view(t, c).Open
	require.NotNil(t, first)

	require.NoError(t, c.Message(context.Background(), "BP1", "Can you add tabs for different shops?", "m3"))
	c.Wait("BP1")

	assert.Equal(t, int32(1), chats.Load(), "the chat run showed the revision")

	got := storedRevision(t, st, 2)
	assert.Equal(t, 2, got.Revision)
	assert.Equal(t, map[string]string{"layout": "rows", "group": "off", "shops": "on"}, got.Settings, "the settings carry over and the new suggestion opens at its default")

	v := view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, *first, *v.Open, "one mockup across its revisions")
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)
	assert.Equal(t, "superseded", refOf(t, v, "mockup", 1).Status)
	assert.Equal(t, "current", refOf(t, v, "mockup", 2).Status)
	assert.NotContains(t, stagesRun(runner), "slicing")

	assert.Len(t, mockupArtifactEvents(rec), 2)

	rounds, err := c.rounds(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, rounds, 1)

	s := rounds[0]
	assert.Equal(t, changeRevision, s.Change)
	require.NotNil(t, s.Mockup)
	assert.Equal(t, 2, s.Mockup.Revision)
	assert.Equal(t, got.Settings, s.Mockup.Opening)

	var from, texts []string
	for _, m := range s.Frame.Chat {
		from, texts = append(from, m.From), append(texts, m.Text)
	}

	assert.Equal(t, []string{"designer", "user", "designer"}, from)
	assert.Equal(t, []string{listRevision().Notes, "Can you add tabs for different shops?", two.Notes}, texts)
	assert.Equal(t, 3, s.Seen)
}

// Under auto the responder steers the mockup: the round is the responder's
// and no prompt waits for the person.
func TestMockup_UnderAutoTheRevisionWaitsForNoPerson(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())

	c, st, _, rec, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, 1, storedRevision(t, st, 1).Revision)

	var posted *roundState

	for _, e := range rec.all(EventRound) {
		var s roundState

		raw, err := json.Marshal(e.Payload)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &s))

		if s.Mockup != nil && s.Change == changePosted {
			posted = &s
		}
	}

	require.NotNil(t, posted, "the revision posted a mockup round")
	assert.True(t, posted.Frame.Responder)

	v := view(t, c)
	assert.Nil(t, v.Open, "no mockup prompt waits for the person")
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)
}

// A revision reaches the live stream as a stored artifact, never as a draft.
func TestMockup_ARevisionReachesTheStreamAsAStoredArtifact(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())

	c, _, _, _, _ := newMockup(t, script)
	c.live.interval = 10 * time.Millisecond
	drive(t, c, "BP1")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := c.Subscribe(ctx, "BP1")
	require.NoError(t, err)

	takeOffer(t, c)

	frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.Type == frameStored && f.Stage == "mockup" })
	assert.Equal(t, 1, frames[len(frames)-1].Version)

	for _, f := range frames {
		assert.NotEqual(t, frameDraft, f.Type, "the stage drafts nothing")
		assert.Nil(t, f.Draft)
	}
}

// A mockup run's end names the revision it showed and is marked a mockup's,
// so the transcript names no round the person never sees.
func TestMockup_ARunsEndNamesItsRevision(t *testing.T) {
	two := listRevision()
	two.Notes = "Each shop gets its own tab."

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(listRevision(), two)

	c, _, _, rec, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)
	require.NoError(t, c.Message(context.Background(), "BP1", "Can you add tabs for different shops?", "m3"))
	c.Wait("BP1")

	var revisions []any

	for _, e := range rec.all(EventStageEnded) {
		if e.Payload["stage"] != "mockup" {
			continue
		}

		assert.Equal(t, true, e.Payload["mockup"])
		revisions = append(revisions, e.Payload["revision"])
	}

	assert.Equal(t, []any{1, 2}, revisions)

	for _, e := range rec.all(EventStageStarted) {
		if e.Payload["stage"] == "mockup" {
			assert.Equal(t, true, e.Payload["mockup"])
		}
	}
}

// While a designer run writes the next revision nobody is asked: the step
// runs and no prompt is open, and the mockup prompt is back with the
// revision.
func TestMockup_NoPromptWaitsWhileTheDesignerWrites(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})

	two := listRevision()
	two.Notes = "Each shop gets its own tab."

	script := withScreen(dialogScript(), true)
	show := showing(listRevision(), two)
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.Chat {
			close(started)
			<-release
		}

		return show(ctx, in)
	}

	c, st, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)

	first := view(t, c).Open
	require.NotNil(t, first)
	assert.Equal(t, promptMockup, first.Kind)

	require.NoError(t, c.Message(context.Background(), "BP1", "Can you add tabs for different shops?", "m3"))
	<-started

	v := view(t, c)
	assert.Nil(t, v.Open, "no prompt while the revision is written")
	assert.Equal(t, "running", stepOf(t, v, "mockup").Status)

	close(release)
	c.Wait("BP1")

	assert.Equal(t, 2, storedRevision(t, st, 2).Revision)

	v = view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, *first, *v.Open)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)
}
