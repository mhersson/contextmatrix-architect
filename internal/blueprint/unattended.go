package blueprint

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// withUnattended adds the pack's unattended instructions after a stage run's
// own when the blueprint is unattended; otherwise the instructions are
// unchanged. Only the stage that writes the cards and the one that judges
// readiness take them: decomposition writes the manual entries and readiness
// judges them, and neither has a reviewer.
func withUnattended(bp store.Blueprint, instructions, overlay string) string {
	if !bp.Unattended || overlay == "" {
		return instructions
	}

	return instructions + "\n\n" + overlay
}

const stopShape = `{"id": "M<n>", "text": "<why the run need not stop there>"}`

// stopProblems checks the manual entries an unattended blueprint's readiness
// verdict judged needless (needless_stops): absent, or a list of entries,
// each a manual entry of stops, the playbook's order, once, with the text
// that says why.
func stopProblems(v any, stops []string) []string {
	if v == nil {
		return nil
	}

	items, isList := v.([]any)
	if !isList {
		return []string{problem("needless_stops", "stop_shape", "needless_stops must be a list of "+stopShape+" entries")}
	}

	var out []string

	seen := map[string]bool{}

	for i, item := range items {
		where := fmt.Sprintf("needless stop %d", i+1)

		e, isObject := item.(map[string]any)
		if !isObject {
			out = append(out, problem(where, "stop_shape", "each needless stop is "+stopShape))

			continue
		}

		id, _ := e["id"].(string)
		text, _ := e["text"].(string)

		if id = strings.TrimSpace(id); id != "" {
			where = id
		}

		switch {
		case len(stops) == 0:
			out = append(out, problem(where, "stop_id", "no manual entry is in the playbook's order, so nothing stops the run; leave needless_stops empty"))
		case !slices.Contains(stops, id):
			out = append(out, problem(where, "stop_id", "only a manual entry in the playbook's order stops the run; name one of "+strings.Join(stops, ", ")))
		case seen[id]:
			out = append(out, problem(where, "stop_id", "the stop is named twice; give it one entry"))
		}

		seen[id] = true

		if strings.TrimSpace(text) == "" {
			out = append(out, problem(where, "stop_text", "say why the run need not stop there"))
		}
	}

	return out
}

// needlessStop is a manual entry readiness judged the run need not stop at.
type needlessStop struct {
	ID   string
	Text string
}

// needlessStops reads the needless stops of a readiness verdict, in its
// order. As crossOf, it reads data a check's last round may have accepted:
// an entry that is not an object, or whose id is not a manual entry of
// stops, is skipped, and a malformed text reads as empty.
func needlessStops(art store.Artifact, stops []string) []needlessStop {
	var data struct {
		Stops []json.RawMessage `json:"needless_stops"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &data) != nil {
		return nil
	}

	var out []needlessStop

	for _, raw := range data.Stops {
		var fields map[string]any
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			continue
		}

		id, _ := fields["id"].(string)
		text, _ := fields["text"].(string)

		if id = strings.TrimSpace(id); !slices.Contains(stops, id) {
			continue
		}

		out = append(out, needlessStop{ID: id, Text: text})
	}

	return out
}

// manualInOrder is the manual entry ids of a draft's stored order, in that
// order: the entries the playbook runner stops at.
func manualInOrder(a store.Artifact) []string {
	var data struct {
		Manual []struct {
			ID string `json:"id"`
		} `json:"manual"`
		Order []string `json:"order"`
	}

	if len(a.Data) == 0 || json.Unmarshal(a.Data, &data) != nil {
		return nil
	}

	manual := make(map[string]bool, len(data.Manual))
	for _, m := range data.Manual {
		manual[m.ID] = true
	}

	var out []string

	for _, id := range data.Order {
		if manual[id] {
			out = append(out, id)
		}
	}

	return out
}

// stopsPayload is an unattended sign-off's stops: each manual entry still in
// the playbook's order, in that order, with its note when it has one and
// readiness's finding when the verdict the sign-off ends on judged it
// needless. A draft of one card has no playbook, so nothing stops.
func stopsPayload(draft contract.Draft, verdict store.Artifact) []any {
	out := []any{}

	if len(draft.Cards) < 2 {
		return out
	}

	manual := make(map[string]contract.Manual, len(draft.Manual))
	for _, m := range draft.Manual {
		manual[m.ID] = m
	}

	var ids []string

	for _, id := range draft.Order {
		if _, ok := manual[id]; ok {
			ids = append(ids, id)
		}
	}

	needless := map[string]string{}

	for _, s := range needlessStops(verdict, ids) {
		if _, named := needless[s.ID]; !named {
			needless[s.ID] = s.Text
		}
	}

	for _, id := range ids {
		entry := map[string]any{"id": id, "text": manual[id].Text}

		if note := strings.TrimSpace(manual[id].Note); note != "" {
			entry["note"] = note
		}

		if text, ok := needless[id]; ok {
			entry["needless"] = text
		}

		out = append(out, entry)
	}

	return out
}
