package cli

import (
	"encoding/json"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// eventSink renders each controller event onto the hub. The blueprint's key
// is retained from its first event, so entries published during /chat/start,
// before CM connects to /logs, and during a consumer reconnect reach the
// panel when the consumer arrives. An ended session releases the backlog.
// An event after which nothing runs until the user replies is followed by an
// idle status frame: CM clears its working indicator only on that frame.
// Status frames are published, never stored.
func eventSink(hub *logbridge.Hub) func(blueprint.Event) {
	return func(e blueprint.Event) {
		if e.Kind == blueprint.EventCreated || e.Kind == blueprint.EventReopened {
			hub.Retain(e.BlueprintID)
		}

		if entry, ok := logEntryFor(e); ok {
			hub.Publish(entry)
		}

		if waitsOnUser(e) {
			ts := e.Time
			if ts.IsZero() {
				ts = time.Now().UTC()
			}

			hub.Publish(protocol.LogEntry{Timestamp: ts, SessionID: e.BlueprintID, Type: "status", Content: "idle"})
		}

		if e.Kind == blueprint.EventEnded {
			hub.Release(e.BlueprintID)
		}
	}
}

func waitsOnUser(e blueprint.Event) bool {
	switch e.Kind {
	case blueprint.EventAborted,
		blueprint.EventAwaitInput, blueprint.EventGate, blueprint.EventSignOff, blueprint.EventRunStarted, blueprint.EventWatchEnded:
		return true
	case blueprint.EventError:
		// A refused responder leaves the asking stage running, a failed
		// mockup responder leaves its revision to be used, and a stale reply
		// that lands while a run works leaves it working.
		duringRun, _ := e.Payload["during_run"].(bool)

		return !goesOn(str(e.Payload["reason"])) && !duringRun
	case blueprint.EventRound:
		// An open round a human answers waits on the user. A closed one, one
		// the responder answers, one whose every question has an answer and
		// every assumption a decision, which closes and goes on at once, or
		// one with chat its stage has not seen, which a chat run answers,
		// leaves the stage working.
		_, frame, ok := roundFrame(e.Payload)
		chatPending := len(frame.Chat) > int(num(e.Payload["seen"]))

		// A mockup asks no questions: it waits on the person until a use or
		// skip decides it.
		if m, isMockup := e.Payload["mockup"].(map[string]any); isMockup {
			return ok && !frame.Closed && !frame.Responder && str(m["action"]) == "" && !chatPending
		}

		return ok && !frame.Closed && !frame.Responder && !blueprint.RoundAnswered(frame) && !chatPending
	case blueprint.EventMockupOffer:
		// A shown offer waits on its person; a decided one runs what follows.
		return str(e.Payload["state"]) == "shown"
	case blueprint.EventPolicy:
		// CM arms its working indicator on every message; a gate reply that
		// leaves nothing running must clear it.
		duringRun, _ := e.Payload["during_run"].(bool)

		return !duringRun
	case blueprint.EventCap:
		// CM arms its working indicator on every message; a cap reply that
		// leaves nothing running must clear it, and one that resumes a stage
		// starts it.
		duringRun, _ := e.Payload["during_run"].(bool)
		resumes, _ := e.Payload["resumes"].(string)

		return !duringRun && resumes == ""
	case blueprint.EventMode:
		// CM arms its working indicator on every message; a mode reply that
		// leaves nothing running must clear it, and one that hands a round to
		// the responder, runs the sign-off again or decides a gate starts work.
		duringRun, _ := e.Payload["during_run"].(bool)
		signoff, _ := e.Payload["signoff"].(bool)

		return !duringRun && num(e.Payload["hands_over"]) == 0 && !signoff && str(e.Payload["gate"]) == ""
	case blueprint.EventResume:
		// CM arms its working indicator on every message; a resume reply that
		// runs nothing and finds nothing running must clear it.
		nothing, _ := e.Payload["nothing"].(bool)
		duringRun, _ := e.Payload["during_run"].(bool)

		return nothing && !duringRun
	case blueprint.EventReport:
		// At a ready end the sign-off follows on the held run slot; the
		// sign-off, error or emission that ends it is followed by the idle
		// frame.
		signoff, _ := e.Payload["signoff"].(bool)

		return !signoff
	case blueprint.EventEmitted:
		// In yolo mode the run start follows; the run start or its error is
		// followed by the idle frame.
		runFollows, _ := e.Payload["run_follows"].(bool)

		return !runFollows
	case blueprint.EventStageEnded:
		// After a completed stage the next one may start at once, and a
		// responder's end leaves its stage working; the await, gate, report
		// or error that ends a chain is followed by the idle frame. A run
		// that asked leaves it to its round's snapshot.
		status, _ := e.Payload["status"].(string)
		kind, _ := e.Payload["kind"].(string)

		return status != store.RunDone && status != store.RunAsked && kind != store.RunKindResponder
	case blueprint.EventRetroReport:
		// A pass's findings end it.
		return true
	case blueprint.EventLessonDecided:
		// An answer to a lesson round runs nothing after its decisions.
		return true
	case blueprint.EventRetroReply:
		// CM arms its working indicator on every message; a retrospective
		// reply that leaves nothing running, or a pass that stopped, must
		// clear it.
		duringRun, _ := e.Payload["during_run"].(bool)

		return !duringRun
	case blueprint.EventExported:
		// CM arms its working indicator on every message, so an export that
		// leaves nothing running must clear it.
		duringRun, _ := e.Payload["during_run"].(bool)

		return !duringRun
	case blueprint.EventNote:
		// A note taken while a stage runs leaves the run working, and a note
		// that re-runs a stage starts one.
		duringRun, _ := e.Payload["during_run"].(bool)
		retries, _ := e.Payload["retries"].(string)

		return !duringRun && retries == ""
	default:
		return false
	}
}

// harnessForwarder bridges each harness line onto the hub, except the
// harness's stop state changes: the bridge would show their raw JSON, and
// the stage-ended entry already reports how the run ended. The controller
// has stored every line before it gets here.
func harnessForwarder(hub *logbridge.Hub) func(string, []byte) {
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub, SurfaceRunState: true})

	return func(id string, line []byte) {
		if isStopLine(line) {
			return
		}

		bridge.BridgeLine(logbridge.Key{SessionID: id}, line, false)
	}
}

func isStopLine(line []byte) bool {
	var ev struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}

	if err := json.Unmarshal(line, &ev); err != nil {
		return false
	}

	_, stop := ev.Data["stop"]

	return ev.Kind == "state_change" && stop
}
