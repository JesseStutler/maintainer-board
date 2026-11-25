package github

import (
	"reflect"
	"testing"
)

func review(login, state, at, sha, body string) ghReview {
	rv := ghReview{Author: ghAuthor{Login: login}, State: state, SubmittedAt: at, Body: body}
	rv.Commit.OID = sha
	return rv
}

func comment(login, at, body string) ghComment {
	return ghComment{Author: ghAuthor{Login: login}, CreatedAt: at, Body: body}
}

func TestMyReview(t *testing.T) {
	const me = "JesseStutler"
	const t1, t2, t3, t4 = "2026-11-10T00:00:00Z", "2026-11-11T00:00:00Z", "2026-11-12T00:00:00Z", "2026-11-13T00:00:00Z"
	cases := []struct {
		name     string
		reviews  []ghReview
		comments []ghComment
		want     MyReview
	}{
		{"no activity", nil, nil, MyReview{}},
		{"only other people",
			[]ghReview{review("dafu-wu", "APPROVED", t1, "a1", "")},
			[]ghComment{comment("dafu-wu", t2, "/lgtm")},
			MyReview{}},
		{"plain comments are not a review", nil, []ghComment{comment(me, t1, "Thanks for raising this")}, MyReview{}},
		{"pending draft review is invisible", []ghReview{review(me, "PENDING", t1, "a1", "/lgtm")}, nil, MyReview{}},

		{"approved", []ghReview{review(me, "APPROVED", t1, "a1", "")}, nil, MyReview{MyReviewApproved, t1, "a1"}},
		{"changes requested", []ghReview{review(me, "CHANGES_REQUESTED", t1, "a1", "needs tests")}, nil, MyReview{MyReviewChangesRequested, t1, "a1"}},
		{"commented", []ghReview{review(me, "COMMENTED", t1, "a1", "")}, nil, MyReview{MyReviewCommented, t1, "a1"}},
		{"login case differs", []ghReview{review("jessestutler", "APPROVED", t1, "a1", "")}, nil, MyReview{MyReviewApproved, t1, "a1"}},

		// GitHub keeps an approval in force when the reviewer comments
		// later; the later review only moves the reference commit.
		{"approval survives a later comment review",
			[]ghReview{review(me, "APPROVED", t1, "a1", ""), review(me, "COMMENTED", t2, "b2", "one nit")}, nil,
			MyReview{MyReviewApproved, t2, "b2"}},
		{"latest decisive review wins",
			[]ghReview{review(me, "APPROVED", t1, "a1", ""), review(me, "CHANGES_REQUESTED", t2, "b2", "")}, nil,
			MyReview{MyReviewChangesRequested, t2, "b2"}},
		{"reviews are ordered by time, not by position",
			[]ghReview{review(me, "CHANGES_REQUESTED", t2, "b2", ""), review(me, "APPROVED", t1, "a1", "")}, nil,
			MyReview{MyReviewChangesRequested, t2, "b2"}},
		{"dismissed approval no longer counts",
			[]ghReview{review(me, "DISMISSED", t1, "a1", "")}, nil,
			MyReview{MyReviewCommented, t1, "a1"}},
		{"re-approved after a dismissal",
			[]ghReview{review(me, "DISMISSED", t1, "a1", ""), review(me, "APPROVED", t3, "c3", "")}, nil,
			MyReview{MyReviewApproved, t3, "c3"}},

		// Volcano uses Prow: /lgtm and /approve in a comment are reviews.
		{"prow lgtm comment", nil, []ghComment{comment(me, t2, "Looks good.\n/lgtm")}, MyReview{MyReviewLGTM, t2, ""}},
		{"prow approve comment", nil, []ghComment{comment(me, t2, "/approve")}, MyReview{MyReviewApproved, t2, ""}},
		{"prow lgtm and approve", nil, []ghComment{comment(me, t2, "/lgtm\r\n/approve\r\n")}, MyReview{MyReviewApproved, t2, ""}},
		{"indented command with arguments", nil, []ghComment{comment(me, t2, "  /approve no-issue")}, MyReview{MyReviewApproved, t2, ""}},
		{"lgtm cancelled", nil,
			[]ghComment{comment(me, t1, "/lgtm"), comment(me, t2, "/lgtm cancel")},
			MyReview{MyReviewCommented, t2, ""}},
		{"lgtm removed", nil,
			[]ghComment{comment(me, t1, "/lgtm"), comment(me, t2, "/remove-lgtm")},
			MyReview{MyReviewCommented, t2, ""}},
		{"approve cancelled keeps the lgtm", nil,
			[]ghComment{comment(me, t1, "/lgtm\n/approve"), comment(me, t2, "/approve cancel")},
			MyReview{MyReviewLGTM, t2, ""}},
		{"lgtm again after a cancel", nil,
			[]ghComment{comment(me, t1, "/lgtm"), comment(me, t2, "/lgtm cancel"), comment(me, t3, "/lgtm")},
			MyReview{MyReviewLGTM, t3, ""}},
		{"quoted and inline commands are not commands", nil,
			[]ghComment{comment(me, t1, "> /lgtm\nplease do not /approve yet, and /lgtmish is not a command")},
			MyReview{}},
		{"command in a review body",
			[]ghReview{review(me, "COMMENTED", t2, "b2", "nice\n/lgtm")}, nil,
			MyReview{MyReviewLGTM, t2, "b2"}},
		{"approve after an earlier dismissal",
			[]ghReview{review(me, "DISMISSED", t1, "a1", "")},
			[]ghComment{comment(me, t3, "/approve")},
			MyReview{MyReviewApproved, t3, ""}},
		{"requested changes outranks an older lgtm",
			[]ghReview{review(me, "CHANGES_REQUESTED", t2, "b2", "")},
			[]ghComment{comment(me, t1, "/lgtm")},
			MyReview{MyReviewChangesRequested, t2, "b2"}},
		// The latest activity is the comment, so no reviewed commit is known.
		{"lgtm comment after a review",
			[]ghReview{review(me, "COMMENTED", t1, "a1", "")},
			[]ghComment{comment(me, t4, "/lgtm")},
			MyReview{MyReviewLGTM, t4, ""}},
	}
	for _, tc := range cases {
		if got := myReview(me, tc.reviews, tc.comments); got != tc.want {
			t.Errorf("%s: myReview = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := myReview("", []ghReview{review("", "APPROVED", t1, "a1", "")}, nil); got != (MyReview{}) {
		t.Errorf("unknown user: myReview = %+v, want nothing", got)
	}
}

func TestProwEvents(t *testing.T) {
	cases := map[string][]string{
		"/lgtm":                       {evLGTM},
		"/LGTM":                       {evLGTM},
		"/lgtm cancel":                {evLGTMCancel},
		"/remove-lgtm":                {evLGTMCancel},
		"/approve":                    {evApprove},
		"/approve cancel":             {evApproveCancel},
		"/remove-approve":             {evApproveCancel},
		"/approve no-issue":           {evApprove},
		"text\n/lgtm\n/approve\nmore": {evLGTM, evApprove},
		"/lgtm-ish":                   nil,
		"/lgtm please":                nil,
		"  /lgtm  \r\n/approve\r\n":   {evLGTM, evApprove},
		"/lgtmx":                      nil,
		"see /lgtm":                   nil,
		"> /approve":                  nil,
		"/hold\n/retest\n/ok-to-test": nil,
	}
	for body, want := range cases {
		if got := prowEvents(body); !reflect.DeepEqual(got, want) {
			t.Errorf("prowEvents(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestLastOtherActivitySkipsMeAndBots(t *testing.T) {
	const me = "JesseStutler"
	reviews := []ghReview{
		review("dafu-wu", "COMMENTED", "2026-11-17T05:58:16Z", "", ""),
		review(me, "COMMENTED", "2026-11-24T13:15:12Z", "", ""),
		review("copilot-pull-request-reviewer", "COMMENTED", "2026-11-25T00:00:00Z", "", ""),
		review("mohitvinchoo", "PENDING", "2026-11-26T00:00:00Z", "", ""),
	}
	comments := []ghComment{
		comment("mohitvinchoo", "2026-11-17T08:45:40Z", "PTAL"),
		comment("volcano-sh-bot", "2026-11-24T13:20:00Z", "[APPROVALNOTIFIER]"),
		comment("k8s-ci-robot", "2026-11-24T13:21:00Z", "retest"),
		comment("dependabot[bot]", "2026-11-24T13:22:00Z", "bump"),
		comment("github-actions", "2026-11-24T13:23:00Z", "report"),
		comment(me, "2026-11-24T14:00:00Z", "my own follow-up"),
	}
	if got := lastOtherActivity(me, reviews, comments); got != "2026-11-17T08:45:40Z" {
		t.Errorf("lastOtherActivity = %q, want the author's comment", got)
	}
	if got := lastOtherActivity(me, nil, nil); got != "" {
		t.Errorf("no activity: %q", got)
	}
	for login, want := range map[string]bool{
		"volcano-sh-bot": true, "k8s-ci-robot": true, "renovate[bot]": true, "codecov": true, "Copilot": true, "app/copilot-swe-agent": true,
		"robotics-fan": false, "botanist": false, "copilotuser": false, "JesseStutler": false,
	} {
		if isBot(login) != want {
			t.Errorf("isBot(%q) = %v, want %v", login, !want, want)
		}
	}
}

func TestEvaluateReview(t *testing.T) {
	const (
		reviewed = "2026-11-17T06:00:00Z"
		before   = "2026-11-17T04:54:40Z"
		later    = "2026-11-18T09:00:00Z"
		marked   = "2026-11-19T10:00:00Z"
		latest   = "2026-11-20T11:00:00Z"
	)
	open := Snapshot{State: StateOpen, HeadSHA: "aaa"}
	onHead := MyReview{State: MyReviewApproved, At: reviewed, CommitSHA: "aaa"}
	cases := []struct {
		name string
		in   ReviewInput
		want ReviewStatus
	}{
		{"never reviewed", ReviewInput{Live: open, LastCommitAt: later}, ReviewStatus{}},
		{"reviewed the current head",
			ReviewInput{GitHub: onHead, Live: open, LastCommitAt: before, LastOtherActivityAt: before},
			ReviewStatus{MyReviewApproved, reviewed, false}},

		// New commits after the review: the head is no longer the reviewed one.
		{"new commit after the review",
			ReviewInput{GitHub: onHead, Live: Snapshot{State: StateOpen, HeadSHA: "bbb"}, LastCommitAt: later},
			ReviewStatus{MyReviewApproved, reviewed, true}},
		// A rebase keeps old commit dates but still changes the head.
		{"force-push with old commit dates",
			ReviewInput{GitHub: onHead, Live: Snapshot{State: StateOpen, HeadSHA: "bbb"}, LastCommitAt: before},
			ReviewStatus{MyReviewApproved, reviewed, true}},
		// A Prow /lgtm comment carries no commit, so commit times decide.
		{"commit after an lgtm comment",
			ReviewInput{GitHub: MyReview{State: MyReviewLGTM, At: reviewed}, Live: open, LastCommitAt: later},
			ReviewStatus{MyReviewLGTM, reviewed, true}},
		{"no commit after an lgtm comment",
			ReviewInput{GitHub: MyReview{State: MyReviewLGTM, At: reviewed}, Live: open, LastCommitAt: before},
			ReviewStatus{MyReviewLGTM, reviewed, false}},

		{"author replied after the review",
			ReviewInput{GitHub: onHead, Live: open, LastCommitAt: before, LastOtherActivityAt: later},
			ReviewStatus{MyReviewApproved, reviewed, true}},
		{"changes requested, then a new push",
			ReviewInput{GitHub: MyReview{State: MyReviewChangesRequested, At: reviewed, CommitSHA: "aaa"}, Live: Snapshot{State: StateOpen, HeadSHA: "bbb"}},
			ReviewStatus{MyReviewChangesRequested, reviewed, true}},

		// Finished items are never waiting for another look.
		{"merged after new commits",
			ReviewInput{GitHub: onHead, Live: Snapshot{State: StateMerged, HeadSHA: "bbb"}, LastCommitAt: later, LastOtherActivityAt: later},
			ReviewStatus{MyReviewApproved, reviewed, false}},
		{"closed", ReviewInput{GitHub: onHead, Live: Snapshot{State: StateClosed, HeadSHA: "bbb"}}, ReviewStatus{MyReviewApproved, reviewed, false}},

		// Marked reviewed by hand on the board, nothing on GitHub.
		{"manual mark is preserved",
			ReviewInput{StoredState: MyReviewReviewed, StoredAt: marked, AckHeadSHA: "aaa", Live: open, LastCommitAt: before, LastOtherActivityAt: later},
			ReviewStatus{MyReviewReviewed, marked, false}},
		{"manual mark, then a new commit",
			ReviewInput{StoredState: MyReviewReviewed, StoredAt: marked, AckHeadSHA: "aaa", Live: Snapshot{State: StateOpen, HeadSHA: "bbb"}, LastCommitAt: latest},
			ReviewStatus{MyReviewReviewed, marked, true}},
		{"manual mark on an issue, then a new comment",
			ReviewInput{StoredState: MyReviewReviewed, StoredAt: marked, Live: Snapshot{State: StateOpen}, LastOtherActivityAt: latest},
			ReviewStatus{MyReviewReviewed, marked, true}},

		// Local stored timestamps never override a real GitHub review: if a
		// new commit landed after the GitHub review, it stays stale until a
		// new review is submitted on GitHub.
		{"local timestamp does not mask stale github review",
			ReviewInput{GitHub: onHead, StoredState: MyReviewApproved, StoredAt: marked, AckHeadSHA: "bbb", Live: Snapshot{State: StateOpen, HeadSHA: "bbb"}, LastCommitAt: later, LastOtherActivityAt: later},
			ReviewStatus{MyReviewApproved, reviewed, true}},
		// A formal review after a manual mark replaces the mark.
		{"github review after a manual mark",
			ReviewInput{GitHub: MyReview{State: MyReviewCommented, At: latest, CommitSHA: "aaa"}, StoredState: MyReviewReviewed, StoredAt: marked, AckHeadSHA: "old", Live: open},
			ReviewStatus{MyReviewCommented, latest, false}},
		// A review that disappeared from GitHub leaves nothing behind.
		{"stored github state without a github review",
			ReviewInput{StoredState: MyReviewApproved, StoredAt: reviewed, Live: open},
			ReviewStatus{}},

		{"own pull request",
			ReviewInput{Own: true, GitHub: onHead, StoredState: MyReviewReviewed, StoredAt: marked, Live: open},
			ReviewStatus{}},
	}
	for _, tc := range cases {
		if got := EvaluateReview(tc.in); got != tc.want {
			t.Errorf("%s: EvaluateReview = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestAfter(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2026-11-17T06:00:00Z", "2026-11-17T05:00:00Z", true},
		{"2026-11-17T05:00:00Z", "2026-11-17T06:00:00Z", false},
		{"2026-11-17T06:00:00Z", "2026-11-17T06:00:00Z", false},
		// The same instant in another zone is not later.
		{"2026-11-17T08:00:00+02:00", "2026-11-17T06:00:00Z", false},
		{"2026-11-17T06:00:00Z", "", true},
		{"", "2026-11-17T06:00:00Z", false},
		{"", "", false},
		{"not a time", "2026-11-17T06:00:00Z", false},
	}
	for _, tc := range cases {
		if got := After(tc.a, tc.b); got != tc.want {
			t.Errorf("After(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
