package db

import (
	"context"
	"database/sql"
)

// Card event kinds recorded in the audit timeline.
const (
	EventCreated = "created"
	EventMove    = "move"
	EventUpdate  = "update"
	EventAck     = "ack"
	EventSummary = "summary"
	EventGitHub  = "github"
	EventSession = "session"
)

type Event struct {
	ID        int64  `json:"id"`
	CardID    string `json:"cardId"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	CreatedAt string `json:"createdAt"`
}

type EventInput struct {
	Kind    string
	Message string
}

func insertEvents(ctx context.Context, tx *sql.Tx, cardID, now string, events []EventInput) error {
	for _, e := range events {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO card_events (card_id, kind, message, created_at) VALUES (?, ?, ?, ?)`,
			cardID, e.Kind, e.Message, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AddEvent(ctx context.Context, cardID, kind, message string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := getCard(ctx, tx, cardID); err != nil {
			return err
		}
		return insertEvents(ctx, tx, cardID, s.now(), []EventInput{{Kind: kind, Message: message}})
	})
}

// ListEvents returns the newest events of a card first.
func (s *Store) ListEvents(ctx context.Context, cardID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, card_id, kind, message, created_at FROM card_events WHERE card_id = ? ORDER BY id DESC LIMIT ?`,
		cardID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.CardID, &e.Kind, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
