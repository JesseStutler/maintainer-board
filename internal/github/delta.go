package github

import (
	"fmt"
	"strings"
)

const (
	StateOpen   = "OPEN"
	StateDraft  = "DRAFT"
	StateMerged = "MERGED"
	StateClosed = "CLOSED"

	CISuccess = "SUCCESS"
	CIFailure = "FAILURE"
	CIPending = "PENDING"
	CIUnknown = "UNKNOWN"

	ReviewApproved         = "APPROVED"
	ReviewChangesRequested = "CHANGES_REQUESTED"
	ReviewRequired         = "REVIEW_REQUIRED"

	// Column automation roles; kept in sync with the db package.
	RoleInbox      = "inbox"
	RoleInProgress = "in_progress"
	RoleWaiting    = "waiting"
	RoleUpdates    = "updates"
	RoleReady      = "ready"
	RoleDone       = "done"
)

// Snapshot is the part of a PR or issue that update tracking compares.
type Snapshot struct {
	State          string
	CI             string
	ReviewDecision string
	HeadSHA        string
	Commits        int
	Comments       int
	ReviewComments int
	UpdatedAt      string
}

// Tracking is the persisted update-tracking state of a card.
type Tracking struct {
	// Ack is the baseline the maintainer last acknowledged.
	Ack Snapshot
	// Prev is what the previous poll saw; it separates a new update from a
	// repeated poll of the same update.
	Prev      Snapshot
	Unread    int
	Cycles    int
	Summary   string
	HasUnread bool
}

type Input struct {
	Live  Snapshot
	Track Tracking
	// FirstSync is true when the card has no acknowledged baseline yet.
	FirstSync bool
	AutoMove  bool
	// ColumnRole is the automation role of the card's current column.
	ColumnRole string
	// UnresolvedThreads counts unresolved review threads; it blocks the move
	// to the ready column. Zero or negative means none or unknown.
	UnresolvedThreads int

	// Kind is KindPR or KindIssue.
	Kind string
	// Own is true when the board's current user authored this PR or issue.
	Own bool
	// HasActorData is true when AuthorReplies, OtherReplies, and
	// LatestOtherActivityAt reflect non-self, non-bot GitHub activity.
	HasActorData bool
	// AuthorReplies counts non-self comments/reviews by the PR or issue author
	// strictly after the acknowledged baseline (used when !Own).
	AuthorReplies int
	// OtherReplies counts non-self, non-bot comments/reviews by other people
	// strictly after the acknowledged baseline.
	OtherReplies int
	// OtherReplyAuthors lists the distinct logins behind OtherReplies, ordered
	// from most recent activity to oldest.
	OtherReplyAuthors []string
	// OwnReviewOnly is true when the current user submitted a review on
	// someone else's PR after the baseline and no other person acted after.
	OwnReviewOnly bool
	// MyReviewSubmitted is true when the current user submitted a newer review
	// on someone else's open PR that covers the live head commit and all
	// non-self human activity.
	MyReviewSubmitted bool
	// MyReviewState is the personal review state for MyReviewSubmitted or
	// initial triage on FirstSync.
	MyReviewState string
	// MyReviewStale is true when the user's GitHub review is out of date
	// (newer commits or non-self comments/reviews exist).
	MyReviewStale bool
	// LatestOtherActivityAt is the newest non-self, non-bot activity timestamp.
	LatestOtherActivityAt string
	// PrevOtherActivityAt is LatestOtherActivityAt from the previous poll.
	PrevOtherActivityAt string
}

type Result struct {
	Track Tracking
	// MoveToRole names the role of the column the card should move to, or
	// is empty when the card stays where it is.
	MoveToRole string
	// NewCycle is true when this poll saw an update the previous poll did not.
	NewCycle bool
	// Events are audit-timeline messages for this evaluation.
	Events []string
}

func IsTerminal(state string) bool { return state == StateMerged || state == StateClosed }

func settledCI(ci string) bool { return ci == CISuccess || ci == CIFailure }

// Ack returns the tracking state after the maintainer acknowledged live.
func Ack(live Snapshot) Tracking {
	t := Tracking{Ack: live, Prev: live}
	if IsTerminal(live.State) {
		t.Summary = terminalNote(live.State)
	}
	return t
}

func terminalNote(state string) string {
	if state == StateMerged {
		return "Merged"
	}
	return "Closed"
}

func myReviewEvent(state string) string {
	switch state {
	case MyReviewApproved:
		return "Your review submitted (approved); baseline updated"
	case MyReviewLGTM:
		return "Your review submitted (/lgtm); baseline updated"
	case MyReviewChangesRequested:
		return "Your review submitted (changes requested); baseline updated"
	default:
		return "Your review submitted (commented); baseline updated"
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("+1 %s", noun)
	}
	return fmt.Sprintf("+%d %ss", n, noun)
}

func formatCommenters(authors []string, count int, kind string) string {
	switch len(authors) {
	case 0:
		if kind == KindIssue {
			return plural(count, "comment")
		}
		return fmt.Sprintf("Reviewer commented (+%d)", count)
	case 1:
		if count > 1 {
			return fmt.Sprintf("@%s commented (+%d)", authors[0], count)
		}
		return fmt.Sprintf("@%s commented", authors[0])
	case 2:
		if count > 2 {
			return fmt.Sprintf("@%s, @%s commented (+%d)", authors[0], authors[1], count)
		}
		return fmt.Sprintf("@%s, @%s commented", authors[0], authors[1])
	default:
		return fmt.Sprintf("@%s +%d commented", authors[0], len(authors)-1)
	}
}

// initialTriageRole picks the landing column on a card's first GitHub sync
// when it is still in Inbox, so imported sessions and newly linked PRs/issues
// start in the column that matches their live GitHub status instead of
// staying in Inbox.
func initialTriageRole(in Input) string {
	if !in.AutoMove || in.ColumnRole != RoleInbox {
		return ""
	}
	live := in.Live
	if IsTerminal(live.State) {
		return RoleDone
	}
	if in.Kind == KindIssue {
		return ""
	}
	if in.Own {
		switch {
		case live.State == StateOpen && live.ReviewDecision == ReviewApproved && live.CI == CISuccess && in.UnresolvedThreads <= 0:
			return RoleReady
		case live.State == StateOpen && live.ReviewDecision == ReviewChangesRequested:
			return RoleUpdates
		default:
			return ""
		}
	}
	if in.MyReviewStale {
		return RoleUpdates
	}
	if in.MyReviewState != "" {
		if live.State == StateOpen && live.ReviewDecision == ReviewApproved && live.CI == CISuccess && in.UnresolvedThreads <= 0 {
			return RoleReady
		}
		return RoleWaiting
	}
	return ""
}

// Evaluate compares the live state against the acknowledged baseline and the
// previous poll, and decides the new tracking state and any column move.
//
// Rules:
//   - The first sync records the baseline without marking unread updates, and
//     triages Inbox cards whose live GitHub state already belongs in Done,
//     Updates, Waiting, or Ready.
//   - Merged and closed items never count as unread; the transition moves the
//     card to the done column.
//   - Otherwise new commits, new comments, a state change, a settled CI result
//     that differs from the baseline, or a changed review decision make the
//     card unread. A cycle is counted only when the poll differs from the
//     previous poll, and only a new cycle may move the card, so a card the
//     maintainer dragged elsewhere is not pulled back by a repeated poll.
func Evaluate(in Input) Result {
	live := in.Live
	if in.FirstSync {
		return Result{
			Track:      Ack(live),
			MoveToRole: initialTriageRole(in),
		}
	}
	ack, prev := in.Track.Ack, in.Track.Prev

	// An unsettled CI state keeps the last settled one as the previous value
	// so a re-run with the same outcome is not counted as a new update.
	nextPrev := live
	if !settledCI(live.CI) {
		nextPrev.CI = prev.CI
	}

	stateChanged := live.State != ack.State

	if IsTerminal(live.State) {
		res := Result{Track: Tracking{Ack: live, Prev: nextPrev, Summary: terminalNote(live.State)}}
		if stateChanged {
			res.NewCycle = true
			res.Events = append(res.Events, terminalNote(live.State))
			if in.AutoMove && in.ColumnRole != RoleDone {
				res.MoveToRole = RoleDone
			}
		}
		return res
	}

	// A fresh review submitted by the maintainer on someone else's PR that
	// covers the latest head and author replies re-baselines the card and
	// moves it out of active review queues into Ready or Waiting once.
	if in.MyReviewSubmitted {
		res := Result{Track: Ack(live)}
		res.Track.Prev = nextPrev
		res.Events = append(res.Events, myReviewEvent(in.MyReviewState))
		if in.AutoMove {
			switch {
			case live.State == StateOpen && live.ReviewDecision == ReviewApproved && live.CI == CISuccess && in.UnresolvedThreads <= 0:
				if in.ColumnRole != RoleReady && in.ColumnRole != RoleDone {
					res.MoveToRole = RoleReady
				}
			case in.ColumnRole == RoleInbox || in.ColumnRole == RoleInProgress || in.ColumnRole == RoleUpdates:
				res.MoveToRole = RoleWaiting
			}
		}
		return res
	}

	newCommits := 0
	forcePushed := false
	// On your own PR, your own pushes should not raise an unread alert; only
	// advance the acknowledged commit baseline quietly.
	if in.HasActorData && in.Own {
		ack.HeadSHA = live.HeadSHA
		ack.Commits = live.Commits
	} else {
		newCommits = max(0, live.Commits-ack.Commits)
		if newCommits == 0 && live.HeadSHA != "" && ack.HeadSHA != "" && live.HeadSHA != ack.HeadSHA {
			forcePushed = true
			newCommits = 1
		}
	}

	var newAuthorReplies, newOtherReplies, newComments int
	if in.HasActorData {
		newAuthorReplies = max(0, in.AuthorReplies)
		newOtherReplies = max(0, in.OtherReplies)
		newComments = newAuthorReplies + newOtherReplies
		if newComments == 0 {
			// Quietly advance comment counts over the user's own or bot comments.
			ack.Comments = live.Comments
			ack.ReviewComments = live.ReviewComments
		}
	} else {
		newComments = max(0, (live.Comments+live.ReviewComments)-(ack.Comments+ack.ReviewComments))
	}

	ciChanged := settledCI(live.CI) && live.CI != ack.CI
	reviewChanged := live.ReviewDecision != ack.ReviewDecision
	ownApprovedReady := false
	if reviewChanged && in.HasActorData && in.OwnReviewOnly {
		reviewChanged = false
		ack.ReviewDecision = live.ReviewDecision
		if live.State == StateOpen && live.ReviewDecision == ReviewApproved && live.CI == CISuccess && in.UnresolvedThreads <= 0 {
			ownApprovedReady = true
		}
	}

	if newCommits == 0 && newComments == 0 && !stateChanged && !ciChanged && !reviewChanged {
		res := Result{Track: Tracking{Ack: ack, Prev: nextPrev}}
		if ownApprovedReady && in.AutoMove && in.ColumnRole != RoleReady && in.ColumnRole != RoleDone {
			res.MoveToRole = RoleReady
		}
		return res
	}

	var parts []string
	reopened := false
	if stateChanged {
		switch {
		case IsTerminal(ack.State):
			reopened = true
			parts = append(parts, "Reopened")
		case live.State == StateDraft:
			parts = append(parts, "Converted to draft")
		case ack.State == StateDraft:
			parts = append(parts, "Ready for review")
		default:
			parts = append(parts, "State changed to "+strings.ToLower(live.State))
		}
	}
	isPRReview := in.HasActorData && !in.Own && in.Kind != KindIssue
	switch {
	case forcePushed && isPRReview:
		parts = append(parts, "Author force-pushed")
	case forcePushed:
		parts = append(parts, "Force-pushed")
	case newCommits > 0 && isPRReview:
		parts = append(parts, "Author pushed "+plural(newCommits, "commit"))
	case newCommits > 0:
		parts = append(parts, plural(newCommits, "commit"))
	}
	if in.HasActorData {
		if !in.Own {
			if newAuthorReplies > 0 {
				parts = append(parts, fmt.Sprintf("Author replied (+%d)", newAuthorReplies))
			}
			if newOtherReplies > 0 {
				parts = append(parts, plural(newOtherReplies, "response"))
			}
		} else if newOtherReplies > 0 {
			parts = append(parts, formatCommenters(in.OtherReplyAuthors, newOtherReplies, in.Kind))
		}
	} else if newComments > 0 {
		parts = append(parts, plural(newComments, "comment"))
	}
	if reviewChanged {
		switch live.ReviewDecision {
		case ReviewApproved:
			parts = append(parts, "Approved")
		case ReviewChangesRequested:
			parts = append(parts, "Changes requested")
		case ReviewRequired:
			if ack.ReviewDecision == ReviewApproved {
				parts = append(parts, "Approval dismissed")
			} else {
				parts = append(parts, "Review required")
			}
		default:
			parts = append(parts, "Review decision cleared")
		}
	}
	if ciChanged {
		if live.CI == CISuccess {
			parts = append(parts, "CI passed")
		} else {
			parts = append(parts, "CI failed")
		}
	}
	summary := strings.Join(parts, ", ")

	var material bool
	if in.HasActorData {
		commitMaterial := !in.Own && (live.HeadSHA != prev.HeadSHA || live.Commits != prev.Commits)
		otherActivityMaterial := in.LatestOtherActivityAt != "" && in.LatestOtherActivityAt != in.PrevOtherActivityAt
		reviewMaterial := !in.OwnReviewOnly && live.ReviewDecision != prev.ReviewDecision
		material = commitMaterial ||
			otherActivityMaterial ||
			live.State != prev.State ||
			reviewMaterial ||
			(settledCI(live.CI) && live.CI != prev.CI)
	} else {
		material = live.HeadSHA != prev.HeadSHA ||
			live.Commits != prev.Commits ||
			live.Comments != prev.Comments ||
			live.ReviewComments != prev.ReviewComments ||
			live.State != prev.State ||
			live.ReviewDecision != prev.ReviewDecision ||
			(settledCI(live.CI) && live.CI != prev.CI)
	}
	newCycle := material || !in.Track.HasUnread

	cycles := in.Track.Cycles
	if !in.Track.HasUnread {
		cycles = 0
	}
	if newCycle {
		cycles++
	}

	res := Result{
		Track: Tracking{
			Ack:       ack,
			Prev:      nextPrev,
			Unread:    max(1, newCommits+newComments),
			Cycles:    cycles,
			Summary:   summary,
			HasUnread: true,
		},
		NewCycle: newCycle,
	}
	if !newCycle {
		return res
	}
	if cycles > 1 {
		res.Events = append(res.Events, fmt.Sprintf("Update detected (%dx since last review): %s", cycles, summary))
	} else {
		res.Events = append(res.Events, "Update detected: "+summary)
	}

	if !in.AutoMove {
		return res
	}
	movable := false
	switch in.ColumnRole {
	case RoleInbox, RoleInProgress, RoleWaiting, RoleUpdates, RoleReady:
		movable = true
	case RoleDone:
		movable = reopened
	}
	if !movable {
		return res
	}
	target := RoleUpdates
	if live.State == StateOpen && live.ReviewDecision == ReviewApproved && live.CI == CISuccess && in.UnresolvedThreads <= 0 {
		target = RoleReady
	}
	if target != in.ColumnRole {
		res.MoveToRole = target
	}
	return res
}
