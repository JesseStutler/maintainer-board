package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/github"
)

// closedResyncInterval is how often the background loop re-checks closed
// items, which can still be reopened. Merged pull requests are final and are
// only fetched on demand.
const closedResyncInterval = 30 * time.Minute

const githubWorkers = 4

var errLinkChanged = errors.New("github link changed during sync")

func liveSnapshot(c *db.Card) github.Snapshot {
	return github.Snapshot{
		State:          c.GHState,
		CI:             c.GHCIStatus,
		ReviewDecision: c.GHReviewDecision,
		HeadSHA:        c.GHHeadSHA,
		Commits:        c.GHCommitCount,
		Comments:       c.GHCommentCount,
		ReviewComments: c.GHReviewCommentCount,
		UpdatedAt:      c.GHUpdatedAt,
	}
}

func trackingOf(c *db.Card) github.Tracking {
	return github.Tracking{
		Ack: github.Snapshot{
			State:          c.AckState,
			CI:             c.AckCIStatus,
			ReviewDecision: c.AckReviewDecision,
			HeadSHA:        c.AckHeadSHA,
			Commits:        c.AckCommitCount,
			Comments:       c.AckCommentCount,
			ReviewComments: c.AckReviewCommentCount,
			UpdatedAt:      c.AckUpdatedAt,
		},
		Prev: github.Snapshot{
			State:          c.PrevState,
			CI:             c.PrevCIStatus,
			ReviewDecision: c.PrevReviewDecision,
			HeadSHA:        c.PrevHeadSHA,
			Commits:        c.PrevCommitCount,
			Comments:       c.PrevCommentCount,
			ReviewComments: c.PrevReviewCommentCount,
			UpdatedAt:      c.PrevUpdatedAt,
		},
		Unread:    c.UnreadUpdates,
		Cycles:    c.UpdateCycles,
		Summary:   c.UpdateSummary,
		HasUnread: c.HasUnreadUpdates,
	}
}

func setTracking(c *db.Card, t github.Tracking) {
	c.AckState, c.AckCIStatus, c.AckReviewDecision = t.Ack.State, t.Ack.CI, t.Ack.ReviewDecision
	c.AckHeadSHA, c.AckUpdatedAt = t.Ack.HeadSHA, t.Ack.UpdatedAt
	c.AckCommitCount, c.AckCommentCount, c.AckReviewCommentCount = t.Ack.Commits, t.Ack.Comments, t.Ack.ReviewComments

	c.PrevState, c.PrevCIStatus, c.PrevReviewDecision = t.Prev.State, t.Prev.CI, t.Prev.ReviewDecision
	c.PrevHeadSHA, c.PrevUpdatedAt = t.Prev.HeadSHA, t.Prev.UpdatedAt
	c.PrevCommitCount, c.PrevCommentCount, c.PrevReviewCommentCount = t.Prev.Commits, t.Prev.Comments, t.Prev.ReviewComments

	c.UnreadUpdates, c.UpdateCycles, c.UpdateSummary, c.HasUnreadUpdates = t.Unread, t.Cycles, t.Summary, t.HasUnread
}

// clearGitHub removes the link and all tracking state from a card.
func clearGitHub(c *db.Card) {
	c.GHRepo, c.GHKind, c.GHNumber = "", "", 0
	c.GHURL, c.GHTitle, c.GHAuthor, c.GHState, c.GHCIStatus, c.GHReviewDecision, c.GHHeadSHA = "", "", "", "", "", "", ""
	c.GHCommitCount, c.GHCommentCount, c.GHReviewCommentCount = 0, 0, 0
	c.GHLabels = db.StringList{}
	c.GHUpdatedAt, c.GHLastSyncedAt, c.GHError = "", "", ""
	c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview = "", "", false
	c.AckAt, c.AckOtherActivityAt, c.PrevOtherActivityAt = "", "", ""
	setTracking(c, github.Tracking{})
}

// isOwn reports whether author is the maintainer running the board.
func isOwn(author, me string) bool {
	return me != "" && strings.EqualFold(author, me)
}

func kindLabel(kind string) string {
	if kind == github.KindIssue {
		return "issue"
	}
	return "pull request"
}

// applyGitHubItem stores a freshly fetched item on the card, evaluates the
// update-tracking rules and performs the resulting automatic column move.
// The card must already reference the item's repository and number.
func (s *Server) applyGitHubItem(ctx context.Context, cardID string, item *github.Item) (*db.Card, github.Result, error) {
	cols, err := s.columnIndex(ctx)
	if err != nil {
		return nil, github.Result{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	me := s.currentUser()
	var result github.Result
	card, err := s.store.UpdateCard(ctx, cardID, func(c *db.Card, m *db.Mutation) error {
		if c.GHRepo != item.Repo || c.GHNumber != item.Number {
			return errLinkChanged
		}
		first := c.AckAt == ""
		c.GHKind = item.Kind
		c.GHURL, c.GHTitle, c.GHAuthor = item.URL, item.Title, item.Author
		c.GHLabels = db.StringList(item.Labels)
		snap := item.Snapshot
		c.GHState, c.GHCIStatus, c.GHReviewDecision = snap.State, snap.CI, snap.ReviewDecision
		c.GHHeadSHA, c.GHUpdatedAt = snap.HeadSHA, snap.UpdatedAt
		c.GHCommitCount, c.GHCommentCount, c.GHReviewCommentCount = snap.Commits, snap.Comments, snap.ReviewComments
		c.GHLastSyncedAt, c.GHError = now, ""

		own := isOwn(item.Author, me)
		baselineAt := github.LatestTime(c.AckOtherActivityAt, c.AckUpdatedAt)
		if baselineAt == "" {
			baselineAt = c.AckAt
		}
		if !own && github.After(c.GHMyReviewedAt, baselineAt) {
			baselineAt = c.GHMyReviewedAt
		}
		var authorReplies, otherReplies int
		var ownReviewOnly, myReviewSubmitted bool
		if item.HasActorData && !first {
			authorReplies, otherReplies = github.CountNewActivities(item.OtherActivities, item.Author, baselineAt, own)
			ownReviewOnly = !own && item.MyReview.State != "" && github.After(item.MyReview.At, baselineAt) && authorReplies == 0 && otherReplies == 0
			myReviewSubmitted = !own && item.Kind == github.KindPR &&
				item.MyReview.State != "" &&
				github.After(item.MyReview.At, c.GHMyReviewedAt) &&
				github.ReviewCoversLive(item.MyReview, snap, item.LastCommitAt, item.LastOtherActivityAt)
		}

		review := github.EvaluateReview(github.ReviewInput{
			GitHub:              item.MyReview,
			StoredState:         c.GHMyReviewState,
			StoredAt:            c.GHMyReviewedAt,
			AckHeadSHA:          c.AckHeadSHA,
			Own:                 own,
			Live:                snap,
			LastCommitAt:        item.LastCommitAt,
			LastOtherActivityAt: item.LastOtherActivityAt,
		})

		result = github.Evaluate(github.Input{
			Live:                  snap,
			Track:                 trackingOf(c),
			FirstSync:             first,
			AutoMove:              c.AutoMove,
			ColumnRole:            cols.byID[c.ColumnID].Role,
			UnresolvedThreads:     item.UnresolvedThreads,
			Kind:                  item.Kind,
			Own:                   own,
			HasActorData:          item.HasActorData,
			AuthorReplies:         authorReplies,
			OtherReplies:          otherReplies,
			OwnReviewOnly:         ownReviewOnly,
			MyReviewSubmitted:     myReviewSubmitted,
			MyReviewState:         review.State,
			MyReviewStale:         review.Stale,
			LatestOtherActivityAt: item.LastOtherActivityAt,
			PrevOtherActivityAt:   c.PrevOtherActivityAt,
		})
		setTracking(c, result.Track)
		if item.HasActorData {
			if first {
				c.AckOtherActivityAt = github.LatestTime(item.LastOtherActivityAt, snap.UpdatedAt)
			} else if myReviewSubmitted {
				c.AckOtherActivityAt = github.LatestTime(item.LastOtherActivityAt, item.MyReview.At)
				c.AckAt = now
			}
			c.PrevOtherActivityAt = item.LastOtherActivityAt
		}
		c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview = review.State, review.At, review.Stale
		if first {
			c.AckAt = now
			m.Event(db.EventGitHub, fmt.Sprintf("Linked %s %s#%d (%s); baseline recorded", kindLabel(item.Kind), item.Repo, item.Number, strings.ToLower(snap.State)))
		}
		for _, msg := range result.Events {
			m.Event(db.EventUpdate, msg)
		}
		if result.MoveToRole != "" {
			target, ok := cols.byRole[result.MoveToRole]
			if ok && target.ID != c.ColumnID {
				m.Event(db.EventMove, fmt.Sprintf("Auto-moved from %s to %s", cols.title(c.ColumnID), target.Title))
				c.ColumnID = target.ID
			} else {
				result.MoveToRole = ""
			}
		}
		return nil
	})
	return card, result, err
}

// syncCard fetches the live GitHub state of one card and applies it.
func (s *Server) syncCard(ctx context.Context, cardID string) (*db.Card, github.Result, error) {
	card, err := s.store.GetCard(ctx, cardID)
	if err != nil {
		return nil, github.Result{}, err
	}
	if card.GHNumber == 0 {
		return card, github.Result{}, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	item, err := s.gh.Fetch(fetchCtx, card.GHRepo, card.GHNumber, card.GHKind)
	cancel()
	if err != nil {
		neverSynced := card.GHLastSyncedAt == ""
		if errors.Is(err, github.ErrNotFound) && neverSynced {
			// An automatic link guessed from a session title pointed at
			// nothing; drop it instead of retrying forever.
			ref := fmt.Sprintf("%s#%d", card.GHRepo, card.GHNumber)
			_, _ = s.store.UpdateCard(ctx, cardID, func(c *db.Card, m *db.Mutation) error {
				if c.GHRepo != card.GHRepo || c.GHNumber != card.GHNumber {
					return errLinkChanged
				}
				clearGitHub(c)
				m.Event(db.EventGitHub, "Removed link to "+ref+": not found on GitHub")
				return nil
			})
			return nil, github.Result{}, err
		}
		msg := s.describeError(err)
		if card.GHError != msg {
			_, _ = s.store.UpdateCard(ctx, cardID, func(c *db.Card, m *db.Mutation) error {
				if c.GHRepo != card.GHRepo || c.GHNumber != card.GHNumber {
					return errLinkChanged
				}
				c.GHError = msg
				return nil
			})
		}
		return nil, github.Result{}, err
	}
	return s.applyGitHubItem(ctx, cardID, item)
}

// SyncReport summarizes one board-wide GitHub sync.
type SyncReport struct {
	StartedAt  string   `json:"startedAt"`
	DurationMs int64    `json:"durationMs"`
	Total      int      `json:"total"`
	Synced     int      `json:"synced"`
	Skipped    int      `json:"skipped"`
	Updated    int      `json:"updated"`
	Moved      int      `json:"moved"`
	Failed     int      `json:"failed"`
	Errors     []string `json:"errors"`
	// Joined is true when the request waited for a sync already in flight.
	Joined bool `json:"joined"`
}

// syncGitHub syncs every linked card. Only one board-wide sync runs at a
// time; a concurrent caller waits for it and receives its report. With
// force false, merged items are skipped and closed items are re-checked at
// most every closedResyncInterval.
func (s *Server) syncGitHub(ctx context.Context, force bool) SyncReport {
	s.mu.Lock()
	if s.ghSyncing {
		done := s.ghSyncDone
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		s.mu.RLock()
		report := s.ghLastReport
		s.mu.RUnlock()
		report.Joined = true
		return report
	}
	s.ghSyncing = true
	done := make(chan struct{})
	s.ghSyncDone = done
	s.mu.Unlock()
	s.hub.Publish()

	started := time.Now()
	report := SyncReport{StartedAt: formatTime(started), Errors: []string{}}
	cards, err := s.store.ListCards(ctx)
	if err != nil {
		report.Failed++
		report.Errors = append(report.Errors, err.Error())
	}
	var queue []*db.Card
	for _, c := range cards {
		if c.GHNumber == 0 {
			continue
		}
		report.Total++
		if !force && c.GHLastSyncedAt != "" {
			if c.GHState == github.StateMerged {
				report.Skipped++
				continue
			}
			if c.GHState == github.StateClosed {
				if last, err := time.Parse(time.RFC3339, c.GHLastSyncedAt); err == nil && time.Since(last) < closedResyncInterval {
					report.Skipped++
					continue
				}
			}
		}
		queue = append(queue, c)
	}

	var (
		wg       sync.WaitGroup
		reportMu sync.Mutex
		jobs     = make(chan *db.Card)
	)
	for range min(githubWorkers, len(queue)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				_, res, err := s.syncCard(ctx, c.ID)
				reportMu.Lock()
				switch {
				case errors.Is(err, errLinkChanged), errors.Is(err, db.ErrNotFound):
					report.Skipped++
				case err != nil:
					report.Failed++
					if len(report.Errors) < 10 {
						report.Errors = append(report.Errors, fmt.Sprintf("%s#%d: %s", c.GHRepo, c.GHNumber, s.describeError(err)))
					}
				default:
					report.Synced++
					if res.NewCycle {
						report.Updated++
					}
					if res.MoveToRole != "" {
						report.Moved++
					}
				}
				reportMu.Unlock()
				if err == nil && (res.NewCycle || res.MoveToRole != "") {
					s.hub.Publish()
				}
			}
		}()
	}
feed:
	for _, c := range queue {
		select {
		case jobs <- c:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	report.DurationMs = time.Since(started).Milliseconds()

	s.mu.Lock()
	s.ghSyncing = false
	s.ghLastSync = time.Now()
	s.ghLastReport = report
	if report.Failed > 0 && len(report.Errors) > 0 {
		s.ghLastError = report.Errors[0]
	} else {
		s.ghLastError = ""
	}
	close(done)
	s.mu.Unlock()
	s.hub.Publish()
	if report.Failed > 0 {
		s.log.Warn("github sync finished with errors", "synced", report.Synced, "failed", report.Failed, "first", s.ghLastError)
	} else {
		s.log.Debug("github sync finished", "synced", report.Synced, "updated", report.Updated, "moved", report.Moved, "skipped", report.Skipped)
	}
	return report
}

func (s *Server) githubLoop(ctx context.Context) {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.resolveCurrentUser(ctx)
			s.syncGitHub(ctx, false)
			timer.Reset(s.cfg.GitHubSyncInterval)
		}
	}
}

// describeError turns an internal error into a short user-facing sentence.
func (s *Server) describeError(err error) string {
	var apiErr *ao.APIError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &apiErr):
		if msg, ok := aoErrorMessages[apiErr.Code]; ok {
			return msg
		}
		return capitalize(apiErr.Message)
	case errors.Is(err, ao.ErrUnavailable):
		return fmt.Sprintf("The AO daemon is not reachable at %s.", s.cfg.AOURL)
	case errors.Is(err, github.ErrNotFound):
		return "Not found on GitHub."
	case errors.Is(err, context.DeadlineExceeded):
		return "The request timed out."
	case errors.Is(err, github.ErrUnavailable):
		return capitalize(strings.TrimPrefix(err.Error(), github.ErrUnavailable.Error()+": "))
	}
	return capitalize(err.Error())
}

func (s *Server) writeGitHubError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, github.ErrNotFound):
		writeError(w, http.StatusNotFound, "GITHUB_NOT_FOUND", "That pull request or issue was not found on GitHub.")
	case errors.Is(err, github.ErrBadRef):
		writeError(w, http.StatusBadRequest, "INVALID_GITHUB_REF", "Unrecognized GitHub reference. Use #123, owner/repo#123 or a pull request or issue URL.")
	case errors.Is(err, errLinkChanged):
		writeError(w, http.StatusConflict, "LINK_CHANGED", "The GitHub link of this card changed while syncing. Try again.")
	case errors.Is(err, db.ErrNotFound), errors.Is(err, db.ErrInvalid):
		s.writeStoreError(w, err)
	default:
		writeError(w, http.StatusBadGateway, "GITHUB_UNAVAILABLE", "GitHub could not be reached through gh: "+s.describeError(err))
	}
}

func (s *Server) handleLinkGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var req struct {
		Ref string `json:"ref"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ref, err := github.ParseRef(req.Ref, s.cfg.DefaultRepo)
	if err != nil {
		s.writeGitHubError(w, err)
		return
	}
	if _, err := s.store.GetCard(ctx, id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	item, err := s.gh.Fetch(fetchCtx, ref.Repo, ref.Number, "")
	cancel()
	if err != nil {
		s.writeGitHubError(w, err)
		return
	}
	// Replace any previous link, then record the new baseline.
	if _, err := s.store.UpdateCard(ctx, id, func(c *db.Card, m *db.Mutation) error {
		if c.GHNumber != 0 && (c.GHRepo != ref.Repo || c.GHNumber != ref.Number) {
			m.Event(db.EventGitHub, fmt.Sprintf("Unlinked %s#%d", c.GHRepo, c.GHNumber))
		}
		clearGitHub(c)
		c.GHRepo, c.GHNumber = ref.Repo, ref.Number
		return nil
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	card, _, err := s.applyGitHubItem(ctx, id, item)
	if err != nil {
		s.writeGitHubError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}

func (s *Server) handleUnlinkGitHub(w http.ResponseWriter, r *http.Request) {
	card, err := s.store.UpdateCard(r.Context(), r.PathValue("id"), func(c *db.Card, m *db.Mutation) error {
		if c.GHNumber != 0 {
			m.Event(db.EventGitHub, fmt.Sprintf("Unlinked %s#%d", c.GHRepo, c.GHNumber))
		}
		clearGitHub(c)
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}

func (s *Server) handleSyncCardGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	existing, err := s.store.GetCard(ctx, id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if existing.GHNumber == 0 {
		writeError(w, http.StatusConflict, "NOT_LINKED", "This card is not linked to a pull request or issue.")
		return
	}
	card, res, err := s.syncCard(ctx, id)
	s.hub.Publish()
	if err != nil {
		s.writeGitHubError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"card":        s.view(card),
		"newUpdate":   res.NewCycle,
		"moved":       res.MoveToRole != "",
		"movedToRole": res.MoveToRole,
	})
}

func (s *Server) handleGitHubSync(w http.ResponseWriter, r *http.Request) {
	// The sync is detached from the request so a closed browser tab does not
	// abort it half way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.bgCtx, cancel)
	defer stop()
	report := s.syncGitHub(ctx, true)
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var req struct {
		MoveToColumnID string `json:"moveToColumnId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	cols, err := s.columnIndex(ctx)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if req.MoveToColumnID != "" {
		if _, ok := cols.byID[req.MoveToColumnID]; !ok {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "No such column.")
			return
		}
	}
	errNotLinked := errors.New("not linked")
	errNotSynced := errors.New("not synced")
	me := s.currentUser()
	card, err := s.store.UpdateCard(ctx, id, func(c *db.Card, m *db.Mutation) error {
		if c.GHNumber == 0 {
			return errNotLinked
		}
		if c.GHLastSyncedAt == "" {
			return errNotSynced
		}
		msg := "Marked reviewed"
		if c.HasUnreadUpdates {
			msg = "Marked caught up"
		}
		now := time.Now().UTC().Format(time.RFC3339)
		setTracking(c, github.Ack(liveSnapshot(c)))
		c.AckAt = now
		c.AckOtherActivityAt = github.LatestTime(c.PrevOtherActivityAt, c.GHUpdatedAt)
		if !isOwn(c.GHAuthor, me) && c.GHKind != github.KindPR {
			// Issues have no GitHub review API, so acknowledging records a
			// local REVIEWED mark. Pull request review state is driven
			// strictly by actual GitHub reviews.
			if c.GHMyReviewState == "" {
				c.GHMyReviewState = github.MyReviewReviewed
			}
			c.GHMyReviewedAt, c.GHIsStaleReview = now, false
		}
		if req.MoveToColumnID != "" && req.MoveToColumnID != c.ColumnID {
			msg += fmt.Sprintf("; moved from %s to %s", cols.title(c.ColumnID), cols.title(req.MoveToColumnID))
			c.ColumnID = req.MoveToColumnID
		}
		m.Event(db.EventAck, msg)
		return nil
	})
	switch {
	case errors.Is(err, errNotLinked):
		writeError(w, http.StatusConflict, "NOT_LINKED", "This card is not linked to a pull request or issue.")
		return
	case errors.Is(err, errNotSynced):
		writeError(w, http.StatusConflict, "NOT_SYNCED", "This card has not been synced with GitHub yet. Run Sync Now first.")
		return
	case err != nil:
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}

// handleUnreview removes a review mark the maintainer set by hand. A review
// read from GitHub cannot be removed here; it would return on the next sync.
func (s *Server) handleUnreview(w http.ResponseWriter, r *http.Request) {
	errFromGitHub := errors.New("review comes from github")
	card, err := s.store.UpdateCard(r.Context(), r.PathValue("id"), func(c *db.Card, m *db.Mutation) error {
		switch c.GHMyReviewState {
		case "":
			return nil
		case github.MyReviewReviewed:
			c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview = "", "", false
			m.Event(db.EventAck, "Review mark cleared")
			return nil
		}
		return errFromGitHub
	})
	switch {
	case errors.Is(err, errFromGitHub):
		writeError(w, http.StatusConflict, "REVIEW_FROM_GITHUB", "This review was submitted on GitHub and cannot be cleared from the board.")
		return
	case err != nil:
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}
