package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Automation roles a column can carry.
const (
	RoleNone       = ""
	RoleInbox      = "inbox"
	RoleInProgress = "in_progress"
	RoleWaiting    = "waiting"
	RoleUpdates    = "updates"
	RoleReady      = "ready"
	RoleDone       = "done"
)

var validRoles = map[string]bool{
	RoleNone: true, RoleInbox: true, RoleInProgress: true, RoleWaiting: true,
	RoleUpdates: true, RoleReady: true, RoleDone: true,
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type Column struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Position int    `json:"position"`
	Color    string `json:"color"`
	Role     string `json:"role"`
}

// ColumnPatch holds optional column updates; nil fields are left unchanged.
type ColumnPatch struct {
	Title *string `json:"title"`
	Color *string `json:"color"`
	Role  *string `json:"role"`
}

func validateColumn(title, color, role string) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("%w: column title is required", ErrInvalid)
	}
	if len([]rune(title)) > 40 {
		return fmt.Errorf("%w: column title is too long", ErrInvalid)
	}
	if !hexColor.MatchString(color) {
		return fmt.Errorf("%w: color must be a #rrggbb hex value", ErrInvalid)
	}
	if !validRoles[role] {
		return fmt.Errorf("%w: unknown column role %q", ErrInvalid, role)
	}
	return nil
}

func (s *Store) ListColumns(ctx context.Context) ([]Column, error) {
	return listColumns(ctx, s.db)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func listColumns(ctx context.Context, q querier) ([]Column, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, title, position, color, role FROM columns ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Column{}
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.ID, &c.Title, &c.Position, &c.Color, &c.Role); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func getColumn(ctx context.Context, q querier, id string) (*Column, error) {
	var c Column
	err := q.QueryRowContext(ctx, `SELECT id, title, position, color, role FROM columns WHERE id = ?`, id).
		Scan(&c.ID, &c.Title, &c.Position, &c.Color, &c.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("column %q: %w", id, ErrNotFound)
	}
	return &c, err
}

func (s *Store) GetColumn(ctx context.Context, id string) (*Column, error) {
	return getColumn(ctx, s.db, id)
}

// ColumnByRole returns the left-most column carrying role, or nil when no
// column has that role.
func (s *Store) ColumnByRole(ctx context.Context, role string) (*Column, error) {
	if role == "" {
		return nil, nil
	}
	return columnByRole(ctx, s.db, role)
}

func columnByRole(ctx context.Context, q querier, role string) (*Column, error) {
	var c Column
	err := q.QueryRowContext(ctx,
		`SELECT id, title, position, color, role FROM columns WHERE role = ? ORDER BY position, id LIMIT 1`, role).
		Scan(&c.ID, &c.Title, &c.Position, &c.Color, &c.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// CreateColumn appends a new column. A non-empty role is exclusive: it is
// taken away from any column that currently carries it.
func (s *Store) CreateColumn(ctx context.Context, title, color, role string) (*Column, error) {
	title = strings.TrimSpace(title)
	if color == "" {
		color = "#6e7681"
	}
	if err := validateColumn(title, color, role); err != nil {
		return nil, err
	}
	col := &Column{ID: newID("col_"), Title: title, Color: strings.ToLower(color), Role: role}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM columns`).Scan(&col.Position); err != nil {
			return err
		}
		if role != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE columns SET role = '' WHERE role = ?`, role); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO columns (id, title, position, color, role) VALUES (?, ?, ?, ?, ?)`,
			col.ID, col.Title, col.Position, col.Color, col.Role)
		return err
	})
	if err != nil {
		return nil, err
	}
	return col, nil
}

func (s *Store) UpdateColumn(ctx context.Context, id string, p ColumnPatch) (*Column, error) {
	var out *Column
	err := s.tx(ctx, func(tx *sql.Tx) error {
		col, err := getColumn(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Title != nil {
			col.Title = strings.TrimSpace(*p.Title)
		}
		if p.Color != nil {
			col.Color = strings.ToLower(*p.Color)
		}
		if p.Role != nil {
			col.Role = *p.Role
		}
		if err := validateColumn(col.Title, col.Color, col.Role); err != nil {
			return err
		}
		if col.Role != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE columns SET role = '' WHERE role = ? AND id <> ?`, col.Role, id); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE columns SET title = ?, color = ?, role = ? WHERE id = ?`,
			col.Title, col.Color, col.Role, id)
		out = col
		return err
	})
	return out, err
}

// ReorderColumns applies the given left-to-right order. Columns missing from
// ids keep their relative order after the listed ones.
func (s *Store) ReorderColumns(ctx context.Context, ids []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		cols, err := listColumns(ctx, tx)
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, c := range cols {
			known[c.ID] = true
		}
		seen := map[string]bool{}
		order := make([]string, 0, len(cols))
		for _, id := range ids {
			if !known[id] {
				return fmt.Errorf("column %q: %w", id, ErrNotFound)
			}
			if seen[id] {
				return fmt.Errorf("%w: duplicate column %q", ErrInvalid, id)
			}
			seen[id] = true
			order = append(order, id)
		}
		for _, c := range cols {
			if !seen[c.ID] {
				order = append(order, c.ID)
			}
		}
		for i, id := range order {
			if _, err := tx.ExecContext(ctx, `UPDATE columns SET position = ? WHERE id = ?`, i, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteColumn removes a column and moves its cards to the inbox-role column,
// or to the first remaining column. It returns the column that received the
// cards.
func (s *Store) DeleteColumn(ctx context.Context, id string) (*Column, error) {
	var target *Column
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := getColumn(ctx, tx, id); err != nil {
			return err
		}
		cols, err := listColumns(ctx, tx)
		if err != nil {
			return err
		}
		if len(cols) <= 1 {
			return ErrLastColumn
		}
		for i := range cols {
			if cols[i].ID == id {
				continue
			}
			if target == nil {
				target = &cols[i]
			}
			if cols[i].Role == RoleInbox {
				target = &cols[i]
				break
			}
		}
		var base int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), -1) + 1 FROM cards WHERE column_id = ?`, target.ID).Scan(&base); err != nil {
			return err
		}
		ids, err := columnCardIDs(ctx, tx, id, "")
		if err != nil {
			return err
		}
		now := s.now()
		for i, cardID := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE cards SET column_id = ?, position = ?, updated_at = ? WHERE id = ?`,
				target.ID, base+i, now, cardID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM columns WHERE id = ?`, id); err != nil {
			return err
		}
		remaining, err := listColumns(ctx, tx)
		if err != nil {
			return err
		}
		for i, c := range remaining {
			if _, err := tx.ExecContext(ctx, `UPDATE columns SET position = ? WHERE id = ?`, i, c.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return target, nil
}
