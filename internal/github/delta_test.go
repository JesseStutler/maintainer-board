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
