// Package db is the SQLite persistence layer for Maintainer Board.
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrInvalid    = errors.New("invalid input")
	ErrLastColumn = errors.New("cannot delete the last column")
)

// Store wraps the SQLite database. A single connection is used so that every
// read-modify-write transaction is serialized.
type Store struct {
	db *sql.DB
	// Now is replaceable in tests.
	Now func() time.Time
}

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	} else {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	s := &Store{db: sqlDB, Now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) now() string { return s.Now().UTC().Format(time.RFC3339) }

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

var migrations = []string{
	// 1: initial schema and default columns.
	`
CREATE TABLE columns (
	id       TEXT PRIMARY KEY,
	title    TEXT NOT NULL,
	position INTEGER NOT NULL DEFAULT 0,
	color    TEXT NOT NULL DEFAULT '#6e7681',
	role     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE cards (
	id             TEXT PRIMARY KEY,
	title          TEXT NOT NULL,
	column_id      TEXT NOT NULL REFERENCES columns(id),
	position       INTEGER NOT NULL DEFAULT 0,
	category       TEXT NOT NULL DEFAULT 'DEV',
	tags           TEXT NOT NULL DEFAULT '[]',
	ao_session_id  TEXT NOT NULL DEFAULT '',
	ao_title       TEXT NOT NULL DEFAULT '',
	summary        TEXT NOT NULL DEFAULT '',
	summary_source TEXT NOT NULL DEFAULT 'auto',
	notes          TEXT NOT NULL DEFAULT '',
	auto_move      INTEGER NOT NULL DEFAULT 1,
	origin         TEXT NOT NULL DEFAULT 'manual',

	gh_repo                 TEXT NOT NULL DEFAULT '',
	gh_kind                 TEXT NOT NULL DEFAULT '',
	gh_number               INTEGER NOT NULL DEFAULT 0,
	gh_url                  TEXT NOT NULL DEFAULT '',
	gh_title                TEXT NOT NULL DEFAULT '',
	gh_author               TEXT NOT NULL DEFAULT '',
	gh_state                TEXT NOT NULL DEFAULT '',
	gh_ci_status            TEXT NOT NULL DEFAULT '',
	gh_review_decision      TEXT NOT NULL DEFAULT '',
	gh_head_sha             TEXT NOT NULL DEFAULT '',
	gh_commit_count         INTEGER NOT NULL DEFAULT 0,
	gh_comment_count        INTEGER NOT NULL DEFAULT 0,
	gh_review_comment_count INTEGER NOT NULL DEFAULT 0,
	gh_labels               TEXT NOT NULL DEFAULT '[]',
	gh_updated_at           TEXT NOT NULL DEFAULT '',
	gh_last_synced_at       TEXT NOT NULL DEFAULT '',
	gh_error                TEXT NOT NULL DEFAULT '',

	ack_head_sha             TEXT NOT NULL DEFAULT '',
	ack_commit_count         INTEGER NOT NULL DEFAULT 0,
	ack_comment_count        INTEGER NOT NULL DEFAULT 0,
	ack_review_comment_count INTEGER NOT NULL DEFAULT 0,
	ack_state                TEXT NOT NULL DEFAULT '',
	ack_ci_status            TEXT NOT NULL DEFAULT '',
	ack_review_decision      TEXT NOT NULL DEFAULT '',
	ack_updated_at           TEXT NOT NULL DEFAULT '',
	ack_at                   TEXT NOT NULL DEFAULT '',

	prev_head_sha             TEXT NOT NULL DEFAULT '',
	prev_commit_count         INTEGER NOT NULL DEFAULT 0,
	prev_comment_count        INTEGER NOT NULL DEFAULT 0,
	prev_review_comment_count INTEGER NOT NULL DEFAULT 0,
	prev_state                TEXT NOT NULL DEFAULT '',
	prev_ci_status            TEXT NOT NULL DEFAULT '',
	prev_review_decision      TEXT NOT NULL DEFAULT '',
	prev_updated_at           TEXT NOT NULL DEFAULT '',

	unread_updates     INTEGER NOT NULL DEFAULT 0,
	update_cycles      INTEGER NOT NULL DEFAULT 0,
	update_summary     TEXT NOT NULL DEFAULT '',
	has_unread_updates INTEGER NOT NULL DEFAULT 0,

	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX idx_cards_column ON cards(column_id, position);
CREATE INDEX idx_cards_session ON cards(ao_session_id);

CREATE TABLE card_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	card_id    TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL,
	message    TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX idx_card_events_card ON card_events(card_id, id);

CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- AO sessions whose cards were deleted or unlinked by the user; AO sync does
-- not create cards for them again.
CREATE TABLE ignored_sessions (
	session_id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
);

INSERT INTO columns (id, title, position, color, role) VALUES
	('inbox',       'Inbox',       0, '#6e7681', 'inbox'),
	('in_progress', 'In Progress', 1, '#3b82f6', 'in_progress'),
	('waiting',     'Waiting',     2, '#a855f7', 'waiting'),
	('updates',     'Updates',     3, '#f59e0b', 'updates'),
	('ready',       'Ready',       4, '#10b981', 'ready'),
	('done',        'Done',        5, '#52525b', 'done');
`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Settings

func (s *Store) GetSetting(ctx context.Context, key, fallback string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Ignored sessions

func (s *Store) IgnoreSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO ignored_sessions (session_id, created_at) VALUES (?, ?)`, sessionID, s.now())
	return err
}

func (s *Store) UnignoreSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ignored_sessions WHERE session_id = ?`, sessionID)
	return err
}

func (s *Store) IgnoredSessions(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id FROM ignored_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
