package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/github"
)

const (
	settingShowOrchestrator = "show_orchestrator"
	settingGitHubUser       = "github_user"
	settingDefaultRepo      = "default_repo"
	settingApprovalMode     = "approval_mode"
)

// ValidApprovalMode reports whether mode is an AO approval mode accepted on
// spawn and conversation settings, or empty for AO's default.
func ValidApprovalMode(mode string) bool {
	switch mode {
	case "", "default", "accept-edits", "auto", "bypass-permissions":
		return true
	default:
		return false
	}
}

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
	// GitHubUser is the explicit user override stored in settings, or ""
	// when the board uses the auto-detected gh login (or --gh-user).
	GitHubUser   string `json:"githubUser"`
	DetectedUser string `json:"detectedUser"`
	ApprovalMode string `json:"approvalMode"`
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
	s.mu.RLock()
	override := s.ghUserOverride
	s.mu.RUnlock()
	if override != "" {
		return override
	}
	if s.cfg.CurrentUser != "" {
		return s.cfg.CurrentUser
	}
	if r, ok := s.gh.(userResolver); ok {
		return r.User()
	}
	return ""
}

// detectedGitHubUser returns the login from --gh-user or gh auto-detection,
// ignoring any override saved in board settings.
func (s *Server) detectedGitHubUser() string {
	if s.cfg.CurrentUser != "" {
		return s.cfg.CurrentUser
	}
	s.mu.RLock()
	detected := s.detectedUser
	override := s.ghUserOverride
	s.mu.RUnlock()
	if detected != "" {
		return detected
	}
	if override == "" {
		if r, ok := s.gh.(userResolver); ok {
			return r.User()
		}
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
		s.mu.Lock()
		s.detectedUser = login
		override := s.ghUserOverride
		s.mu.Unlock()
		if override != "" {
			if setter, ok := s.gh.(userSetter); ok {
				setter.SetUser(override)
			}
		}
		s.log.Info("github user detected", "login", login)
		s.hub.Publish()
	}
}

func (s *Server) showOrchestrator(ctx context.Context) bool {
	v, err := s.store.GetSetting(ctx, settingShowOrchestrator, "false")
	return err == nil && v == "true"
}

func (s *Server) defaultRepo(ctx context.Context) string {
	if v, err := s.store.GetSetting(ctx, settingDefaultRepo, ""); err == nil && github.ValidRepo(v) {
		return v
	}
	return s.cfg.DefaultRepo
}

func (s *Server) approvalMode(ctx context.Context) string {
	if v, err := s.store.GetSetting(ctx, settingApprovalMode, ""); err == nil && v != "" && ValidApprovalMode(v) {
		return v
	}
	return s.cfg.ApprovalMode
}

func (s *Server) boardSettings(ctx context.Context) boardSettings {
	s.mu.RLock()
	override := s.ghUserOverride
	s.mu.RUnlock()
	return boardSettings{
		ShowOrchestrator: s.showOrchestrator(ctx),
		DefaultRepo:      s.defaultRepo(ctx),
		GitHubUser:       override,
		DetectedUser:     s.detectedGitHubUser(),
		ApprovalMode:     s.approvalMode(ctx),
	}
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
		Settings:    s.boardSettings(ctx),
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
	ctx := r.Context()
	var req struct {
		ShowOrchestrator *bool   `json:"showOrchestrator"`
		DefaultRepo      *string `json:"defaultRepo"`
		GitHubUser       *string `json:"githubUser"`
		ApprovalMode     *string `json:"approvalMode"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	var defaultRepo, ghUser, approvalMode string
	if req.DefaultRepo != nil {
		defaultRepo = strings.TrimSpace(*req.DefaultRepo)
		if defaultRepo != "" && !github.ValidRepo(defaultRepo) {
			writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Default repository must be owner/name (or empty for the server default).")
			return
		}
	}
	if req.GitHubUser != nil {
		ghUser = strings.TrimPrefix(strings.TrimSpace(*req.GitHubUser), "@")
		if ghUser != "" && !github.ValidLogin(ghUser) {
			writeError(w, http.StatusBadRequest, "INVALID_INPUT", "GitHub username must be a valid GitHub login (or empty to auto-detect).")
			return
		}
	}
	if req.ApprovalMode != nil {
		approvalMode = strings.TrimSpace(*req.ApprovalMode)
		if !ValidApprovalMode(approvalMode) {
			writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Approval mode must be empty, default, accept-edits, auto, or bypass-permissions.")
			return
		}
	}

	changed := false
	syncAO := false
	if req.ShowOrchestrator != nil {
		if err := s.store.SetSetting(ctx, settingShowOrchestrator, strconv.FormatBool(*req.ShowOrchestrator)); err != nil {
			s.writeStoreError(w, err)
			return
		}
		changed, syncAO = true, true
	}
	if req.DefaultRepo != nil {
		if err := s.store.SetSetting(ctx, settingDefaultRepo, defaultRepo); err != nil {
			s.writeStoreError(w, err)
			return
		}
		changed = true
	}
	if req.GitHubUser != nil {
		if err := s.store.SetSetting(ctx, settingGitHubUser, ghUser); err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.mu.Lock()
		s.ghUserOverride = ghUser
		s.mu.Unlock()
		if setter, ok := s.gh.(userSetter); ok {
			if ghUser != "" {
				setter.SetUser(ghUser)
			} else {
				setter.SetUser(s.detectedGitHubUser())
			}
		}
		changed = true
	}
	if req.ApprovalMode != nil {
		if err := s.store.SetSetting(ctx, settingApprovalMode, approvalMode); err != nil {
			s.writeStoreError(w, err)
			return
		}
		changed = true
	}
	if changed {
		s.hub.Publish()
	}
	if syncAO {
		s.requestAOSync()
	}
	writeJSON(w, http.StatusOK, s.boardSettings(ctx))
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
	// Model, Effort and ApprovalMode choose the initial settings of the new
	// session; empty falls back to the board default (for ApprovalMode) or
	// the agent's default.
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	ApprovalMode string `json:"approvalMode"`
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
	// Model, Effort and ApprovalMode are a shorthand for the fields of Agent.
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	ApprovalMode string `json:"approvalMode"`
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
		if req.Agent.ApprovalMode == "" {
			req.Agent.ApprovalMode = req.ApprovalMode
		}
		req.Agent.Model = strings.TrimSpace(req.Agent.Model)
		req.Agent.Effort = strings.TrimSpace(req.Agent.Effort)
		req.Agent.ApprovalMode = strings.TrimSpace(req.Agent.ApprovalMode)
		if !ValidApprovalMode(req.Agent.ApprovalMode) {
			writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Approval mode must be empty, default, accept-edits, auto, or bypass-permissions.")
			return
		}
		// Checked before the card exists, so a typo does not leave a card
		// behind without its session.
		if err := s.validateSpawnModel(ctx, *req.Agent); err != nil {
			writeError(w, http.StatusBadRequest, "UNKNOWN_MODEL", capitalize(strings.TrimPrefix(err.Error(), errUnknownModel.Error()+": "))+".")
			return
		}
	}

	warnings := []string{}
	var refs []github.Ref
	var items []*github.Item
	if req.GitHubRef != "" {
		parsed, err := github.ParseRefs(req.GitHubRef, s.defaultRepo(ctx))
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_GITHUB_REF", "Unrecognized GitHub reference. Use #123, owner/repo#123 or a pull request or issue URL.")
			return
		}
		refs = parsed
		for _, ref := range refs {
			fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			item, err := s.gh.Fetch(fetchCtx, ref.Repo, ref.Number, "")
			cancel()
			switch {
			case errors.Is(err, github.ErrNotFound):
				writeError(w, http.StatusNotFound, "GITHUB_NOT_FOUND", fmt.Sprintf("%s was not found on GitHub.", ref))
				return
			case err != nil:
				if req.Title == "" && len(items) == 0 {
					s.writeGitHubError(w, err)
					return
				}
				warnings = append(warnings, fmt.Sprintf("GitHub could not be reached; %s will be synced on the next cycle.", ref))
			default:
				items = append(items, item)
			}
		}
	}
	var primaryItem *github.Item
	if len(items) > 0 {
		primaryItem = items[0]
	}
	if req.Title == "" && primaryItem != nil {
		req.Title = primaryItem.Title
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
		case primaryItem != nil && primaryItem.Kind == github.KindPR:
			if isOwn(primaryItem.Author, s.currentUser()) {
				category = db.CategoryDev
			} else {
				category = db.CategoryPRReview
			}
		case primaryItem != nil && primaryItem.Kind == github.KindIssue:
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
	if len(refs) > 0 {
		for _, ref := range refs {
			card.GHLinks = append(card.GHLinks, db.CardLink{Repo: ref.Repo, Number: ref.Number})
		}
		recomputeCardGitHub(card, s.defaultRepo(ctx))
	}
	card, err = s.store.CreateCard(ctx, card, events...)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if req.AOSessionID != "" {
		_ = s.store.UnignoreSession(ctx, req.AOSessionID)
	}
	for _, item := range items {
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

// deleteAOSession kills an AO session and permanently removes it from AO,
// then forgets everything the board kept about it. It is detached from the
// request: a kill without the delete that follows it would leave the
// session terminated but still listed.
func (s *Server) deleteAOSession(ctx context.Context, sessionID string) error {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := s.ao.DeleteSession(dctx, sessionID, true); err != nil {
		// The kill may have gone through; show the new state soon.
		s.requestAOSync()
		return err
	}
	s.mu.Lock()
	if _, ok := s.sessions[sessionID]; ok {
		delete(s.sessions, sessionID)
		s.sessionOrder = slices.DeleteFunc(s.sessionOrder, func(id string) bool { return id == sessionID })
	}
	s.mu.Unlock()
	s.index.mu.Lock()
	delete(s.index.seen, sessionID)
	s.index.mu.Unlock()
	// The session is gone for good: drop its conversation index and the
	// mark that kept it off the board.
	if err := s.store.DeleteContent(dctx, sessionID); err != nil {
		s.log.Warn("delete session: content index not cleared", "session", sessionID, "err", err)
	}
	_ = s.store.UnignoreSession(dctx, sessionID)
	s.forgetImportCandidates()
	s.requestAOSync()
	return nil
}

// writeSessionDeleteError reports that AO did not delete a session, with
// AO's reason, and says that the board was left as it was.
func (s *Server) writeSessionDeleteError(w http.ResponseWriter, sessionID string, err error) {
	s.log.Warn("ao session delete failed", "session", sessionID, "err", err)
	status, code, msg := s.aoErrorResponse(err)
	msg = strings.TrimRight(msg, ". ") + "."
	writeError(w, status, code, fmt.Sprintf("AO did not delete session %s: %s Nothing was removed from the board.", sessionID, msg))
}

func queryBool(r *http.Request, name string) bool {
	v, err := strconv.ParseBool(r.URL.Query().Get(name))
	return err == nil && v
}

// handleDeleteCard removes a card. With deleteAoSession=true (query
// parameter or JSON body) the linked AO session is killed and deleted from
// AO first; if AO refuses, nothing is removed. Without it the session stays
// in AO and is remembered as dismissed, so that the AO sync does not give
// it a new card.
func (s *Server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var req struct {
		DeleteAOSession bool `json:"deleteAoSession"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	deleteSession := req.DeleteAOSession || queryBool(r, "deleteAoSession")
	card, err := s.store.GetCard(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	sessionID := card.AOSessionID
	sessionDeleted := false
	if deleteSession && sessionID != "" {
		if err := s.deleteAOSession(ctx, sessionID); err != nil {
			s.writeSessionDeleteError(w, sessionID, err)
			return
		}
		sessionDeleted = true
	}
	if _, err := s.store.DeleteCard(context.WithoutCancel(ctx), id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	if sessionID != "" && !sessionDeleted {
		// Keep AO sync from recreating a card for the session that stays.
		_ = s.store.IgnoreSession(context.WithoutCancel(ctx), sessionID)
	}
	s.mu.Lock()
	delete(s.summaryStamp, id)
	s.mu.Unlock()
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "aoSessionId": sessionID, "aoSessionDeleted": sessionDeleted})
}

// handleDeleteCardSession kills the AO session of a card, deletes it from
// AO and detaches it. The card itself, with its notes and GitHub link,
// stays on the board.
func (s *Server) handleDeleteCardSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	card, err := s.store.GetCard(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	sessionID := card.AOSessionID
	if sessionID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION", "This card has no agent session.")
		return
	}
	if err := s.deleteAOSession(ctx, sessionID); err != nil {
		s.writeSessionDeleteError(w, sessionID, err)
		return
	}
	updated, err := s.store.UpdateCard(context.WithoutCancel(ctx), id, func(c *db.Card, m *db.Mutation) error {
		if c.AOSessionID != sessionID {
			return nil
		}
		c.AOSessionID, c.AOTitle = "", ""
		m.Event(db.EventSession, "Deleted AO session "+sessionID+" from Agent Orchestrator")
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"card": s.view(updated), "aoSessionId": sessionID, "aoSessionDeleted": true})
}

// handleUnlinkSession detaches the AO session from a card and nothing
// else: the session stays in AO untouched, and is remembered as dismissed
// so that the AO sync does not create a second card for it. Linking or
// importing it again lifts that.
func (s *Server) handleUnlinkSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := ""
	updated, err := s.store.UpdateCard(ctx, r.PathValue("id"), func(c *db.Card, m *db.Mutation) error {
		sessionID = c.AOSessionID
		if sessionID == "" {
			return nil
		}
		c.AOSessionID, c.AOTitle = "", ""
		m.Event(db.EventSession, "Unlinked AO session "+sessionID)
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if sessionID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION", "This card has no agent session.")
		return
	}
	_ = s.store.IgnoreSession(ctx, sessionID)
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"card": s.view(updated), "aoSessionId": sessionID})
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
