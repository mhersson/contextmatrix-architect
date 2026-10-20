package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"golang.org/x/net/html"
)

// ShowMockupName is the tool a designer's run shows a mockup revision with.
const ShowMockupName = "show_mockup"

// A suggestion's kinds: a switch is on or off, a choice one of its options.
const (
	MockupSwitch = "switch"
	MockupChoice = "choice"
)

// MaxMockupBytes caps a revision's source: ContextMatrix frames it as sent
// and sets no cap of its own.
const MaxMockupBytes = 64 << 10

// A switch's values.
const (
	switchOn  = "on"
	switchOff = "off"
)

const (
	maxMockupScreens     = 8
	maxMockupSuggestions = 12
	minChoiceOptions     = 2
	maxChoiceOptions     = 4
)

var (
	mockupID    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	mockupValue = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

// MockupScreen is one screen a revision shows, as a tab, step or view
// inside its one document.
type MockupScreen struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// MockupOption is one value of a choice and its label.
type MockupOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// MockupSuggestion is one setting the person flips on a revision; its id
// keeps its meaning across revisions.
type MockupSuggestion struct {
	ID      string         `json:"id"`
	Title   string         `json:"title"`
	Kind    string         `json:"kind"`
	Why     string         `json:"why"`
	Default string         `json:"default"`
	Options []MockupOption `json:"options,omitempty"`
}

// Accepts reports whether v is a value of s: on or off for a switch, one of
// its options for a choice.
func (s MockupSuggestion) Accepts(v string) bool {
	if s.Kind == MockupSwitch {
		return v == switchOn || v == switchOff
	}

	return slices.ContainsFunc(s.Options, func(o MockupOption) bool { return o.Value == v })
}

// Label is value v of s as a person reads it: On or Off for a switch, the
// option's label for a choice; "" for a value s does not take.
func (s MockupSuggestion) Label(v string) string {
	if s.Kind == MockupSwitch {
		switch v {
		case switchOn:
			return "On"
		case switchOff:
			return "Off"
		}

		return ""
	}

	for _, o := range s.Options {
		if o.Value == v {
			return o.Label
		}
	}

	return ""
}

// MockupRevision is what show_mockup posts: the designer's notes, the HTML
// document, the screens it shows and the suggestions it renders.
type MockupRevision struct {
	Notes       string
	Source      string
	Screens     []MockupScreen
	Suggestions []MockupSuggestion
}

// Designer shows a designer run's revisions. The controller implements it;
// an error goes back to the model as the tool result and the run goes on.
type Designer interface {
	Show(ctx context.Context, m MockupRevision) (string, error)
}

type showMockupTool struct {
	d    Designer
	mark *terminalMark
}

func (showMockupTool) Name() string   { return ShowMockupName }
func (showMockupTool) Terminal() bool { return true }

const mockupSuggestionSchema = `{"type":"object","properties":{
	"id":{"type":"string","description":"lower case letters, digits and _, starting with a letter; keep an id's meaning in every revision"},
	"title":{"type":"string","description":"the setting, a few words"},
	"kind":{"type":"string","enum":["switch","choice"],"description":"switch: on or off; choice: one of 2 to 4 options"},
	"why":{"type":"string","description":"one line on why the person might want it"},
	"default":{"type":"string","description":"a switch's on or off (off when left out); a choice's option value (its first when left out)"},
	"options":{"type":"array","items":{"type":"object","properties":{
		"value":{"type":"string","description":"lower case letters, digits, _ and -"},
		"label":{"type":"string","description":"what the person reads"}},"required":["value","label"]},
		"description":"a choice's 2 to 4 options; a switch has none"}},
	"required":["id","title","kind"]}`

func (showMockupTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: ShowMockupName,
		Description: "Show the person the next revision of the mockup: the screen people will use, imitated in one HTML document, " +
			"the screens it shows and the suggestions they can flip beside it, each re-rendering the document at once with no model run. " +
			"Your run ends when the revision is shown; the person's reaction arrives as your next message.",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"notes":{"type":"string","description":"what this revision shows and what changed since the last, a few sentences for the person"},
			"source":{"type":"string","description":"the screen people will use, imitated in one HTML document: a phone or tablet app, a desktop window, a full-screen terminal interface, a web page, whatever the need has. ` +
			`At most 64 KiB and self-contained, as it runs sealed in a frame: inline script and style elements and style attributes only, images as data: URIs or inline SVG, system fonts only; ` +
			`no network calls, form submission, popups, alert, confirm or prompt, nested frames, navigating links (switch views in script), localStorage, sessionStorage or cookies. ` +
			`With suggestions it renders from window.mockupSettings (each suggestion id to its value) at load, and again on every window mockup:settings event, whose detail is the settings, without resetting the screen or tab the person is on."},
			"screens":{"type":"array","items":{"type":"object","properties":{
				"id":{"type":"string","description":"lower case letters, digits and _, starting with a letter"},
				"title":{"type":"string","description":"the screen's name, a few words"}},"required":["id","title"]},
				"description":"the 1 to 8 screens the document shows, as tabs, steps or views inside it"},
			"suggestions":{"type":"array","items":` + mockupSuggestionSchema + `,"description":"at most 12 settings the document renders, for the person to try"}},
			"required":["notes","source","screens"]}`),
	}}
}

func (t showMockupTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	m, err := parseMockup(args)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: %w", ShowMockupName, err)
	}

	text, err := t.d.Show(ctx, m)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: %w", ShowMockupName, err)
	}

	t.mark.asked = true

	return tools.Result{Text: text}, nil
}

// parseMockup reads show_mockup's arguments, every text trimmed, and checks
// the source against the frame contract.
func parseMockup(args map[string]any) (MockupRevision, error) {
	m := MockupRevision{
		Notes:  strings.TrimSpace(stringArg(args, "notes")),
		Source: strings.TrimSpace(stringArg(args, "source")),
	}

	if m.Notes == "" {
		return MockupRevision{}, errors.New("notes are required: what this revision shows, for the person")
	}

	screens, err := mockupScreens(args["screens"])
	if err != nil {
		return MockupRevision{}, err
	}

	sugs, err := mockupSuggestions(args["suggestions"])
	if err != nil {
		return MockupRevision{}, err
	}

	if err := checkSource(m.Source, len(sugs) > 0); err != nil {
		return MockupRevision{}, err
	}

	m.Screens, m.Suggestions = screens, sugs

	return m, nil
}

// objects reads v as a list of objects; nil for none.
func objects(v any, what string) ([]map[string]any, error) {
	if v == nil {
		return nil, nil
	}

	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", what)
	}

	out := make([]map[string]any, 0, len(items))

	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s %d must be an object", what, i+1)
		}

		out = append(out, obj)
	}

	return out, nil
}

// mockupTitle checks a screen's or a suggestion's title.
func mockupTitle(title, what string) error {
	switch {
	case title == "":
		return fmt.Errorf("%s needs a title", what)
	case utf8.RuneCountInString(title) > maxTitleRunes:
		return fmt.Errorf("%s's title is at most %d characters", what, maxTitleRunes)
	}

	return nil
}

// mockupIDOf checks id, unique among seen, and records it.
func mockupIDOf(id, what string, seen map[string]bool) error {
	switch {
	case !mockupID.MatchString(id):
		return fmt.Errorf("%s id %q must be lower case letters, digits and _, starting with a letter, at most 32", what, id)
	case seen[id]:
		return fmt.Errorf("%s id %q is given twice", what, id)
	}

	seen[id] = true

	return nil
}

func mockupScreens(v any) ([]MockupScreen, error) {
	objs, err := objects(v, "screens")
	if err != nil {
		return nil, err
	}

	if len(objs) == 0 || len(objs) > maxMockupScreens {
		return nil, fmt.Errorf("a mockup shows 1 to %d screens", maxMockupScreens)
	}

	seen := map[string]bool{}
	out := make([]MockupScreen, 0, len(objs))

	for _, obj := range objs {
		s := MockupScreen{ID: strings.TrimSpace(stringArg(obj, "id")), Title: strings.TrimSpace(stringArg(obj, "title"))}

		if err := mockupIDOf(s.ID, "screen", seen); err != nil {
			return nil, err
		}

		if err := mockupTitle(s.Title, "screen "+s.ID); err != nil {
			return nil, err
		}

		out = append(out, s)
	}

	return out, nil
}

func mockupSuggestions(v any) ([]MockupSuggestion, error) {
	objs, err := objects(v, "suggestions")
	if err != nil {
		return nil, err
	}

	if len(objs) > maxMockupSuggestions {
		return nil, fmt.Errorf("a revision offers at most %d suggestions", maxMockupSuggestions)
	}

	seen := map[string]bool{}

	var out []MockupSuggestion

	for _, obj := range objs {
		s := MockupSuggestion{
			ID: strings.TrimSpace(stringArg(obj, "id")), Title: strings.TrimSpace(stringArg(obj, "title")),
			Kind: strings.TrimSpace(stringArg(obj, "kind")), Why: strings.TrimSpace(stringArg(obj, "why")),
			Default: strings.TrimSpace(stringArg(obj, "default")),
		}

		if err := mockupIDOf(s.ID, "suggestion", seen); err != nil {
			return nil, err
		}

		if err := mockupTitle(s.Title, "suggestion "+s.ID); err != nil {
			return nil, err
		}

		options, err := objects(obj["options"], "suggestion "+s.ID+"'s options")
		if err != nil {
			return nil, err
		}

		switch s.Kind {
		case MockupSwitch:
			s, err = switchSuggestion(s, options)
		case MockupChoice:
			s, err = choiceSuggestion(s, options)
		default:
			err = fmt.Errorf("suggestion %s's kind is switch or choice", s.ID)
		}

		if err != nil {
			return nil, err
		}

		out = append(out, s)
	}

	return out, nil
}

func switchSuggestion(s MockupSuggestion, options []map[string]any) (MockupSuggestion, error) {
	if len(options) > 0 {
		return MockupSuggestion{}, fmt.Errorf("suggestion %s is a switch, which has no options", s.ID)
	}

	if s.Default == "" {
		s.Default = switchOff
	}

	if !s.Accepts(s.Default) {
		return MockupSuggestion{}, fmt.Errorf("suggestion %s is a switch: its default is on or off", s.ID)
	}

	return s, nil
}

func choiceSuggestion(s MockupSuggestion, options []map[string]any) (MockupSuggestion, error) {
	if len(options) < minChoiceOptions || len(options) > maxChoiceOptions {
		return MockupSuggestion{}, fmt.Errorf("suggestion %s is a choice of %d to %d options", s.ID, minChoiceOptions, maxChoiceOptions)
	}

	for _, obj := range options {
		o := MockupOption{Value: strings.TrimSpace(stringArg(obj, "value")), Label: strings.TrimSpace(stringArg(obj, "label"))}

		switch {
		case !mockupValue.MatchString(o.Value):
			return MockupSuggestion{}, fmt.Errorf("suggestion %s: option value %q must be lower case letters, digits, _ and -, at most 32", s.ID, o.Value)
		case s.Accepts(o.Value):
			return MockupSuggestion{}, fmt.Errorf("suggestion %s: option value %q is given twice", s.ID, o.Value)
		case o.Label == "":
			return MockupSuggestion{}, fmt.Errorf("suggestion %s: option %s needs a label", s.ID, o.Value)
		}

		s.Options = append(s.Options, o)
	}

	if s.Default == "" {
		s.Default = s.Options[0].Value
	}

	if !s.Accepts(s.Default) {
		return MockupSuggestion{}, fmt.Errorf("suggestion %s: its default %q is none of its option values", s.ID, s.Default)
	}

	return s, nil
}

// blockedTag reports an element that loads, submits or navigates, which
// the frame blocks.
func blockedTag(name string) bool {
	switch name {
	case "link", "iframe", "frame", "object", "embed", "form", "base":
		return true
	}

	return false
}

// addressAttr reports an attribute whose value is an address.
func addressAttr(name string) bool {
	switch name {
	case "src", "href", "xlink:href", "srcset", "poster", "action", "formaction", "data":
		return true
	}

	return false
}

// blockedCalls are the script names, lower case, the frame blocks or a
// sealed origin throws on.
func blockedCalls() []string {
	return []string{
		"localstorage", "sessionstorage", "document.cookie", "fetch(", "xmlhttprequest", "websocket", "eventsource",
		"alert(", "confirm(", "prompt(", "window.open(",
	}
}

// checkSource refuses a source the frame contract does not let run as
// drawn, naming the rule: ContextMatrix's frame is the boundary, and this
// check keeps a revision from showing broken in it. settings is set when the
// revision has suggestions, which the source must read.
func checkSource(src string, settings bool) error {
	switch {
	case strings.TrimSpace(src) == "":
		return errors.New("source is required: the screen as one HTML document")
	case len(src) > MaxMockupBytes:
		return fmt.Errorf("the source is %d bytes and a mockup takes at most %d; inline less, or draw fewer screens", len(src), MaxMockupBytes)
	}

	code, err := checkMarkup(src)
	if err != nil {
		return err
	}

	for _, st := range code.styles {
		if err := checkStyle(st.text, st.where); err != nil {
			return err
		}
	}

	// Only code is scanned: a screen may show these names as text, as a
	// developer tool or a terminal does.
	for _, sc := range code.scripts {
		lower := strings.ToLower(sc.text)

		for _, call := range blockedCalls() {
			if strings.Contains(lower, call) {
				return fmt.Errorf("the source's %s uses %s, which the mockup's frame blocks: keep the state in script variables and show messages on the screen", sc.where, strings.TrimSuffix(call, "("))
			}
		}
	}

	if settings && (!strings.Contains(src, "mockupSettings") || !strings.Contains(src, "mockup:settings")) {
		return errors.New("the source must render from window.mockupSettings at load and again on each window mockup:settings event, or flipping a suggestion changes nothing")
	}

	return nil
}

// piece is css or script a source holds, and where it holds it.
type piece struct{ text, where string }

// code is a source's css, from its style elements and attributes, and its
// script, from its script elements and on* attributes, in document order.
type code struct{ styles, scripts []piece }

// checkMarkup tokenizes src, refuses a tag or an address that loads,
// submits or navigates, and returns its code.
func checkMarkup(src string) (code, error) {
	var (
		out code
		// in is the raw-text element the tokenizer stands in, "" for none.
		in string
	)

	z := html.NewTokenizer(strings.NewReader(src))

	for {
		switch z.Next() {
		case html.ErrorToken:
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return code{}, fmt.Errorf("the source does not read as HTML: %w", err)
			}

			return out, nil
		case html.TextToken:
			switch in {
			case "style":
				out.styles = append(out.styles, piece{text: string(z.Text()), where: "style element"})
			case "script":
				out.scripts = append(out.scripts, piece{text: string(z.Text()), where: "script element"})
			}
		case html.EndTagToken:
			in = ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, err := checkTag(z, &out)
			if err != nil {
				return code{}, err
			}

			in = name
		}
	}
}

// checkTag checks the tag z stands at, adds the code of its style and on*
// attributes to out, and returns the tag's name.
func checkTag(z *html.Tokenizer, out *code) (string, error) {
	raw, more := z.TagName()
	name := string(raw)

	if blockedTag(name) {
		return "", fmt.Errorf("the source has a <%s> element, which the mockup's frame blocks: draw everything in the one document, and switch views in script", name)
	}

	for more {
		var key, val []byte

		key, val, more = z.TagAttr()
		attr, value := string(key), strings.TrimSpace(string(val))

		switch {
		case name == "meta" && attr == "http-equiv":
			return "", errors.New("the source has a <meta http-equiv> element, which the mockup's frame ignores or blocks")
		case attr == "style":
			out.styles = append(out.styles, piece{text: value, where: "style attribute"})
		case strings.HasPrefix(attr, "on"):
			out.scripts = append(out.scripts, piece{text: value, where: attr + " attribute"})
		case addressAttr(attr) && !inlineAddress(attr, value):
			return "", fmt.Errorf("the source's %s %s=%q loads or navigates outside the document, which the mockup's frame blocks: use a data: URI or inline SVG, and switch views in script", name, attr, value)
		}
	}

	return name, nil
}

// inlineAddress reports an address that stays inside the document: a data:
// URI, or a fragment for a link.
func inlineAddress(attr, value string) bool {
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		return true
	}

	return (attr == "href" || attr == "xlink:href") && strings.HasPrefix(value, "#")
}

// checkStyle refuses an import, or a url( that is no data: URI or fragment,
// in css from where.
func checkStyle(css, where string) error {
	rest := strings.ToLower(css)

	if strings.Contains(rest, "@import") {
		return fmt.Errorf("the source has an @import in a %s, which the mockup's frame blocks: inline the styles and use system fonts", where)
	}

	for {
		i := strings.Index(rest, "url(")
		if i < 0 {
			return nil
		}

		rest = rest[i+len("url("):]

		arg := strings.TrimLeft(strings.TrimSpace(rest), `"'`)
		if !strings.HasPrefix(arg, "data:") && !strings.HasPrefix(arg, "#") {
			return fmt.Errorf("the source has a url( in a %s that is no data: URI, which the mockup's frame blocks: inline the image or font, or use system fonts", where)
		}
	}
}
