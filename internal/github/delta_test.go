package github

import (
	"reflect"
	"testing"
)

// tracker replays a sequence of polls against the decision engine the way
// the server does: it carries the tracking state from one poll to the next
// and applies column moves.
type tracker struct {
	t        *testing.T
	track    Tracking
	role     string
	autoMove bool
	linked   bool
}

func newTracker(t *testing.T, role string) *tracker {
	return &tracker{t: t, role: role, autoMove: true}
}

func (tr *tracker) poll(live Snapshot) Result {
	tr.t.Helper()
	return tr.pollThreads(live, 0)
}

func (tr *tracker) pollThreads(live Snapshot, unresolved int) Result {
	tr.t.Helper()
	res := Evaluate(Input{
		Live: live, Track: tr.track, FirstSync: !tr.linked, AutoMove: tr.autoMove,
		ColumnRole: tr.role, UnresolvedThreads: unresolved,
	})
	tr.linked = true
	tr.track = res.Track
	if res.MoveToRole != "" {
		tr.role = res.MoveToRole
	}
	return res
}

func (tr *tracker) ack(live Snapshot) { tr.track = Ack(live) }

func (tr *tracker) expect(wantRole string, unread bool, count, cycles int, summary string) {
	tr.t.Helper()
	got := tr.track
	if tr.role != wantRole || got.HasUnread != unread || got.Unread != count || got.Cycles != cycles || got.Summary != summary {
		tr.t.Fatalf("state = role %q unread %v count %d cycles %d summary %q;\n want role %q unread %v count %d cycles %d summary %q",
			tr.role, got.HasUnread, got.Unread, got.Cycles, got.Summary, wantRole, unread, count, cycles, summary)
	}
}

func openPR() Snapshot {
	return Snapshot{State: StateOpen, CI: CISuccess, ReviewDecision: ReviewRequired, HeadSHA: "aaa", Commits: 3, Comments: 2, ReviewComments: 1, UpdatedAt: "t0"}
}

func TestFirstSyncRecordsBaselineWithoutMoving(t *testing.T) {
	for _, state := range []string{StateOpen, StateDraft, StateMerged, StateClosed} {
		tr := newTracker(t, RoleInbox)
		live := openPR()
		live.State = state
		res := tr.poll(live)
		if res.MoveToRole != "" || res.NewCycle || len(res.Events) != 0 {
			t.Fatalf("%s: first sync produced %+v", state, res)
		}
		if tr.track.Ack != live || tr.track.Prev != live || tr.track.HasUnread {
			t.Fatalf("%s: baseline not recorded: %+v", state, tr.track)
		}
	}
}

func TestUnchangedPollsStayQuiet(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())
	live := openPR()
	live.UpdatedAt = "t1" // a label edit bumps the timestamp but nothing tracked
	res := tr.poll(live)
	if res.NewCycle || res.MoveToRole != "" {
		t.Fatalf("untracked change produced %+v", res)
	}
	tr.expect(RoleWaiting, false, 0, 0, "")
}

func TestNewCommitAndCommentMoveWaitingToUpdates(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())

	live := openPR()
	live.Commits, live.HeadSHA, live.Comments, live.CI = 5, "bbb", 3, CIPending
	res := tr.poll(live)
	if !res.NewCycle || !reflect.DeepEqual(res.Events, []string{"Update detected: +2 commits, +1 comment"}) {
		t.Fatalf("result = %+v", res)
	}
	tr.expect(RoleUpdates, true, 3, 1, "+2 commits, +1 comment")
}

func TestRepeatedPollsDoNotDoubleCountButNewPushesDo(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())

	push1 := openPR()
	push1.Commits, push1.HeadSHA = 4, "bbb"
	tr.poll(push1)
	tr.expect(RoleUpdates, true, 1, 1, "+1 commit")

	for range 3 {
		if res := tr.poll(push1); res.NewCycle || len(res.Events) != 0 {
			t.Fatalf("repeat poll counted as a new cycle: %+v", res)
		}
	}
	tr.expect(RoleUpdates, true, 1, 1, "+1 commit")

	push2 := push1
	push2.Commits, push2.HeadSHA = 5, "ccc"
	res := tr.poll(push2)
	if !reflect.DeepEqual(res.Events, []string{"Update detected (2x since last review): +2 commits"}) {
		t.Fatalf("events = %v", res.Events)
	}
	tr.expect(RoleUpdates, true, 2, 2, "+2 commits")

	comment := push2
	comment.ReviewComments = 4
	tr.poll(comment)
	tr.expect(RoleUpdates, true, 5, 3, "+2 commits, +3 comments")
}

func TestForcePushWithSameCommitCount(t *testing.T) {
	tr := newTracker(t, RoleInProgress)
	tr.poll(openPR())
	live := openPR()
	live.HeadSHA = "rebased"
	tr.poll(live)
	tr.expect(RoleUpdates, true, 1, 1, "Force-pushed")
}

func TestCIResultIsAnUpdateOnlyWhenSettledAndDifferent(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	base := openPR()
	base.CI = CIPending
	tr.poll(base)

	// A re-run that is still pending is not news.
	tr.poll(base)
	tr.expect(RoleWaiting, false, 0, 0, "")

	failed := base
	failed.CI = CIFailure
	tr.poll(failed)
	tr.expect(RoleUpdates, true, 1, 1, "CI failed")

	// Re-run: pending again, then the same failure. Still one cycle.
	tr.poll(base)
	tr.expect(RoleUpdates, false, 0, 0, "")
	tr.poll(failed)
	tr.expect(RoleUpdates, true, 1, 1, "CI failed")

	passed := base
	passed.CI = CISuccess
	tr.poll(passed)
	tr.expect(RoleUpdates, true, 1, 2, "CI passed")

	// UNKNOWN (for example gh returned no checks) never counts.
	tr2 := newTracker(t, RoleWaiting)
	tr2.poll(openPR())
	unknown := openPR()
	unknown.CI = CIUnknown
	tr2.poll(unknown)
	tr2.expect(RoleWaiting, false, 0, 0, "")
}

func TestApprovedWithGreenCIMovesToReady(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())

	approved := openPR()
	approved.ReviewDecision = ReviewApproved
	tr.poll(approved)
	tr.expect(RoleReady, true, 1, 1, "Approved")

	// A later comment keeps an approved, green PR in Ready.
	commented := approved
	commented.Comments++
	tr.poll(commented)
	tr.expect(RoleReady, true, 1, 2, "+1 comment, Approved")

	// A new push resets CI and dismisses the approval: back to Updates.
	pushed := commented
	pushed.Commits, pushed.HeadSHA, pushed.CI, pushed.ReviewDecision = 4, "bbb", CIPending, ReviewRequired
	tr.poll(pushed)
	tr.expect(RoleUpdates, true, 2, 3, "+1 commit, +1 comment")
}

func TestApprovalWithoutGreenCIOrWithOpenThreadsGoesToUpdates(t *testing.T) {
	cases := map[string]struct {
		mutate     func(*Snapshot)
		unresolved int
	}{
		"CI pending":         {func(s *Snapshot) { s.CI = CIPending }, 0},
		"CI failing":         {func(s *Snapshot) { s.CI = CIFailure }, 0},
		"draft":              {func(s *Snapshot) { s.State = StateDraft }, 0},
		"unresolved threads": {func(s *Snapshot) {}, 2},
	}
	for name, tc := range cases {
		tr := newTracker(t, RoleWaiting)
		base := openPR()
		tc.mutate(&base)
		tr.poll(base)
		approved := base
		approved.ReviewDecision = ReviewApproved
		tr.pollThreads(approved, tc.unresolved)
		if tr.role != RoleUpdates {
			t.Errorf("%s: moved to %q, want updates", name, tr.role)
		}
	}
}

func TestMergedAndClosedMoveToDoneAndClearUnread(t *testing.T) {
	for _, terminal := range []string{StateMerged, StateClosed} {
		tr := newTracker(t, RoleWaiting)
		tr.poll(openPR())

		pushed := openPR()
		pushed.Commits, pushed.HeadSHA = 4, "bbb"
		tr.poll(pushed)
		tr.expect(RoleUpdates, true, 1, 1, "+1 commit")

		done := pushed
		done.State, done.Comments = terminal, 9
		res := tr.poll(done)
		note := terminalNote(terminal)
		if !reflect.DeepEqual(res.Events, []string{note}) {
			t.Fatalf("%s: events = %v", terminal, res.Events)
		}
		tr.expect(RoleDone, false, 0, 0, note)

		// Later comments on a finished item never pull it out of Done.
		more := done
		more.Comments = 12
		if res := tr.poll(more); res.MoveToRole != "" || res.NewCycle {
			t.Fatalf("%s: post-terminal comment produced %+v", terminal, res)
		}
		tr.expect(RoleDone, false, 0, 0, note)
	}
}

func TestReopenedLeavesDone(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())
	closed := openPR()
	closed.State = StateClosed
	tr.poll(closed)
	tr.expect(RoleDone, false, 0, 0, "Closed")

	reopened := closed
	reopened.State, reopened.Comments = StateOpen, 3
	tr.poll(reopened)
	tr.expect(RoleUpdates, true, 1, 1, "Reopened, +1 comment")
}

func TestDraftTransitions(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	draft := openPR()
	draft.State = StateDraft
	tr.poll(draft)
	tr.poll(openPR())
	tr.expect(RoleUpdates, true, 1, 1, "Ready for review")

	tr.ack(openPR())
	tr.poll(draft)
	tr.expect(RoleUpdates, true, 1, 1, "Converted to draft")
}

func TestReviewDecisionSummaries(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{ReviewRequired, ReviewChangesRequested, "Changes requested"},
		{ReviewApproved, ReviewRequired, "Approval dismissed"},
		{ReviewChangesRequested, ReviewRequired, "Review required"},
		{ReviewApproved, "", "Review decision cleared"},
	}
	for _, tc := range cases {
		tr := newTracker(t, RoleWaiting)
		base := openPR()
		base.CI, base.ReviewDecision = CIFailure, tc.from
		tr.poll(base)
		next := base
		next.ReviewDecision = tc.to
		tr.poll(next)
		tr.expect(RoleUpdates, true, 1, 1, tc.want)
	}
}

func TestAckResetsUnreadAndRebaselines(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())
	pushed := openPR()
	pushed.Commits, pushed.HeadSHA, pushed.Comments = 6, "bbb", 5
	tr.poll(pushed)
	tr.poll(pushed)
	tr.expect(RoleUpdates, true, 6, 1, "+3 commits, +3 comments")

	tr.ack(pushed)
	tr.role = RoleWaiting
	tr.expect(RoleWaiting, false, 0, 0, "")
	if tr.track.Ack != pushed {
		t.Fatalf("ack baseline = %+v, want %+v", tr.track.Ack, pushed)
	}

	// The same state stays quiet after the ack; only newer activity counts.
	if res := tr.poll(pushed); res.NewCycle || res.MoveToRole != "" {
		t.Fatalf("poll after ack produced %+v", res)
	}
	tr.expect(RoleWaiting, false, 0, 0, "")

	again := pushed
	again.Comments++
	tr.poll(again)
	tr.expect(RoleUpdates, true, 1, 1, "+1 comment")
}

func TestAutoMoveDisabledStillTracks(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.autoMove = false
	tr.poll(openPR())

	pushed := openPR()
	pushed.Commits, pushed.HeadSHA = 4, "bbb"
	tr.poll(pushed)
	tr.expect(RoleWaiting, true, 1, 1, "+1 commit")

	merged := pushed
	merged.State = StateMerged
	tr.poll(merged)
	tr.expect(RoleWaiting, false, 0, 0, "Merged")
}

func TestOnlyANewCycleMovesTheCard(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())
	pushed := openPR()
	pushed.Commits, pushed.HeadSHA = 4, "bbb"
	tr.poll(pushed)
	tr.expect(RoleUpdates, true, 1, 1, "+1 commit")

	// The maintainer drags the card to In Progress to start the re-review
	// without acknowledging. Repeated polls must not pull it back.
	tr.role = RoleInProgress
	tr.poll(pushed)
	tr.poll(pushed)
	tr.expect(RoleInProgress, true, 1, 1, "+1 commit")

	// A genuinely new push does.
	push2 := pushed
	push2.Commits, push2.HeadSHA = 5, "ccc"
	tr.poll(push2)
	tr.expect(RoleUpdates, true, 2, 2, "+2 commits")
}

func TestMaintainerReviewIgnoresSelfAndTracksAuthorPushAndReplies(t *testing.T) {
	base := openPR()
	track := Ack(base)
	prevOther := "2026-11-20T10:00:00Z"

	// 1. Maintainer posts their own review comment and requests changes:
	//    no unread update, stays in Waiting, baseline advances quietly.
	ownComment := base
	ownComment.ReviewComments++
	ownComment.ReviewDecision = ReviewChangesRequested
	res := Evaluate(Input{
		Live:                  ownComment,
		Track:                 track,
		AutoMove:              true,
		ColumnRole:            RoleWaiting,
		Kind:                  KindPR,
		Own:                   false,
		HasActorData:          true,
		AuthorReplies:         0,
		OtherReplies:          0,
		OwnReviewOnly:         true,
		LatestOtherActivityAt: prevOther,
		PrevOtherActivityAt:   prevOther,
	})
	if res.Track.HasUnread || res.MoveToRole != "" || res.NewCycle {
		t.Fatalf("own review triggered unread/move: %+v", res)
	}
	if res.Track.Ack.ReviewComments != ownComment.ReviewComments || res.Track.Ack.ReviewDecision != ReviewChangesRequested {
		t.Fatalf("ack did not quietly absorb own review: %+v", res.Track.Ack)
	}
	track = res.Track

	// 2. Author replies in the review thread: moves to Updates with Maintainer wording.
	authorReply := ownComment
	authorReply.ReviewComments++
	tReply := "2026-11-20T11:00:00Z"
	res = Evaluate(Input{
		Live:                  authorReply,
		Track:                 track,
		AutoMove:              true,
		ColumnRole:            RoleWaiting,
		Kind:                  KindPR,
		Own:                   false,
		HasActorData:          true,
		AuthorReplies:         1,
		LatestOtherActivityAt: tReply,
		PrevOtherActivityAt:   prevOther,
	})
	if !res.Track.HasUnread || !res.NewCycle || res.MoveToRole != RoleUpdates || res.Track.Cycles != 1 {
		t.Fatalf("author reply result = %+v", res)
	}
	if res.Track.Summary != "Author replied (+1)" {
		t.Fatalf("summary = %q, want %q", res.Track.Summary, "Author replied (+1)")
	}
	track = res.Track
	prevOther = tReply

	// 3. Maintainer replies back without acknowledging yet: must NOT count as a 2nd cycle.
	maintainerFollowup := authorReply
	maintainerFollowup.ReviewComments++
	res = Evaluate(Input{
		Live:                  maintainerFollowup,
		Track:                 track,
		AutoMove:              true,
		ColumnRole:            RoleInProgress,
		Kind:                  KindPR,
		Own:                   false,
		HasActorData:          true,
		AuthorReplies:         1,
		LatestOtherActivityAt: tReply,
		PrevOtherActivityAt:   prevOther,
	})
	if res.NewCycle || res.MoveToRole != "" || res.Track.Cycles != 1 {
		t.Fatalf("own follow-up bumped cycle: %+v", res)
	}
	track = res.Track

	// 4. Author pushes a new commit: 2nd cycle and moves back to Updates.
	authorPush := maintainerFollowup
	authorPush.Commits++
	authorPush.HeadSHA = "bbb"
	res = Evaluate(Input{
		Live:                  authorPush,
		Track:                 track,
		AutoMove:              true,
		ColumnRole:            RoleInProgress,
		Kind:                  KindPR,
		Own:                   false,
		HasActorData:          true,
		AuthorReplies:         1,
		LatestOtherActivityAt: tReply,
		PrevOtherActivityAt:   prevOther,
	})
	if !res.NewCycle || res.MoveToRole != RoleUpdates || res.Track.Cycles != 2 {
		t.Fatalf("author push result = %+v", res)
	}
	if res.Track.Summary != "Author pushed +1 commit, Author replied (+1)" {
		t.Fatalf("summary = %q", res.Track.Summary)
	}
}

func TestOwnPRIgnoresOwnPushesAndCommentsAndTracksReviewers(t *testing.T) {
	base := openPR()
	track := Ack(base)
	prevOther := "2026-11-20T10:00:00Z"

	// 1. Author pushes commits and comments on their own PR: ignored cleanly.
	ownPush := base
	ownPush.Commits += 2
	ownPush.HeadSHA = "bbb"
	ownPush.Comments++
	res := Evaluate(Input{
		Live:                  ownPush,
		Track:                 track,
		AutoMove:              true,
		ColumnRole:            RoleWaiting,
		Kind:                  KindPR,
		Own:                   true,
		HasActorData:          true,
		OtherReplies:          0,
		LatestOtherActivityAt: prevOther,
		PrevOtherActivityAt:   prevOther,
	})
	if res.Track.HasUnread || res.MoveToRole != "" || res.NewCycle {
		t.Fatalf("own push/comment triggered unread: %+v", res)
	}
	if res.Track.Ack.HeadSHA != "bbb" || res.Track.Ack.Commits != ownPush.Commits {
		t.Fatalf("own push did not advance ack baseline: %+v", res.Track.Ack)
	}
	track = res.Track

	// 2. A reviewer leaves comments and requests changes: moves to Updates.
	reviewed := ownPush
	reviewed.ReviewComments += 2
	reviewed.ReviewDecision = ReviewChangesRequested
	tRev := "2026-11-20T12:00:00Z"
	res = Evaluate(Input{
		Live:                  reviewed,
		Track:                 track,
		AutoMove:              true,
		ColumnRole:            RoleWaiting,
		Kind:                  KindPR,
		Own:                   true,
		HasActorData:          true,
		OtherReplies:          2,
		LatestOtherActivityAt: tRev,
		PrevOtherActivityAt:   prevOther,
	})
	if !res.Track.HasUnread || !res.NewCycle || res.MoveToRole != RoleUpdates {
		t.Fatalf("reviewer update result = %+v", res)
	}
	if res.Track.Summary != "Reviewer commented (+2), Changes requested" {
		t.Fatalf("summary = %q", res.Track.Summary)
	}
}

func TestCountNewActivitiesAndInlineReviewComments(t *testing.T) {
	me := "JesseStutler"
	reviews := []ghReview{
		{Author: ghAuthor{Login: me}, State: "COMMENTED", Body: "nit", SubmittedAt: "2026-11-20T10:05:00Z"},
		{Author: ghAuthor{Login: "k8s-ci-robot"}, State: "COMMENTED", Body: "bot", SubmittedAt: "2026-11-20T10:06:00Z"},
	}
	comments := []ghComment{
		{Author: ghAuthor{Login: "contrib"}, Body: "updated", CreatedAt: "2026-11-20T10:10:00Z"},
		{Author: ghAuthor{Login: me}, Body: "thanks", CreatedAt: "2026-11-20T10:12:00Z"},
	}
	inline := []ghReviewComment{
		{User: ghAuthor{Login: "contrib"}, CreatedAt: "2026-11-20T10:15:00Z"},
		{User: ghAuthor{Login: "other-reviewer"}, CreatedAt: "2026-11-20T10:16:00Z"},
		{User: ghAuthor{Login: me}, CreatedAt: "2026-11-20T10:20:00Z"},
	}

	if got := lastOtherActivity(me, reviews, comments, inline...); got != "2026-11-20T10:16:00Z" {
		t.Fatalf("lastOtherActivity = %q, want inline comment timestamp", got)
	}
	acts := otherActivities(me, reviews, comments, inline)
	authorN, otherN := CountNewActivities(acts, "contrib", "2026-11-20T10:00:00Z", false)
	if authorN != 2 || otherN != 1 {
		t.Fatalf("CountNewActivities = (%d, %d), want (2, 1)", authorN, otherN)
	}
}

func TestReviewCoversLive(t *testing.T) {
	live := openPR()
	rv := MyReview{State: MyReviewCommented, At: "2026-11-20T12:00:00Z", CommitSHA: "aaa"}

	if !ReviewCoversLive(rv, live, "2026-11-20T11:00:00Z", "2026-11-20T11:30:00Z") {
		t.Fatal("expected review covering head SHA and older activity to be current")
	}
	if ReviewCoversLive(rv, live, "2026-11-20T11:00:00Z", "2026-11-20T12:05:00Z") {
		t.Fatal("expected newer non-self activity to invalidate ReviewCoversLive")
	}
	pushed := live
	pushed.HeadSHA = "bbb"
	if ReviewCoversLive(rv, pushed, "2026-11-20T11:00:00Z", "2026-11-20T11:30:00Z") {
		t.Fatal("expected newer head SHA to invalidate ReviewCoversLive")
	}
	merged := live
	merged.State = StateMerged
	if ReviewCoversLive(rv, merged, "2026-11-20T11:00:00Z", "2026-11-20T11:30:00Z") {
		t.Fatal("expected terminal PR to return false")
	}
}

func TestMyReviewSubmittedAutoMovesToWaitingOrReady(t *testing.T) {
	// Submitting a review from Inbox, In Progress, or Updates clears unread
	// state and moves the card to Waiting when the PR is not ready to merge.
	for _, fromRole := range []string{RoleInbox, RoleInProgress, RoleUpdates} {
		live := openPR()
		live.ReviewComments++
		live.ReviewDecision = ReviewChangesRequested
		track := Ack(openPR())
		track.HasUnread = true
		track.Unread = 2
		track.Cycles = 1
		track.Summary = "Author pushed +1 commit"

		res := Evaluate(Input{
			Live:              live,
			Track:             track,
			AutoMove:          true,
			ColumnRole:        fromRole,
			Kind:              KindPR,
			HasActorData:      true,
			MyReviewSubmitted: true,
			MyReviewState:     MyReviewChangesRequested,
		})
		if res.MoveToRole != RoleWaiting {
			t.Errorf("from %q: MoveToRole = %q, want %q", fromRole, res.MoveToRole, RoleWaiting)
		}
		if res.Track.HasUnread || res.Track.Unread != 0 || res.Track.Cycles != 0 || res.Track.Summary != "" {
			t.Errorf("from %q: unread not cleared: %+v", fromRole, res.Track)
		}
		if len(res.Events) != 1 || res.Events[0] != "Your review submitted (changes requested); baseline updated" {
			t.Errorf("from %q: events = %v", fromRole, res.Events)
		}
	}

	// Approving when PR is approved + CI green + 0 unresolved threads moves directly to Ready.
	approvedGreen := openPR()
	approvedGreen.ReviewDecision = ReviewApproved
	approvedGreen.CI = CISuccess
	res := Evaluate(Input{
		Live:              approvedGreen,
		Track:             Ack(openPR()),
		AutoMove:          true,
		ColumnRole:        RoleInProgress,
		Kind:              KindPR,
		HasActorData:      true,
		MyReviewSubmitted: true,
		MyReviewState:     MyReviewApproved,
	})
	if res.MoveToRole != RoleReady {
		t.Fatalf("approved+green: MoveToRole = %q, want %q", res.MoveToRole, RoleReady)
	}

	// Approving while CI is pending moves to Waiting first, then CI turning green moves to Ready.
	approvedPending := openPR()
	approvedPending.ReviewDecision = ReviewApproved
	approvedPending.CI = CIPending
	res = Evaluate(Input{
		Live:              approvedPending,
		Track:             Ack(openPR()),
		AutoMove:          true,
		ColumnRole:        RoleInProgress,
		Kind:              KindPR,
		HasActorData:      true,
		MyReviewSubmitted: true,
		MyReviewState:     MyReviewApproved,
	})
	if res.MoveToRole != RoleWaiting {
		t.Fatalf("approved+pending: MoveToRole = %q, want %q", res.MoveToRole, RoleWaiting)
	}
	res2 := Evaluate(Input{
		Live:         approvedGreen,
		Track:        res.Track,
		AutoMove:     true,
		ColumnRole:   RoleWaiting,
		Kind:         KindPR,
		HasActorData: true,
	})
	if res2.MoveToRole != RoleReady {
		t.Fatalf("ci finished after approval: MoveToRole = %q, want %q", res2.MoveToRole, RoleReady)
	}

	// AutoMove disabled still re-baselines and clears unread without moving.
	resNoMove := Evaluate(Input{
		Live:              openPR(),
		Track:             Tracking{Ack: openPR(), Prev: openPR(), HasUnread: true, Unread: 1, Cycles: 1},
		AutoMove:          false,
		ColumnRole:        RoleUpdates,
		Kind:              KindPR,
		HasActorData:      true,
		MyReviewSubmitted: true,
		MyReviewState:     MyReviewLGTM,
	})
	if resNoMove.MoveToRole != "" || resNoMove.Track.HasUnread {
		t.Fatalf("AutoMove=false result = %+v", resNoMove)
	}
}

func TestColumnsOutsideTheAutomationFlowAreLeftAlone(t *testing.T) {
	for _, role := range []string{"", RoleDone} {
		tr := newTracker(t, role)
		tr.poll(openPR())
		pushed := openPR()
		pushed.Commits, pushed.HeadSHA = 4, "bbb"
		res := tr.poll(pushed)
		if res.MoveToRole != "" {
			t.Errorf("role %q: card moved to %q", role, res.MoveToRole)
		}
		if !tr.track.HasUnread {
			t.Errorf("role %q: update was not tracked", role)
		}
	}
}

func TestDeletedCommentClearsUnread(t *testing.T) {
	tr := newTracker(t, RoleWaiting)
	tr.poll(openPR())
	commented := openPR()
	commented.Comments++
	tr.poll(commented)
	tr.expect(RoleUpdates, true, 1, 1, "+1 comment")
	tr.poll(openPR())
	tr.expect(RoleUpdates, false, 0, 0, "")
}

func TestIssueUpdates(t *testing.T) {
	issue := Snapshot{State: StateOpen, CI: CIUnknown, Comments: 4, UpdatedAt: "t0"}
	tr := newTracker(t, RoleWaiting)
	tr.poll(issue)
	next := issue
	next.Comments = 6
	tr.poll(next)
	tr.expect(RoleUpdates, true, 2, 1, "+2 comments")

	closed := next
	closed.State = StateClosed
	tr.poll(closed)
	tr.expect(RoleDone, false, 0, 0, "Closed")
}
