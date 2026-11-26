// Command maintainer-board serves a kanban board for maintainer work that
// sits on top of a local agent-orchestrator (AO) daemon and the gh CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/github"
	"maintainer-board/internal/history"
	"maintainer-board/internal/native"
	"maintainer-board/internal/server"
	"maintainer-board/internal/summary"
	"maintainer-board/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "maintainer-board:", err)
		os.Exit(1)
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func run() error {
	var (
		addr           = flag.String("addr", "127.0.0.1:3012", "listen address")
		dbPath         = flag.String("db", "./data/board.db", "SQLite database path")
		runtimeMode    = flag.String("runtime", "native", "agent runtime: native (direct codex/copilot CLI, default) or ao (legacy AO daemon)")
		workspaces     = flag.String("workspace", "", "comma-separated local workspace paths for native mode (default: current directory)")
		aoURL          = flag.String("ao-url", "http://127.0.0.1:3001", "base URL of the local AO daemon (used with --runtime=ao)")
		aoProject      = flag.String("ao-project", "", "project for new agent sessions (default: the first workspace or AO project)")
		defaultRepo    = flag.String("default-repo", "volcano-sh/volcano", "repository used for bare #123 references")
		syncInterval   = flag.Duration("sync-interval", 120*time.Second, "GitHub background sync interval")
		aoSyncInterval = flag.Duration("ao-sync-interval", 5*time.Second, "AO session sync interval")
		ghPath         = flag.String("gh-path", "gh", "path of the gh CLI")
		ghUser         = flag.String("gh-user", "", "your GitHub login, for author and review status (default: the account gh is logged in as)")
		approvalMode   = flag.String("approval-mode", "", "default AO approval mode for newly spawned sessions: default, accept-edits, auto, or bypass-permissions (empty uses AO's default)")
		summaryModel   = flag.String("summary-model", summary.DefaultSummaryModel, "Copilot CLI model for Summarize with AI and AI Search; empty uses the CLI default (copilot's default model and codex remain as fallbacks)")
		summaryEffort  = flag.String("summary-effort", summary.DefaultSummaryEffort, "reasoning effort passed with --summary-model; empty passes none (a model that rejects the effort is retried without it)")
		copilotPath    = flag.String("copilot-path", "copilot", "path of the copilot CLI used for Summarize with AI and AI Search")
		codexPath      = flag.String("codex-path", "codex", "path of the codex CLI, the last fallback of the AI features")
		historyHome    = flag.String("history-home", "", "home directory whose .copilot and .codex session stores enrich Import History (default: your home directory)")
		ciIgnore       = flag.String("ci-ignore-contexts", "tide", "comma-separated status contexts and check names ignored in the CI rollup")
		prowLabels     = flag.Bool("prow-approval-labels", true, "treat the lgtm + approved label pair as an approval")
		allowedHosts   = flag.String("allowed-hosts", "", "comma-separated extra Host header values to accept besides loopback")
		logLevel       = flag.String("log-level", "info", "log level: debug, info, warn or error")
	)
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q", *logLevel)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if !github.ValidRepo(*defaultRepo) {
		return fmt.Errorf("invalid --default-repo %q: expected owner/name", *defaultRepo)
	}
	if m := strings.TrimSpace(*approvalMode); !server.ValidApprovalMode(m) {
		return fmt.Errorf("invalid --approval-mode %q: expected default, accept-edits, auto, or bypass-permissions", *approvalMode)
	}
	if m := strings.TrimSpace(*summaryModel); m != "" && !summary.ValidModel(m) {
		return fmt.Errorf("invalid --summary-model %q: expected a model name such as %s", *summaryModel, summary.DefaultSummaryModel)
	}
	if e := strings.TrimSpace(*summaryEffort); e != "" && !summary.ValidEffort(e) {
		return fmt.Errorf("invalid --summary-effort %q: expected an effort such as %s", *summaryEffort, summary.DefaultSummaryEffort)
	}
	if strings.TrimSpace(*copilotPath) == "" || strings.TrimSpace(*codexPath) == "" {
		return errors.New("--copilot-path and --codex-path must not be empty")
	}
	if *syncInterval < 10*time.Second {
		return errors.New("--sync-interval must be at least 10s")
	}
	if *aoSyncInterval < time.Second {
		return errors.New("--ao-sync-interval must be at least 1s")
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", *dbPath, err)
	}
	defer store.Close()

	gh := github.NewClient(*ghPath)
	gh.ProwLabels = *prowLabels
	gh.IgnoreContexts = map[string]bool{}
	for _, name := range splitList(*ciIgnore) {
		gh.IgnoreContexts[name] = true
	}

	if *ghUser != "" {
		if !github.ValidLogin(strings.TrimPrefix(strings.TrimSpace(*ghUser), "@")) {
			return fmt.Errorf("invalid --gh-user %q: expected a GitHub login", *ghUser)
		}
		gh.SetUser(*ghUser)
	} else {
		// Not fatal: offline or logged out, the board works without knowing
		// who "you" are and asks again before every GitHub sync.
		detectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := gh.DetectUser(detectCtx); err != nil {
			logger.Warn("could not detect the GitHub user; personal review status stays empty until gh answers (or pass --gh-user)", "err", err)
		}
		cancel()
	}

	helperDir := filepath.Join(filepath.Dir(*dbPath), "summarizer")
	ai := summary.NewAI(helperDir)
	ai.SummaryModel = strings.TrimSpace(*summaryModel)
	ai.SummaryEffort = strings.TrimSpace(*summaryEffort)
	ai.CopilotPath, ai.CodexPath = strings.TrimSpace(*copilotPath), strings.TrimSpace(*codexPath)

	histIdx := history.New(*historyHome)
	var (
		agentClient server.AOClient
		nativeMgr   *native.Manager
		runtimeInfo string
	)
	switch strings.ToLower(strings.TrimSpace(*runtimeMode)) {
	case "native", "":
		mgr, err := native.New(native.Config{
			DataDir:             filepath.Dir(*dbPath),
			HomeDir:             strings.TrimSpace(*historyHome),
			Workspaces:          splitList(*workspaces),
			CodexPath:           strings.TrimSpace(*codexPath),
			CopilotPath:         strings.TrimSpace(*copilotPath),
			DefaultApprovalMode: strings.TrimSpace(*approvalMode),
			History:             histIdx,
			Logger:              logger,
		})
		if err != nil {
			return fmt.Errorf("init native runtime: %w", err)
		}
		nativeMgr = mgr
		agentClient = mgr
		runtimeInfo = "native"
	case "ao":
		agentClient = ao.New(*aoURL)
		runtimeInfo = *aoURL
	default:
		return fmt.Errorf("invalid --runtime %q: expected native or ao", *runtimeMode)
	}

	srv := server.New(server.Config{
		DefaultRepo:        *defaultRepo,
		AOURL:              runtimeInfo,
		AOProject:          *aoProject,
		ApprovalMode:       strings.TrimSpace(*approvalMode),
		GitHubSyncInterval: *syncInterval,
		AOSyncInterval:     *aoSyncInterval,
		AllowedHosts:       splitList(*allowedHosts),
		CurrentUser:        strings.TrimPrefix(strings.TrimSpace(*ghUser), "@"),
		History:            histIdx,
		HelperDir:          helperDir,
	}, store, agentClient, gh, ai, web.Files, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	srv.Start(ctx)
	logger.Info("maintainer-board listening", "url", "http://"+listener.Addr().String(), "db", *dbPath, "runtime", runtimeInfo, "repo", *defaultRepo, "user", gh.User())

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	select {
	case err := <-serveErr:
		stop()
		srv.Wait()
		if nativeMgr != nil {
			nativeMgr.Close()
		}
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
	}
	srv.Wait()
	if nativeMgr != nil {
		nativeMgr.Close()
	}
	return nil
}
