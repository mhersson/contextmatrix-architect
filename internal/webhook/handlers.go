package webhook

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"

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

	// Controller calls are several store steps (Start adds crash recovery);
	// a client that hangs up must not cut one off midway.
	in := blueprint.StartInput{
		ID:                  p.SessionID,
		Projects:            p.Architect.Projects,
		Mode:                p.Architect.Mode,
		CostCapUSD:          p.Architect.CostCapUSD,
		Model:               p.Model,
		Endpoint:            *p.LLMEndpoint,
		Price:               p.Architect.Price,
		ContextWindow:       p.Architect.ContextWindow,
		PlainLanguage:       p.Architect.PlainLanguage,
		MCPAPIKey:           p.MCPAPIKey,
		GitCredentialsToken: p.GitCredentialsToken,
	}

	// No research block is research off.
	if rs := p.Architect.Research; rs != nil {
		in.SearchProvider, in.SearchAPIKey = rs.Provider, rs.APIKey
	}

	err := s.ctl.Start(context.WithoutCancel(r.Context()), in)

	switch {
	case errors.Is(err, blueprint.ErrShuttingDown):
		writeDraining(w)

		return
	case err != nil:
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

	err := s.ctl.End(context.WithoutCancel(r.Context()), p.SessionID)

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

	if p.Mockup != nil && !slices.Contains([]string{protocol.MockupUse, protocol.MockupSkip, protocol.MockupChat}, p.Mockup.Action) {
		webhookcore.WriteError(w, http.StatusBadRequest, protocol.CodeInvalidField, "mockup.action must be use, skip or chat")

		return
	}

	if s.dedup.CheckAndRecord(p.SessionID, p.MessageID) {
		webhookcore.WriteJSON(w, http.StatusOK, protocol.SuccessResponse{
			OK: true, Message: "duplicate message acknowledged", MessageID: p.MessageID,
		})

		return
	}

	ctx := context.WithoutCancel(r.Context())

	var err error

	// Content is only the readable summary of a structured reply, or a
	// mockup chat's text; it is never parsed as a command.
	switch {
	case p.Round != nil:
		err = s.ctl.Reply(ctx, p.SessionID, *p.Round, p.MessageID)
	case p.Mockup != nil:
		err = s.ctl.Mockup(ctx, p.SessionID, *p.Mockup, p.Content, p.MessageID)
	default:
		switch name, args := blueprint.ParseCommand(p.Content); name {
		case blueprint.CommandAbort:
			err = s.ctl.Abort(ctx, p.SessionID, p.MessageID)
		case blueprint.CommandExport:
			err = s.ctl.Export(ctx, p.SessionID, p.MessageID)
		case blueprint.CommandGate:
			err = s.ctl.SetPolicy(ctx, p.SessionID, args[0], args[1], p.MessageID)
		case blueprint.CommandCap:
			err = s.ctl.SetCap(ctx, p.SessionID, args[0], p.MessageID)
		case blueprint.CommandMode:
			err = s.ctl.SetMode(ctx, p.SessionID, args[0], p.MessageID)
		case blueprint.CommandResume:
			err = s.ctl.Resume(ctx, p.SessionID, p.MessageID)
		case blueprint.CommandRetrospective:
			err = s.ctl.Retrospective(ctx, p.SessionID, p.MessageID)
		case blueprint.CommandLessons:
			err = s.ctl.Lessons(ctx, p.SessionID, p.MessageID)
		case blueprint.CommandRetire:
			err = s.ctl.Retire(ctx, p.SessionID, args[0], p.MessageID)
		default:
			err = s.ctl.Message(ctx, p.SessionID, p.Content, p.MessageID)
		}
	}

	switch {
	case errors.Is(err, blueprint.ErrNotFound):
		s.dedup.Rollback(p.SessionID, p.MessageID)
		webhookcore.WriteError(w, http.StatusNotFound, protocol.CodeNotFound, "no such blueprint session")
	case errors.Is(err, blueprint.ErrShuttingDown):
		s.dedup.Rollback(p.SessionID, p.MessageID)
		writeDraining(w)
	case err != nil:
		s.dedup.Rollback(p.SessionID, p.MessageID)
		s.logger.Error("message: failed", "session_id", p.SessionID, "error", err)
		webhookcore.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "message failed")
	default:
		webhookcore.WriteJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true, MessageID: p.MessageID})
	}
}

// writeDraining answers as backendkit's drain gate does, so CM treats a
// controller that is shutting down like a draining backend.
func writeDraining(w http.ResponseWriter) {
	webhookcore.WriteError(w, http.StatusServiceUnavailable, protocol.CodeDraining, "backend is draining")
}
