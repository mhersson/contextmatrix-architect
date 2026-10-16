package cli

import (
	"encoding/json"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

// eventSink renders each controller event onto the hub. An event after which
// nothing runs until the user replies is followed by an idle status frame:
// CM clears its working indicator only on that frame. Status frames are
// published, never stored.
func eventSink(hub *logbridge.Hub) func(blueprint.Event) {
	return func(e blueprint.Event) {
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
	}
}

func waitsOnUser(e blueprint.Event) bool {
	switch e.Kind {
	case blueprint.EventQuestion, blueprint.EventStageEnded, blueprint.EventError, blueprint.EventAborted:
		return true
	case blueprint.EventNote:
		// A note taken while a stage runs leaves the run working.
		duringRun, _ := e.Payload["during_run"].(bool)

		return !duringRun
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
