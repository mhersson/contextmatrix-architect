package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

// liveKeepalive spaces the live stream's heartbeat comments.
const liveKeepalive = 15 * time.Second

// stagePattern accepts an artifact's stage as CM forwards it: a pack stage
// id, signoff or emission, never a review's dotted pseudo-stage.
var stagePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// versionPattern accepts only the canonical decimal form of a version.
var versionPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// handleView serves the blueprint's view. Query parameters are ignored:
// CM signs a nonce into each read so two reads of one URI never replay.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sessionIDPattern.MatchString(id) {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid blueprint id")

		return
	}

	view, err := s.ctl.View(r.Context(), id)
	if !s.readOK(w, id, "view", err) {
		return
	}

	webhookcore.WriteJSON(w, http.StatusOK, view)
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	id, stageID := r.PathValue("id"), r.PathValue("stage")

	rawVersion := r.PathValue("version")

	version, err := strconv.Atoi(rawVersion)
	if !versionPattern.MatchString(rawVersion) || err != nil || !sessionIDPattern.MatchString(id) || !stagePattern.MatchString(stageID) {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid blueprint id, stage or version")

		return
	}

	art, err := s.ctl.Artifact(r.Context(), id, stageID, version)
	if !s.readOK(w, id, "artifact", err) {
		return
	}

	webhookcore.WriteJSON(w, http.StatusOK, art)
}

// handleLive streams the blueprint's live frames as Server-Sent Events: a
// connected comment at once, one data line per frame, a keepalive comment
// on the ticker. It ends when the client goes or the drain begins.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "streaming not supported")

		return
	}

	id := r.PathValue("id")
	if !sessionIDPattern.MatchString(id) {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid blueprint id")

		return
	}

	frames, err := s.ctl.Subscribe(r.Context(), id)
	if !s.readOK(w, id, "live", err) {
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		s.logger.Debug("live initial write failed", "error", err)

		return
	}

	flusher.Flush()

	s.logger.Info("blueprint live client connected", "blueprint", id, "remote_addr", r.RemoteAddr)

	ticker := time.NewTicker(s.keepalive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			s.logger.Info("blueprint live client disconnected", "blueprint", id, "remote_addr", r.RemoteAddr)

			return
		case <-s.liveDone:
			s.logger.Info("blueprint live client closed on drain", "blueprint", id, "remote_addr", r.RemoteAddr)

			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				s.logger.Debug("live keepalive write failed", "error", err)

				return
			}

			flusher.Flush()
		case f, ok := <-frames:
			if !ok {
				s.logger.Info("blueprint live stream ended", "blueprint", id, "remote_addr", r.RemoteAddr)

				return
			}

			data, err := json.Marshal(f)
			if err != nil {
				s.logger.Warn("live: encode frame", "blueprint", id, "error", err)

				continue
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				s.logger.Debug("live event write failed", "error", err)

				return
			}

			flusher.Flush()
		}
	}
}

// readOK answers a failed read, 404 for an unknown blueprint or version and
// 500 for anything else, and reports whether the read succeeded.
func (s *Server) readOK(w http.ResponseWriter, id, what string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, blueprint.ErrNotFound):
		webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no such blueprint or artifact")
	default:
		s.logger.Error("blueprint read failed", "blueprint", id, "read", what, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, what+" failed")
	}

	return false
}
