package retro

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Lesson is a proposed lesson of an at-fault finding.
type Lesson struct {
	Text, Rationale, Scope string
	Overlay                contract.Overlay
}

// Finding is the retro run's judgement of one struggled card.
type Finding struct {
	Card, Rule, Evidence string
	AtFault              bool
	Lesson               *Lesson // set exactly when AtFault
}

// ParseFindings checks a retro run's submitted data against the struggled
// cards and returns one finding per card in struggled's order. All problems
// come back in one error.
func ParseFindings(data map[string]any, struggled []Struggle) ([]Finding, error) {
	raw, ok := data["findings"].([]any)
	if !ok {
		return nil, errors.New("findings: want an array under the key findings")
	}

	bodies := make(map[string]string, len(struggled))
	for _, s := range struggled {
		bodies[s.Card] = collapse(s.Body)
	}

	var (
		problems []string
		byCard   = map[string]Finding{}
	)

	for i, entry := range raw {
		pos := i + 1

		obj, ok := entry.(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("finding %d: not an object", pos))

			continue
		}

		card, _ := obj["card"].(string)
		label := fmt.Sprintf("finding %d (%s)", pos, card)

		body, known := bodies[card]
		if !known {
			problems = append(problems, label+": card is not one of the struggled cards")

			continue
		}

		if _, dup := byCard[card]; dup {
			problems = append(problems, label+": card named twice")

			continue
		}

		f, ps := parseFinding(obj, card, body)
		for _, p := range ps {
			problems = append(problems, label+": "+p)
		}

		byCard[card] = f
	}

	out := make([]Finding, 0, len(struggled))

	for _, s := range struggled {
		f, ok := byCard[s.Card]
		if !ok {
			problems = append(problems, fmt.Sprintf("card %s: struggled but has no finding", s.Card))

			continue
		}

		out = append(out, f)
	}

	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	return out, nil
}

func parseFinding(obj map[string]any, card, body string) (Finding, []string) {
	var problems []string

	f := Finding{Card: card}
	f.Rule, _ = obj["rule"].(string)
	f.Evidence, _ = obj["evidence"].(string)

	if strings.TrimSpace(f.Rule) == "" {
		problems = append(problems, "rule is blank")
	}

	switch ev := collapse(f.Evidence); {
	case ev == "":
		problems = append(problems, "evidence is blank")
	case !strings.Contains(body, ev):
		problems = append(problems, "evidence is not quoted from the card's body")
	}

	atFault, isBool := obj["at_fault"].(bool)
	if !isBool {
		return f, append(problems, "at_fault is not a boolean")
	}

	f.AtFault = atFault

	lessonRaw, has := obj["lesson"]
	if lessonRaw == nil {
		has = false
	}

	switch {
	case !atFault && has:
		return f, append(problems, "a lesson on a card not at fault")
	case atFault && !has:
		return f, append(problems, "no lesson on a card at fault")
	case !atFault:
		return f, problems
	}

	lesson, ps := parseLesson(lessonRaw)
	f.Lesson = lesson

	return f, append(problems, ps...)
}

func parseLesson(raw any) (*Lesson, []string) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, []string{"lesson is not an object"}
	}

	var problems []string

	l := &Lesson{}
	l.Text, _ = obj["text"].(string)
	l.Rationale, _ = obj["rationale"].(string)
	l.Scope, _ = obj["scope"].(string)

	if strings.TrimSpace(l.Text) == "" {
		problems = append(problems, "lesson text is blank")
	}

	if strings.TrimSpace(l.Rationale) == "" {
		problems = append(problems, "lesson rationale is blank")
	}

	if l.Scope != store.ScopePack && l.Scope != store.ScopeProject {
		problems = append(problems, fmt.Sprintf("lesson scope %q is not %s or %s", l.Scope, store.ScopePack, store.ScopeProject))
	}

	if ov, present := obj["overlay"]; present && ov != nil {
		encoded, err := json.Marshal(ov)
		if err != nil {
			return l, append(problems, "lesson overlay: "+err.Error())
		}

		dec := json.NewDecoder(bytes.NewReader(encoded))
		dec.DisallowUnknownFields()

		if err := dec.Decode(&l.Overlay); err != nil {
			return l, append(problems, "lesson overlay: "+err.Error())
		}

		for _, p := range l.Overlay.ForbiddenReferences {
			if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Pattern) == "" {
				problems = append(problems, "lesson overlay: a forbidden reference needs a name and a pattern")

				break
			}
		}
	}

	return l, problems
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
