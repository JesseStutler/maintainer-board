package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// MaxIndexedRunes bounds the text kept per conversation message. Pasted
// logs and long diffs are cut there; the head of a message is what a search
// usually needs.
const MaxIndexedRunes = 20000

// ContentMessage is one conversation message stored for searching.
type ContentMessage struct {
	MessageID string
	Sequence  int64
	Role      string
	Text      string
	CreatedAt string
}

// ContentHit summarizes the messages of one session that contain a term.
type ContentHit struct {
	SessionID string
	// Count is the number of messages containing the term.
	Count int
	// The remaining fields describe the newest of those messages.
	Sequence  int64
	Role      string
	Text      string
	CreatedAt string
}

// Fold normalizes text for case-insensitive substring search.
func Fold(s string) string { return strings.ToLower(s) }

// ContentState returns how far the conversation of a session is indexed:
// the highest indexed sequence and the activity stamp seen at that time.
func (s *Store) ContentState(ctx context.Context, sessionID string) (lastSequence int64, stamp string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT last_sequence, stamp FROM content_index_state WHERE session_id = ?`, sessionID).Scan(&lastSequence, &stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return lastSequence, stamp, err
}

// IndexContent stores messages of a session, replacing earlier versions of
// the same message, and records the new index state. With reset true the
// existing rows of the session are dropped first.
func (s *Store) IndexContent(ctx context.Context, sessionID string, msgs []ContentMessage, lastSequence int64, stamp string, reset bool) error {
	if sessionID == "" {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if reset {
			if _, err := tx.ExecContext(ctx, `DELETE FROM content_index WHERE session_id = ?`, sessionID); err != nil {
				return err
			}
		}
		for _, m := range msgs {
			text := strings.TrimSpace(m.Text)
			if m.MessageID == "" || text == "" {
				continue
			}
			if r := []rune(text); len(r) > MaxIndexedRunes {
				text = string(r[:MaxIndexedRunes])
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO content_index (session_id, message_id, sequence, role, text, folded, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(session_id, message_id) DO UPDATE SET
					sequence = excluded.sequence, role = excluded.role, text = excluded.text,
					folded = excluded.folded, created_at = excluded.created_at`,
				sessionID, m.MessageID, m.Sequence, m.Role, text, Fold(text), m.CreatedAt)
			if err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO content_index_state (session_id, last_sequence, stamp, updated_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT(session_id) DO UPDATE SET last_sequence = excluded.last_sequence, stamp = excluded.stamp, updated_at = excluded.updated_at`,
			sessionID, lastSequence, stamp, s.now())
		return err
	})
}

// ContentHits returns, per session, how many indexed messages contain term
// and the newest one of them. The match is a case-insensitive substring
// match, which also works for text without word boundaries such as Chinese.
func (s *Store) ContentHits(ctx context.Context, term string) ([]ContentHit, error) {
	term = Fold(strings.TrimSpace(term))
	if term == "" {
		return nil, nil
	}
	// With a single MAX() aggregate SQLite takes the other columns from the
	// row that holds the maximum, here the newest matching message.
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, COUNT(*), MAX(sequence), role, text, created_at
		 FROM content_index WHERE instr(folded, ?) > 0 GROUP BY session_id ORDER BY session_id`, term)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentHit
	for rows.Next() {
		var h ContentHit
		if err := rows.Scan(&h.SessionID, &h.Count, &h.Sequence, &h.Role, &h.Text, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ContentExcerpts returns the first user message of a session followed by
// its newest messages (oldest first), for building a compact description
// of the conversation.
func (s *Store) ContentExcerpts(ctx context.Context, sessionID string, recent int) ([]ContentMessage, error) {
	if sessionID == "" {
		return nil, nil
	}
	scan := func(rows *sql.Rows) ([]ContentMessage, error) {
		defer rows.Close()
		var out []ContentMessage
		for rows.Next() {
			var m ContentMessage
			if err := rows.Scan(&m.MessageID, &m.Sequence, &m.Role, &m.Text, &m.CreatedAt); err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, rows.Err()
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT message_id, sequence, role, text, created_at FROM content_index
		 WHERE session_id = ? AND role = 'user' ORDER BY sequence LIMIT 1`, sessionID)
	if err != nil {
		return nil, err
	}
	first, err := scan(rows)
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx,
		`SELECT message_id, sequence, role, text, created_at FROM content_index
		 WHERE session_id = ? ORDER BY sequence DESC LIMIT ?`, sessionID, max(recent, 0))
	if err != nil {
		return nil, err
	}
	latest, err := scan(rows)
	if err != nil {
		return nil, err
	}
	out := first
	for i := len(latest) - 1; i >= 0; i-- {
		if len(first) > 0 && latest[i].MessageID == first[0].MessageID {
			continue
		}
		out = append(out, latest[i])
	}
	return out, nil
}

// ContentStats reports how many sessions and messages are indexed.
func (s *Store) ContentStats(ctx context.Context) (sessions, messages int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT session_id), COUNT(*) FROM content_index`).Scan(&sessions, &messages)
	return sessions, messages, err
}
