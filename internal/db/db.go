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
	// 2: personal review columns on cards; see migrationSteps.
	"",
	// 3: text of the AO session conversations, kept locally so that the
	// board search also finds what was said in a chat. folded is the
	// lower-cased text the search runs on.
	`
CREATE TABLE IF NOT EXISTS content_index (
	session_id TEXT NOT NULL,
	message_id TEXT NOT NULL,
	sequence   INTEGER NOT NULL DEFAULT 0,
	role       TEXT NOT NULL DEFAULT '',
	text       TEXT NOT NULL,
	folded     TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (session_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_content_index_session ON content_index(session_id, sequence);

CREATE TABLE IF NOT EXISTS content_index_state (
	session_id    TEXT PRIMARY KEY,
	last_sequence INTEGER NOT NULL DEFAULT 0,
	stamp         TEXT NOT NULL DEFAULT '',
	updated_at    TEXT NOT NULL
);
`,
}

// columnDef is a column added to an existing table by a migration.
type columnDef struct {
	table, name, decl string
}

// migrationSteps holds the Go part of each migration, by migration index.
// Steps run after the SQL of the same index, inside the same transaction.
var migrationSteps = map[int]func(ctx context.Context, tx *sql.Tx) error{
	// 2: the maintainer's own review standing on the linked pull request or
	// issue. Purely additive, so a database written by an older binary keeps
	// every row and an older binary can still read a migrated database.
	1: func(ctx context.Context, tx *sql.Tx) error {
		return addColumns(ctx, tx, []columnDef{
			{"cards", "gh_my_review_state", "TEXT NOT NULL DEFAULT ''"},
			{"cards", "gh_my_reviewed_at", "TEXT NOT NULL DEFAULT ''"},
			{"cards", "gh_is_stale_review", "INTEGER NOT NULL DEFAULT 0"},
		})
	},
}

// schemaVersion is the user_version of a fully migrated database.
var schemaVersion = len(migrations)

// addColumns adds each column that the table does not have yet, so the
// migration also succeeds on a database where some of them already exist.
func addColumns(ctx context.Context, tx *sql.Tx, cols []columnDef) error {
	for _, col := range cols {
		var n int
		err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, col.table, col.name).Scan(&n)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, col.table, col.name, col.decl)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < schemaVersion; i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if migrations[i] != "" {
				if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
					return err
				}
			}
			if step := migrationSteps[i]; step != nil {
				if err := step(ctx, tx); err != nil {
					return err
				}
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
