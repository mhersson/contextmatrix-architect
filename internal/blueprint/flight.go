package blueprint

import (
	"encoding/json"
	"slices"
	"sync"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// flight is a run in flight as the view shows it: who runs it and under
// which cap, set when it starts, and what its harness lines have counted so
// far, written by its sink under mu.
type flight struct {
	runID     int64
	stage     string
	kind      string
	persona   string
	model     string
	round     int
	startedAt time.Time
	price     *protocol.ModelPrice
	// limit is read afresh by each view: a cap reply moves what binds a
	// priced run.
	limit runLimit

	mu        sync.Mutex
	turns     int
	toolCalls int
	tokens    int64
	// spent is the run's own usage, each line at the higher of the cost the
	// endpoint reported and its priced tokens; charged is what runs served on
	// its behalf cost once they ended.
	spent   float64
	charged float64
}

// flightView is a copy of a flight for one view.
type flightView struct {
	runID            int64
	stage, kind      string
	persona, model   string
	round            int
	startedAt        time.Time
	priced           bool
	capUSD           float64
	turns, toolCalls int
	tokens           int64
	spent, charged   float64
}

// count adds one harness line: a model request is a turn, a tool call is
// counted, and a usage line adds its tokens and its cost. A nil flight
// counts nothing.
func (f *flight) count(line []byte) {
	if f == nil {
		return
	}

	var ev struct {
		Kind string `json:"kind"`
		Data struct {
			Turn       int     `json:"turn"`
			Prompt     int64   `json:"prompt_tokens"`
			Completion int64   `json:"completion_tokens"`
			CacheRead  int64   `json:"cache_read_tokens"`
			CacheWrite int64   `json:"cache_creation_tokens"`
			Cost       float64 `json:"cost_usd"`
		} `json:"data"`
	}

	if json.Unmarshal(line, &ev) != nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch ev.Kind {
	case "model_request":
		f.turns = max(f.turns, ev.Data.Turn)
	case "tool_call":
		f.toolCalls++
	case "usage":
		u := store.RunUsage{
			PromptTokens: ev.Data.Prompt, CompletionTokens: ev.Data.Completion,
			CacheReadTokens: ev.Data.CacheRead, CacheWriteTokens: ev.Data.CacheWrite,
		}
		f.tokens += u.PromptTokens + u.CompletionTokens + u.CacheReadTokens + u.CacheWriteTokens
		f.spent += max(ev.Data.Cost, priced(f.price, u))
	}
}

// charge adds the cost of a run served on this run's behalf.
func (f *flight) charge(usd float64) {
	if f == nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.charged += usd
}

func (f *flight) view() flightView {
	// Outside mu: read locks the cap cell and the asking run's sink.
	capUSD := f.limit.read().usd

	f.mu.Lock()
	defer f.mu.Unlock()

	return flightView{
		runID: f.runID, stage: f.stage, kind: f.kind, persona: f.persona, model: f.model, round: f.round,
		startedAt: f.startedAt, priced: f.price != nil, capUSD: capUSD,
		turns: f.turns, toolCalls: f.toolCalls, tokens: f.tokens, spent: f.spent, charged: f.charged,
	}
}

// fly records f as in flight for blueprint id until the returned func lands
// it. A run registers before its stage_started event, so a view built on
// that event sees it.
func (c *Controller) fly(id string, f *flight) func() {
	c.mu.Lock()
	c.flights[id] = append(c.flights[id], f)
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		c.flights[id] = slices.DeleteFunc(c.flights[id], func(g *flight) bool { return g == f })
		if len(c.flights[id]) == 0 {
			delete(c.flights, id)
		}
	}
}

// inFlight copies blueprint id's runs in flight, outermost first: a
// responder run sits inside the stage run that asked.
func (c *Controller) inFlight(id string) []flightView {
	c.mu.Lock()
	fs := slices.Clone(c.flights[id])
	c.mu.Unlock()

	out := make([]flightView, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.view())
	}

	return out
}
