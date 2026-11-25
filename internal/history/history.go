// Package history reads the local session stores of the Copilot and Codex
// CLIs, read-only, so that the board can show and search what an importable
// session was about: its native ID, working directory, branch, summary and
// the text of its conversation.
package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	HarnessCopilot = "copilot"
	HarnessCodex   = "codex"

	// DefaultTTL is how long a scan of the local stores is reused.
	DefaultTTL = 30 * time.Second

	// A Codex rollout file can be hundreds of megabytes of tool output. The
	// requests of the user sit at its beginning and its end, so only those
	// two windows are read.
	codexHeadBytes = 1 << 20
	codexTailBytes = 256 << 10
	// A line is classified by its first codexProbeBytes only.
	codexProbeBytes = 1 << 10
	codexWorkers    = 4

	maxUserRunes      = 4000
	maxAssistantRunes = 1200
	maxUserMessages   = 60
	maxAgentMessages  = 80
	maxSessionRunes   = 60000
	maxCodexFiles     = 5000
)

// Message is one piece of a session conversation kept for searching.
type Message struct {
	// Role is "user", "assistant" or "checkpoint" (a stored summary).
	Role string
	Text string
}

// Session is what the local stores know about one Copilot or Codex session.
type Session struct {
	// CandidateID is the ID AO gives the session in its import list.
	CandidateID string
	NativeID    string
	Harness     string
	CWD         string
	Repository  string
	Branch      string
	// Title is the name the CLI stored for the session; for a session
	// without one it is the beginning of the first prompt.
	Title string
	// Summary is the overview of the newest checkpoint, or empty.
	Summary   string
	UpdatedAt string
	// Messages holds the user prompts first, in order, then the checkpoint
	// summaries and excerpts of the agent's replies.
	Messages []Message

	folded string
}

// CandidateID computes the ID AO assigns to a native session in
// GET /api/v1/session-import: the SHA-256 of harness, configuration
// directory and native ID, separated by NUL bytes.
func CandidateID(harness, configDir, nativeID string) string {
	sum := sha256.Sum256([]byte(harness + "\x00" + configDir + "\x00" + nativeID))
	return hex.EncodeToString(sum[:])
}

// Prompts returns up to n of the first user messages.
func (s *Session) Prompts(n int) []string {
	var out []string
	for _, m := range s.Messages {
		if len(out) == n {
			break
		}
		if m.Role == "user" {
			out = append(out, m.Text)
		}
	}
	return out
}

// Folded returns the lower-cased text of every message, one per line, for
// case-insensitive substring search.
func (s *Session) Folded() string { return s.folded }

func (s *Session) finish() {
	var b strings.Builder
	for _, m := range s.Messages {
		b.WriteString(strings.ToLower(m.Text))
		b.WriteByte('\n')
	}
	s.folded = b.String()
}

func cutRunes(s string, n int) string {
	if len(s) <= n || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// collector gathers the messages of one session within the size limits.
type collector struct {
	users, agents, notes []string
	seen                 map[string]bool
}

func (c *collector) add(role, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	limit := maxAssistantRunes
	if role == "user" {
		limit = maxUserRunes
	}
	text = cutRunes(text, limit)
	key := role + "\x00" + text
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	switch role {
	case "user":
		if len(c.users) < maxUserMessages {
			c.users = append(c.users, text)
		}
	case "assistant":
		if len(c.agents) < maxAgentMessages {
			c.agents = append(c.agents, text)
		}
	default:
		c.notes = append(c.notes, text)
	}
}

// messages lists the user prompts, then the checkpoint notes, then the
// agent excerpts, cut off at the session budget: what the user asked is
// what a search is most likely to be about.
func (c *collector) messages() []Message {
	var out []Message
	budget := maxSessionRunes
	for _, group := range []struct {
		role  string
		texts []string
	}{{"user", c.users}, {"checkpoint", c.notes}, {"assistant", c.agents}} {
		for _, text := range group.texts {
			n := utf8.RuneCountInString(text)
			if n > budget {
				return out
			}
			budget -= n
			out = append(out, Message{Role: group.role, Text: text})
		}
	}
	return out
}

// Index scans the local Copilot and Codex stores and caches the result.
// The zero value is not usable; create one with New.
type Index struct {
	// CopilotDir and CodexDir are the configuration directories of the two
	// CLIs. They are part of the candidate ID, so they have to be spelled
	// the way AO spells them: $HOME/.copilot and $HOME/.codex.
	CopilotDir string
	CodexDir   string
	// TTL is how long a scan is reused.
	TTL time.Duration
	// Now is replaceable in tests.
	Now func() time.Time

	mu       sync.Mutex
	at       time.Time
	sessions map[string]*Session

	copilotStamp string
	copilot      []*Session
	codex        map[string]codexEntry
}

type codexEntry struct {
	size    int64
	modTime time.Time
	session *Session
}

// New returns an index of the stores under home. An empty home means the
// home directory of the current user.
func New(home string) *Index {
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = dir
		}
	}
	return &Index{
		CopilotDir: filepath.Join(home, ".copilot"),
		CodexDir:   filepath.Join(home, ".codex"),
		TTL:        DefaultTTL,
		Now:        time.Now,
		codex:      map[string]codexEntry{},
	}
}

// Sessions returns every session found locally, keyed by AO candidate ID.
// Stores that are missing or unreadable are skipped: the result is then
// smaller, never an error. The map is shared and must not be modified.
func (ix *Index) Sessions(ctx context.Context) map[string]*Session {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.sessions != nil && ix.Now().Sub(ix.at) < ix.TTL {
		return ix.sessions
	}
	out := map[string]*Session{}
	for _, s := range ix.loadCopilot(ctx) {
		out[s.CandidateID] = s
	}
	for _, s := range ix.loadCodex(ctx) {
		out[s.CandidateID] = s
	}
	if ctx.Err() != nil && ix.sessions != nil {
		// A cancelled scan is incomplete; keep what the last one found.
		return ix.sessions
	}
	ix.sessions, ix.at = out, ix.Now()
	return out
}

// Invalidate makes the next call of Sessions scan again.
func (ix *Index) Invalidate() {
	if ix == nil {
		return
	}
	ix.mu.Lock()
	ix.at = time.Time{}
	ix.mu.Unlock()
}

// Copilot

// fileStamp changes whenever one of the files is written.
func fileStamp(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%d@%d", st.Size(), st.ModTime().UnixNano())
		}
		b.WriteByte('|')
	}
	return b.String()
}

// loadCopilot reads sessions, turns and checkpoints from the session store
// of the Copilot CLI. The database is opened read-only and is reread only
// when the file or its write-ahead log changed.
func (ix *Index) loadCopilot(ctx context.Context) []*Session {
	path := filepath.Join(ix.CopilotDir, "session-store.db")
	if _, err := os.Stat(path); err != nil {
		ix.copilot, ix.copilotStamp = nil, ""
		return nil
	}
	stamp := fileStamp(path, path+"-wal")
	if stamp == ix.copilotStamp {
		return ix.copilot
	}
	sessions, err := readCopilotStore(ctx, path, ix.CopilotDir)
	if err != nil {
		// Locked, corrupt or of an unknown schema: keep the last good read.
		return ix.copilot
	}
	ix.copilot, ix.copilotStamp = sessions, stamp
	return sessions
}

func readCopilotStore(ctx context.Context, path, configDir string) ([]*Session, error) {
	store, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	store.SetMaxOpenConns(1)

	rows, err := store.QueryContext(ctx,
		`SELECT id, COALESCE(cwd, ''), COALESCE(repository, ''), COALESCE(branch, ''), COALESCE(summary, ''), COALESCE(updated_at, '')
		 FROM sessions`)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Session{}
	collectors := map[string]*collector{}
	var order []*Session
	for rows.Next() {
		s := &Session{Harness: HarnessCopilot}
		if err := rows.Scan(&s.NativeID, &s.CWD, &s.Repository, &s.Branch, &s.Title, &s.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		s.CandidateID = CandidateID(HarnessCopilot, configDir, s.NativeID)
		byID[s.NativeID] = s
		collectors[s.NativeID] = &collector{}
		order = append(order, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A store without turns or checkpoints still yields the session list.
	rows, err = store.QueryContext(ctx,
		`SELECT session_id, COALESCE(substr(user_message, 1, 16000), ''), COALESCE(substr(assistant_response, 1, 6000), '')
		 FROM turns ORDER BY session_id, turn_index`)
	if err == nil {
		for rows.Next() {
			var id, user, assistant string
			if rows.Scan(&id, &user, &assistant) != nil {
				break
			}
			if c := collectors[id]; c != nil {
				c.add("user", user)
				c.add("assistant", assistant)
			}
		}
		rows.Close()
	}
	rows, err = store.QueryContext(ctx,
		`SELECT session_id, COALESCE(title, ''), COALESCE(substr(overview, 1, 6000), '')
		 FROM checkpoints ORDER BY session_id, checkpoint_number`)
	if err == nil {
		for rows.Next() {
			var id, title, overview string
			if rows.Scan(&id, &title, &overview) != nil {
				break
			}
			if s := byID[id]; s != nil {
				// The newest checkpoint describes the whole session so far.
				if overview = strings.TrimSpace(overview); overview != "" {
					s.Summary = overview
				}
				collectors[id].add("checkpoint", strings.TrimSpace(title+"\n"+overview))
			}
		}
		rows.Close()
	}
	for _, s := range order {
		s.Messages = collectors[s.NativeID].messages()
		s.finish()
	}
	return order, nil
}

// Codex

var codexFileRe = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// loadCodex walks the rollout files of the Codex CLI. A file is parsed
// again only when its size or modification time changed.
func (ix *Index) loadCodex(ctx context.Context) []*Session {
	root := filepath.Join(ix.CodexDir, "sessions")
	type found struct {
		path string
		info fs.FileInfo
	}
	var files []found
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil || len(files) >= maxCodexFiles {
			return filepath.SkipAll
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, found{path, info})
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil
	}

	// Parse the new and changed files on a few workers: a first scan of a
	// long history is CPU-bound.
	entries := make([]codexEntry, len(files))
	var stale []int
	for i, f := range files {
		entry, ok := ix.codex[f.path]
		if ok && entry.size == f.info.Size() && entry.modTime.Equal(f.info.ModTime()) {
			entries[i] = entry
		} else {
			stale = append(stale, i)
		}
	}
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < min(codexWorkers, len(stale)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				f := files[i]
				entries[i] = codexEntry{size: f.info.Size(), modTime: f.info.ModTime(), session: readCodexRollout(f.path, f.info, ix.CodexDir)}
			}
		}()
	}
	for _, i := range stale {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	cache := make(map[string]codexEntry, len(files))
	var out []*Session
	for i, f := range files {
		cache[f.path] = entries[i]
		if entries[i].session != nil {
			out = append(out, entries[i].session)
		}
	}
	ix.codex = cache
	return out
}

type codexContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		CWD  string `json:"cwd"`
		Git  *struct {
			Branch        string `json:"branch"`
			RepositoryURL string `json:"repository_url"`
		} `json:"git"`
		Role    string         `json:"role"`
		Message string         `json:"message"`
		Content []codexContent `json:"content"`
		Item    *struct {
			Type    string         `json:"type"`
			Content []codexContent `json:"content"`
		} `json:"item"`
	} `json:"payload"`
}

// The markers a line must contain to be worth decoding. Rollout files are
// written as compact JSON, and tool output that quotes such a marker has
// its quotes escaped, so a plain byte search is exact enough and keeps the
// megabytes of tool output from ever being parsed.
var (
	markSessionMeta  = []byte(`"type":"session_meta"`)
	markUserItem     = []byte(`"type":"UserMessage"`)
	markAgentItem    = []byte(`"type":"AgentMessage"`)
	markUserEvent    = []byte(`"type":"user_message"`)
	markAgentEvent   = []byte(`"type":"agent_message"`)
	markResponseItem = []byte(`"type":"response_item"`)
	markRoleUser     = []byte(`"role":"user"`)
	markRoleAgent    = []byte(`"role":"assistant"`)
)

func contentText(parts []codexContent) string {
	var texts []string
	for _, p := range parts {
		if strings.TrimSpace(p.Text) != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// injectedContext reports whether a user-role message of the raw model
// transcript is context the CLI inserted rather than something the user
// typed: instruction files, the environment description and the like.
func injectedContext(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "<") || strings.HasPrefix(text, "# AGENTS.md")
}

// readCodexWindows returns the complete lines within the first
// codexHeadBytes and the last codexTailBytes of the file.
func readCodexWindows(path string, size int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	whole := size <= codexHeadBytes+codexTailBytes
	headLen := int64(codexHeadBytes)
	if whole {
		headLen = size
	}
	head := make([]byte, headLen)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	head = head[:n]
	if !whole {
		// The last line of the window is cut off; drop it.
		if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
			head = head[:i]
		} else {
			head = nil
		}
	}
	lines := bytes.Split(head, []byte{'\n'})
	if whole {
		return lines, nil
	}
	tail := make([]byte, codexTailBytes)
	n, err = f.ReadAt(tail, size-codexTailBytes)
	if err != nil && err != io.EOF {
		return lines, nil
	}
	tail = tail[:n]
	// The first line of the window starts somewhere in the middle.
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		lines = append(lines, bytes.Split(tail[i+1:], []byte{'\n'})...)
	}
	return lines, nil
}

// readCodexRollout extracts the session of one rollout file, or nil when
// the file does not identify one.
func readCodexRollout(path string, info fs.FileInfo, configDir string) *Session {
	lines, err := readCodexWindows(path, info.Size())
	if err != nil {
		return nil
	}
	s := &Session{Harness: HarnessCodex, UpdatedAt: info.ModTime().UTC().Format(time.RFC3339)}
	if m := codexFileRe.FindStringSubmatch(filepath.Base(path)); m != nil {
		s.NativeID = strings.ToLower(m[1])
	}
	// Events carry exactly what the user typed and the agent answered. The
	// raw model transcript is the fallback for files without such events.
	var events, transcript collector
	metaSeen := false
	for _, line := range lines {
		// The record type and role come first in a line; looking only at
		// its beginning skips megabytes of tool output without scanning it.
		probe := line
		if len(probe) > codexProbeBytes {
			probe = probe[:codexProbeBytes]
		}
		isMeta := !metaSeen && bytes.Contains(probe, markSessionMeta)
		isEvent := bytes.Contains(probe, markUserItem) || bytes.Contains(probe, markAgentItem) ||
			bytes.Contains(probe, markUserEvent) || bytes.Contains(probe, markAgentEvent)
		isTranscript := bytes.Contains(probe, markResponseItem) && (bytes.Contains(probe, markRoleUser) || bytes.Contains(probe, markRoleAgent))
		if !isMeta && !isEvent && !isTranscript {
			continue
		}
		var rec codexLine
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		p := rec.Payload
		switch {
		case rec.Type == "session_meta":
			if metaSeen {
				continue
			}
			metaSeen = true
			if p.ID != "" {
				s.NativeID = p.ID
			}
			s.CWD = p.CWD
			if p.Git != nil {
				s.Branch = p.Git.Branch
				s.Repository = strings.TrimSuffix(strings.TrimPrefix(p.Git.RepositoryURL, "https://github.com/"), ".git")
			}
		case rec.Type == "event_msg" && p.Type == "item_completed" && p.Item != nil:
			switch p.Item.Type {
			case "UserMessage":
				events.add("user", contentText(p.Item.Content))
			case "AgentMessage":
				events.add("assistant", contentText(p.Item.Content))
			}
		case rec.Type == "event_msg" && p.Type == "user_message":
			events.add("user", p.Message)
		case rec.Type == "event_msg" && p.Type == "agent_message":
			events.add("assistant", p.Message)
		case rec.Type == "response_item" && p.Type == "message":
			text := contentText(p.Content)
			if p.Role == "user" && !injectedContext(text) {
				transcript.add("user", text)
			} else if p.Role == "assistant" {
				transcript.add("assistant", text)
			}
		}
	}
	if s.NativeID == "" {
		return nil
	}
	if len(events.users) == 0 {
		events.users = transcript.users
	}
	if len(events.agents) == 0 {
		events.agents = transcript.agents
	}
	s.CandidateID = CandidateID(HarnessCodex, configDir, s.NativeID)
	s.Messages = events.messages()
	if prompts := s.Prompts(1); len(prompts) > 0 {
		s.Title = cutRunes(strings.Join(strings.Fields(prompts[0]), " "), 120)
	}
	s.finish()
	return s
}
