package webhook

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

// sessionIDPattern accepts CM's ULIDs and any other path-safe id.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var p protocol.ChatStartPayload
	if !webhookcore.Decode(w, r, &p) {
		return
	}

	if !sessionIDPattern.MatchString(p.SessionID) {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid session_id")

		return
	}

	// Fail-closed, as the chat backend does: no endpoint means no inference
	// for the session's whole lifetime.
	if p.LLMEndpoint == nil {
		s.logger.Warn("chat/start: no llm endpoint", "session_id", p.SessionID)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "CM did not provision an llm endpoint")

		return
	}

	if p.Architect == nil {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "architect block required")

		return
	}

	if len(p.Architect.Projects) == 0 {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "architect.projects must not be empty")

		return
	}

	if !protocol.ValidArchitectMode(p.Architect.Mode) {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "architect.mode is not a known mode")

		return
	}

	if err := s.ctl.Start(r.Context(), blueprint.StartInput{
		ID:         p.SessionID,
		Projects:   p.Architect.Projects,
		Mode:       p.Architect.Mode,
		CostCapUSD: p.Architect.CostCapUSD,
		Model:      p.Model,
		Endpoint:   *p.LLMEndpoint,
	}); err != nil {
		s.logger.Error("chat/start: controller start failed", "session_id", p.SessionID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "start failed")

		return
	}

	s.logger.Info("chat/start: blueprint session started", "session_id", p.SessionID,
		"projects", p.Architect.Projects, "mode", p.Architect.Mode)

	// CM records ContainerID on the session; there is no container, so the
	// blueprint id stands in.
	webhookcore.WriteJSON(w, http.StatusAccepted, protocol.ChatStartResponse{OK: true, ContainerID: p.SessionID})
}

func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) {
	var p protocol.ChatEndPayload
	if !webhookcore.Decode(w, r, &p) {
		return
	}

	err := s.ctl.End(r.Context(), p.SessionID)

	switch {
	case errors.Is(err, blueprint.ErrNotFound):
		// Idempotent: nothing to detach.
		webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{OK: true})
	case err != nil:
		s.logger.Error("chat/end: failed", "session_id", p.SessionID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "end failed")
	default:
		s.logger.Info("chat/end: session detached", "session_id", p.SessionID)
		webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})
	}
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var p protocol.MessagePayload
	if !webhookcore.Decode(w, r, &p) {
		return
	}

	if p.SessionID == "" {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "session_id required")

		return
	}

	if s.dedup.CheckAndRecord(p.SessionID, p.MessageID) {
		webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{
			OK: true, Message: "duplicate message acknowledged", MessageID: p.MessageID,
		})

		return
	}

	err := s.ctl.Message(r.Context(), p.SessionID, p.Content, p.MessageID)

	switch {
	case errors.Is(err, blueprint.ErrNotFound):
		s.dedup.Rollback(p.SessionID, p.MessageID)
		webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no such blueprint session")
	case err != nil:
		s.dedup.Rollback(p.SessionID, p.MessageID)
		s.logger.Error("message: failed", "session_id", p.SessionID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "message failed")
	default:
		webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true, MessageID: p.MessageID})
	}
}
