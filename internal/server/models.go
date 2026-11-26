package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
)

// modelView is one selectable model, in the same shape whether it comes
// from an agent's catalog or from a running session.
type modelView struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	IsDefault   bool   `json:"isDefault"`
	// Efforts lists the reasoning efforts the model accepts; empty when it
	// has no such setting.
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort"`
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func agentModelViews(models []ao.AgentModel) []modelView {
	out := make([]modelView, 0, len(models))
	for _, m := range models {
		label := m.Label
		if label == "" {
			label = m.ID
		}
		out = append(out, modelView{ID: m.ID, Label: label, IsDefault: m.IsDefault, Efforts: nonNil(m.Efforts), DefaultEffort: m.DefaultEffort})
	}
	return out
}

func conversationModelViews(models []ao.ConversationModel) []modelView {
	out := make([]modelView, 0, len(models))
	for _, m := range models {
		label := m.DisplayName
		if label == "" {
			label = m.ID
		}
		out = append(out, modelView{ID: m.ID, Label: label, Description: m.Description, IsDefault: m.Default, Efforts: nonNil(m.Efforts), DefaultEffort: m.DefaultEffort})
	}
	return out
}

func findModel(models []modelView, id string) (modelView, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return modelView{}, false
}

var errUnknownModel = errors.New("unknown model")

// checkModelChoice validates a model and effort against a model list. An
// empty model or effort means "the default" and is always accepted. AO
// stores whatever name it is given, so this check is what keeps a typo
// from breaking the next turn of a session.
func checkModelChoice(models []modelView, model, effort, agent string) error {
	if model == "" {
		if effort == "" {
			return nil
		}
		// An effort without a model applies to the default model.
		for _, m := range models {
			if m.IsDefault {
				if !slices.Contains(m.Efforts, effort) {
					return fmt.Errorf("%w: the default %s model does not support the effort %q", errUnknownModel, agent, effort)
				}
				return nil
			}
		}
		return nil
	}
	m, ok := findModel(models, model)
	if !ok {
		return fmt.Errorf("%w: %s has no model named %q", errUnknownModel, agent, model)
	}
	if effort != "" && !slices.Contains(m.Efforts, effort) {
		if len(m.Efforts) == 0 {
			return fmt.Errorf("%w: the model %s has no effort setting", errUnknownModel, model)
		}
		return fmt.Errorf("%w: the model %s supports the efforts %s, not %q", errUnknownModel, model, strings.Join(m.Efforts, ", "), effort)
	}
	return nil
}

type agentModelsResponse struct {
	Agent  string      `json:"agent"`
	Models []modelView `json:"models"`
}

func (s *Server) handleAgentModels(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	if !validHarness(agent) {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Agent must be copilot or codex.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	catalog, err := s.ao.AgentModels(ctx, agent)
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentModelsResponse{Agent: agent, Models: agentModelViews(catalog.Models)})
}

// validateSpawnModel checks the model and effort chosen for a new session
// against the agent's catalog. When AO cannot be asked the choice passes;
// the spawn itself will then report the problem.
func (s *Server) validateSpawnModel(ctx context.Context, req agentRequest) error {
	if req.Model == "" && req.Effort == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	catalog, err := s.ao.AgentModels(cctx, req.Harness)
	if err != nil || len(catalog.Models) == 0 {
		return nil
	}
	return checkModelChoice(agentModelViews(catalog.Models), req.Model, req.Effort, harnessLabel(req.Harness))
}

type sessionModelsResponse struct {
	SessionID string `json:"sessionId"`
	Harness   string `json:"harness"`
	// NeedsResume is true for a session whose agent is not running. Its
	// settings can still be changed; they apply once it is resumed.
	NeedsResume bool `json:"needsResume"`
	// Source is "session" when the list came from the running session and
	// "agent" when it is the agent's general catalog.
	Source   string                  `json:"source"`
	Models   []modelView             `json:"models"`
	Selected ao.ConversationSettings `json:"selected"`
}

// sessionModels returns the model picker data of a card's session. A
// session that is not running reports no models of its own, so the picker
// falls back to the catalog of its agent and keeps the stored selection.
func (s *Server) sessionModels(ctx context.Context, sessionID string) (*sessionModelsResponse, error) {
	s.mu.RLock()
	sess, known := s.sessions[sessionID]
	s.mu.RUnlock()
	out := &sessionModelsResponse{SessionID: sessionID, Source: "session", Models: []modelView{}}
	if known {
		out.Harness, out.NeedsResume = sess.Harness, sess.NeedsResume
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conv, err := s.ao.ConversationModels(cctx, sessionID)
	if err != nil {
		// Without the session's own answer the catalog can still fill the
		// picker, but only when the session is known at all.
		if !known || !validHarness(sess.Harness) {
			return nil, err
		}
	} else {
		out.Selected = conv.Selected
		out.Models = conversationModelViews(conv.Models)
	}
	if len(out.Models) == 0 && validHarness(out.Harness) {
		catalog, cerr := s.ao.AgentModels(cctx, out.Harness)
		switch {
		case cerr == nil:
			out.Source, out.Models = "agent", agentModelViews(catalog.Models)
		case err != nil:
			return nil, err
		}
	}
	return out, nil
}

// cardSession returns the session ID of a card, or writes the error.
func (s *Server) cardSession(w http.ResponseWriter, r *http.Request) (*db.Card, bool) {
	card, err := s.store.GetCard(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return nil, false
	}
	if card.AOSessionID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION", "This card has no agent session.")
		return nil, false
	}
	return card, true
}

func (s *Server) handleCardModels(w http.ResponseWriter, r *http.Request) {
	card, ok := s.cardSession(w, r)
	if !ok {
		return
	}
	out, err := s.sessionModels(r.Context(), card.AOSessionID)
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type sessionSettingsResponse struct {
	SessionID   string                  `json:"sessionId"`
	NeedsResume bool                    `json:"needsResume"`
	Selected    ao.ConversationSettings `json:"selected"`
}

// handleCardSettings switches the model, reasoning effort, and/or approval
// mode a session uses from its next turn on. Omitted fields are left
// unchanged so changing model does not alter approval mode, and vice versa.
func (s *Server) handleCardSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Model           *string `json:"model"`
		ReasoningEffort *string `json:"reasoningEffort"`
		ApprovalMode    *string `json:"approvalMode"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	card, ok := s.cardSession(w, r)
	if !ok {
		return
	}
	if req.Model == nil && req.ReasoningEffort == nil && req.ApprovalMode == nil {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Send a model, reasoningEffort, or approvalMode.")
		return
	}
	for _, v := range []*string{req.Model, req.ReasoningEffort, req.ApprovalMode} {
		if v != nil {
			*v = strings.TrimSpace(*v)
		}
	}
	if req.ApprovalMode != nil && !ValidApprovalMode(*req.ApprovalMode) {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Approval mode must be empty, default, accept-edits, auto, or bypass-permissions.")
		return
	}
	current, err := s.sessionModels(ctx, card.AOSessionID)
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	if req.Model != nil || req.ReasoningEffort != nil {
		model, effort := current.Selected.Model, current.Selected.ReasoningEffort
		if req.Model != nil {
			model = *req.Model
		}
		if req.ReasoningEffort != nil {
			effort = *req.ReasoningEffort
		}
		agent := harnessLabel(current.Harness)
		if agent == "" {
			agent = "The agent"
		}
		if len(current.Models) > 0 {
			if err := checkModelChoice(current.Models, model, effort, agent); err != nil {
				writeError(w, http.StatusBadRequest, "UNKNOWN_MODEL", capitalize(strings.TrimPrefix(err.Error(), errUnknownModel.Error()+": "))+".")
				return
			}
		}
	}
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	selected, err := s.ao.UpdateConversationSettings(uctx, card.AOSessionID, ao.ConversationTurnSettings{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		ApprovalMode:    req.ApprovalMode,
	})
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	var parts []string
	if req.Model != nil || req.ReasoningEffort != nil {
		what := "the default model"
		if selected.Model != "" {
			what = selected.Model
		}
		m := "model set to " + what
		if selected.ReasoningEffort != "" {
			m += " (effort " + selected.ReasoningEffort + ")"
		}
		parts = append(parts, m)
	}
	if req.ApprovalMode != nil {
		mode := "AO default"
		if selected.ApprovalMode != "" {
			mode = selected.ApprovalMode
		}
		parts = append(parts, "approval mode set to "+mode)
	}
	msg := "Agent " + strings.Join(parts, ", ")
	if current.NeedsResume {
		msg += "; applies once the session is resumed"
	}
	_ = s.store.AddEvent(ctx, card.ID, db.EventSession, msg)
	s.hub.Publish()
	writeJSON(w, http.StatusOK, sessionSettingsResponse{SessionID: card.AOSessionID, NeedsResume: current.NeedsResume, Selected: *selected})
}
