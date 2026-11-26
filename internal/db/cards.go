package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	CategoryPRReview = "PR_REVIEW"
	CategoryIssue    = "ISSUE"
	CategoryDev      = "DEV"
	CategoryDesign   = "DESIGN"
	CategoryTriage   = "TRIAGE"

	OriginManual = "manual"
	OriginAO     = "ao"

	SummaryAuto   = "auto"
	SummaryAI     = "ai"
	SummaryManual = "manual"
)

var validCategories = map[string]bool{
	CategoryPRReview: true, CategoryIssue: true, CategoryDev: true, CategoryDesign: true, CategoryTriage: true,
}

var validSummarySources = map[string]bool{SummaryAuto: true, SummaryAI: true, SummaryManual: true}

func ValidCategory(c string) bool { return validCategories[c] }

// StringList is a []string persisted as a JSON array and never encoded as null.
type StringList []string

func (l StringList) Value() (driver.Value, error) {
	b, err := json.Marshal([]string(l.nonNil()))
	return string(b), err
}

func (l *StringList) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	case nil:
		*l = StringList{}
		return nil
	default:
		return fmt.Errorf("StringList: unsupported type %T", src)
	}
	var out []string
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
	}
	*l = StringList(out).nonNil()
	return nil
}

func (l StringList) MarshalJSON() ([]byte, error) { return json.Marshal([]string(l.nonNil())) }

func (l StringList) nonNil() StringList {
	if l == nil {
		return StringList{}
	}
	return l
}

// NormalizeTags trims, de-duplicates and bounds a tag list.
func NormalizeTags(in []string) StringList {
	out := StringList{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if r := []rune(t); len(r) > 40 {
			t = string(r[:40])
		}
		key := strings.ToLower(t)
		if t == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) == 20 {
			break
		}
	}
	return out
}

type Card struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	ColumnID      string     `json:"columnId"`
	Position      int        `json:"position"`
	Category      string     `json:"category"`
	Tags          StringList `json:"tags"`
	AOSessionID   string     `json:"aoSessionId"`
	AOTitle       string     `json:"-"`
	Summary       string     `json:"summary"`
	SummarySource string     `json:"summarySource"`
	Notes         string     `json:"notes"`
	AutoMove      bool       `json:"autoMove"`
	// Origin is "manual" for cards created by the user and "ao" for cards
	// created automatically from an AO session.
	Origin string `json:"origin"`

	GHRepo               string     `json:"ghRepo"`
	GHKind               string     `json:"ghKind"`
	GHNumber             int        `json:"ghNumber"`
	GHURL                string     `json:"ghUrl"`
	GHTitle              string     `json:"ghTitle"`
	GHAuthor             string     `json:"ghAuthor"`
	GHState              string     `json:"ghState"`
	GHCIStatus           string     `json:"ghCiStatus"`
	GHReviewDecision     string     `json:"ghReviewDecision"`
	GHHeadSHA            string     `json:"ghHeadSha"`
	GHCommitCount        int        `json:"ghCommitCount"`
	GHCommentCount       int        `json:"ghCommentCount"`
	GHReviewCommentCount int        `json:"ghReviewCommentCount"`
	GHLabels             StringList `json:"ghLabels"`
	GHUpdatedAt          string     `json:"ghUpdatedAt"`
	GHLastSyncedAt       string     `json:"ghLastSyncedAt"`
	GHError              string     `json:"ghError"`

	// GHMyReviewState is the maintainer's own review standing: "", APPROVED,
	// LGTM, CHANGES_REQUESTED, COMMENTED (all read from GitHub) or REVIEWED
	// (marked by hand on the board).
	GHMyReviewState string `json:"ghMyReviewState"`
	// GHMyReviewedAt is the time of the maintainer's latest review or mark.
	GHMyReviewedAt string `json:"ghMyReviewedAt"`
	// GHIsStaleReview is true when the item changed after that review.
	GHIsStaleReview bool `json:"ghIsStaleReview"`
	// GHPendingFeedback summarizes reviewers who commented on the user's own
	// open PR or issue after the user's latest commit or reply.
	GHPendingFeedback string `json:"ghPendingFeedback"`

	AckHeadSHA            string `json:"ackHeadSha"`
	AckCommitCount        int    `json:"ackCommitCount"`
	AckCommentCount       int    `json:"ackCommentCount"`
	AckReviewCommentCount int    `json:"ackReviewCommentCount"`
	AckState              string `json:"ackState"`
	AckCIStatus           string `json:"ackCiStatus"`
	AckReviewDecision     string `json:"ackReviewDecision"`
	AckUpdatedAt          string `json:"ackUpdatedAt"`
	AckOtherActivityAt    string `json:"-"`
	AckAt                 string `json:"ackAt"`

	PrevHeadSHA            string `json:"-"`
	PrevCommitCount        int    `json:"-"`
	PrevCommentCount       int    `json:"-"`
	PrevReviewCommentCount int    `json:"-"`
	PrevState              string `json:"-"`
	PrevCIStatus           string `json:"-"`
	PrevReviewDecision     string `json:"-"`
	PrevUpdatedAt          string `json:"-"`
	PrevOtherActivityAt    string `json:"-"`

	UnreadUpdates    int    `json:"unreadUpdates"`
	UpdateCycles     int    `json:"updateCycles"`
	UpdateSummary    string `json:"updateSummary"`
	HasUnreadUpdates bool   `json:"hasUnreadUpdates"`

	// GHLinks holds every GitHub PR or issue tracked by this card in order
	// (index 0 is the primary link mirrored into the top-level GH* fields).
	GHLinks []CardLink `json:"ghLinks"`

	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

const cardColumns = `id, title, column_id, position, category, tags, ao_session_id, ao_title, summary, summary_source, notes, auto_move, origin,
	gh_repo, gh_kind, gh_number, gh_url, gh_title, gh_author, gh_state, gh_ci_status, gh_review_decision, gh_head_sha,
	gh_commit_count, gh_comment_count, gh_review_comment_count, gh_labels, gh_updated_at, gh_last_synced_at, gh_error,
	gh_my_review_state, gh_my_reviewed_at, gh_is_stale_review, gh_pending_feedback,
	ack_head_sha, ack_commit_count, ack_comment_count, ack_review_comment_count, ack_state, ack_ci_status, ack_review_decision, ack_updated_at, ack_other_activity_at, ack_at,
	prev_head_sha, prev_commit_count, prev_comment_count, prev_review_comment_count, prev_state, prev_ci_status, prev_review_decision, prev_updated_at, prev_other_activity_at,
	unread_updates, update_cycles, update_summary, has_unread_updates, created_at, updated_at`

// fields returns pointers to every persisted field, in cardColumns order.
func (c *Card) fields() []any {
	return []any{
		&c.ID, &c.Title, &c.ColumnID, &c.Position, &c.Category, &c.Tags, &c.AOSessionID, &c.AOTitle, &c.Summary, &c.SummarySource, &c.Notes, &c.AutoMove, &c.Origin,
		&c.GHRepo, &c.GHKind, &c.GHNumber, &c.GHURL, &c.GHTitle, &c.GHAuthor, &c.GHState, &c.GHCIStatus, &c.GHReviewDecision, &c.GHHeadSHA,
		&c.GHCommitCount, &c.GHCommentCount, &c.GHReviewCommentCount, &c.GHLabels, &c.GHUpdatedAt, &c.GHLastSyncedAt, &c.GHError,
		&c.GHMyReviewState, &c.GHMyReviewedAt, &c.GHIsStaleReview, &c.GHPendingFeedback,
		&c.AckHeadSHA, &c.AckCommitCount, &c.AckCommentCount, &c.AckReviewCommentCount, &c.AckState, &c.AckCIStatus, &c.AckReviewDecision, &c.AckUpdatedAt, &c.AckOtherActivityAt, &c.AckAt,
		&c.PrevHeadSHA, &c.PrevCommitCount, &c.PrevCommentCount, &c.PrevReviewCommentCount, &c.PrevState, &c.PrevCIStatus, &c.PrevReviewDecision, &c.PrevUpdatedAt, &c.PrevOtherActivityAt,
		&c.UnreadUpdates, &c.UpdateCycles, &c.UpdateSummary, &c.HasUnreadUpdates, &c.CreatedAt, &c.UpdatedAt,
	}
}

// values returns the persisted field values, in cardColumns order.
func (c *Card) values() []any {
	ptrs := c.fields()
	out := make([]any, len(ptrs))
	for i, p := range ptrs {
		switch v := p.(type) {
		case *string:
			out[i] = *v
		case *int:
			out[i] = *v
		case *bool:
			if *v {
				out[i] = 1
			} else {
				out[i] = 0
			}
		case *StringList:
			out[i] = *v
		default:
			panic(fmt.Sprintf("card field %d: unsupported type %T", i, p))
		}
	}
	return out
}

var (
	cardInsertSQL string
	cardUpdateSQL string
)

func init() {
	names := strings.Split(strings.Join(strings.Fields(cardColumns), ""), ",")
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	cardInsertSQL = `INSERT INTO cards (` + cardColumns + `) VALUES (` + placeholders + `)`
	sets := make([]string, 0, len(names)-1)
	for _, n := range names[1:] {
		sets = append(sets, n+" = ?")
	}
	cardUpdateSQL = `UPDATE cards SET ` + strings.Join(sets, ", ") + ` WHERE id = ?`
}

func (c *Card) normalize() error {
	c.Title = strings.TrimSpace(c.Title)
	if c.Title == "" {
		return fmt.Errorf("%w: card title is required", ErrInvalid)
	}
	if r := []rune(c.Title); len(r) > 300 {
		c.Title = string(r[:300])
	}
	if c.Category == "" {
		c.Category = CategoryDev
	}
	if !validCategories[c.Category] {
		return fmt.Errorf("%w: unknown category %q", ErrInvalid, c.Category)
	}
	if c.SummarySource == "" {
		c.SummarySource = SummaryAuto
	}
	if !validSummarySources[c.SummarySource] {
		return fmt.Errorf("%w: unknown summary source %q", ErrInvalid, c.SummarySource)
	}
	if c.Origin == "" {
		c.Origin = OriginManual
	}
	c.Tags = NormalizeTags(c.Tags)
	c.GHLabels = c.GHLabels.nonNil()
	if c.GHLinks == nil {
		c.GHLinks = []CardLink{}
	}
	return nil
}

func scanCard(row interface{ Scan(dest ...any) error }) (*Card, error) {
	var c Card
	if err := row.Scan(c.fields()...); err != nil {
		return nil, err
	}
	c.GHLinks = []CardLink{}
	return &c, nil
}

func getCard(ctx context.Context, q querier, id string) (*Card, error) {
	c, err := scanCard(q.QueryRowContext(ctx, `SELECT `+cardColumns+` FROM cards WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("card %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	links, err := loadCardLinks(ctx, q, c.ID)
	if err != nil {
		return nil, err
	}
	c.GHLinks = links
	return c, nil
}

func (s *Store) GetCard(ctx context.Context, id string) (*Card, error) {
	return getCard(ctx, s.db, id)
}

func (s *Store) ListCards(ctx context.Context) ([]*Card, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cardColumns+` FROM cards ORDER BY column_id, position, id`)
	if err != nil {
		return nil, err
	}
	out := []*Card{}
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byCard, err := loadAllCardLinks(ctx, s.db)
	if err != nil {
		return nil, err
	}
	for _, c := range out {
		if links, ok := byCard[c.ID]; ok {
			c.GHLinks = links
		}
	}
	return out, nil
}

// CardBySession returns the card linked to an AO session, or nil.
func (s *Store) CardBySession(ctx context.Context, sessionID string) (*Card, error) {
	if sessionID == "" {
		return nil, nil
	}
	c, err := scanCard(s.db.QueryRowContext(ctx,
		`SELECT `+cardColumns+` FROM cards WHERE ao_session_id = ? ORDER BY created_at LIMIT 1`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	links, err := loadCardLinks(ctx, s.db, c.ID)
	if err != nil {
		return nil, err
	}
	c.GHLinks = links
	return c, nil
}

func columnCardIDs(ctx context.Context, q querier, columnID, exclude string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM cards WHERE column_id = ? AND id <> ? ORDER BY position, id`, columnID, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// placeCard puts cardID at index within columnID and renumbers the column.
// It returns the final index.
func placeCard(ctx context.Context, tx *sql.Tx, cardID, columnID string, index int) (int, error) {
	ids, err := columnCardIDs(ctx, tx, columnID, cardID)
	if err != nil {
		return 0, err
	}
	if index < 0 {
		index = 0
	}
	if index > len(ids) {
		index = len(ids)
	}
	ordered := make([]string, 0, len(ids)+1)
	ordered = append(ordered, ids[:index]...)
	ordered = append(ordered, cardID)
	ordered = append(ordered, ids[index:]...)
	for i, id := range ordered {
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET column_id = ?, position = ? WHERE id = ?`, columnID, i, id); err != nil {
			return 0, err
		}
	}
	return index, nil
}

func renumberColumn(ctx context.Context, tx *sql.Tx, columnID string) error {
	ids, err := columnCardIDs(ctx, tx, columnID, "")
	if err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET position = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	return nil
}

// CreateCard inserts a card at the top of its column. Missing ID, timestamps,
// category and summary source are filled in.
func (s *Store) CreateCard(ctx context.Context, c *Card, events ...EventInput) (*Card, error) {
	if err := c.normalize(); err != nil {
		return nil, err
	}
	if c.ID == "" {
		c.ID = newID("c_")
	}
	now := s.now()
	c.CreatedAt, c.UpdatedAt = now, now
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := getColumn(ctx, tx, c.ColumnID); err != nil {
			return err
		}
		c.Position = -1
		if _, err := tx.ExecContext(ctx, cardInsertSQL, c.values()...); err != nil {
			return err
		}
		if err := saveCardLinks(ctx, tx, c, now); err != nil {
			return err
		}
		pos, err := placeCard(ctx, tx, c.ID, c.ColumnID, 0)
		if err != nil {
			return err
		}
		c.Position = pos
		return insertEvents(ctx, tx, c.ID, now, events)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Mutation collects side effects requested by an UpdateCard callback.
type Mutation struct {
	Events []EventInput
}

func (m *Mutation) Event(kind, message string) {
	m.Events = append(m.Events, EventInput{Kind: kind, Message: message})
}

// UpdateCard runs fn inside a transaction against the current card row and
// persists the result. When fn changes ColumnID the card is placed at the top
// of the new column. ID, Position and CreatedAt cannot be changed by fn.
func (s *Store) UpdateCard(ctx context.Context, id string, fn func(c *Card, m *Mutation) error) (*Card, error) {
	var out *Card
	err := s.tx(ctx, func(tx *sql.Tx) error {
		c, err := getCard(ctx, tx, id)
		if err != nil {
			return err
		}
		before := *c
		m := &Mutation{}
		if err := fn(c, m); err != nil {
			return err
		}
		c.ID, c.Position, c.CreatedAt = before.ID, before.Position, before.CreatedAt
		if err := c.normalize(); err != nil {
			return err
		}
		now := s.now()
		c.UpdatedAt = now
		moved := c.ColumnID != before.ColumnID
		if moved {
			if _, err := getColumn(ctx, tx, c.ColumnID); err != nil {
				return err
			}
		}
		args := append(c.values()[1:], c.ID)
		if _, err := tx.ExecContext(ctx, cardUpdateSQL, args...); err != nil {
			return err
		}
		if err := saveCardLinks(ctx, tx, c, now); err != nil {
			return err
		}
		if moved {
			pos, err := placeCard(ctx, tx, c.ID, c.ColumnID, 0)
			if err != nil {
				return err
			}
			c.Position = pos
			if err := renumberColumn(ctx, tx, before.ColumnID); err != nil {
				return err
			}
		}
		out = c
		return insertEvents(ctx, tx, c.ID, now, m.Events)
	})
	return out, err
}

// MoveCard places a card at position (0-based) within columnID, renumbering
// both the source and target columns.
func (s *Store) MoveCard(ctx context.Context, id, columnID string, position int) (*Card, error) {
	var out *Card
	err := s.tx(ctx, func(tx *sql.Tx) error {
		c, err := getCard(ctx, tx, id)
		if err != nil {
			return err
		}
		target, err := getColumn(ctx, tx, columnID)
		if err != nil {
			return err
		}
		from := c.ColumnID
		pos, err := placeCard(ctx, tx, id, columnID, position)
		if err != nil {
			return err
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET updated_at = ? WHERE id = ?`, now, id); err != nil {
			return err
		}
		c.ColumnID, c.Position, c.UpdatedAt = columnID, pos, now
		out = c
		if from == columnID {
			return nil
		}
		if err := renumberColumn(ctx, tx, from); err != nil {
			return err
		}
		fromTitle := from
		if fc, err := getColumn(ctx, tx, from); err == nil {
			fromTitle = fc.Title
		}
		return insertEvents(ctx, tx, id, now, []EventInput{{
			Kind: EventMove, Message: fmt.Sprintf("Moved from %s to %s", fromTitle, target.Title),
		}})
	})
	return out, err
}

func (s *Store) DeleteCard(ctx context.Context, id string) (*Card, error) {
	var out *Card
	err := s.tx(ctx, func(tx *sql.Tx) error {
		c, err := getCard(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM card_events WHERE card_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cards WHERE id = ?`, id); err != nil {
			return err
		}
		out = c
		return renumberColumn(ctx, tx, c.ColumnID)
	})
	return out, err
}
