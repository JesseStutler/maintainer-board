// Package server exposes the Maintainer Board HTTP API, serves the embedded
// web UI and runs the AO and GitHub background sync loops.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/github"
	"maintainer-board/internal/summary"
)

// AOClient is the part of the AO daemon API the board uses.
type AOClient interface {
	ListSessions(ctx context.Context) ([]ao.Session, error)
	ListProjects(ctx context.Context) ([]ao.Project, error)
	Spawn(ctx context.Context, req ao.SpawnRequest) (*ao.Session, error)
	Conversation(ctx context.Context, sessionID string, q ao.ConversationQuery) (*ao.Conversation, error)
	SendMessage(ctx context.Context, sessionID, text, clientMessageID string) (*ao.SendResult, error)
	Interrupt(ctx context.Context, sessionID string) error
	Resume(ctx context.Context, sessionID string) (*ao.Session, error)
	Kill(ctx context.Context, sessionID string) error
	Rename(ctx context.Context, sessionID, displayName string) error
	ImportCandidates(ctx context.Context) ([]ao.ImportCandidate, error)
	Import(ctx context.Context, ids []string) ([]ao.ImportResult, error)
	Events(ctx context.Context, lastEventID string, fn func(ao.Event)) error
}

// GitHubClient fetches the live state of a pull request or issue.
type GitHubClient interface {
	Fetch(ctx context.Context, repo string, number int, knownKind string) (*github.Item, error)
}

// Summarizer produces an AI summary and names the engine that wrote it.
type Summarizer interface {
	Summarize(ctx context.Context, in summary.Input) (text, engine string, err error)
}

type Config struct {
	// DefaultRepo resolves bare "#123" references.
	DefaultRepo string
	// AOURL is shown in error messages only; the client carries the real URL.
	AOURL string
	// AOProject is the AO project new sessions are spawned in. Empty means
	// the first project the daemon reports.
	AOProject string
	// GitHubSyncInterval is the period of the background GitHub sync.
	GitHubSyncInterval time.Duration
	// AOSyncInterval is the period of the background AO session sync.
	AOSyncInterval time.Duration
	// AllowedHosts lists extra Host header values accepted besides loopback.
	AllowedHosts []string
}

type Server struct {
	cfg   Config
	store *db.Store
	ao    AOClient
	gh    GitHubClient
	ai    Summarizer
	log   *slog.Logger
	web   fs.FS
	hub   *hub

	// aoSyncMu serializes AO reconciliation with spawn and import, so a new
	// session never gets two cards.
	aoSyncMu sync.Mutex

	mu             sync.RWMutex
	sessions       map[string]ao.Session
	sessionOrder   []string
	projects       []ao.Project
	projectsAt     time.Time
	aoConnected    bool
	aoError        string
	aoLastSync     time.Time
	aoFingerprint  string
	summaryStamp   map[string]summaryMark
	ghSyncing      bool
	ghLastSync     time.Time
	ghLastError    string
	ghSyncDone     chan struct{}
	ghLastReport   SyncReport
	aoSyncRequests chan struct{}

	bg sync.WaitGroup
	// bgCtx is cancelled on shutdown; background work started by handlers
	// is tied to it.
	bgCtx context.Context
}

type summaryMark struct {
	stamp string
	at    time.Time
}

func New(cfg Config, store *db.Store, aoClient AOClient, gh GitHubClient, ai Summarizer, web fs.FS, logger *slog.Logger) *Server {
	if cfg.GitHubSyncInterval <= 0 {
		cfg.GitHubSyncInterval = 120 * time.Second
	}
	if cfg.AOSyncInterval <= 0 {
		cfg.AOSyncInterval = 5 * time.Second
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		cfg:            cfg,
		store:          store,
		ao:             aoClient,
		gh:             gh,
		ai:             ai,
		log:            logger,
		web:            web,
		hub:            newHub(),
		sessions:       map[string]ao.Session{},
		summaryStamp:   map[string]summaryMark{},
		aoSyncRequests: make(chan struct{}, 1),
		bgCtx:          context.Background(),
	}
}

// Start launches the background sync loops. They stop when ctx is cancelled;
// Wait blocks until they have finished.
func (s *Server) Start(ctx context.Context) {
	s.bgCtx = ctx
	s.bg.Add(3)
	go func() {
		defer s.bg.Done()
		s.aoLoop(ctx)
	}()
	go func() {
		defer s.bg.Done()
		s.aoEventLoop(ctx)
	}()
	go func() {
		defer s.bg.Done()
		s.githubLoop(ctx)
	}()
}

func (s *Server) Wait() { s.bg.Wait() }

// goBackground runs fn on the server's background context.
func (s *Server) goBackground(fn func(ctx context.Context)) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn(s.bgCtx)
	}()
}

// Handler returns the HTTP handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/board", s.handleBoard)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("PATCH /api/settings", s.handlePatchSettings)

	mux.HandleFunc("GET /api/columns", s.handleListColumns)
	mux.HandleFunc("POST /api/columns", s.handleCreateColumn)
	mux.HandleFunc("POST /api/columns/reorder", s.handleReorderColumns)
	mux.HandleFunc("PATCH /api/columns/{id}", s.handlePatchColumn)
	mux.HandleFunc("DELETE /api/columns/{id}", s.handleDeleteColumn)

	mux.HandleFunc("GET /api/cards", s.handleListCards)
	mux.HandleFunc("POST /api/cards", s.handleCreateCard)
	mux.HandleFunc("GET /api/cards/{id}", s.handleGetCard)
	mux.HandleFunc("PATCH /api/cards/{id}", s.handlePatchCard)
	mux.HandleFunc("DELETE /api/cards/{id}", s.handleDeleteCard)
	mux.HandleFunc("PATCH /api/cards/{id}/move", s.handleMoveCard)
	mux.HandleFunc("GET /api/cards/{id}/events", s.handleCardEvents)
	mux.HandleFunc("POST /api/cards/{id}/link-github", s.handleLinkGitHub)
	mux.HandleFunc("POST /api/cards/{id}/unlink-github", s.handleUnlinkGitHub)
	mux.HandleFunc("POST /api/cards/{id}/sync-github", s.handleSyncCardGitHub)
	mux.HandleFunc("POST /api/cards/{id}/ack", s.handleAck)
	mux.HandleFunc("POST /api/cards/{id}/summarize", s.handleSummarize)
	mux.HandleFunc("POST /api/cards/{id}/session", s.handleStartSession)

	mux.HandleFunc("POST /api/github/sync", s.handleGitHubSync)

	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handleRenameSession)
	mux.HandleFunc("GET /api/sessions/{id}/conversation", s.handleConversation)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.handleSendMessage)
	mux.HandleFunc("POST /api/sessions/{id}/interrupt", s.handleInterrupt)
	mux.HandleFunc("POST /api/sessions/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/sessions/{id}/kill", s.handleKill)

	mux.HandleFunc("GET /api/import/candidates", s.handleImportCandidates)
	mux.HandleFunc("POST /api/import", s.handleImport)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "No such API endpoint.")
	})
	mux.Handle("/", s.staticHandler())

	return s.securityMiddleware(mux)
}

func (s *Server) staticHandler() http.Handler {
	files := http.FileServerFS(s.web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed.")
			return
		}
		if strings.HasSuffix(r.URL.Path, ".go") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; "+
				"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		files.ServeHTTP(w, r)
	})
}

// securityMiddleware protects a loopback-bound server that is reached
// through a browser: it rejects foreign Host headers (DNS rebinding) and
// cross-origin state-changing requests (CSRF), and requires JSON bodies.
func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	allowed := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}
	for _, h := range s.cfg.AllowedHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			allowed[h] = true
		}
	}
	hostOK := func(hostport string) bool {
		host := hostport
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			host = h
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		return allowed[host]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")

		if !hostOK(r.Host) {
			writeError(w, http.StatusForbidden, "FORBIDDEN_HOST", "This host name is not allowed. Use 127.0.0.1 or localhost, or start the server with --allowed-hosts.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					writeError(w, http.StatusForbidden, "FORBIDDEN_ORIGIN", "Cross-origin requests are not allowed.")
					return
				}
			} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeError(w, http.StatusForbidden, "FORBIDDEN_ORIGIN", "Cross-origin requests are not allowed.")
				return
			}
			if r.ContentLength != 0 {
				ct := strings.ToLower(r.Header.Get("Content-Type"))
				if !strings.HasPrefix(ct, "application/json") {
					writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Request bodies must be application/json.")
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// JSON helpers

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message}})
}

// decodeBody parses an optional JSON body into v. An empty body is accepted.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(v)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "Request body is too large.")
		return false
	}
	writeError(w, http.StatusBadRequest, "INVALID_JSON", "Request body is not valid JSON: "+err.Error())
	return false
}

// writeStoreError maps persistence errors to HTTP responses.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", capitalize(err.Error())+".")
	case errors.Is(err, db.ErrInvalid):
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", capitalize(strings.TrimPrefix(err.Error(), db.ErrInvalid.Error()+": "))+".")
	case errors.Is(err, db.ErrLastColumn):
		writeError(w, http.StatusConflict, "LAST_COLUMN", "The board needs at least one column.")
	default:
		s.log.Error("store error", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Internal error: "+err.Error())
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// aoErrorMessages replaces terse daemon messages with actionable ones.
var aoErrorMessages = map[string]string{
	"SESSION_OPEN_ELSEWHERE":        "This session is open in another terminal. Close it there (or exit the agent CLI), then resume again.",
	"SESSION_NOT_FOUND":             "AO no longer has this session. It may have been deleted in AO desktop.",
	"SESSION_BUSY":                  "The session is busy with another operation. Try again in a moment.",
	"SESSION_OPERATION_IN_PROGRESS": "Another operation is already running for this session. Try again in a moment.",
	"SESSION_NOT_RESUMABLE":         "This session cannot be resumed. Start a new agent session from the card instead.",
	"NATIVE_SESSION_MISSING":        "The original agent session files are missing, so AO cannot resume it.",
	"SESSION_IMPORT_RESUME_TIMEOUT": "AO timed out while resuming the imported session. Try again.",
	"AGENT_EXIT_IN_PROGRESS":        "The agent is still shutting down. Try again in a moment.",
	"CHAT_NO_ACTIVE_TURN":           "There is no running turn to interrupt.",
}

// writeAOError maps AO client errors to HTTP responses, keeping AO's error
// code so the UI can react to specific conditions.
func (s *Server) writeAOError(w http.ResponseWriter, err error) {
	var apiErr *ao.APIError
	switch {
	case errors.As(err, &apiErr):
		status := apiErr.Status
		if status >= 500 || status < 400 {
			status = http.StatusBadGateway
		}
		code := apiErr.Code
		if code == "" {
			code = "AO_ERROR"
		}
		msg, ok := aoErrorMessages[apiErr.Code]
		if !ok {
			msg = capitalize(strings.TrimSpace(apiErr.Message))
			if msg == "" {
				msg = fmt.Sprintf("AO returned HTTP %d.", apiErr.Status)
			}
		}
		writeError(w, status, code, msg)
	case errors.Is(err, ao.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "AO_OFFLINE", fmt.Sprintf("The AO daemon is not reachable at %s.", s.cfg.AOURL))
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "AO_TIMEOUT", "The AO daemon did not answer in time.")
	default:
		s.log.Error("ao error", "err", err)
		writeError(w, http.StatusBadGateway, "AO_ERROR", err.Error())
	}
}

// hub fans board-changed notifications out to SSE subscribers.
type hub struct {
	mu      sync.Mutex
	version int64
	subs    map[chan int64]struct{}
}

func newHub() *hub { return &hub{subs: map[chan int64]struct{}{}} }

func (h *hub) Version() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.version
}

// Publish bumps the board version and wakes every subscriber. Slow
// subscribers only ever miss intermediate versions, never the latest.
func (h *hub) Publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.version++
	for ch := range h.subs {
		select {
		case ch <- h.version:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- h.version:
			default:
			}
		}
	}
}

func (h *hub) Subscribe() (ch chan int64, cancel func()) {
	ch = make(chan int64, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Streaming is not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, cancel := s.hub.Subscribe()
	defer cancel()
	fmt.Fprintf(w, "retry: 3000\nevent: board\ndata: {\"version\":%d}\n\n", s.hub.Version())
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.bgCtx.Done():
			return
		case v := <-ch:
			fmt.Fprintf(w, "event: board\ndata: {\"version\":%d}\n\n", v)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
