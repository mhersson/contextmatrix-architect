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

// bookingSource imitates a salon's booking screen on a tablet: the day's
// free times and a confirm step.
const bookingSource = `<!doctype html><html><head><style>body{margin:0;font-family:system-ui;width:1024px}
.slot{display:inline-block;margin:6px;padding:14px;border:1px solid #aaa;border-radius:8px}.row{padding:10px}</style></head>
<body><h1>Book a visit</h1><div id="day"></div><div id="confirm" hidden>Confirm your visit</div><script>
function draw(s){var c = s.slots === "grid" ? "slot" : "row"; var head = s.staff === "on" ? "<p>Stylist: anyone free</p>" : "";
document.getElementById("day").innerHTML = head + ["09:00", "09:30", "10:00"].map(function(t){ return "<div class=" + c + ">" + t + "</div>" }).join("")}
draw(window.mockupSettings || {}); window.addEventListener("mockup:settings", function(e){ draw(e.detail) });
</script></body></html>`

// bookingRevision is a salon's booking screen on a tablet.
func bookingRevision() stage.MockupRevision {
	return stage.MockupRevision{
		Notes:   "Pick a day, then a free time, then confirm.",
		Source:  bookingSource,
		Screens: []stage.MockupScreen{{ID: "day", Title: "The day"}, {ID: "confirm", Title: "Confirm"}},
		Suggestions: []stage.MockupSuggestion{
			{
				ID: "slots", Title: "Free times as", Kind: stage.MockupChoice, Why: "A grid shows the whole day at a glance.", Default: "list",
				Options: []stage.MockupOption{{Value: "list", Label: "A list"}, {Value: "grid", Label: "A grid"}},
			},
			{ID: "staff", Title: "Pick a stylist", Kind: stage.MockupSwitch, Why: "Regulars ask for the same stylist.", Default: "off"},
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
// and no prompt waits for the person while its turn runs.
func TestMockup_UnderAutoTheRevisionWaitsForNoPerson(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())
	script["mockup.responder"] = func(context.Context, stage.Input) (stage.Output, error) {
		close(started)
		<-release

		return done("", map[string]any{"use": true}), nil
	}

	c, st, _, rec, _ := newMockup(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "the responder's turn")

	t.Cleanup(func() {
		close(release)
		c.Wait("BP1")
	})

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
	assert.Equal(t, "running", stepOf(t, v, "mockup").Status, "the responder's run is in flight")
	require.NotNil(t, v.Current)
	assert.Equal(t, store.RunKindResponder, v.Current.Kind)
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

// mockupReply sends a reply of action to the open mockup at its newest
// revision and waits for what follows.
func mockupReply(t *testing.T, c *Controller, action, content, msgID string, settings map[string]string) {
	t.Helper()

	v := view(t, c)
	require.NotNil(t, v.Open)
	require.NoError(t, c.Mockup(context.Background(), "BP1", protocol.MockupReply{
		PromptID: v.Open.PromptID, Revision: stepOf(t, v, "mockup").LatestVersion, Action: action, Settings: settings,
	}, content, msgID))
	c.Wait("BP1")
}

// useMockup uses the open mockup's newest revision with settings.
func useMockup(t *testing.T, c *Controller, settings map[string]string) {
	t.Helper()

	mockupReply(t, c, protocol.MockupUse, "Use this design", "use-1", settings)
}

// chatMockup asks for a change on the open mockup, with the settings the
// person looks at.
func chatMockup(t *testing.T, c *Controller, text string, settings map[string]string) {
	t.Helper()

	mockupReply(t, c, protocol.MockupChat, text, "chat-1", settings)
}

// skipMockup skips the open mockup at its newest revision.
func skipMockup(t *testing.T, c *Controller) {
	t.Helper()

	mockupReply(t, c, protocol.MockupSkip, "Skip mockups", "skip-1", nil)
}

// lastRunOf is BP1's newest stage run of stageID.
func lastRunOf(t *testing.T, st *store.Store, stageID string) store.Run {
	t.Helper()

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	var (
		out   store.Run
		found bool
	)

	for _, r := range runs {
		if r.Kind == store.RunKindStage && r.Stage == stageID {
			out, found = r, true
		}
	}

	require.True(t, found, "no stage run of %s", stageID)

	return out
}

// mockupQuestions are BP1's questions rows of the mockup stage.
func mockupQuestions(t *testing.T, st *store.Store) []store.Question {
	t.Helper()

	rows, err := st.ListQuestions(context.Background(), "BP1")
	require.NoError(t, err)

	var out []store.Question

	for _, q := range rows {
		if q.Stage == "mockup" {
			out = append(out, q)
		}
	}

	return out
}

func TestCleanSettings(t *testing.T) {
	sugs := listRevision().Suggestions // layout (rows|cards), group (switch)
	fallback := map[string]string{"layout": "cards", "group": "off"}

	tests := []struct {
		name    string
		given   map[string]string
		want    map[string]string
		dropped int
	}{
		{"every value valid", map[string]string{"layout": "rows", "group": "on"}, map[string]string{"layout": "rows", "group": "on"}, 0},
		{"one left out takes the fallback", map[string]string{"group": "on"}, map[string]string{"layout": "cards", "group": "on"}, 0},
		{"an invalid value is dropped", map[string]string{"layout": "tiles", "group": "on"}, map[string]string{"layout": "cards", "group": "on"}, 1},
		{"an id the revision lacks is dropped", map[string]string{"gone": "x", "group": "on"}, map[string]string{"layout": "cards", "group": "on"}, 1},
		{"none given", nil, map[string]string{"layout": "cards", "group": "off"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dropped := cleanSettings(sugs, tt.given, fallback)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.dropped, dropped)
		})
	}
}

func TestMockup_UseRecordsTheChosenSettingsAndGoesOn(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(listRevision())

	c, st, runner, _, _ := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, map[string]string{"layout": "cards", "group": "on"})

	assert.Contains(t, stagesRun(runner), "slicing")

	gates, err := st.ListGates(ctx, "BP1", "mockup")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, 1, gates[0].Version)
	assert.Equal(t, store.ResponderHuman, gates[0].Responder)

	sugs := listRevision().Suggestions
	answers, options, contexts := map[string]string{}, map[string][]string{}, map[string]string{}

	for _, q := range mockupQuestions(t, st) {
		answers[q.Text], options[q.Text], contexts[q.Text] = q.Answer, q.Options, q.Context
		assert.Equal(t, store.ResponderHuman, q.Responder)
		assert.False(t, q.Assumption)
		assert.Equal(t, "use-1", q.MessageID)
	}

	assert.Equal(t, map[string]string{sugs[0].Title: "Cards", sugs[1].Title: "On"}, answers)
	assert.Equal(t, map[string][]string{sugs[0].Title: {"Rows", "Cards"}, sugs[1].Title: {"Off", "On"}}, options)
	assert.Equal(t, map[string]string{sugs[0].Title: sugs[0].Why, sugs[1].Title: sugs[1].Why}, contexts)

	art, err := c.Artifact(ctx, "BP1", "mockup", "", 1)
	require.NoError(t, err)

	var data mockupData
	require.NoError(t, json.Unmarshal(art.Data, &data))
	assert.Equal(t, map[string]string{"layout": "cards", "group": "on"}, data.Chosen)
	assert.Equal(t, listRevision().Source, data.Source)

	v := view(t, c)
	assert.Equal(t, "done", stepOf(t, v, "mockup").Status)
	ref := refOf(t, v, "mockup", 1)
	assert.Equal(t, "approved", ref.Status)
	assert.Equal(t, "you", ref.DecidedBy)

	settle := lastRunOf(t, st, "mockup")
	assert.Equal(t, store.RunDone, settle.Status)
	assert.Equal(t, mockupUsed, settle.Reason)
	assert.Empty(t, settle.Model)
	assert.Zero(t, settle.Usage.CostUSD)
}

func TestMockup_AChangeRequestMakesANewRevisionThatKeepsWhatStillApplies(t *testing.T) {
	two := listRevision()
	two.Suggestions = append(two.Suggestions, stage.MockupSuggestion{ID: "shops", Title: "A tab per shop", Kind: stage.MockupSwitch, Default: "on"})

	var chatTask atomic.Value

	script := withScreen(dialogScript(), true)
	show := showing(listRevision(), two)
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.Chat {
			chatTask.Store(in.Task)
		}

		return show(ctx, in)
	}

	c, st, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)
	chatMockup(t, c, "Can you add tabs for different shops?", map[string]string{"layout": "cards", "group": "on"})

	task, _ := chatTask.Load().(string)
	assert.Contains(t, task, "Can you add tabs for different shops?")
	assert.Contains(t, task, "Cards", "the chat run is told what the person is looking at")

	got := storedRevision(t, st, 2)
	assert.Equal(t, map[string]string{"layout": "cards", "group": "on", "shops": "on"}, got.Settings)
	assert.Equal(t, promptMockup, view(t, c).Open.Kind)
}

// A plain message while the mockup waits for the person is chat about it,
// with the settings unknown: those the architect last knew stay.
func TestMockup_APlainMessageIsChatWithTheSettingsUnknown(t *testing.T) {
	var (
		chats    atomic.Int32
		chatTask atomic.Value
	)

	script := withScreen(dialogScript(), true)
	first := showing(listRevision())
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !in.Chat {
			return first(ctx, in)
		}

		// The first chat is answered in words, so the round keeps the
		// person's settings beside the revision's opening ones.
		if chats.Add(1) == 1 {
			if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Grouped by aisle reads well on a phone."}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		chatTask.Store(in.Task)

		if _, err := in.Designer.Show(ctx, listRevision()); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	c, st, _, rec, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)

	sent := map[string]string{"layout": "cards", "group": "on"}
	chatMockup(t, c, "Group them, please.", sent)

	require.NoError(t, c.Message(context.Background(), "BP1", "Can the rows be taller?", "m4"))
	c.Wait("BP1")

	var chat *roundState

	for _, e := range rec.all(EventRound) {
		var s roundState

		raw, err := json.Marshal(e.Payload)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &s))

		if s.Change == changeChat {
			chat = &s
		}
	}

	require.NotNil(t, chat)
	require.NotNil(t, chat.Mockup, "the message went on the mockup round")
	assert.Equal(t, protocol.RoundChat{From: "user", Text: "Can the rows be taller?"}, chat.Frame.Chat[len(chat.Frame.Chat)-1])
	assert.NotEqual(t, chat.Mockup.Opening, chat.Mockup.Settings, "the person's settings differ from the revision's opening ones")
	assert.Equal(t, sent, chat.Mockup.Settings, "a plain message sends no settings, so the last known ones stay")

	task, _ := chatTask.Load().(string)
	assert.Contains(t, task, "Can the rows be taller?")
	assert.Equal(t, sent, storedRevision(t, st, 2).Settings)
}

// A blank chat stores nothing and says so, so the panel's working indicator
// clears.
func TestMockup_ABlankChatStoresNothingAndSaysSo(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(listRevision())

	c, _, runner, rec, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)

	rounds, errs, runs := len(rec.all(EventRound)), len(rec.all(EventError)), len(stagesRun(runner))

	open := view(t, c).Open
	require.NotNil(t, open)
	require.NoError(t, c.Mockup(context.Background(), "BP1", protocol.MockupReply{PromptID: open.PromptID, Revision: 1, Action: protocol.MockupChat}, " \n ", "blank-1"))
	c.Wait("BP1")

	assert.Len(t, rec.all(EventRound), rounds, "nothing was stored")
	assert.Len(t, rec.all(EventError), errs+1)
	assert.Equal(t, reasonEmptyChat, lastError(t, rec).Payload["reason"])
	assert.Equal(t, false, lastError(t, rec).Payload["during_run"])
	assert.Len(t, stagesRun(runner), runs, "no chat run")
	assert.Equal(t, promptMockup, view(t, c).Open.Kind)
}

func TestMockup_SkipAfterARevisionRecordsItRejected(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(listRevision())

	c, st, runner, _, _ := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)
	skipMockup(t, c)

	gates, err := st.ListGates(ctx, "BP1", "mockup")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateReject, gates[0].Decision)
	assert.Equal(t, 1, gates[0].Version)
	assert.Equal(t, store.ResponderHuman, gates[0].Responder)

	assert.Empty(t, mockupQuestions(t, st))
	assert.Contains(t, stagesRun(runner), "slicing")

	v := view(t, c)
	assert.Equal(t, "done", stepOf(t, v, "mockup").Status)
	ref := refOf(t, v, "mockup", 1)
	assert.Equal(t, "rejected", ref.Status)
	assert.Equal(t, "you", ref.DecidedBy)

	res, err := c.mockupResult(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, mockupSkipped, res.Outcome)
	assert.Equal(t, 1, res.Revision)
	assert.Equal(t, 1, res.Art.Version)

	art, err := c.Artifact(ctx, "BP1", "mockup", "", 1)
	require.NoError(t, err)

	var data mockupData
	require.NoError(t, json.Unmarshal(art.Data, &data))
	assert.Nil(t, data.Chosen)

	settle := lastRunOf(t, st, "mockup")
	assert.Equal(t, store.RunDone, settle.Status)
	assert.Equal(t, mockupSkipped, settle.Reason)
}

func TestMockup_AStaleReplyIsRefused(t *testing.T) {
	two := listRevision()
	two.Notes = "Each shop gets its own tab."

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(listRevision(), two)

	c, st, _, rec, _ := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)
	chatMockup(t, c, "Can you add tabs for different shops?", nil)

	open := view(t, c).Open
	require.NotNil(t, open)
	require.Equal(t, 2, stepOf(t, view(t, c), "mockup").LatestVersion)

	rounds := len(rec.all(EventRound))

	for _, action := range []string{protocol.MockupUse, protocol.MockupSkip} {
		require.NoError(t, c.Mockup(ctx, "BP1", protocol.MockupReply{PromptID: open.PromptID, Revision: 1, Action: action}, "Use this design", "old-"+action))
		c.Wait("BP1")

		assert.Equal(t, reasonStaleRevision, lastError(t, rec).Payload["reason"], action)

		v := view(t, c)
		require.NotNil(t, v.Open)
		assert.Equal(t, promptMockup, v.Open.Kind)
	}

	require.NoError(t, c.Mockup(ctx, "BP1", protocol.MockupReply{PromptID: "mockup:999", Revision: 2, Action: protocol.MockupUse}, "Use this design", "gone-1"))
	c.Wait("BP1")
	assert.Equal(t, reasonStaleReply, lastError(t, rec).Payload["reason"])

	gates, err := st.ListGates(ctx, "BP1", "mockup")
	require.NoError(t, err)
	assert.Empty(t, gates)
	assert.Len(t, rec.all(EventRound), rounds, "nothing was stored")

	// The newest revision is taken, and the one before it is superseded.
	useMockup(t, c, nil)

	v := view(t, c)
	assert.Equal(t, "superseded", refOf(t, v, "mockup", 1).Status)
	assert.Equal(t, "approved", refOf(t, v, "mockup", 2).Status)
}

func TestMockup_AUseDuringAChatRunWinsOverTheRevisionInFlight(t *testing.T) {
	two := listRevision()
	two.Notes = "Each shop gets its own tab."

	started, release := make(chan struct{}), make(chan struct{})

	var refused atomic.Bool

	script := withScreen(dialogScript(), true)
	first := showing(listRevision())
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !in.Chat {
			return first(ctx, in)
		}

		close(started)
		<-release

		_, err := in.Designer.Show(ctx, two)
		refused.Store(err != nil)

		if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Revision 1 stays as you chose it."}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	c, st, runner, _, _ := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)

	open := view(t, c).Open
	require.NotNil(t, open)

	require.NoError(t, c.Mockup(ctx, "BP1", protocol.MockupReply{PromptID: open.PromptID, Revision: 1, Action: protocol.MockupChat}, "Add a tab per shop", "chat-1"))
	<-started
	require.NoError(t, c.Mockup(ctx, "BP1", protocol.MockupReply{
		PromptID: open.PromptID, Revision: 1, Action: protocol.MockupUse, Settings: map[string]string{"layout": "cards"},
	}, "Use this design", "use-1"))
	close(release)
	c.Wait("BP1")

	assert.True(t, refused.Load(), "the revision in flight is refused")

	rounds, err := c.rounds(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, rounds, 1)

	chat := rounds[0].Frame.Chat
	assert.Equal(t, protocol.RoundChat{From: "designer", Text: "Revision 1 stays as you chose it."}, chat[len(chat)-1], "answer_chat still replies")

	gates, err := st.ListGates(ctx, "BP1", "mockup")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, 1, gates[0].Version)

	latest, err := st.LatestArtifact(ctx, "BP1", "mockup")
	require.NoError(t, err)
	assert.Equal(t, 1, latest.Version)

	assert.Contains(t, stagesRun(runner), "slicing")
}

func TestMockup_ARestartAfterUseSettlesOnce(t *testing.T) {
	sugs := listRevision().Suggestions

	// Each shape is what a reply cut short leaves on the decided round s.
	shapes := []struct {
		name  string
		leave func(t *testing.T, c *Controller, sess *session, bp store.Blueprint, s roundState)
	}{
		{"before the close", func(*testing.T, *Controller, *session, store.Blueprint, roundState) {}},
		{"with one row written", func(t *testing.T, c *Controller, sess *session, bp store.Blueprint, s roundState) {
			t.Helper()

			n, err := c.nextQuestionNumber(context.Background(), bp.ID)
			require.NoError(t, err)
			require.NoError(t, c.opts.Store.AddQuestion(context.Background(), store.Question{
				BlueprintID: bp.ID, RunID: s.PostedBy, Stage: "mockup", Number: n, Text: sugs[0].Title,
				Answer: "Cards", Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC(),
			}))
		}},
		{"after the close", func(t *testing.T, c *Controller, sess *session, bp store.Blueprint, s roundState) {
			t.Helper()

			_, err := c.closeMockup(context.Background(), sess.audit, bp, s)
			require.NoError(t, err)
		}},
		{"after the gates row", func(t *testing.T, c *Controller, sess *session, bp store.Blueprint, s roundState) {
			t.Helper()

			ctx := context.Background()
			_, err := c.closeMockup(ctx, sess.audit, bp, s)
			require.NoError(t, err)
			require.True(t, c.decideWith(ctx, sess.audit, bp.ID, "mockup", 1, store.GateApprove, "", store.ResponderHuman, "use-1", map[string]any{"mockup": true}))
		}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			script := withScreen(dialogScript(), true)
			script["mockup"] = showing(listRevision())

			c, st, _, _, ws := newMockup(t, script)
			ctx := context.Background()

			drive(t, c, "BP1")
			takeOffer(t, c)

			sess := sessionOf(t, c, "BP1")
			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			lock := c.roundLock("BP1")
			lock.Lock()

			place, found, err := c.roundAt(ctx, "BP1")
			require.NoError(t, err)
			require.True(t, found)
			require.NotNil(t, place.state.Mockup)

			s := place.state.clone()
			s.Mockup.Action, s.Mockup.Chosen, s.Mockup.By = protocol.MockupUse, map[string]string{"layout": "cards", "group": "on"}, store.ResponderHuman
			s.Change = changeChosen
			c.putRound(ctx, sess.audit, "BP1", 0, s)
			shape.leave(t, c, sess, bp, s)

			lock.Unlock()
			require.NoError(t, c.End(ctx, "BP1"))

			c2, _, runner2, _, _ := newPipelineOn(t, st, script, ws)
			c2.opts.Pack = mockupPack(t)
			require.NoError(t, c2.Start(ctx, startInput("BP1")))
			c2.Wait("BP1")

			gates, err := st.ListGates(ctx, "BP1", "mockup")
			require.NoError(t, err)
			require.Len(t, gates, 1)
			assert.Equal(t, store.GateApprove, gates[0].Decision)

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)

			settles := 0

			for _, r := range runs {
				if r.Kind == store.RunKindStage && r.Stage == "mockup" && r.Reason == mockupUsed {
					settles++
				}
			}

			assert.Equal(t, 1, settles)

			texts, numbers := map[string]int{}, map[int]bool{}
			for _, q := range mockupQuestions(t, st) {
				texts[q.Text]++
				numbers[q.Number] = true
			}

			assert.Equal(t, map[string]int{sugs[0].Title: 1, sugs[1].Title: 1}, texts, "one row per suggestion")
			assert.Len(t, numbers, len(sugs))

			slicing := 0

			for _, s := range stagesRun(runner2) {
				if s == "slicing" {
					slicing++
				}
			}

			assert.Equal(t, 1, slicing)
		})
	}
}

func TestMockup_PlainLanguageChangesNothing(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(listRevision())

	c, st, runner, rec, _ := newMockup(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))
	assert.Equal(t, []any{offerShown}, offerStates(rec))

	takeOffer(t, c)
	assert.Equal(t, 1, storedRevision(t, st, 1).Revision)

	useMockup(t, c, nil)

	gates, err := st.ListGates(ctx, "BP1", "mockup")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, 1, gates[0].Version)
	assert.Equal(t, store.ResponderHuman, gates[0].Responder)
	assert.Contains(t, stagesRun(runner), "slicing")
}

// A designer's chat reply on a mockup asks nothing and tabs nothing; the
// reply alone goes in.
func TestMockup_AChatReplyTakesNoQuestions(t *testing.T) {
	var refusedQuestion, refusedTab atomic.Bool

	script := withScreen(dialogScript(), true)
	first := showing(listRevision())
	script["mockup"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !in.Chat {
			return first(ctx, in)
		}

		_, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Which shops?", Questions: []stage.RoundQuestion{roundQ("Shops", "Which shops?", "Corner shop", "Market")}})
		refusedQuestion.Store(err != nil)

		_, err = in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Two shops.", Assumptions: []string{"The household uses two shops."}})
		refusedTab.Store(err != nil)

		if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Tabs come with the next revision."}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	c, _, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)
	chatMockup(t, c, "Add a tab per shop", nil)

	assert.True(t, refusedQuestion.Load())
	assert.True(t, refusedTab.Load())

	rounds, err := c.rounds(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, rounds, 1)

	s := rounds[0]
	assert.Empty(t, s.Frame.Questions)
	assert.Empty(t, s.Frame.Assumptions)
	assert.Equal(t, "Tabs come with the next revision.", s.Frame.Chat[len(s.Frame.Chat)-1].Text)
	assert.Equal(t, promptMockup, view(t, c).Open.Kind)
}

func TestMockup_LaterStagesGetTheApprovedDesignInWords(t *testing.T) {
	var (
		task  atomic.Value
		tools atomic.Value
		read  atomic.Value
	)

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())
	script["slicing"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		task.Store(in.Task)
		tools.Store(toolNames(in))

		if reader := toolNamed(t, in, "read_artifact"); reader != nil {
			res, err := reader.Execute(ctx, map[string]any{"stage": "mockup"})
			if err != nil {
				return stage.Output{}, err
			}

			read.Store(res.Text)
		}

		return done("# Slices", oneSlice()), nil
	}

	c, _, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, map[string]string{"preview": "on", "order": "name"})

	got, _ := task.Load().(string)
	assert.Contains(t, got, photoRevision().Notes)
	assert.Contains(t, got, photoRevision().Suggestions[1].Title)
	assert.Contains(t, got, "Old name", "the chosen label, not the default")
	assert.NotContains(t, got, photoRevision().Source, "the source stays out of the task")
	assert.Contains(t, tools.Load(), "read_artifact")

	whole, _ := read.Load().(string)
	assert.Contains(t, whole, "IMG_0001.jpg", "read_artifact returns the source")
}

// readData decodes the data a read_artifact result holds.
func readData(t *testing.T, text string) mockupData {
	t.Helper()

	_, rest, ok := strings.Cut(text, "```json\n")
	require.True(t, ok, "the read holds data")

	raw, _, ok := strings.Cut(rest, "\n```")
	require.True(t, ok)

	var d mockupData
	require.NoError(t, json.Unmarshal([]byte(raw), &d))

	return d
}

// read_artifact gives a later stage the revision the person used with the
// settings they chose, and no other revision of the mockup.
func TestMockup_ReadArtifactReturnsTheUsedRevisionAsChosen(t *testing.T) {
	type reads struct {
		used   string
		older  error
		latest string
	}

	var got atomic.Value

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(listRevision())
	script["slicing"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if reader := toolNamed(t, in, "read_artifact"); reader != nil {
			used, err := reader.Execute(ctx, map[string]any{"stage": "mockup", "version": float64(2)})
			if err != nil {
				return stage.Output{}, err
			}

			latest, err := reader.Execute(ctx, map[string]any{"stage": "mockup"})
			if err != nil {
				return stage.Output{}, err
			}

			_, older := reader.Execute(ctx, map[string]any{"stage": "mockup", "version": float64(1)})
			got.Store(reads{used: used.Text, older: older, latest: latest.Text})
		}

		return done("# Slices", oneSlice()), nil
	}

	c, _, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)
	chatMockup(t, c, "Group the list by aisle, please.", nil)
	useMockup(t, c, map[string]string{"layout": "cards", "group": "on"})

	r, ok := got.Load().(reads)
	require.True(t, ok, "slicing read the mockup")

	used := readData(t, r.used)
	assert.Equal(t, 2, used.Revision)
	assert.Equal(t, map[string]string{"layout": "cards", "group": "on"}, used.Chosen)
	assert.Equal(t, used, readData(t, r.latest), "a read without a version is the used one")
	assert.Error(t, r.older, "a revision the person did not use is not read")
}

func TestMockup_ASkippedMockupGivesLaterStagesNothing(t *testing.T) {
	var (
		task  atomic.Value
		tools atomic.Value
	)

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())
	script["slicing"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		task.Store(in.Task)
		tools.Store(toolNames(in))

		return done("# Slices", oneSlice()), nil
	}

	c, _, _, _, _ := newMockup(t, script)
	drive(t, c, "BP1")
	takeOffer(t, c)
	skipMockup(t, c)

	got, _ := task.Load().(string)
	require.NotEmpty(t, got, "slicing ran after the skip")
	assert.NotContains(t, got, photoRevision().Notes)

	for _, s := range photoRevision().Suggestions {
		assert.NotContains(t, got, s.Title)
	}

	assert.NotContains(t, got, photoRevision().Source)
	assert.NotContains(t, tools.Load(), "read_artifact")
}
