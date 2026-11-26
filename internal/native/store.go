// Package native runs Copilot CLI (--acp) and Codex CLI (app-server)
// directly without Agent Orchestrator, preserving 100% native prompts,
// tools, permissions, and working directories.
package native

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// SessionRecord is the persisted metadata of one native session managed by
// the board. Its ID is the real native Codex thread UUID or Copilot session
// UUID stored under ~/.codex or ~/.copilot.
type SessionRecord struct {
	ID             string `json:"id"`
	Harness        string `json:"harness"` // "codex" or "copilot"
	ProjectID      string `json:"projectId,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	Branch         string `json:"branch,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	ApprovalMode   string `json:"approvalMode,omitempty"`
	CreatedAt      string `json:"createdAt,omitempty"`
	UpdatedAt      string `json:"updatedAt,omitempty"`
	LastActivityAt string `json:"lastActivityAt,omitempty"`
	Terminated     bool   `json:"terminated,omitempty"`
}

type persistedState struct {
	Version  int             `json:"version"`
	Sessions []SessionRecord `json:"sessions"`
}

// sessionStore persists managed native sessions to a JSON file atomically.
type sessionStore struct {
	mu   sync.Mutex
	path string
	list []SessionRecord
	byID map[string]int
}

func openSessionStore(path string) (*sessionStore, error) {
	st := &sessionStore{
		path: path,
		byID: map[string]int{},
	}
	if err := st.load(); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *sessionStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return err
	}
	s.list = ps.Sessions
	s.byID = make(map[string]int, len(s.list))
	for i, rec := range s.list {
		s.byID[rec.ID] = i
	}
	return nil
}

func (s *sessionStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(persistedState{
		Version:  1,
		Sessions: s.list,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *sessionStore) List() []SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionRecord, len(s.list))
	copy(out, s.list)
	return out
}

func (s *sessionStore) Get(id string) (SessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byID[id]
	if !ok {
		return SessionRecord{}, false
	}
	return s.list[idx], true
}

func (s *sessionStore) Upsert(rec SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx, ok := s.byID[rec.ID]; ok {
		s.list[idx] = rec
	} else {
		s.byID[rec.ID] = len(s.list)
		s.list = append(s.list, rec)
	}
	return s.saveLocked()
}

func (s *sessionStore) Update(id string, fn func(*SessionRecord)) (SessionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byID[id]
	if !ok {
		return SessionRecord{}, false, nil
	}
	fn(&s.list[idx])
	rec := s.list[idx]
	return rec, true, s.saveLocked()
}

func (s *sessionStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.byID[id]
	if !ok {
		return nil
	}
	s.list = append(s.list[:idx], s.list[idx+1:]...)
	s.byID = make(map[string]int, len(s.list))
	for i, rec := range s.list {
		s.byID[rec.ID] = i
	}
	return s.saveLocked()
}
