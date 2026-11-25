package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/github"
)

const settingShowOrchestrator = "show_orchestrator"

type cardView struct {
	*db.Card
	// Session is the cached AO session, or nil when the card has none or AO
	// does not report it.
	Session *ao.Session `json:"session"`
	// SessionMissing is true when the card references a session AO does not
	// currently report.
	SessionMissing bool `json:"sessionMissing"`
}

type aoStatus struct {
	Connected      bool         `json:"connected"`
	URL            string       `json:"url"`
	Error          string       `json:"error"`
	LastSyncAt     string       `json:"lastSyncAt"`
	Projects       []ao.Project `json:"projects"`
	DefaultProject string       `json:"defaultProject"`
}

type githubStatus struct {
	Syncing         bool   `json:"syncing"`
	LastSyncAt      string `json:"lastSyncAt"`
	Error           string `json:"error"`
	IntervalSeconds int    `json:"intervalSeconds"`
}

type boardSettings struct {
	ShowOrchestrator bool   `json:"showOrchestrator"`
	DefaultRepo      string `json:"defaultRepo"`
}

type boardView struct {
	Version int64 `json:"version"`
	// CurrentUser is the GitHub login of the maintainer, or "" when unknown.
	CurrentUser string        `json:"currentUser"`
	Columns     []db.Column   `json:"columns"`
	Cards       []cardView    `json:"cards"`
	AO          aoStatus      `json:"ao"`
	GitHub      githubStatus  `json:"github"`
	Settings    boardSettings `json:"settings"`
	UnreadCount int           `json:"unreadCount"`
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) view(c *db.Card) cardView {
	v := cardView{Card: c}
	if c.AOSessionID == "" {
		return v
	}
	s.mu.RLock()
	sess, ok := s.sessions[c.AOSessionID]
	s.mu.RUnlock()
	if ok {
		v.Session = &sess
	} else {
		v.SessionMissing = true
	}
	return v
}

// currentUser returns the GitHub login of the maintainer, or "".
func (s *Server) currentUser() string {
	if s.cfg.CurrentUser != "" {
		return s.cfg.CurrentUser
	}
	if r, ok := s.gh.(userResolver); ok {
		return r.User()
	}
	return ""
}

// resolveCurrentUser retries the login detection that failed at startup,
// for example because the machine was offline.
func (s *Server) resolveCurrentUser(ctx context.Context) {
	if s.currentUser() != "" {
		return
	}
	r, ok := s.gh.(userResolver)
	if !ok {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if login, err := r.DetectUser(dctx); err == nil {
		s.log.Info("github user detected", "login", login)
		s.hub.Publish()
	}
}

func (s *Server) showOrchestrator(ctx context.Context) bool {
	v, err := s.store.GetSetting(ctx, settingShowOrchestrator, "false")
	return err == nil && v == "true"
}

// defaultProject returns the AO project new sessions are spawned in.
func (s *Server) defaultProject() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg.AOProject != "" {
		return s.cfg.AOProject
	}
	if len(s.projects) > 0 {
		return s.projects[0].ID
	}
	return ""
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	version := s.hub.Version()
	cols, err := s.store.ListColumns(ctx)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	cards, err := s.store.ListCards(ctx)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := boardView{
		Version:     version,
		CurrentUser: s.currentUser(),
		Columns:     cols,
		Cards:       make([]cardView, 0, len(cards)),
		Settings: boardSettings{
			ShowOrchestrator: s.showOrchestrator(ctx),
			DefaultRepo:      s.cfg.DefaultRepo,
		},
	}
	for _, c := range cards {
		if c.HasUnreadUpdates {
			out.UnreadCount++
		}
		out.Cards = append(out.Cards, s.view(c))
	}
	defaultProject := s.defaultProject()
	s.mu.RLock()
	out.AO = aoStatus{
		Connected:      s.aoConnected,
		URL:            s.cfg.AOURL,
		Error:          s.aoError,
		LastSyncAt:     formatTime(s.aoLastSync),
		Projects:       append([]ao.Project{}, s.projects...),
		DefaultProject: defaultProject,
	}
	out.GitHub = githubStatus{
		Syncing:         s.ghSyncing,
		LastSyncAt:      formatTime(s.ghLastSync),
		Error:           s.ghLastError,
		IntervalSeconds: int(s.cfg.GitHubSyncInterval / time.Second),
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShowOrchestrator *bool `json:"showOrchestrator"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ShowOrchestrator != nil {
		if err := s.store.SetSetting(r.Context(), settingShowOrchestrator, strconv.FormatBool(*req.ShowOrchestrator)); err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.hub.Publish()
		s.requestAOSync()
	}
	writeJSON(w, http.StatusOK, boardSettings{
		ShowOrchestrator: s.showOrchestrator(r.Context()),
		DefaultRepo:      s.cfg.DefaultRepo,
	})
}

// Columns

func (s *Server) handleListColumns(w http.ResponseWriter, r *http.Request) {
	cols, err := s.store.ListColumns(r.Context())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols})
}

func (s *Server) handleCreateColumn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
		Color string `json:"color"`
		Role  string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	col, err := s.store.CreateColumn(r.Context(), req.Title, req.Color, req.Role)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusCreated, col)
}

func (s *Server) handlePatchColumn(w http.ResponseWriter, r *http.Request) {
	var req db.ColumnPatch
	if !decodeBody(w, r, &req) {
		return
	}
	col, err := s.store.UpdateColumn(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, col)
}

func (s *Server) handleReorderColumns(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.store.ReorderColumns(r.Context(), req.IDs); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	s.handleListColumns(w, r)
}

func (s *Server) handleDeleteColumn(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.DeleteColumn(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"movedCardsTo": target})
}

// columnIndex is a lookup of the board columns by ID and automation role.
type columnIndex struct {
	list   []db.Column
	byID   map[string]db.Column
	byRole map[string]db.Column
}

func (s *Server) columnIndex(ctx context.Context) (*columnIndex, error) {
	cols, err := s.store.ListColumns(ctx)
	if err != nil {
		return nil, err
	}
	idx := &columnIndex{list: cols, byID: map[string]db.Column{}, byRole: map[string]db.Column{}}
	for _, c := range cols {
		idx.byID[c.ID] = c
		if c.Role != "" {
			if _, taken := idx.byRole[c.Role]; !taken {
				idx.byRole[c.Role] = c
			}
		}
	}
	return idx, nil
}

// intake returns the column new cards land in: the inbox-role column, or
// the left-most one.
func (idx *columnIndex) intake() (db.Column, bool) {
	if c, ok := idx.byRole[db.RoleInbox]; ok {
		return c, true
	}
	if len(idx.list) > 0 {
		return idx.list[0], true
	}
	return db.Column{}, false
}

func (idx *columnIndex) title(id string) string {
	if c, ok := idx.byID[id]; ok {
		return c.Title
	}
	return id
}

// Cards

func (s *Server) handleListCards(w http.ResponseWriter, r *http.Request) {
	cards, err := s.store.ListCards(r.Context())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := make([]cardView, 0, len(cards))
	for _, c := range cards {
		out = append(out, s.view(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": out})
}

func (s *Server) handleGetCard(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCard(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(c))
}

type agentRequest struct {
	Harness   string `json:"harness"`
	Prompt    string `json:"prompt"`
	ProjectID string `json:"projectId"`
	// Model and Effort choose the model and reasoning effort of the new
	// session; empty means the agent's default.
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type createCardRequest struct {
	Title       string        `json:"title"`
	Category    string        `json:"category"`
	ColumnID    string        `json:"columnId"`
	Tags        []string      `json:"tags"`
	Notes       string        `json:"notes"`
	Summary     string        `json:"summary"`
	AutoMove    *bool         `json:"autoMove"`
	GitHubRef   string        `json:"githubRef"`
	AOSessionID string        `json:"aoSessionId"`
	Agent       *agentRequest `json:"agent"`
	// Model and Effort are a shorthand for the fields of Agent.
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type cardResponse struct {
	Card     cardView `json:"card"`
	Warnings []string `json:"warnings"`
}

func (s *Server) handleCreateCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createCardRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.GitHubRef = strings.TrimSpace(req.GitHubRef)
	req.AOSessionID = strings.TrimSpace(req.AOSessionID)
	if req.Category != "" && !db.ValidCategory(req.Category) {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Unknown category.")
		return
	}
	if req.Agent != nil {
		if req.AOSessionID != "" {
			writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Choose either an existing session or a new agent session, not both.")
			return
		}
		if !validHarness(req.Agent.Harness) {
			writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Agent must be copilot or codex.")
			return
		}
		if req.Agent.Model == "" {
			req.Agent.Model = req.Model
		}
		if req.Agent.Effort == "" {
			req.Agent.Effort = req.Effort
		}
		req.Agent.Model, req.Agent.Effort = strings.TrimSpace(req.Agent.Model), strings.TrimSpace(req.Agent.Effort)
		// Checked before the card exists, so a typo does not leave a card
		// behind without its session.
		if err := s.validateSpawnModel(ctx, *req.Agent); err != nil {
			writeError(w, http.StatusBadRequest, "UNKNOWN_MODEL", capitalize(strings.TrimPrefix(err.Error(), errUnknownModel.Error()+": "))+".")
			return
		}
	}

	warnings := []string{}
	var ref *github.Ref
	var item *github.Item
	if req.GitHubRef != "" {
		parsed, err := github.ParseRef(req.GitHubRef, s.cfg.DefaultRepo)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_GITHUB_REF", "Unrecognized GitHub reference. Use #123, owner/repo#123 or a pull request or issue URL.")
			return
		}
		ref = &parsed
		fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		item, err = s.gh.Fetch(fetchCtx, ref.Repo, ref.Number, "")
		cancel()
		switch {
		case errors.Is(err, github.ErrNotFound):
			writeError(w, http.StatusNotFound, "GITHUB_NOT_FOUND", fmt.Sprintf("%s was not found on GitHub.", ref))
			return
		case err != nil:
			if req.Title == "" {
				s.writeGitHubError(w, err)
				return
			}
			warnings = append(warnings, fmt.Sprintf("GitHub could not be reached; %s will be synced on the next cycle.", ref))
		}
	}
	if req.Title == "" && item != nil {
		req.Title = item.Title
	}
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "A title is required.")
		return
	}
	if req.AOSessionID != "" {
		if other, err := s.store.CardBySession(ctx, req.AOSessionID); err != nil {
			s.writeStoreError(w, err)
			return
		} else if other != nil {
			writeError(w, http.StatusConflict, "SESSION_ALREADY_LINKED", fmt.Sprintf("Session %s is already linked to the card %q.", req.AOSessionID, other.Title))
			return
		}
	}

	cols, err := s.columnIndex(ctx)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	columnID := req.ColumnID
	if columnID == "" {
		intake, ok := cols.intake()
		if !ok {
			writeError(w, http.StatusConflict, "NO_COLUMNS", "The board has no columns.")
			return
		}
		columnID = intake.ID
	}
	category := req.Category
	if category == "" {
		switch {
		case item != nil && item.Kind == github.KindPR:
			category = db.CategoryPRReview
		case item != nil && item.Kind == github.KindIssue:
			category = db.CategoryIssue
		default:
			category = InferCategory(req.Title)
		}
	}
	card := &db.Card{
		Title:       req.Title,
		ColumnID:    columnID,
		Category:    category,
		Tags:        req.Tags,
		Notes:       req.Notes,
		Summary:     strings.TrimSpace(req.Summary),
		AutoMove:    req.AutoMove == nil || *req.AutoMove,
		AOSessionID: req.AOSessionID,
		Origin:      db.OriginManual,
	}
	if card.Summary != "" {
		card.SummarySource = db.SummaryManual
	}
	events := []db.EventInput{{Kind: db.EventCreated, Message: "Card created in " + cols.title(columnID)}}
	if req.AOSessionID != "" {
		events = append(events, db.EventInput{Kind: db.EventSession, Message: "Linked AO session " + req.AOSessionID})
	}
	if ref != nil {
		card.GHRepo, card.GHNumber = ref.Repo, ref.Number
	}
	card, err = s.store.CreateCard(ctx, card, events...)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if req.AOSessionID != "" {
		_ = s.store.UnignoreSession(ctx, req.AOSessionID)
	}
	if item != nil {
		if updated, _, err := s.applyGitHubItem(ctx, card.ID, item); err != nil {
			warnings = append(warnings, "GitHub link could not be saved: "+err.Error())
		} else {
			card = updated
		}
	}
	if req.Agent != nil {
		updated, err := s.startSession(ctx, card.ID, *req.Agent)
		if err != nil {
			warnings = append(warnings, "The card was created, but the agent session could not be started: "+s.describeError(err))
		} else {
			card = updated
		}
	}
	s.hub.Publish()
	writeJSON(w, http.StatusCreated, cardResponse{Card: s.view(card), Warnings: warnings})
}

type patchCardRequest struct {
	Title       *string   `json:"title"`
	Category    *string   `json:"category"`
	Tags        *[]string `json:"tags"`
	Summary     *string   `json:"summary"`
	Notes       *string   `json:"notes"`
	AutoMove    *bool     `json:"autoMove"`
	ColumnID    *string   `json:"columnId"`
	AOSessionID *string   `json:"aoSessionId"`
}

func (s *Server) handlePatchCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var req patchCardRequest
	if !decodeBody(w, r, &req) {
		return
	}
	cols, err := s.columnIndex(ctx)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if req.ColumnID != nil {
		if _, ok := cols.byID[*req.ColumnID]; !ok {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "No such column.")
			return
		}
	}
	if req.AOSessionID != nil {
		sid := strings.TrimSpace(*req.AOSessionID)
		req.AOSessionID = &sid
		if sid != "" {
			other, err := s.store.CardBySession(ctx, sid)
			if err != nil {
				s.writeStoreError(w, err)
				return
			}
			if other != nil && other.ID != id {
				writeError(w, http.StatusConflict, "SESSION_ALREADY_LINKED", fmt.Sprintf("Session %s is already linked to the card %q.", sid, other.Title))
				return
			}
		}
	}
	var oldSession, newSession string
	card, err := s.store.UpdateCard(ctx, id, func(c *db.Card, m *db.Mutation) error {
		if req.Title != nil {
			c.Title = *req.Title
		}
		if req.Category != nil {
			c.Category = *req.Category
		}
		if req.Tags != nil {
			c.Tags = *req.Tags
		}
		if req.Notes != nil {
			c.Notes = *req.Notes
		}
		if req.AutoMove != nil && *req.AutoMove != c.AutoMove {
			c.AutoMove = *req.AutoMove
			if c.AutoMove {
				m.Event(db.EventUpdate, "Auto-move enabled")
			} else {
				m.Event(db.EventUpdate, "Auto-move disabled")
			}
		}
		if req.Summary != nil {
			text := strings.TrimSpace(*req.Summary)
			if text != c.Summary {
				c.Summary = text
				if text == "" {
					// An emptied summary hands control back to auto extraction.
					c.SummarySource = db.SummaryAuto
					m.Event(db.EventSummary, "Summary cleared")
				} else {
					c.SummarySource = db.SummaryManual
					m.Event(db.EventSummary, "Summary edited manually")
				}
			}
		}
		if req.ColumnID != nil && *req.ColumnID != c.ColumnID {
			m.Event(db.EventMove, fmt.Sprintf("Moved from %s to %s", cols.title(c.ColumnID), cols.title(*req.ColumnID)))
			c.ColumnID = *req.ColumnID
		}
		if req.AOSessionID != nil && *req.AOSessionID != c.AOSessionID {
			oldSession, newSession = c.AOSessionID, *req.AOSessionID
			if oldSession != "" {
				m.Event(db.EventSession, "Unlinked AO session "+oldSession)
			}
			if newSession != "" {
				m.Event(db.EventSession, "Linked AO session "+newSession)
			}
			c.AOSessionID = newSession
			c.AOTitle = ""
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if oldSession != "" {
		_ = s.store.IgnoreSession(ctx, oldSession)
	}
	if newSession != "" {
		_ = s.store.UnignoreSession(ctx, newSession)
		s.requestAOSync()
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}

func (s *Server) handleMoveCard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ColumnID string `json:"columnId"`
		Position *int   `json:"position"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ColumnID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "columnId is required.")
		return
	}
	position := 0
	if req.Position != nil {
		position = *req.Position
	}
	card, err := s.store.MoveCard(r.Context(), r.PathValue("id"), req.ColumnID, position)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}

func (s *Server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	card, err := s.store.DeleteCard(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if card.AOSessionID != "" {
		// Keep AO sync from recreating a card for the same session.
		_ = s.store.IgnoreSession(r.Context(), card.AOSessionID)
	}
	s.mu.Lock()
	delete(s.summaryStamp, card.ID)
	s.mu.Unlock()
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": card.ID})
}

func (s *Server) handleCardEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetCard(r.Context(), id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListEvents(r.Context(), id, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
