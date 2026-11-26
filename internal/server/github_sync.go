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

func liveSnapshotLink(l *db.CardLink) github.Snapshot {
	return github.Snapshot{
		State:          l.State,
		CI:             l.CIStatus,
		ReviewDecision: l.ReviewDecision,
		HeadSHA:        l.HeadSHA,
		Commits:        l.CommitCount,
		Comments:       l.CommentCount,
		ReviewComments: l.ReviewCommentCount,
		UpdatedAt:      l.UpdatedAt,
	}
}

func trackingOfLink(l *db.CardLink) github.Tracking {
	return github.Tracking{
		Ack: github.Snapshot{
			State:          l.AckState,
			CI:             l.AckCIStatus,
			ReviewDecision: l.AckReviewDecision,
			HeadSHA:        l.AckHeadSHA,
			Commits:        l.AckCommitCount,
			Comments:       l.AckCommentCount,
			ReviewComments: l.AckReviewCommentCount,
			UpdatedAt:      l.AckUpdatedAt,
		},
		Prev: github.Snapshot{
			State:          l.PrevState,
			CI:             l.PrevCIStatus,
			ReviewDecision: l.PrevReviewDecision,
			HeadSHA:        l.PrevHeadSHA,
			Commits:        l.PrevCommitCount,
			Comments:       l.PrevCommentCount,
			ReviewComments: l.PrevReviewCommentCount,
			UpdatedAt:      l.PrevUpdatedAt,
		},
		Unread:    l.UnreadUpdates,
		Cycles:    l.UpdateCycles,
		Summary:   l.UpdateSummary,
		HasUnread: l.HasUnreadUpdates,
	}
}

func setTrackingLink(l *db.CardLink, t github.Tracking) {
	l.AckState, l.AckCIStatus, l.AckReviewDecision = t.Ack.State, t.Ack.CI, t.Ack.ReviewDecision
	l.AckHeadSHA, l.AckUpdatedAt = t.Ack.HeadSHA, t.Ack.UpdatedAt
	l.AckCommitCount, l.AckCommentCount, l.AckReviewCommentCount = t.Ack.Commits, t.Ack.Comments, t.Ack.ReviewComments

	l.PrevState, l.PrevCIStatus, l.PrevReviewDecision = t.Prev.State, t.Prev.CI, t.Prev.ReviewDecision
	l.PrevHeadSHA, l.PrevUpdatedAt = t.Prev.HeadSHA, t.Prev.UpdatedAt
	l.PrevCommitCount, l.PrevCommentCount, l.PrevReviewCommentCount = t.Prev.Commits, t.Prev.Comments, t.Prev.ReviewComments

	l.UnreadUpdates, l.UpdateCycles, l.UpdateSummary, l.HasUnreadUpdates = t.Unread, t.Cycles, t.Summary, t.HasUnread
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

// clearGitHub removes every GitHub link and all tracking state from a card.
func clearGitHub(c *db.Card) {
	c.GHRepo, c.GHKind, c.GHNumber = "", "", 0
	c.GHURL, c.GHTitle, c.GHAuthor, c.GHState, c.GHCIStatus, c.GHReviewDecision, c.GHHeadSHA = "", "", "", "", "", "", ""
	c.GHCommitCount, c.GHCommentCount, c.GHReviewCommentCount = 0, 0, 0
	c.GHLabels = db.StringList{}
	c.GHUpdatedAt, c.GHLastSyncedAt, c.GHError = "", "", ""
	c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview, c.GHPendingFeedback = "", "", false, ""
	c.AckAt, c.AckOtherActivityAt, c.PrevOtherActivityAt = "", "", ""
	c.GHLinks = []db.CardLink{}
	setTracking(c, github.Tracking{})
}

func shortRef(repo string, number int, defaultRepo string) string {
	if defaultRepo != "" && strings.EqualFold(repo, defaultRepo) {
		return fmt.Sprintf("#%d", number)
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

func sameLinkRef(l db.CardLink, repo string, number int) bool {
	return l.Number == number && strings.EqualFold(l.Repo, repo)
}

func ensureCardLinks(c *db.Card) {
	if len(c.GHLinks) == 0 && c.GHNumber > 0 && c.GHRepo != "" {
		c.GHLinks = []db.CardLink{db.LinkFromCard(c)}
	}
}

func allLinksTerminal(links []db.CardLink) bool {
	if len(links) == 0 {
		return false
	}
	for _, l := range links {
		if !github.IsTerminal(l.State) {
			return false
		}
	}
	return true
}

func anyLinkNeedsAttention(links []db.CardLink) bool {
	for _, l := range links {
		if l.HasUnreadUpdates || l.IsStaleReview || l.PendingFeedback != "" {
			return true
		}
	}
	return false
}

// recomputeCardGitHub mirrors the primary link into the card's legacy top-level
// GH* columns and aggregates unread/attention state across all linked items.
func recomputeCardGitHub(c *db.Card, defaultRepo string) {
	if len(c.GHLinks) == 0 {
		clearGitHub(c)
		return
	}
	p := c.GHLinks[0]
	c.GHRepo, c.GHKind, c.GHNumber = p.Repo, p.Kind, p.Number
	c.GHURL, c.GHTitle, c.GHAuthor = p.URL, p.Title, p.Author
	c.GHState, c.GHCIStatus, c.GHReviewDecision, c.GHHeadSHA = p.State, p.CIStatus, p.ReviewDecision, p.HeadSHA
	c.GHCommitCount, c.GHCommentCount, c.GHReviewCommentCount = p.CommitCount, p.CommentCount, p.ReviewCommentCount
	c.GHLabels = p.Labels
	c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview = p.MyReviewState, p.MyReviewedAt, p.IsStaleReview
	c.AckAt, c.AckOtherActivityAt, c.PrevOtherActivityAt = p.AckAt, p.AckOtherActivityAt, p.PrevOtherActivityAt
	setTracking(c, trackingOfLink(&p))

	latestUpdated := p.UpdatedAt
	latestSynced := p.LastSyncedAt
	pendingFeedback := p.PendingFeedback
	firstErr := p.Error
	anyUnread := false
	unreadTotal := 0
	maxCycles := 0
	var unreadParts []string
	multi := len(c.GHLinks) > 1

	for _, l := range c.GHLinks {
		latestUpdated = github.LatestTime(latestUpdated, l.UpdatedAt)
		latestSynced = github.LatestTime(latestSynced, l.LastSyncedAt)
		if l.HasUnreadUpdates {
			anyUnread = true
			unreadTotal += max(1, l.UnreadUpdates)
			if l.UpdateCycles > maxCycles {
				maxCycles = l.UpdateCycles
			}
			sum := l.UpdateSummary
			if sum == "" {
				sum = "New activity"
			}
			if multi {
				unreadParts = append(unreadParts, shortRef(l.Repo, l.Number, defaultRepo)+": "+sum)
			} else {
				unreadParts = append(unreadParts, sum)
			}
		}
		if pendingFeedback == "" && l.PendingFeedback != "" {
			if multi {
				pendingFeedback = shortRef(l.Repo, l.Number, defaultRepo) + ": " + l.PendingFeedback
			} else {
				pendingFeedback = l.PendingFeedback
			}
		}
		if firstErr == "" && l.Error != "" {
			if multi {
				firstErr = shortRef(l.Repo, l.Number, defaultRepo) + ": " + l.Error
			} else {
				firstErr = l.Error
			}
		}
	}

	c.GHUpdatedAt = latestUpdated
	c.GHLastSyncedAt = latestSynced
	c.GHPendingFeedback = pendingFeedback
	c.GHError = firstErr
	c.HasUnreadUpdates = anyUnread
	c.UnreadUpdates = unreadTotal
	c.UpdateCycles = maxCycles
	switch {
	case anyUnread:
		c.UpdateSummary = strings.Join(unreadParts, "; ")
	case !multi:
		c.UpdateSummary = p.UpdateSummary
	case allLinksTerminal(c.GHLinks):
		c.UpdateSummary = p.UpdateSummary
	default:
		c.UpdateSummary = ""
	}
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

// applyGitHubItem stores a freshly fetched item on its matching card link,
// evaluates update-tracking rules, updates the card's primary/aggregate fields,
// and performs any resulting automatic column move.
func (s *Server) applyGitHubItem(ctx context.Context, cardID string, item *github.Item) (*db.Card, github.Result, error) {
	cols, err := s.columnIndex(ctx)
	if err != nil {
		return nil, github.Result{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	me := s.currentUser()
	defaultRepo := s.defaultRepo(ctx)
	var result github.Result
	card, err := s.store.UpdateCard(ctx, cardID, func(c *db.Card, m *db.Mutation) error {
		ensureCardLinks(c)
		idx := -1
		for i, l := range c.GHLinks {
			if sameLinkRef(l, item.Repo, item.Number) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errLinkChanged
		}
		l := &c.GHLinks[idx]
		first := l.AckAt == ""
		l.Kind = item.Kind
		l.URL, l.Title, l.Author = item.URL, item.Title, item.Author
		l.Labels = db.StringList(item.Labels)
		snap := item.Snapshot
		l.State, l.CIStatus, l.ReviewDecision = snap.State, snap.CI, snap.ReviewDecision
		l.HeadSHA, l.UpdatedAt = snap.HeadSHA, snap.UpdatedAt
		l.CommitCount, l.CommentCount, l.ReviewCommentCount = snap.Commits, snap.Comments, snap.ReviewComments
		l.LastSyncedAt, l.Error = now, ""

		own := isOwn(item.Author, me)
		if first && idx == 0 && item.Kind == github.KindPR && own && c.Category == db.CategoryPRReview {
			c.Category = db.CategoryDev
		}
		baselineAt := github.LatestTime(l.AckOtherActivityAt, l.AckUpdatedAt)
		if baselineAt == "" {
			baselineAt = l.AckAt
		}
		if !own && github.After(l.MyReviewedAt, baselineAt) {
			baselineAt = l.MyReviewedAt
		}
		var authorReplies, otherReplies int
		var otherReplyAuthors []string
		var ownReviewOnly, myReviewSubmitted bool
		if item.HasActorData && !first {
			authorReplies, otherReplies = github.CountNewActivities(item.OtherActivities, item.Author, baselineAt, own)
			if item.Kind == github.KindIssue && authorReplies+otherReplies > 0 {
				otherReplyAuthors = github.NewOtherAuthors(item.OtherActivities, item.Author, baselineAt, true)
			} else if otherReplies > 0 {
				otherReplyAuthors = github.NewOtherAuthors(item.OtherActivities, item.Author, baselineAt, own)
			}
			ownReviewOnly = !own && item.MyReview.State != "" && github.After(item.MyReview.At, baselineAt) && authorReplies == 0 && otherReplies == 0
			lastCommit := item.LastCommitAt
			if item.Kind == github.KindIssue {
				lastCommit = ""
			}
			myReviewSubmitted = !own &&
				item.MyReview.State != "" &&
				github.After(item.MyReview.At, l.MyReviewedAt) &&
				github.ReviewCoversLive(item.MyReview, snap, lastCommit, item.LastOtherActivityAt)
		}

		review := github.EvaluateReview(github.ReviewInput{
			GitHub:              item.MyReview,
			StoredState:         l.MyReviewState,
			StoredAt:            l.MyReviewedAt,
			AckHeadSHA:          l.AckHeadSHA,
			Own:                 own,
			Live:                snap,
			LastCommitAt:        item.LastCommitAt,
			LastOtherActivityAt: item.LastOtherActivityAt,
		})

		pendingFeedback := ""
		if !github.IsTerminal(snap.State) && item.HasActorData {
			switch {
			case own:
				_, pendingCount := github.CountNewActivities(item.OtherActivities, item.Author, item.LastOwnActivityAt, true)
				if pendingCount > 0 {
					pendingAuthors := github.NewOtherAuthors(item.OtherActivities, item.Author, item.LastOwnActivityAt, true)
					pendingFeedback = github.FormatCommenters(pendingAuthors, pendingCount, item.Kind)
				}
			case item.Kind == github.KindIssue && review.State != "" && review.Stale:
				_, pendingCount := github.CountNewActivities(item.OtherActivities, item.Author, review.At, true)
				if pendingCount > 0 {
					pendingAuthors := github.NewOtherAuthors(item.OtherActivities, item.Author, review.At, true)
					pendingFeedback = github.FormatCommenters(pendingAuthors, pendingCount, item.Kind)
				}
			}
		}

		result = github.Evaluate(github.Input{
			Live:                  snap,
			Track:                 trackingOfLink(l),
			FirstSync:             first,
			AutoMove:              c.AutoMove,
			ColumnRole:            cols.byID[c.ColumnID].Role,
			UnresolvedThreads:     item.UnresolvedThreads,
			Kind:                  item.Kind,
			Own:                   own,
			HasActorData:          item.HasActorData,
			AuthorReplies:         authorReplies,
			OtherReplies:          otherReplies,
			OtherReplyAuthors:     otherReplyAuthors,
			OwnReviewOnly:         ownReviewOnly,
			MyReviewSubmitted:     myReviewSubmitted,
			MyReviewState:         review.State,
			MyReviewStale:         review.Stale,
			OwnPendingFeedback:    pendingFeedback != "",
			LatestOtherActivityAt: item.LastOtherActivityAt,
			PrevOtherActivityAt:   l.PrevOtherActivityAt,
		})
		setTrackingLink(l, result.Track)
		if item.HasActorData {
			if first {
				l.AckOtherActivityAt = github.LatestTime(item.LastOtherActivityAt, snap.UpdatedAt)
			} else if myReviewSubmitted {
				l.AckOtherActivityAt = github.LatestTime(item.LastOtherActivityAt, item.MyReview.At)
				l.AckAt = now
			}
			l.PrevOtherActivityAt = item.LastOtherActivityAt
		}
		l.MyReviewState, l.MyReviewedAt, l.IsStaleReview = review.State, review.At, review.Stale
		l.PendingFeedback = pendingFeedback
		if first {
			l.AckAt = now
			m.Event(db.EventGitHub, fmt.Sprintf("Linked %s %s#%d (%s); baseline recorded", kindLabel(item.Kind), item.Repo, item.Number, strings.ToLower(snap.State)))
		}
		prefix := ""
		if len(c.GHLinks) > 1 {
			prefix = shortRef(item.Repo, item.Number, defaultRepo) + ": "
		}
		for _, msg := range result.Events {
			m.Event(db.EventUpdate, prefix+msg)
		}

		recomputeCardGitHub(c, defaultRepo)

		if result.MoveToRole != "" && len(c.GHLinks) > 1 {
			switch result.MoveToRole {
			case github.RoleDone:
				if !allLinksTerminal(c.GHLinks) {
					result.MoveToRole = ""
				}
			case github.RoleReady, github.RoleWaiting:
				if anyLinkNeedsAttention(c.GHLinks) {
					result.MoveToRole = ""
				}
			}
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

func shouldSkipLink(l db.CardLink, force bool) bool {
	if force || l.LastSyncedAt == "" {
		return false
	}
	if l.State == github.StateMerged {
		return true
	}
	if l.State == github.StateClosed {
		if last, err := time.Parse(time.RFC3339, l.LastSyncedAt); err == nil && time.Since(last) < closedResyncInterval {
			return true
		}
	}
	return false
}

// syncCard fetches the live GitHub state of all links on one card and applies them.
func (s *Server) syncCard(ctx context.Context, cardID string) (*db.Card, github.Result, error) {
	return s.syncCardLinks(ctx, cardID, true)
}

func (s *Server) syncCardLinks(ctx context.Context, cardID string, force bool) (*db.Card, github.Result, error) {
	card, err := s.store.GetCard(ctx, cardID)
	if err != nil {
		return nil, github.Result{}, err
	}
	ensureCardLinks(card)
	if len(card.GHLinks) == 0 {
		return card, github.Result{}, nil
	}
	defaultRepo := s.defaultRepo(ctx)
	var (
		agg      github.Result
		firstErr error
		synced   int
		latest   = card
	)
	for _, link := range card.GHLinks {
		if shouldSkipLink(link, force) {
			continue
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		item, ferr := s.gh.Fetch(fetchCtx, link.Repo, link.Number, link.Kind)
		cancel()
		if ferr != nil {
			neverSynced := link.LastSyncedAt == ""
			if errors.Is(ferr, github.ErrNotFound) && neverSynced {
				// An automatic link guessed from a session title pointed at
				// nothing; drop just that link instead of retrying forever.
				ref := fmt.Sprintf("%s#%d", link.Repo, link.Number)
				updated, _ := s.store.UpdateCard(ctx, cardID, func(c *db.Card, m *db.Mutation) error {
					ensureCardLinks(c)
					filtered := make([]db.CardLink, 0, len(c.GHLinks))
					removed := false
					for _, cur := range c.GHLinks {
						if sameLinkRef(cur, link.Repo, link.Number) {
							removed = true
							continue
						}
						filtered = append(filtered, cur)
					}
					if !removed {
						return errLinkChanged
					}
					c.GHLinks = filtered
					recomputeCardGitHub(c, defaultRepo)
					m.Event(db.EventGitHub, "Removed link to "+ref+": not found on GitHub")
					return nil
				})
				if updated != nil {
					latest = updated
				}
				if firstErr == nil {
					firstErr = ferr
				}
				continue
			}
			msg := s.describeError(ferr)
			if link.Error != msg {
				updated, _ := s.store.UpdateCard(ctx, cardID, func(c *db.Card, m *db.Mutation) error {
					ensureCardLinks(c)
					found := false
					for i := range c.GHLinks {
						if sameLinkRef(c.GHLinks[i], link.Repo, link.Number) {
							c.GHLinks[i].Error = msg
							found = true
							break
						}
					}
					if !found {
						return errLinkChanged
					}
					recomputeCardGitHub(c, defaultRepo)
					return nil
				})
				if updated != nil {
					latest = updated
				}
			}
			if firstErr == nil {
				firstErr = ferr
			}
			continue
		}
		updated, res, aerr := s.applyGitHubItem(ctx, cardID, item)
		if aerr != nil {
			if firstErr == nil {
				firstErr = aerr
			}
			continue
		}
		latest = updated
		synced++
		if res.NewCycle {
			agg.NewCycle = true
		}
		if res.MoveToRole != "" {
			agg.MoveToRole = res.MoveToRole
		}
		agg.Events = append(agg.Events, res.Events...)
	}
	if synced == 0 && firstErr != nil {
		return nil, github.Result{}, firstErr
	}
	return latest, agg, firstErr
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
		ensureCardLinks(c)
		if len(c.GHLinks) == 0 {
			continue
		}
		report.Total++
		needsSync := false
		for _, l := range c.GHLinks {
			if !shouldSkipLink(l, force) {
				needsSync = true
				break
			}
		}
		if !needsSync {
			report.Skipped++
			continue
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
				_, res, err := s.syncCardLinks(ctx, c.ID, force)
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
		Ref     string   `json:"ref"`
		Refs    []string `json:"refs"`
		Replace bool     `json:"replace"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	defaultRepo := s.defaultRepo(ctx)
	rawInputs := make([]string, 0, len(req.Refs)+1)
	if strings.TrimSpace(req.Ref) != "" {
		rawInputs = append(rawInputs, req.Ref)
	}
	rawInputs = append(rawInputs, req.Refs...)
	var refs []github.Ref
	seen := map[string]bool{}
	for _, raw := range rawInputs {
		parsed, err := github.ParseRefs(raw, defaultRepo)
		if err != nil {
			s.writeGitHubError(w, err)
			return
		}
		for _, ref := range parsed {
			key := fmt.Sprintf("%s#%d", strings.ToLower(ref.Repo), ref.Number)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, ref)
			}
		}
	}
	if len(refs) == 0 {
		s.writeGitHubError(w, fmt.Errorf("%w: empty input", github.ErrBadRef))
		return
	}
	if _, err := s.store.GetCard(ctx, id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	items := make([]*github.Item, 0, len(refs))
	for _, ref := range refs {
		fetchCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		item, err := s.gh.Fetch(fetchCtx, ref.Repo, ref.Number, "")
		cancel()
		if err != nil {
			s.writeGitHubError(w, err)
			return
		}
		items = append(items, item)
	}
	// Attach every new link to the card (or replace existing ones when requested).
	card, err := s.store.UpdateCard(ctx, id, func(c *db.Card, m *db.Mutation) error {
		ensureCardLinks(c)
		if req.Replace {
			for _, old := range c.GHLinks {
				m.Event(db.EventGitHub, fmt.Sprintf("Unlinked %s#%d", old.Repo, old.Number))
			}
			clearGitHub(c)
		}
		for _, ref := range refs {
			exists := false
			for _, cur := range c.GHLinks {
				if sameLinkRef(cur, ref.Repo, ref.Number) {
					exists = true
					break
				}
			}
			if !exists {
				c.GHLinks = append(c.GHLinks, db.CardLink{Repo: ref.Repo, Number: ref.Number})
			}
		}
		recomputeCardGitHub(c, defaultRepo)
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	for _, item := range items {
		updated, _, err := s.applyGitHubItem(ctx, id, item)
		if err != nil {
			s.writeGitHubError(w, err)
			return
		}
		card = updated
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}

func (s *Server) handleUnlinkGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		LinkID string `json:"linkId"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if r.ContentLength != 0 {
		if !decodeBody(w, r, &req) {
			return
		}
	}
	defaultRepo := s.defaultRepo(ctx)
	card, err := s.store.UpdateCard(ctx, r.PathValue("id"), func(c *db.Card, m *db.Mutation) error {
		ensureCardLinks(c)
		if req.LinkID == "" && req.Number == 0 {
			for _, old := range c.GHLinks {
				m.Event(db.EventGitHub, fmt.Sprintf("Unlinked %s#%d", old.Repo, old.Number))
			}
			clearGitHub(c)
			return nil
		}
		filtered := make([]db.CardLink, 0, len(c.GHLinks))
		for _, cur := range c.GHLinks {
			matchID := req.LinkID != "" && cur.ID == req.LinkID
			matchRef := req.Number > 0 && sameLinkRef(cur, req.Repo, req.Number)
			if matchID || matchRef {
				m.Event(db.EventGitHub, fmt.Sprintf("Unlinked %s#%d", cur.Repo, cur.Number))
				continue
			}
			filtered = append(filtered, cur)
		}
		c.GHLinks = filtered
		recomputeCardGitHub(c, defaultRepo)
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.hub.Publish()
	writeJSON(w, http.StatusOK, s.view(card))
}

func (s *Server) handlePrimaryGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		LinkID string `json:"linkId"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	defaultRepo := s.defaultRepo(ctx)
	errNoLink := errors.New("link not found")
	card, err := s.store.UpdateCard(ctx, r.PathValue("id"), func(c *db.Card, m *db.Mutation) error {
		ensureCardLinks(c)
		idx := -1
		for i, cur := range c.GHLinks {
			if (req.LinkID != "" && cur.ID == req.LinkID) || (req.Number > 0 && sameLinkRef(cur, req.Repo, req.Number)) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return errNoLink
		}
		if idx > 0 {
			chosen := c.GHLinks[idx]
			copy(c.GHLinks[1:idx+1], c.GHLinks[0:idx])
			c.GHLinks[0] = chosen
			recomputeCardGitHub(c, defaultRepo)
			m.Event(db.EventGitHub, fmt.Sprintf("Set %s#%d as primary link", chosen.Repo, chosen.Number))
		}
		return nil
	})
	if errors.Is(err, errNoLink) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "No such GitHub link on this card.")
		return
	}
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
	ensureCardLinks(existing)
	if len(existing.GHLinks) == 0 {
		writeError(w, http.StatusConflict, "NOT_LINKED", "This card is not linked to a pull request or issue.")
		return
	}
	card, res, err := s.syncCard(ctx, id)
	s.hub.Publish()
	if err != nil && card == nil {
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
		LinkID         string `json:"linkId"`
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
	defaultRepo := s.defaultRepo(ctx)
	card, err := s.store.UpdateCard(ctx, id, func(c *db.Card, m *db.Mutation) error {
		ensureCardLinks(c)
		if len(c.GHLinks) == 0 {
			return errNotLinked
		}
		now := time.Now().UTC().Format(time.RFC3339)
		acked := 0
		hadUpdates := false
		for i := range c.GHLinks {
			l := &c.GHLinks[i]
			if req.LinkID != "" && l.ID != req.LinkID {
				continue
			}
			if l.LastSyncedAt == "" {
				if req.LinkID != "" {
					return errNotSynced
				}
				continue
			}
			if l.HasUnreadUpdates {
				hadUpdates = true
			}
			setTrackingLink(l, github.Ack(liveSnapshotLink(l)))
			l.AckAt = now
			l.AckOtherActivityAt = github.LatestTime(l.PrevOtherActivityAt, l.UpdatedAt)
			if !isOwn(l.Author, me) && l.Kind != github.KindPR {
				// Issues have no GitHub review API, so acknowledging records a
				// local REVIEWED mark. Pull request review state is driven
				// strictly by actual GitHub reviews.
				if l.MyReviewState == "" {
					l.MyReviewState = github.MyReviewReviewed
				}
				l.MyReviewedAt, l.IsStaleReview, l.PendingFeedback = now, false, ""
			}
			acked++
		}
		if acked == 0 {
			return errNotSynced
		}
		recomputeCardGitHub(c, defaultRepo)
		msg := "Marked reviewed"
		if hadUpdates {
			msg = "Marked caught up"
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
	ctx := r.Context()
	var req struct {
		LinkID string `json:"linkId"`
	}
	if r.ContentLength != 0 {
		if !decodeBody(w, r, &req) {
			return
		}
	}
	defaultRepo := s.defaultRepo(ctx)
	errFromGitHub := errors.New("review comes from github")
	card, err := s.store.UpdateCard(ctx, r.PathValue("id"), func(c *db.Card, m *db.Mutation) error {
		ensureCardLinks(c)
		cleared := false
		sawGitHub := false
		for i := range c.GHLinks {
			l := &c.GHLinks[i]
			if req.LinkID != "" && l.ID != req.LinkID {
				continue
			}
			switch l.MyReviewState {
			case "":
			case github.MyReviewReviewed:
				l.MyReviewState, l.MyReviewedAt, l.IsStaleReview, l.PendingFeedback = "", "", false, ""
				cleared = true
			default:
				sawGitHub = true
			}
		}
		if !cleared && sawGitHub {
			return errFromGitHub
		}
		if cleared {
			recomputeCardGitHub(c, defaultRepo)
			m.Event(db.EventAck, "Review mark cleared")
		}
		return nil
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
