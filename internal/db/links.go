package db

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// CardLink is one GitHub pull request or issue tracked by a card, with its
// own live state, personal review standing, and incremental update baseline.
type CardLink struct {
	ID       string `json:"id"`
	CardID   string `json:"-"`
	Position int    `json:"position"`

	Repo               string     `json:"repo"`
	Kind               string     `json:"kind"`
	Number             int        `json:"number"`
	URL                string     `json:"url"`
	Title              string     `json:"title"`
	Author             string     `json:"author"`
	State              string     `json:"state"`
	CIStatus           string     `json:"ciStatus"`
	ReviewDecision     string     `json:"reviewDecision"`
	HeadSHA            string     `json:"headSha"`
	CommitCount        int        `json:"commitCount"`
	CommentCount       int        `json:"commentCount"`
	ReviewCommentCount int        `json:"reviewCommentCount"`
	Labels             StringList `json:"labels"`
	UpdatedAt          string     `json:"updatedAt"`
	LastSyncedAt       string     `json:"lastSyncedAt"`
	Error              string     `json:"error"`

	MyReviewState   string `json:"myReviewState"`
	MyReviewedAt    string `json:"myReviewedAt"`
	IsStaleReview   bool   `json:"isStaleReview"`
	PendingFeedback string `json:"pendingFeedback"`

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

	CreatedAt string `json:"createdAt"`
}

const cardLinkColumns = `id, card_id, position,
	repo, kind, number, url, title, author, state, ci_status, review_decision, head_sha,
	commit_count, comment_count, review_comment_count, labels, updated_at, last_synced_at, error,
	my_review_state, my_reviewed_at, is_stale_review, pending_feedback,
	ack_head_sha, ack_commit_count, ack_comment_count, ack_review_comment_count, ack_state, ack_ci_status, ack_review_decision, ack_updated_at, ack_other_activity_at, ack_at,
	prev_head_sha, prev_commit_count, prev_comment_count, prev_review_comment_count, prev_state, prev_ci_status, prev_review_decision, prev_updated_at, prev_other_activity_at,
	unread_updates, update_cycles, update_summary, has_unread_updates, created_at`

func (l *CardLink) fields() []any {
	return []any{
		&l.ID, &l.CardID, &l.Position,
		&l.Repo, &l.Kind, &l.Number, &l.URL, &l.Title, &l.Author, &l.State, &l.CIStatus, &l.ReviewDecision, &l.HeadSHA,
		&l.CommitCount, &l.CommentCount, &l.ReviewCommentCount, &l.Labels, &l.UpdatedAt, &l.LastSyncedAt, &l.Error,
		&l.MyReviewState, &l.MyReviewedAt, &l.IsStaleReview, &l.PendingFeedback,
		&l.AckHeadSHA, &l.AckCommitCount, &l.AckCommentCount, &l.AckReviewCommentCount, &l.AckState, &l.AckCIStatus, &l.AckReviewDecision, &l.AckUpdatedAt, &l.AckOtherActivityAt, &l.AckAt,
		&l.PrevHeadSHA, &l.PrevCommitCount, &l.PrevCommentCount, &l.PrevReviewCommentCount, &l.PrevState, &l.PrevCIStatus, &l.PrevReviewDecision, &l.PrevUpdatedAt, &l.PrevOtherActivityAt,
		&l.UnreadUpdates, &l.UpdateCycles, &l.UpdateSummary, &l.HasUnreadUpdates, &l.CreatedAt,
	}
}

func (l *CardLink) values() []any {
	ptrs := l.fields()
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
			out[i] = v.nonNil()
		default:
			panic("cardLink field: unsupported type")
		}
	}
	return out
}

var cardLinkInsertSQL string

func init() {
	names := strings.Split(strings.Join(strings.Fields(cardLinkColumns), ""), ",")
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	cardLinkInsertSQL = `INSERT INTO card_links (` + cardLinkColumns + `) VALUES (` + placeholders + `)`
}

// LinkFromCard builds a CardLink from a card's legacy single-link columns.
func LinkFromCard(c *Card) CardLink {
	return CardLink{
		CardID:                c.ID,
		Repo:                  c.GHRepo,
		Kind:                  c.GHKind,
		Number:                c.GHNumber,
		URL:                   c.GHURL,
		Title:                 c.GHTitle,
		Author:                c.GHAuthor,
		State:                 c.GHState,
		CIStatus:              c.GHCIStatus,
		ReviewDecision:        c.GHReviewDecision,
		HeadSHA:               c.GHHeadSHA,
		CommitCount:           c.GHCommitCount,
		CommentCount:          c.GHCommentCount,
		ReviewCommentCount:    c.GHReviewCommentCount,
		Labels:                c.GHLabels.nonNil(),
		UpdatedAt:             c.GHUpdatedAt,
		LastSyncedAt:          c.GHLastSyncedAt,
		Error:                 c.GHError,
		MyReviewState:         c.GHMyReviewState,
		MyReviewedAt:          c.GHMyReviewedAt,
		IsStaleReview:         c.GHIsStaleReview,
		PendingFeedback:       c.GHPendingFeedback,
		AckHeadSHA:            c.AckHeadSHA,
		AckCommitCount:        c.AckCommitCount,
		AckCommentCount:       c.AckCommentCount,
		AckReviewCommentCount: c.AckReviewCommentCount,
		AckState:              c.AckState,
		AckCIStatus:           c.AckCIStatus,
		AckReviewDecision:     c.AckReviewDecision,
		AckUpdatedAt:          c.AckUpdatedAt,
		AckOtherActivityAt:    c.AckOtherActivityAt,
		AckAt:                 c.AckAt,
		PrevHeadSHA:           c.PrevHeadSHA,
		PrevCommitCount:       c.PrevCommitCount,
		PrevCommentCount:      c.PrevCommentCount,
		PrevReviewCommentCount: c.PrevReviewCommentCount,
		PrevState:             c.PrevState,
		PrevCIStatus:          c.PrevCIStatus,
		PrevReviewDecision:    c.PrevReviewDecision,
		PrevUpdatedAt:         c.PrevUpdatedAt,
		PrevOtherActivityAt:   c.PrevOtherActivityAt,
		UnreadUpdates:         c.UnreadUpdates,
		UpdateCycles:          c.UpdateCycles,
		UpdateSummary:         c.UpdateSummary,
		HasUnreadUpdates:      c.HasUnreadUpdates,
		CreatedAt:             c.CreatedAt,
	}
}

func scanCardLink(row interface{ Scan(dest ...any) error }) (CardLink, error) {
	var l CardLink
	if err := row.Scan(l.fields()...); err != nil {
		return CardLink{}, err
	}
	l.Labels = l.Labels.nonNil()
	return l, nil
}

func loadCardLinks(ctx context.Context, q querier, cardID string) ([]CardLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+cardLinkColumns+` FROM card_links WHERE card_id = ? ORDER BY position, id`, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CardLink{}
	for rows.Next() {
		l, err := scanCardLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func loadAllCardLinks(ctx context.Context, q querier) (map[string][]CardLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+cardLinkColumns+` FROM card_links ORDER BY card_id, position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byCard := map[string][]CardLink{}
	for rows.Next() {
		l, err := scanCardLink(rows)
		if err != nil {
			return nil, err
		}
		byCard[l.CardID] = append(byCard[l.CardID], l)
	}
	return byCard, rows.Err()
}

func saveCardLinks(ctx context.Context, tx *sql.Tx, c *Card, now string) error {
	if len(c.GHLinks) == 0 && c.GHNumber > 0 && c.GHRepo != "" {
		c.GHLinks = []CardLink{LinkFromCard(c)}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM card_links WHERE card_id = ?`, c.ID); err != nil {
		return err
	}
	if len(c.GHLinks) == 0 {
		c.GHLinks = []CardLink{}
		return nil
	}
	seen := map[string]bool{}
	cleaned := make([]CardLink, 0, len(c.GHLinks))
	for _, l := range c.GHLinks {
		if l.Number <= 0 || strings.TrimSpace(l.Repo) == "" {
			continue
		}
		dedup := strings.ToLower(l.Repo) + "#" + strconv.Itoa(l.Number)
		if seen[dedup] {
			continue
		}
		seen[dedup] = true
		if l.ID == "" {
			l.ID = newID("lnk_")
		}
		l.CardID = c.ID
		l.Position = len(cleaned)
		l.Labels = l.Labels.nonNil()
		if l.CreatedAt == "" {
			l.CreatedAt = now
		}
		if _, err := tx.ExecContext(ctx, cardLinkInsertSQL, l.values()...); err != nil {
			return err
		}
		cleaned = append(cleaned, l)
	}
	c.GHLinks = cleaned
	return nil
}
