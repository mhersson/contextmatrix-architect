package stage

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDesigner struct{ got []MockupRevision }

func (d *fakeDesigner) Show(_ context.Context, m MockupRevision) (string, error) {
	d.got = append(d.got, m)

	return "shown", nil
}

// terminalSource imitates a full-screen terminal interface for renaming
// photos: a file list and a key bar.
const terminalSource = `<!doctype html><html><head><style>body{margin:0;background:#111;color:#ddd;font-family:monospace}
#bar{position:fixed;bottom:0;width:100%;background:#335;color:#fff}</style></head>
<body><pre id="out"></pre><div id="bar">F2 rename  Space mark  q quit</div><img src="data:image/png;base64,iVBORw0KGgo=" alt=""><svg width="8" height="8"></svg><script>
function draw(s){document.getElementById("out").textContent = s.preview === "on" ? "[x] IMG_0001.jpg -> beach-001.jpg" : "[x] IMG_0001.jpg"}
draw(window.mockupSettings || {}); window.addEventListener("mockup:settings", function(e){ draw(e.detail) });
</script></body></html>`

func mockupArgs(source string) map[string]any {
	return map[string]any{
		"notes":   "A dry run lists every new name first.",
		"screens": []any{map[string]any{"id": "run", "title": "The run"}},
		"source":  source,
		"suggestions": []any{
			map[string]any{"id": "preview", "title": "Show a dry run", "kind": "switch", "why": "See the names before anything moves."},
			map[string]any{
				"id": "order", "title": "Number by", "kind": "choice", "why": "Dates sort as taken.",
				"options": []any{map[string]any{"value": "date", "label": "Date taken"}, map[string]any{"value": "name", "label": "Old name"}},
			},
		},
	}
}

func TestShowMockup_PostsTheRevision(t *testing.T) {
	d := &fakeDesigner{}
	mark := &terminalMark{}

	_, err := showMockupTool{d: d, mark: mark}.Execute(context.Background(), mockupArgs(terminalSource))
	require.NoError(t, err)
	require.Len(t, d.got, 1)
	assert.True(t, mark.asked)

	m := d.got[0]
	assert.Equal(t, terminalSource, m.Source)
	assert.Equal(t, []MockupScreen{{ID: "run", Title: "The run"}}, m.Screens)
	assert.Equal(t, "off", m.Suggestions[0].Default, "a switch defaults to off")
	assert.Equal(t, "date", m.Suggestions[1].Default, "a choice defaults to its first option")
	assert.True(t, m.Suggestions[1].Accepts("name"))
	assert.False(t, m.Suggestions[1].Accepts("size"))
	assert.Equal(t, "Old name", m.Suggestions[1].Label("name"))
	assert.Equal(t, "On", m.Suggestions[0].Label("on"))
}

func TestShowMockup_RefusesWhatTheFrameBlocks(t *testing.T) {
	src := func(old, repl string) string { return strings.Replace(terminalSource, old, repl, 1) }
	tests := []struct {
		name string
		args func() map[string]any
	}{
		{"an outside script", func() map[string]any {
			return mockupArgs(src("<script>", `<script src="https://cdn.example.org/x.js"></script><script>`))
		}},
		{"a stylesheet link", func() map[string]any {
			return mockupArgs(src("<style>", `<link rel="stylesheet" href="https://example.org/a.css"><style>`))
		}},
		{"a web font", func() map[string]any {
			return mockupArgs(src("body{", "@import url(https://fonts.example.org/x);body{"))
		}},
		{"a relative image", func() map[string]any { return mockupArgs(src(`data:image/png;base64,iVBORw0KGgo=`, "beach.png")) }},
		{"a form", func() map[string]any { return mockupArgs(src("<pre", "<form><button>Go</button></form><pre")) }},
		{"a nested frame", func() map[string]any { return mockupArgs(src("<pre", `<iframe srcdoc="x"></iframe><pre`)) }},
		{"local storage", func() map[string]any {
			return mockupArgs(src("draw(window", `localStorage.setItem("k","v");draw(window`))
		}},
		{"a network call", func() map[string]any { return mockupArgs(src("draw(window", `fetch("/x");draw(window`)) }},
		{"a dialog in an event attribute", func() map[string]any {
			return mockupArgs(src(`<div id="bar">`, `<div id="bar" onclick="alert('renamed')">`))
		}},
		{"a socket in an event attribute", func() map[string]any {
			return mockupArgs(src(`<pre id="out">`, `<pre id="out" onload="new WebSocket('ws://x')">`))
		}},
		{"settings never read", func() map[string]any { return mockupArgs(src("window.mockupSettings || {}", "{}")) }},
		{"a line that reads as a card heading", func() map[string]any {
			return mockupArgs(src(`<pre id="out"></pre>`, "<pre id=\"notes\">\n  ## Plan\n</pre><pre id=\"out\"></pre>"))
		}},
		{"too big", func() map[string]any {
			return mockupArgs(terminalSource + "<!--" + strings.Repeat("x", MaxMockupBytes) + "-->")
		}},
		{"no source", func() map[string]any { return mockupArgs("") }},
		{"no screen", func() map[string]any {
			a := mockupArgs(terminalSource)
			a["screens"] = []any{}

			return a
		}},
		{"an id with a capital", func() map[string]any {
			a := mockupArgs(terminalSource)
			a["suggestions"].([]any)[0].(map[string]any)["id"] = "Preview"

			return a
		}},
		{"a repeated id", func() map[string]any {
			a := mockupArgs(terminalSource)
			a["suggestions"].([]any)[1].(map[string]any)["id"] = "preview"

			return a
		}},
		{"a choice of one", func() map[string]any {
			a := mockupArgs(terminalSource)
			a["suggestions"].([]any)[1].(map[string]any)["options"] = []any{map[string]any{"value": "date", "label": "Date taken"}}

			return a
		}},
		{"a default that is no option", func() map[string]any {
			a := mockupArgs(terminalSource)
			a["suggestions"].([]any)[1].(map[string]any)["default"] = "size"

			return a
		}},
		{"a switch that defaults to yes", func() map[string]any {
			a := mockupArgs(terminalSource)
			a["suggestions"].([]any)[0].(map[string]any)["default"] = "yes"

			return a
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDesigner{}
			mark := &terminalMark{}

			_, err := showMockupTool{d: d, mark: mark}.Execute(context.Background(), tt.args())
			require.Error(t, err)
			assert.Empty(t, d.got)
			assert.False(t, mark.asked, "a refused revision ends nothing")
		})
	}
}

// A screen may show code as text, as a developer tool or a terminal does:
// only the source's own script is held to the frame contract.
func TestShowMockup_ShowsCallsAsText(t *testing.T) {
	d := &fakeDesigner{}
	mark := &terminalMark{}

	source := strings.Replace(terminalSource, `<pre id="out"></pre>`,
		`<pre id="log">GET /photos via fetch("/photos") ... WebSocket open, localStorage cleared</pre><pre id="out"></pre>`, 1)

	_, err := showMockupTool{d: d, mark: mark}.Execute(context.Background(), mockupArgs(source))
	require.NoError(t, err)
	require.Len(t, d.got, 1)
	assert.Equal(t, source, d.got[0].Source)
	assert.True(t, mark.asked)
}

// A line that reads as a card heading cuts the rest of the mockup from the
// agent's prompts; written with an entity, the screen shows the same text.
func TestShowMockup_TakesAHeadingWrittenWithAnEntity(t *testing.T) {
	d := &fakeDesigner{}
	mark := &terminalMark{}

	source := strings.Replace(terminalSource, `<pre id="out"></pre>`, "<pre id=\"notes\">\n  &#35;# Plan\n</pre><pre id=\"out\"></pre>", 1)

	_, err := showMockupTool{d: d, mark: mark}.Execute(context.Background(), mockupArgs(source))
	require.NoError(t, err)
	require.Len(t, d.got, 1)
	assert.Equal(t, source, d.got[0].Source)
}
