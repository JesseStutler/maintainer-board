package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/history"
)

// Config configures the native Codex/Copilot runtime.
type Config struct {
	DataDir             string
	HomeDir             string
	Workspaces          []string
	CodexPath           string
	CopilotPath         string
	DefaultApprovalMode string
	History             *history.Index
	Logger              *slog.Logger
}

type liveController interface {
	ID() string
	Harness() string
	State() (string, time.Time)
	Conversation(title string, q ao.ConversationQuery) *ao.Conversation
	Models() *ao.ConversationModels
	UpdateSettings(s ao.ConversationTurnSettings) ao.ConversationSettings
	SendMessage(ctx context.Context, text, clientMsgID string) (*ao.SendResult, error)
	Interrupt(ctx context.Context) error
	Close() error
}

type liveEntry struct {
	ctrl        liveController
	lastDiskMod time.Time
	needsReload bool
}

// Manager implements server.AOClient directly over native `codex app-server`
// and `copilot --acp` processes, without Agent Orchestrator.
type Manager struct {
	cfg      Config
	store    *sessionStore
	projects []ao.Project
	logger   *slog.Logger

	mu   sync.Mutex
	live map[string]*liveEntry

	modelsMu    sync.Mutex
	modelsCache map[string]*ao.AgentModels

	evMu    sync.Mutex
	evSeq   atomic.Int64
	evSubs  map[int]chan ao.Event
	nextSub int
}

// New opens the native session store and prepares the runtime manager.
func New(cfg Config) (*Manager, error) {
	if cfg.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		cfg.HomeDir = home
	}
	if cfg.CodexPath == "" {
		cfg.CodexPath = "codex"
	}
	if cfg.CopilotPath == "" {
		cfg.CopilotPath = "copilot"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.History == nil {
		cfg.History = history.New(cfg.HomeDir)
	}
	storePath := filepath.Join(cfg.DataDir, "native_sessions.json")
	st, err := openSessionStore(storePath)
	if err != nil {
		return nil, fmt.Errorf("open native session store: %w", err)
	}

	projects := buildProjects(cfg.Workspaces)
	return &Manager{
		cfg:         cfg,
		store:       st,
		projects:    projects,
		logger:      cfg.Logger,
		live:        map[string]*liveEntry{},
		modelsCache: map[string]*ao.AgentModels{},
		evSubs:      map[int]chan ao.Event{},
	}, nil
}

func buildProjects(workspaces []string) []ao.Project {
	seen := map[string]bool{}
	var out []ao.Project
	for _, raw := range workspaces {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		abs, err := filepath.Abs(raw)
		if err != nil {
			abs = raw
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		name := filepath.Base(abs)
		if name == "" || name == "." || name == string(filepath.Separator) {
			name = "workspace"
		}
		out = append(out, ao.Project{
			ID:            name,
			Name:          name,
			Path:          abs,
			Kind:          "git",
			SessionPrefix: name,
		})
	}
	if len(out) == 0 {
		cwd, _ := os.Getwd()
		if cwd != "" {
			name := filepath.Base(cwd)
			out = append(out, ao.Project{
				ID:            name,
				Name:          name,
				Path:          cwd,
				Kind:          "git",
				SessionPrefix: name,
			})
		}
	}
	return out
}

// Close terminates all running child agent processes gracefully.
func (m *Manager) Close() {
	m.mu.Lock()
	entries := make([]*liveEntry, 0, len(m.live))
	for _, e := range m.live {
		entries = append(entries, e)
	}
	m.live = map[string]*liveEntry{}
	m.mu.Unlock()
	for _, e := range entries {
		_ = e.ctrl.Close()
	}
}

func (m *Manager) emit(eventType string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ev := ao.Event{
		ID:   strconv.FormatInt(m.evSeq.Add(1), 10),
		Type: eventType,
		Data: string(b),
	}
	m.evMu.Lock()
	for _, ch := range m.evSubs {
		select {
		case ch <- ev:
		default:
		}
	}
	m.evMu.Unlock()
}

func (m *Manager) resolveWorkspace(projectID string) (string, string) {
	if projectID != "" {
		for _, p := range m.projects {
			if p.ID == projectID || p.Name == projectID || p.Path == projectID {
				return p.ID, p.Path
			}
		}
	}
	if len(m.projects) > 0 {
		return m.projects[0].ID, m.projects[0].Path
	}
	cwd, _ := os.Getwd()
	return filepath.Base(cwd), cwd
}

func (m *Manager) projectForCWD(cwd string) string {
	if cwd == "" {
		return ""
	}
	clean := filepath.Clean(cwd)
	for _, p := range m.projects {
		root := filepath.Clean(p.Path)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return p.ID
		}
	}
	return ""
}

func (m *Manager) effectiveApproval(requested string) string {
	if s := strings.TrimSpace(requested); s != "" {
		return s
	}
	return strings.TrimSpace(m.cfg.DefaultApprovalMode)
}

func (m *Manager) sessionFromRecord(rec SessionRecord) ao.Session {
	m.mu.Lock()
	entry := m.live[rec.ID]
	m.mu.Unlock()

	state := "idle"
	lastAct := rec.LastActivityAt
	needsResume := true
	isTerminated := rec.Terminated

	if entry != nil {
		st, ts := entry.ctrl.State()
		if st != "exited" {
			state = st
			needsResume = false
			isTerminated = false
			if !ts.IsZero() {
				lastAct = ts.UTC().Format(time.RFC3339)
			}
		}
	}
	if lastAct == "" {
		lastAct = rec.UpdatedAt
	}
	if lastAct == "" {
		lastAct = rec.CreatedAt
	}

	status := "idle"
	displayStatus := "Idle"
	if state == "active" {
		status = "running"
		displayStatus = "Working"
	} else if state == "ready" {
		status = "ready"
		displayStatus = "Ready"
	} else if isTerminated {
		status = "terminated"
		displayStatus = "Stopped"
	}

	return ao.Session{
		ID:              rec.ID,
		NativeSessionID: rec.ID,
		CWD:             rec.CWD,
		ProjectID:       rec.ProjectID,
		Kind:            "native",
		Harness:         rec.Harness,
		DisplayName:     rec.DisplayName,
		Mode:            "native",
		Activity: ao.Activity{
			State:          state,
			LastActivityAt: lastAct,
		},
		IsTerminated:  isTerminated,
		NeedsResume:   needsResume,
		Status:        status,
		DisplayStatus: displayStatus,
		Branch:        rec.Branch,
		CreatedAt:     rec.CreatedAt,
		UpdatedAt:     rec.UpdatedAt,
	}
}

// ListProjects returns the configured local workspace directories.
func (m *Manager) ListProjects(_ context.Context) ([]ao.Project, error) {
	out := make([]ao.Project, len(m.projects))
	copy(out, m.projects)
	return out, nil
}

// ListSessions returns all native sessions tracked by the board.
func (m *Manager) ListSessions(_ context.Context) ([]ao.Session, error) {
	recs := m.store.List()
	out := make([]ao.Session, 0, len(recs))
	for _, rec := range recs {
		out = append(out, m.sessionFromRecord(rec))
	}
	return out, nil
}

// Spawn creates a brand-new native Codex or Copilot session directly inside
// the workspace directory, with zero AO prompt injection.
func (m *Manager) Spawn(ctx context.Context, req ao.SpawnRequest) (*ao.Session, error) {
	harness := strings.ToLower(strings.TrimSpace(req.Harness))
	if harness != "codex" && harness != "copilot" {
		return nil, &ao.APIError{Status: 400, Code: "INVALID_HARNESS", Message: fmt.Sprintf("unsupported harness %q", req.Harness)}
	}
	projectID, cwd := m.resolveWorkspace(req.ProjectID)
	approval := m.effectiveApproval(req.ApprovalMode)

	ctrl, err := m.startController(ctx, harness, cwd, "", req.Model, req.Effort, approval, nil)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	models := ctrl.Models()
	rec := SessionRecord{
		ID:             ctrl.ID(),
		Harness:        harness,
		ProjectID:      projectID,
		CWD:            cwd,
		Branch:         gitBranch(cwd),
		DisplayName:    strings.TrimSpace(req.DisplayName),
		Model:          models.Selected.Model,
		Effort:         models.Selected.ReasoningEffort,
		ApprovalMode:   approval,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	}
	if err := m.store.Upsert(rec); err != nil {
		_ = ctrl.Close()
		return nil, err
	}

	m.mu.Lock()
	m.live[rec.ID] = &liveEntry{
		ctrl:        ctrl,
		lastDiskMod: m.transcriptModTime(harness, rec.ID),
	}
	m.mu.Unlock()

	if prompt := strings.TrimSpace(req.Prompt); prompt != "" {
		if _, err := ctrl.SendMessage(ctx, prompt, ""); err != nil {
			m.logger.Warn("native spawn initial prompt failed", "session", rec.ID, "err", err)
		}
	}

	m.emit("session.created", map[string]any{"sessionId": rec.ID})
	sess := m.sessionFromRecord(rec)
	return &sess, nil
}

func (m *Manager) startController(
	ctx context.Context,
	harness, cwd, resumeID, model, effort, approval string,
	initialBuf *convBuffer,
) (liveController, error) {
	emit := func(evType string, payload map[string]any) {
		if evType == "session.updated" {
			if sid, ok := payload["sessionId"].(string); ok && sid != "" {
				now := time.Now().UTC().Format(time.RFC3339)
				_, _, _ = m.store.Update(sid, func(r *SessionRecord) {
					r.UpdatedAt = now
					r.LastActivityAt = now
				})
				m.mu.Lock()
				if e := m.live[sid]; e != nil {
					if st, _ := e.ctrl.State(); st == "ready" {
						e.lastDiskMod = m.transcriptModTime(harness, sid)
					}
				}
				m.mu.Unlock()
			}
		}
		m.emit(evType, payload)
	}

	switch harness {
	case "codex":
		return startCodexSession(ctx, m.cfg.CodexPath, cwd, resumeID, model, effort, approval, initialBuf, emit)
	case "copilot":
		return startCopilotSession(ctx, m.cfg.CopilotPath, cwd, resumeID, model, effort, approval, initialBuf, emit)
	default:
		return nil, fmt.Errorf("unsupported harness %q", harness)
	}
}

func (m *Manager) transcriptModTime(harness, sessionID string) time.Time {
	switch harness {
	case "codex":
		path, err := findCodexRolloutPath(m.cfg.HomeDir, sessionID)
		if err != nil {
			return time.Time{}
		}
		if fi, err := os.Stat(path); err == nil {
			return fi.ModTime()
		}
	case "copilot":
		path := filepath.Join(m.cfg.HomeDir, ".copilot", "session-state", sessionID, "events.jsonl")
		if fi, err := os.Stat(path); err == nil {
			return fi.ModTime()
		}
	}
	return time.Time{}
}

func (m *Manager) readDiskTranscript(harness, sessionID string) (*convBuffer, error) {
	switch harness {
	case "codex":
		return readCodexRollout(m.cfg.HomeDir, sessionID)
	case "copilot":
		return readCopilotEvents(m.cfg.HomeDir, sessionID)
	default:
		return newConvBuffer(), nil
	}
}

func (m *Manager) ensureLive(ctx context.Context, sessionID string) (SessionRecord, liveController, error) {
	rec, ok := m.store.Get(sessionID)
	if !ok {
		return SessionRecord{}, nil, notFoundErr(sessionID)
	}

	m.mu.Lock()
	entry := m.live[sessionID]
	var staleCtrl liveController
	if entry != nil {
		st, _ := entry.ctrl.State()
		if st == "exited" {
			delete(m.live, sessionID)
			entry = nil
		} else if st != "active" {
			// If the user continued this session in a local terminal, the disk
			// transcript is newer than our last turn; reload so both sides stay
			// in sync before sending the next turn.
			diskMod := m.transcriptModTime(rec.Harness, rec.ID)
			if entry.needsReload || (!entry.lastDiskMod.IsZero() && diskMod.After(entry.lastDiskMod.Add(2*time.Second))) {
				staleCtrl = entry.ctrl
				delete(m.live, sessionID)
				entry = nil
			}
		}
	}
	m.mu.Unlock()

	if staleCtrl != nil {
		_ = staleCtrl.Close()
	}
	if entry != nil {
		return rec, entry.ctrl, nil
	}

	cwd := rec.CWD
	if cwd == "" {
		_, cwd = m.resolveWorkspace(rec.ProjectID)
	}
	if _, err := os.Stat(cwd); err != nil {
		_, cwd = m.resolveWorkspace(rec.ProjectID)
	}

	diskBuf, _ := m.readDiskTranscript(rec.Harness, rec.ID)
	approval := m.effectiveApproval(rec.ApprovalMode)
	ctrl, err := m.startController(ctx, rec.Harness, cwd, rec.ID, rec.Model, rec.Effort, approval, diskBuf)
	if err != nil {
		return rec, nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	updated, _, _ := m.store.Update(rec.ID, func(r *SessionRecord) {
		r.Terminated = false
		r.UpdatedAt = now
		r.CWD = cwd
		if r.ApprovalMode == "" {
			r.ApprovalMode = approval
		}
		if b := gitBranch(cwd); b != "" {
			r.Branch = b
		}
	})

	m.mu.Lock()
	m.live[rec.ID] = &liveEntry{
		ctrl:        ctrl,
		lastDiskMod: m.transcriptModTime(rec.Harness, rec.ID),
	}
	m.mu.Unlock()

	m.emit("session.updated", map[string]any{"sessionId": rec.ID})
	return updated, ctrl, nil
}

// Conversation returns the conversation history and live streaming buffer for
// sessionID. When the session is idle and the local CLI wrote newer turns to
// disk, the fresher disk transcript is returned automatically.
func (m *Manager) Conversation(_ context.Context, sessionID string, q ao.ConversationQuery) (*ao.Conversation, error) {
	rec, ok := m.store.Get(sessionID)
	if !ok {
		return nil, notFoundErr(sessionID)
	}

	m.mu.Lock()
	entry := m.live[sessionID]
	m.mu.Unlock()

	if entry != nil {
		st, _ := entry.ctrl.State()
		if st == "active" {
			return entry.ctrl.Conversation(rec.DisplayName, q), nil
		}
		if st != "exited" {
			diskMod := m.transcriptModTime(rec.Harness, rec.ID)
			if !entry.lastDiskMod.IsZero() && diskMod.After(entry.lastDiskMod.Add(2*time.Second)) {
				m.mu.Lock()
				if cur := m.live[sessionID]; cur == entry {
					cur.needsReload = true
				}
				m.mu.Unlock()
			} else {
				return entry.ctrl.Conversation(rec.DisplayName, q), nil
			}
		}
	}

	buf, err := m.readDiskTranscript(rec.Harness, rec.ID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("read native transcript failed", "session", sessionID, "err", err)
	}
	if buf == nil {
		buf = newConvBuffer()
	}
	return buf.snapshot(rec.ID, rec.Harness, "native", rec.DisplayName, q), nil
}

// SendMessage sends a user prompt to the native session, resuming it on demand.
func (m *Manager) SendMessage(ctx context.Context, sessionID, text, clientMessageID string) (*ao.SendResult, error) {
	_, ctrl, err := m.ensureLive(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return ctrl.SendMessage(ctx, text, clientMessageID)
}

// Interrupt cancels the in-flight turn on a running session.
func (m *Manager) Interrupt(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	entry := m.live[sessionID]
	m.mu.Unlock()
	if entry == nil {
		return nil
	}
	return entry.ctrl.Interrupt(ctx)
}

// Resume starts the native CLI process for an idle or stopped session.
func (m *Manager) Resume(ctx context.Context, sessionID string) (*ao.Session, error) {
	rec, _, err := m.ensureLive(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	sess := m.sessionFromRecord(rec)
	return &sess, nil
}

// Kill stops the live CLI subprocess while keeping the session registered.
func (m *Manager) Kill(_ context.Context, sessionID string) error {
	if _, ok := m.store.Get(sessionID); !ok {
		return notFoundErr(sessionID)
	}
	m.mu.Lock()
	entry := m.live[sessionID]
	delete(m.live, sessionID)
	m.mu.Unlock()
	if entry != nil {
		_ = entry.ctrl.Close()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, _, err := m.store.Update(sessionID, func(r *SessionRecord) {
		r.Terminated = true
		r.UpdatedAt = now
	})
	m.emit("session.updated", map[string]any{"sessionId": sessionID})
	return err
}

// DeleteSession stops any live process and removes the session from the board's
// native registry without deleting the user's local ~/.codex or ~/.copilot files.
func (m *Manager) DeleteSession(_ context.Context, sessionID string, _ bool) error {
	m.mu.Lock()
	entry := m.live[sessionID]
	delete(m.live, sessionID)
	m.mu.Unlock()
	if entry != nil {
		_ = entry.ctrl.Close()
	}
	if err := m.store.Delete(sessionID); err != nil {
		return err
	}
	m.emit("session.deleted", map[string]any{"sessionId": sessionID})
	return nil
}

// Rename updates the session's display name.
func (m *Manager) Rename(_ context.Context, sessionID, displayName string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, ok, err := m.store.Update(sessionID, func(r *SessionRecord) {
		r.DisplayName = strings.TrimSpace(displayName)
		r.UpdatedAt = now
	})
	if err != nil {
		return err
	}
	if !ok {
		return notFoundErr(sessionID)
	}
	m.emit("session.updated", map[string]any{"sessionId": sessionID})
	return nil
}

// ImportCandidates lists local Copilot and Codex CLI sessions discovered on disk.
func (m *Manager) ImportCandidates(ctx context.Context) ([]ao.ImportCandidate, error) {
	sessions := m.cfg.History.Sessions(ctx)
	existing := map[string]SessionRecord{}
	for _, r := range m.store.List() {
		existing[r.ID] = r
	}

	out := make([]ao.ImportCandidate, 0, len(sessions))
	for _, s := range sessions {
		cand := ao.ImportCandidate{
			ID:           s.CandidateID,
			Harness:      s.Harness,
			Title:        s.Title,
			ProjectID:    m.projectForCWD(s.CWD),
			Folder:       s.CWD,
			LastActivity: s.UpdatedAt,
			MessageCount: len(s.Messages),
		}
		if rec, ok := existing[s.NativeID]; ok {
			cand.SessionID = rec.ID
		}
		out = append(out, cand)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivity > out[j].LastActivity
	})
	return out, nil
}

// Import registers local CLI sessions by candidate ID (or native UUID) so they
// can be tracked on the board and resumed natively in their original CWD.
func (m *Manager) Import(ctx context.Context, ids []string) ([]ao.ImportResult, error) {
	sessions := m.cfg.History.Sessions(ctx)
	byNative := map[string]*history.Session{}
	for _, s := range sessions {
		byNative[s.NativeID] = s
	}

	results := make([]ao.ImportResult, 0, len(ids))
	for _, id := range ids {
		s := sessions[id]
		if s == nil {
			s = byNative[id]
		}
		if s == nil {
			results = append(results, ao.ImportResult{
				ID:     id,
				Status: "error",
				Error:  "session not found in local ~/.codex or ~/.copilot stores",
			})
			continue
		}

		if existing, ok := m.store.Get(s.NativeID); ok {
			results = append(results, ao.ImportResult{
				ID:        id,
				Status:    "already_imported",
				SessionID: existing.ID,
			})
			continue
		}

		now := time.Now().UTC().Format(time.RFC3339)
		lastAct := s.UpdatedAt
		if lastAct == "" {
			lastAct = now
		}
		rec := SessionRecord{
			ID:             s.NativeID,
			Harness:        s.Harness,
			ProjectID:      m.projectForCWD(s.CWD),
			CWD:            s.CWD,
			Branch:         s.Branch,
			DisplayName:    s.Title,
			ApprovalMode:   m.effectiveApproval(""),
			CreatedAt:      lastAct,
			UpdatedAt:      now,
			LastActivityAt: lastAct,
		}
		if err := m.store.Upsert(rec); err != nil {
			results = append(results, ao.ImportResult{
				ID:     id,
				Status: "error",
				Error:  err.Error(),
			})
			continue
		}
		m.emit("session.created", map[string]any{"sessionId": rec.ID})
		results = append(results, ao.ImportResult{
			ID:        id,
			Status:    "imported",
			SessionID: rec.ID,
		})
	}
	return results, nil
}

// Events streams live session and conversation updates until ctx is cancelled.
func (m *Manager) Events(ctx context.Context, _ string, fn func(ao.Event)) error {
	ch := make(chan ao.Event, 128)
	m.evMu.Lock()
	m.nextSub++
	subID := m.nextSub
	m.evSubs[subID] = ch
	m.evMu.Unlock()

	defer func() {
		m.evMu.Lock()
		delete(m.evSubs, subID)
		m.evMu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-ch:
			fn(ev)
		}
	}
}

// AgentModels returns the available models for "codex" or "copilot", caching
// the result after the first CLI probe.
func (m *Manager) AgentModels(ctx context.Context, agent string) (*ao.AgentModels, error) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	m.modelsMu.Lock()
	if cached, ok := m.modelsCache[agent]; ok {
		m.modelsMu.Unlock()
		return cached, nil
	}
	m.modelsMu.Unlock()

	var (
		res *ao.AgentModels
		err error
	)
	switch agent {
	case "codex":
		_, cwd := m.resolveWorkspace("")
		res, err = listCodexModels(ctx, m.cfg.CodexPath, cwd)
	case "copilot":
		res, err = listCopilotModels(ctx, m.cfg.CopilotPath, m.cfg.HomeDir)
	default:
		return nil, &ao.APIError{Status: 400, Code: "INVALID_AGENT", Message: fmt.Sprintf("unknown agent %q", agent)}
	}
	if err != nil {
		return nil, err
	}

	m.modelsMu.Lock()
	m.modelsCache[agent] = res
	m.modelsMu.Unlock()
	return res, nil
}

// ConversationModels returns the model list and active per-turn settings of a
// session. If the session is idle, cached agent models are returned alongside
// the persisted settings so the user can adjust settings before the next turn.
func (m *Manager) ConversationModels(ctx context.Context, sessionID string) (*ao.ConversationModels, error) {
	rec, ok := m.store.Get(sessionID)
	if !ok {
		return nil, notFoundErr(sessionID)
	}

	m.mu.Lock()
	entry := m.live[sessionID]
	m.mu.Unlock()
	if entry != nil {
		if st, _ := entry.ctrl.State(); st != "exited" {
			return entry.ctrl.Models(), nil
		}
	}

	out := &ao.ConversationModels{
		Selected: ao.ConversationSettings{
			Model:           rec.Model,
			ReasoningEffort: rec.Effort,
			ApprovalMode:    m.effectiveApproval(rec.ApprovalMode),
		},
	}
	if catalog, err := m.AgentModels(ctx, rec.Harness); err == nil && catalog != nil {
		out.Models = make([]ao.ConversationModel, 0, len(catalog.Models))
		for _, am := range catalog.Models {
			out.Models = append(out.Models, ao.ConversationModel{
				ID:            am.ID,
				DisplayName:   am.Label,
				Default:       am.IsDefault,
				Efforts:       am.Efforts,
				DefaultEffort: am.DefaultEffort,
			})
		}
	}
	return out, nil
}

// UpdateConversationSettings updates the model, reasoning effort, and approval
// mode for the next turn of sessionID.
func (m *Manager) UpdateConversationSettings(_ context.Context, sessionID string, settings ao.ConversationTurnSettings) (*ao.ConversationSettings, error) {
	rec, ok, err := m.store.Update(sessionID, func(r *SessionRecord) {
		if settings.Model != nil {
			r.Model = strings.TrimSpace(*settings.Model)
		}
		if settings.ReasoningEffort != nil {
			r.Effort = strings.TrimSpace(*settings.ReasoningEffort)
		}
		if settings.ApprovalMode != nil {
			r.ApprovalMode = strings.TrimSpace(*settings.ApprovalMode)
		}
		r.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFoundErr(sessionID)
	}

	m.mu.Lock()
	entry := m.live[sessionID]
	m.mu.Unlock()
	if entry != nil {
		if st, _ := entry.ctrl.State(); st != "exited" {
			cur := entry.ctrl.UpdateSettings(settings)
			return &cur, nil
		}
	}
	return &ao.ConversationSettings{
		Model:           rec.Model,
		ReasoningEffort: rec.Effort,
		ApprovalMode:    rec.ApprovalMode,
	}, nil
}

func notFoundErr(sessionID string) error {
	return &ao.APIError{
		Status:  404,
		Code:    "SESSION_NOT_FOUND",
		Message: fmt.Sprintf("session %q not found", sessionID),
	}
}

func gitBranch(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" {
		return ""
	}
	return b
}
