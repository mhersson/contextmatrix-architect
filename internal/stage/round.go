package stage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

const (
	askQuestionsName = "ask_questions"
	answerChatName   = "answer_chat"
)

// A round's frame must stay well under what ContextMatrix stores per round
// (256 KiB) and reads per log line (1 MiB); a title is the tab label.
const (
	maxRoundQuestions = 10
	maxOptions        = 6
	maxTitleRunes     = 48
)

// RoundOption is one candidate answer to a round's question. Sources are
// the source tags it rests on, normalised to S<n>.
type RoundOption struct {
	Title   string
	Summary string
	Pros    []string
	Cons    []string
	Sources []string
}

// RoundQuestion is one question of a round. Number names the open round's
// question an answer_chat revision replaces; 0 is a new question.
type RoundQuestion struct {
	Number      int
	Title       string
	Text        string
	Why         string
	Options     []RoundOption
	Recommended int
}

// Round is what ask_questions posts.
type Round struct {
	Questions []RoundQuestion
}

// ChatAnswer is what answer_chat sends: the reply for the round's chat and
// the questions it revises or adds.
type ChatAnswer struct {
	Reply     string
	Questions []RoundQuestion
}

// Interviewer posts a run's rounds and answers the chat about the open one.
// The controller implements it; an error goes back to the model as the tool
// result and the run goes on.
type Interviewer interface {
	Ask(ctx context.Context, r Round) (string, error)
	Answer(ctx context.Context, a ChatAnswer) (string, error)
}

// terminalMark records that a round tool ended the run, so Run reports a
// round instead of decoding an artifact. The harness runs a run's tool calls
// one at a time on the run's goroutine.
type terminalMark struct{ asked bool }

const optionSchema = `{"type":"object","properties":{
	"title":{"type":"string","description":"the option, a few words; never write (recommended) into it"},
	"summary":{"type":"string","description":"one line on what choosing it means"},
	"pros":{"type":"array","items":{"type":"string"}},
	"cons":{"type":"array","items":{"type":"string"}},
	"sources":{"type":"array","items":{"type":"string"},"description":"the source tags (S<n>) of tool results this option rests on"}},
	"required":["title"]}`

func questionSchema(revise bool) string {
	number := ""
	if revise {
		number = `"number":{"type":"integer","description":"the number of the open round's question this replaces; leave it out to add a question"},`
	}

	return `{"type":"object","properties":{` + number + `
		"title":{"type":"string","description":"the tab label, two to four words"},
		"question":{"type":"string","description":"the question, one sentence"},
		"why":{"type":"string","description":"why the answer matters for this need"},
		"options":{"type":"array","items":` + optionSchema + `,"description":"the real alternatives; the person can always type their own answer, so never add an other option"},
		"recommended":{"type":"integer","description":"the position, from 1, of the option you recommend"}},
		"required":["title","question"]}`
}

type askQuestionsTool struct {
	iv   Interviewer
	mark *terminalMark
}

func (askQuestionsTool) Name() string   { return askQuestionsName }
func (askQuestionsTool) Terminal() bool { return true }

func (askQuestionsTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: askQuestionsName,
		Description: "Ask the person driving this blueprint a round of questions you cannot settle from the code or your sources. " +
			"Your run ends when the round is posted; the answers arrive as your next message, numbered Q<n>, which is how to cite them.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":` + questionSchema(false) + `}},"required":["questions"]}`),
	}}
}

func (t askQuestionsTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	qs, err := parseQuestions(args["questions"], false)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: %w", askQuestionsName, err)
	}

	if len(qs) == 0 {
		return tools.Result{}, fmt.Errorf("%s: a round needs at least one question", askQuestionsName)
	}

	text, err := t.iv.Ask(ctx, Round{Questions: qs})
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: %w", askQuestionsName, err)
	}

	t.mark.asked = true

	return tools.Result{Text: text}, nil
}

type answerChatTool struct {
	iv   Interviewer
	mark *terminalMark
}

func (answerChatTool) Name() string   { return answerChatName }
func (answerChatTool) Terminal() bool { return true }

func (answerChatTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: answerChatName,
		Description: "Reply in the open round's chat. Revise a question (give its number) or add one when the person's point changes the round; " +
			"a question whose options you change loses its answer, and the others keep theirs. Your run ends with the reply.",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"reply":{"type":"string","description":"your reply in the round's chat"},
			"questions":{"type":"array","items":` + questionSchema(true) + `}},"required":["reply"]}`),
	}}
}

func (t answerChatTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	reply := strings.TrimSpace(stringArg(args, "reply"))
	if reply == "" {
		return tools.Result{}, fmt.Errorf("%s: reply is required", answerChatName)
	}

	qs, err := parseQuestions(args["questions"], true)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: %w", answerChatName, err)
	}

	text, err := t.iv.Answer(ctx, ChatAnswer{Reply: reply, Questions: qs})
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: %w", answerChatName, err)
	}

	t.mark.asked = true

	return tools.Result{Text: text}, nil
}

func parseQuestions(v any, revise bool) ([]RoundQuestion, error) {
	if v == nil {
		return nil, nil
	}

	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("questions must be a list")
	}

	if len(items) > maxRoundQuestions {
		return nil, fmt.Errorf("a round asks at most %d questions; ask the rest in a later round", maxRoundQuestions)
	}

	out := make([]RoundQuestion, 0, len(items))

	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("question %d must be an object", i+1)
		}

		q, err := parseQuestion(obj, revise)
		if err != nil {
			return nil, fmt.Errorf("question %d: %w", i+1, err)
		}

		out = append(out, q)
	}

	return out, nil
}

func parseQuestion(obj map[string]any, revise bool) (RoundQuestion, error) {
	q := RoundQuestion{
		Title: strings.TrimSpace(stringArg(obj, "title")),
		Text:  strings.TrimSpace(stringArg(obj, "question")),
		Why:   strings.TrimSpace(stringArg(obj, "why")),
	}

	if v, ok := obj["number"]; revise && ok && v != nil {
		n, isNumber := v.(float64)
		if !isNumber || n != float64(int(n)) || n < 1 {
			return RoundQuestion{}, errors.New("number is the open round's question number, or left out for a new question")
		}

		q.Number = int(n)
	}

	switch {
	case q.Title == "":
		return RoundQuestion{}, errors.New("title is required: the tab label, a few words")
	case utf8.RuneCountInString(q.Title) > maxTitleRunes:
		return RoundQuestion{}, fmt.Errorf("title is the tab label: at most %d characters", maxTitleRunes)
	case q.Text == "":
		return RoundQuestion{}, errors.New("question is required")
	}

	var given []any

	if raw, ok := obj["options"]; ok && raw != nil {
		list, isList := raw.([]any)
		if !isList {
			return RoundQuestion{}, errors.New("options must be a list of {title, summary, pros, cons, sources}")
		}

		if len(list) > maxOptions {
			return RoundQuestion{}, fmt.Errorf("a question offers at most %d options; keep the real alternatives", maxOptions)
		}

		given = list
	}

	// kept[i] is the position, from 1, of given[i] among the kept options, 0
	// for a blank one; marked holds the kept positions whose title carried
	// the marker.
	kept := make([]int, len(given))

	var marked []int

	for i, o := range given {
		om, ok := o.(map[string]any)
		if !ok {
			return RoundQuestion{}, fmt.Errorf("option %d must be an object with a title", i+1)
		}

		title, isMarked := cutRecommended(stringArg(om, "title"))
		if title = strings.TrimSpace(title); title == "" {
			continue
		}

		q.Options = append(q.Options, RoundOption{
			Title: title, Summary: strings.TrimSpace(stringArg(om, "summary")),
			Pros: textList(om["pros"]), Cons: textList(om["cons"]), Sources: tagList(om["sources"]),
		})
		kept[i] = len(q.Options)

		if isMarked {
			marked = append(marked, kept[i])
		}
	}

	rec, err := recommendedPosition(obj["recommended"], len(given), kept, marked)
	if err != nil {
		return RoundQuestion{}, err
	}

	q.Recommended = rec

	return q, nil
}

// textList reads a list of strings, blanks dropped; nil when none is left.
func textList(v any) []string {
	items, _ := v.([]any)

	var out []string

	for _, item := range items {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}

	return out
}

// tagList reads source tags as the model wrote them ("[s3]") in the form the
// controller prints them ("S3"), each once.
func tagList(v any) []string {
	var out []string

	for _, s := range textList(v) {
		tag := strings.ToUpper(strings.TrimSpace(strings.Trim(s, "[]")))
		if tag != "" && !slices.Contains(out, tag) {
			out = append(out, tag)
		}
	}

	return out
}
