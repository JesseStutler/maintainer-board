package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/github"
	"maintainer-board/internal/summary"
)

const (
	projectsRefreshInterval = time.Minute
	// autoSummaryMinInterval keeps a busy session from re-extracting its
	// summary on every sync.
	autoSummaryMinInterval = 15 * time.Second
	autoSummaryTurns       = 30
)

var (
	reviewWordsRe = regexp.MustCompile(`(?i)\b(prs?|pull[ -]requests?|review\w*)\b`)
	designWordsRe = regexp.MustCompile(`(?i)\bdesign\w*\b`)
	issueWordsRe  = regexp.MustCompile(`(?i)\bissues?\b`)
	triageWordsRe = regexp.MustCompile(`(?i)\btriag\w*\b`)
)

// InferCategory guesses a card category from a session or task title.
func InferCategory(title string) string {
	switch {
	case reviewWordsRe.MatchString(title):
		return db.CategoryPRReview
	case designWordsRe.MatchString(title) || strings.Contains(title, "\u8bbe\u8ba1"): // "design" in Chinese
		return db.CategoryDesign
	case issueWordsRe.MatchString(title):
		return db.CategoryIssue
	case triageWordsRe.MatchString(title):
		return db.CategoryTriage
	}
	return db.CategoryDev
}

func validHarness(h string) bool { return h == "copilot" || h == "codex" }

func harnessLabel(h string) string {
	switch h {
	case "copilot":
		return "Copilot"
	case "codex":
		return "Codex"
	}
	return h
}

// engineLabel names what wrote an AI summary: one of the two CLIs, or a
// model the Copilot CLI was asked to use.
func engineLabel(engine string) string {
	if validHarness(engine) {
		return harnessLabel(engine)
	}
	return engine + " (Copilot CLI)"
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// requestAOSync asks the AO loop for an immediate sync; requests coalesce.
func (s *Server) requestAOSync() {
	select {
	case s.aoSyncRequests <- struct{}{}:
	default:
	}
}

func (s *Server) aoLoop(ctx context.Context) {
	s.syncAO(ctx)
	ticker := time.NewTicker(s.cfg.AOSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncAO(ctx)
		case <-s.aoSyncRequests:
			// Let a burst of events settle before syncing once.
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			s.syncAO(ctx)
		}
	}
}

// aoEventLoop follows AO's event stream and turns every event into a sync
// request, so the board reacts within a moment instead of on the next tick.
// The periodic sync remains the source of truth when the stream is down.
func (s *Server) aoEventLoop(ctx context.Context) {
	backoff := 2 * time.Second
	lastID := ""
	for ctx.Err() == nil {
		started := time.Now()
		err := s.ao.Events(ctx, lastID, func(ev ao.Event) {
			if ev.ID != "" {
				lastID = ev.ID
			}
			s.requestAOSync()
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 30*time.Second {
			backoff = 2 * time.Second
		}
		s.log.Debug("ao event stream ended", "err", err, "retryIn", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// syncAO refreshes the session cache from the AO daemon and reconciles
// cards with it.
func (s *Server) syncAO(ctx context.Context) {
	s.aoSyncMu.Lock()
	defer s.aoSyncMu.Unlock()

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sessions, err := s.ao.ListSessions(listCtx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		msg := s.describeError(err)
		s.mu.Lock()
		changed := s.aoConnected || s.aoError != msg
		wasConnected := s.aoConnected
		s.aoConnected, s.aoError = false, msg
		s.mu.Unlock()
		if wasConnected {
			s.log.Warn("ao daemon went offline", "err", err)
		}
		if changed {
			s.hub.Publish()
		}
		return
	}

	s.mu.RLock()
	needProjects := time.Since(s.projectsAt) > projectsRefreshInterval
	s.mu.RUnlock()
	var projects []ao.Project
	if needProjects {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		projects, err = s.ao.ListProjects(pctx)
		cancel()
		if err != nil {
			needProjects = false
		}
	}

	fp := fingerprint(sessions)
	s.mu.Lock()
	changed := !s.aoConnected || fp != s.aoFingerprint
	if !s.aoConnected {
		s.log.Info("ao daemon connected", "sessions", len(sessions))
	}
	s.aoConnected, s.aoError, s.aoLastSync, s.aoFingerprint = true, "", time.Now(), fp
	s.sessions = make(map[string]ao.Session, len(sessions))
	s.sessionOrder = s.sessionOrder[:0]
	for _, sess := range sessions {
		s.sessions[sess.ID] = sess
		s.sessionOrder = append(s.sessionOrder, sess.ID)
	}
	if needProjects {
		if fingerprint(projects) != fingerprint(s.projects) {
			changed = true
		}
		s.projects, s.projectsAt = projects, time.Now()
	}
	s.mu.Unlock()

	if s.reconcileSessions(ctx, sessions) {
		changed = true
	}
	if changed {
		s.hub.Publish()
	}
	s.requestContentIndex()
}

// reconcileSessions creates cards for sessions that have none, follows AO
// renames, and refreshes automatic summaries. It reports whether any card
// changed.
func (s *Server) reconcileSessions(ctx context.Context, sessions []ao.Session) bool {
	cards, err := s.store.ListCards(ctx)
	if err != nil {
		s.log.Error("ao sync: list cards", "err", err)
		return false
	}
	ignored, err := s.store.IgnoredSessions(ctx)
	if err != nil {
		s.log.Error("ao sync: list ignored sessions", "err", err)
		return false
	}
	bySession := map[string]*db.Card{}
	for _, c := range cards {
		if c.AOSessionID != "" {
			bySession[c.AOSessionID] = c
		}
	}
	showOrchestrator := s.showOrchestrator(ctx)
	changed := false

	for _, sess := range sessions {
		card := bySession[sess.ID]
		if card == nil {
			if ignored[sess.ID] || sess.IsTerminated {
				continue
			}
			if sess.Kind != "worker" && !(sess.Kind == "orchestrator" && showOrchestrator) {
				continue
			}
			created, err := s.createSessionCard(ctx, sess)
			if err != nil {
				s.log.Error("ao sync: create card", "session", sess.ID, "err", err)
				continue
			}
			card = created
			changed = true
		} else if sess.DisplayName != "" && card.AOTitle != "" && card.Title == card.AOTitle && sess.DisplayName != card.AOTitle {
			// The title still mirrors AO, so follow the rename.
			updated, err := s.store.UpdateCard(ctx, card.ID, func(c *db.Card, m *db.Mutation) error {
				if c.Title != c.AOTitle {
					return nil
				}
				c.Title, c.AOTitle = sess.DisplayName, sess.DisplayName
				m.Event(db.EventSession, "Title updated from AO session rename")
				return nil
			})
			if err == nil {
				card = updated
				changed = true
			}
		}
		if s.refreshAutoSummary(ctx, card, sess) {
			changed = true
		}
	}
	return changed
}

func (s *Server) createSessionCard(ctx context.Context, sess ao.Session) (*db.Card, error) {
	cols, err := s.columnIndex(ctx)
	if err != nil {
		return nil, err
	}
	intake, ok := cols.intake()
	if !ok {
		return nil, errors.New("board has no columns")
	}
	title := strings.TrimSpace(sess.DisplayName)
	aoTitle := title
	if title == "" {
		title = sess.ID
		if sess.Kind == "orchestrator" {
			title = "Orchestrator " + sess.ID
		}
	}
	card := &db.Card{
		Title:       title,
		ColumnID:    intake.ID,
		Category:    InferCategory(title),
		AOSessionID: sess.ID,
		AOTitle:     aoTitle,
		AutoMove:    true,
		Origin:      db.OriginAO,
	}
	events := []db.EventInput{{
		Kind:    db.EventCreated,
		Message: fmt.Sprintf("Card created from AO session %s (%s)", sess.ID, harnessLabel(sess.Harness)),
	}}
	// Link the pull request AO already attached to the session, or a
	// reference mentioned in its title such as "[PR #6247]".
	var ref github.Ref
	linked := false
	if len(sess.PRs) > 0 && sess.PRs[0].URL != "" {
		if parsed, err := github.ParseRef(sess.PRs[0].URL, s.cfg.DefaultRepo); err == nil {
			ref, linked = parsed, true
		}
	}
	if !linked {
		ref, linked = github.FindRef(sess.DisplayName, s.cfg.DefaultRepo)
	}
	if linked {
		card.GHRepo, card.GHNumber = ref.Repo, ref.Number
		events = append(events, db.EventInput{Kind: db.EventGitHub, Message: "Auto-detected GitHub reference " + ref.String()})
	}
	card, err = s.store.CreateCard(ctx, card, events...)
	if err != nil {
		return nil, err
	}
	s.log.Info("card created from ao session", "session", sess.ID, "card", card.ID, "github", linked)
	if linked {
		id := card.ID
		s.goBackground(func(ctx context.Context) {
			if _, _, err := s.syncCard(ctx, id); err != nil {
				s.log.Warn("initial github sync failed", "card", id, "ref", ref.String(), "err", err)
			}
			s.hub.Publish()
		})
	}
	return card, nil
}

func conversationTurns(conv *ao.Conversation) []summary.Turn {
	turns := make([]summary.Turn, 0, len(conv.Messages))
	for _, m := range conv.Messages {
		if m.Streaming {
			continue
		}
		turns = append(turns, summary.Turn{Role: m.Role, Text: m.Text})
	}
	return turns
}

// refreshAutoSummary re-extracts the summary of a card whose summary is
// still automatic, when its session saw new activity.
func (s *Server) refreshAutoSummary(ctx context.Context, card *db.Card, sess ao.Session) bool {
	if card.Summary != "" && card.SummarySource != db.SummaryAuto {
		return false
	}
	stamp := sess.Activity.LastActivityAt + "|" + sess.Activity.State
	s.mu.RLock()
	mark, seen := s.summaryStamp[card.ID]
	s.mu.RUnlock()
	if seen {
		if mark.stamp == stamp || time.Since(mark.at) < autoSummaryMinInterval {
			return false
		}
	}
	s.mu.Lock()
	s.summaryStamp[card.ID] = summaryMark{stamp: stamp, at: time.Now()}
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	conv, err := s.ao.Conversation(cctx, sess.ID, ao.ConversationQuery{Limit: autoSummaryTurns})
	cancel()
	if err != nil {
		s.log.Debug("auto summary: conversation unavailable", "session", sess.ID, "err", err)
		return false
	}
	turns := conversationTurns(conv)
	if len(turns) == 0 {
		return false
	}
	text := summary.Extract(turns, "")
	if text == "" || text == card.Summary {
		return false
	}
	first := card.Summary == ""
	updated := false
	_, err = s.store.UpdateCard(ctx, card.ID, func(c *db.Card, m *db.Mutation) error {
		if c.Summary != "" && c.SummarySource != db.SummaryAuto {
			return nil
		}
		c.Summary, c.SummarySource = text, db.SummaryAuto
		updated = true
		if first {
			m.Event(db.EventSummary, "Summary extracted from the session conversation")
		}
		return nil
	})
	return err == nil && updated
}

// startSession spawns a new AO worker session for a card and links it.
func (s *Server) startSession(ctx context.Context, cardID string, req agentRequest) (*db.Card, error) {
	s.aoSyncMu.Lock()
	defer s.aoSyncMu.Unlock()

	card, err := s.store.GetCard(ctx, cardID)
	if err != nil {
		return nil, err
	}
	if card.AOSessionID != "" {
		return nil, fmt.Errorf("%w: the card already has an agent session", errConflict)
	}
	project := strings.TrimSpace(req.ProjectID)
	if project == "" {
		project = s.defaultProject()
	}
	if project == "" {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		projects, perr := s.ao.ListProjects(pctx)
		cancel()
		if perr != nil {
			return nil, perr
		}
		if len(projects) == 0 {
			return nil, fmt.Errorf("%w: AO has no projects; add one in AO first", errConflict)
		}
		project = projects[0].ID
	}
	spawnCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	sess, err := s.ao.Spawn(spawnCtx, ao.SpawnRequest{
		ProjectID:           project,
		Harness:             req.Harness,
		Mode:                "chat",
		Kind:                "worker",
		DisplayName:         truncateRunes(card.Title, 100),
		DisplayNameExplicit: true,
		Prompt:              strings.TrimSpace(req.Prompt),
		Model:               req.Model,
		Effort:              req.Effort,
	})
	if err != nil {
		return nil, err
	}
	// The session exists now; link it even if the request was cancelled.
	saveCtx := context.WithoutCancel(ctx)
	updated, err := s.store.UpdateCard(saveCtx, cardID, func(c *db.Card, m *db.Mutation) error {
		c.AOSessionID = sess.ID
		c.AOTitle = ""
		if sess.DisplayName == c.Title {
			c.AOTitle = sess.DisplayName
		}
		started := fmt.Sprintf("Started %s session %s", harnessLabel(req.Harness), sess.ID)
		if req.Model != "" {
			started += " with model " + req.Model
			if req.Effort != "" {
				started += " (effort " + req.Effort + ")"
			}
		}
		m.Event(db.EventSession, started)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("session %s was started but could not be linked: %w", sess.ID, err)
	}
	_ = s.store.UnignoreSession(saveCtx, sess.ID)
	s.mu.Lock()
	if _, ok := s.sessions[sess.ID]; !ok {
		s.sessionOrder = append(s.sessionOrder, sess.ID)
	}
	s.sessions[sess.ID] = *sess
	s.mu.Unlock()
	s.requestAOSync()
	return updated, nil
}

var errConflict = errors.New("conflict")

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	var req agentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validHarness(req.Harness) {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Agent must be copilot or codex.")
		return
	}
	req.Model, req.Effort = strings.TrimSpace(req.Model), strings.TrimSpace(req.Effort)
	if err := s.validateSpawnModel(r.Context(), req); err != nil {
		writeError(w, http.StatusBadRequest, "UNKNOWN_MODEL", capitalize(strings.TrimPrefix(err.Error(), errUnknownModel.Error()+": "))+".")
		return
	}
	card, err := s.startSession(r.Context(), r.PathValue("id"), req)
	switch {
	case errors.Is(err, errConflict):
		writeError(w, http.StatusConflict, "CONFLICT", capitalize(strings.TrimPrefix(err.Error(), errConflict.Error()+": "))+".")
		return
	case errors.Is(err, db.ErrNotFound):
		s.writeStoreError(w, err)
		return
	case err != nil:
		s.writeAOError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusCreated, s.view(card))
}

// Session endpoints

type sessionListItem struct {
	ao.Session
	CardID string `json:"cardId"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	cards, err := s.store.ListCards(r.Context())
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	linked := map[string]string{}
	for _, c := range cards {
		if c.AOSessionID != "" {
			linked[c.AOSessionID] = c.ID
		}
	}
	s.mu.RLock()
	out := make([]sessionListItem, 0, len(s.sessionOrder))
	for _, id := range s.sessionOrder {
		out = append(out, sessionListItem{Session: s.sessions[id], CardID: linked[id]})
	}
	connected := s.aoConnected
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "connected": connected})
}

func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := ao.ConversationQuery{Limit: 60}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		query.Limit = min(v, 200)
	}
	if v, err := strconv.ParseInt(q.Get("beforeSequence"), 10, 64); err == nil && v > 0 {
		query.BeforeSequence = v
	}
	if v, err := strconv.ParseInt(q.Get("afterSequence"), 10, 64); err == nil && v > 0 {
		query.AfterSequence = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conv, err := s.ao.Conversation(ctx, r.PathValue("id"), query)
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text            string `json:"text"`
		ClientMessageID string `json:"clientMessageId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Message text is required.")
		return
	}
	if req.ClientMessageID == "" {
		req.ClientMessageID = newUUID()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.ao.SendMessage(ctx, r.PathValue("id"), req.Text, req.ClientMessageID)
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	s.requestAOSync()
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.ao.Interrupt(ctx, r.PathValue("id")); err != nil {
		s.writeAOError(w, err)
		return
	}
	s.requestAOSync()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	sess, err := s.ao.Resume(ctx, id)
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	if sess != nil && sess.ID != "" {
		s.mu.Lock()
		if _, ok := s.sessions[sess.ID]; ok {
			s.sessions[sess.ID] = *sess
		}
		s.mu.Unlock()
	}
	if card, err := s.store.CardBySession(r.Context(), id); err == nil && card != nil {
		_ = s.store.AddEvent(r.Context(), card.ID, db.EventSession, "Resumed AO session "+id)
	}
	s.requestAOSync()
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": sess})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.ao.Kill(ctx, id); err != nil {
		s.writeAOError(w, err)
		return
	}
	if card, err := s.store.CardBySession(r.Context(), id); err == nil && card != nil {
		_ = s.store.AddEvent(r.Context(), card.ID, db.EventSession, "Terminated AO session "+id)
	}
	s.requestAOSync()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string `json:"displayName"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" || len([]rune(name)) > 100 {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "displayName must be 1 to 100 characters.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.ao.Rename(ctx, r.PathValue("id"), name); err != nil {
		s.writeAOError(w, err)
		return
	}
	s.requestAOSync()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Import

type importResultView struct {
	ao.ImportResult
	CardID string `json:"cardId"`
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Select between 1 and 500 sessions to import.")
		return
	}
	// Import is detached from the request: once AO created the sessions the
	// cards should be created even if the browser went away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()

	s.aoSyncMu.Lock()
	results, err := s.ao.Import(ctx, req.IDs)
	if err == nil {
		for _, res := range results {
			if res.SessionID != "" {
				// A previously deleted card must not block a deliberate re-import.
				_ = s.store.UnignoreSession(ctx, res.SessionID)
			}
		}
	}
	s.aoSyncMu.Unlock()
	s.forgetImportCandidates()
	if err != nil {
		s.writeAOError(w, err)
		return
	}
	s.syncAO(ctx)

	out := make([]importResultView, 0, len(results))
	imported, failed := 0, 0
	for _, res := range results {
		v := importResultView{ImportResult: res}
		if res.Status == "failed" {
			failed++
		} else {
			imported++
		}
		if res.SessionID != "" {
			if card, err := s.store.CardBySession(ctx, res.SessionID); err == nil && card != nil {
				v.CardID = card.ID
			}
		}
		out = append(out, v)
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"results": out, "imported": imported, "failed": failed})
}

// Summaries

func githubContext(c *db.Card) string {
	if c.GHNumber == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%s %s#%d", kindLabel(c.GHKind), c.GHRepo, c.GHNumber)}
	if c.GHTitle != "" {
		parts = append(parts, fmt.Sprintf("titled %q", c.GHTitle))
	}
	if c.GHAuthor != "" {
		parts = append(parts, "by "+c.GHAuthor)
	}
	if c.GHState != "" {
		parts = append(parts, "state "+c.GHState)
	}
	if c.GHKind == github.KindPR {
		if c.GHCIStatus != "" {
			parts = append(parts, "CI "+c.GHCIStatus)
		}
		if c.GHReviewDecision != "" {
			parts = append(parts, "review "+c.GHReviewDecision)
		}
	}
	if c.HasUnreadUpdates && c.UpdateSummary != "" {
		parts = append(parts, "unreviewed updates: "+c.UpdateSummary)
	}
	return strings.Join(parts, ", ")
}

// describer is implemented by GitHub clients that can fetch the body of a
// pull request or issue.
type describer interface {
	Description(ctx context.Context, repo string, number int) (string, error)
}

// summaryOpeningWindow is how many of the first conversation items are
// read for a summary: the opening requests say what the card is about.
const (
	summaryOpeningWindow = 40
	summaryRecentTurns   = 12
)

// summaryInput collects what the AI summary is built on: the linked GitHub
// item with its description, the notes, and the opening of the session
// conversation. The latest turns are added only when the rest is too thin
// to tell what the card is about.
func (s *Server) summaryInput(ctx context.Context, card *db.Card) summary.Input {
	in := summary.Input{Title: card.Title, GitHub: githubContext(card), Notes: card.Notes}
	var wg sync.WaitGroup
	description := ""
	if d, ok := s.gh.(describer); ok && card.GHNumber != 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			body, err := d.Description(dctx, card.GHRepo, card.GHNumber)
			if err != nil {
				s.log.Debug("summarize: description unavailable", "card", card.ID, "err", err)
				return
			}
			description = body
		}()
	}
	whole := true
	if card.AOSessionID != "" {
		// AO pages backwards from a sequence number, so asking for what
		// precedes item 41 yields the first 40 items.
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		conv, err := s.ao.Conversation(cctx, card.AOSessionID, ao.ConversationQuery{Limit: summaryOpeningWindow, BeforeSequence: summaryOpeningWindow + 1})
		cancel()
		if err == nil {
			in.Turns = conversationTurns(conv)
			whole = conv.LatestSequence <= summaryOpeningWindow
		} else {
			s.log.Warn("summarize: conversation unavailable", "session", card.AOSessionID, "err", err)
		}
	}
	wg.Wait()
	in.Description = description
	if card.AOSessionID != "" && !whole && summary.NeedsRecent(in) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		conv, err := s.ao.Conversation(cctx, card.AOSessionID, ao.ConversationQuery{Limit: summaryRecentTurns})
		cancel()
		if err == nil {
			in.Recent = conversationTurns(conv)
		}
	}
	return in
}

func (s *Server) handleSummarize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	card, err := s.store.GetCard(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	in := s.summaryInput(ctx, card)
	if len(in.Turns) == 0 && len(in.Recent) == 0 && in.GitHub == "" && strings.TrimSpace(in.Notes) == "" {
		writeError(w, http.StatusConflict, "NOTHING_TO_SUMMARIZE", "There is nothing to summarize yet. Link an agent session or a pull request or issue, or add notes first.")
		return
	}
	text, engine, err := s.ai.Summarize(ctx, in)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("ai summary failed", "card", id, "err", err)
		writeError(w, http.StatusBadGateway, "SUMMARY_FAILED", capitalize(err.Error())+".")
		return
	}
	updated, err := s.store.UpdateCard(ctx, id, func(c *db.Card, m *db.Mutation) error {
		c.Summary, c.SummarySource = text, db.SummaryAI
		m.Event(db.EventSummary, "AI summary refreshed with "+engineLabel(engine))
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, map[string]any{"card": s.view(updated), "summary": text, "engine": engine})
}
