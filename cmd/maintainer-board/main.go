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
		dbPath         = flag.String("db", "/root/tools/maintainer-board/data/board.db", "SQLite database path")
		aoURL          = flag.String("ao-url", "http://127.0.0.1:3001", "base URL of the local AO daemon")
		aoProject      = flag.String("ao-project", "", "AO project for new agent sessions (default: the first project AO reports)")
		defaultRepo    = flag.String("default-repo", "volcano-sh/volcano", "repository used for bare #123 references")
		syncInterval   = flag.Duration("sync-interval", 120*time.Second, "GitHub background sync interval")
		aoSyncInterval = flag.Duration("ao-sync-interval", 5*time.Second, "AO session sync interval")
		ghPath         = flag.String("gh-path", "gh", "path of the gh CLI")
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

	ai := summary.NewAI(filepath.Join(filepath.Dir(*dbPath), "summarizer"))

	srv := server.New(server.Config{
		DefaultRepo:        *defaultRepo,
		AOURL:              *aoURL,
		AOProject:          *aoProject,
		GitHubSyncInterval: *syncInterval,
		AOSyncInterval:     *aoSyncInterval,
		AllowedHosts:       splitList(*allowedHosts),
	}, store, ao.New(*aoURL), gh, ai, web.Files, logger)

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
	logger.Info("maintainer-board listening", "url", "http://"+listener.Addr().String(), "db", *dbPath, "ao", *aoURL, "repo", *defaultRepo)

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	select {
	case err := <-serveErr:
		stop()
		srv.Wait()
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
	return nil
}
