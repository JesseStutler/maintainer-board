package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fakeGH answers gh invocations from canned responses keyed by a prefix of
// the joined argument list, and records every call.
type fakeGH struct {
	responses map[string]string
	errs      map[string]error
	calls     []string
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	for prefix, err := range f.errs {
		if strings.HasPrefix(joined, prefix) {
			return nil, err
		}
	}
	for prefix, out := range f.responses {
		if strings.HasPrefix(joined, prefix) {
			return []byte(out), nil
		}
	}
	return nil, fmt.Errorf("unexpected gh call: %s", joined)
}

func (f *fakeGH) called(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func newFakeClient(f *fakeGH) *Client {
	return &Client{Run: f.run, IgnoreContexts: map[string]bool{"tide": true}, ProwLabels: true}
}

const prViewJSON = `{
  "number": 6247, "state": "OPEN", "isDraft": false, "reviewDecision": "REVIEW_REQUIRED",
  "title": "fix(scheduler): enforce CSI volume attach limits",
  "url": "https://github.com/volcano-sh/volcano/pull/6247",
  "author": {"login": "mohitvinchoo"},
  "headRefOid": "c87a817ab8563627f4ccc8ad0d36e3e2b6e65b8d",
  "updatedAt": "2026-11-17T08:45:40Z",
  "labels": [{"name": "kind/bug"}, {"name": "size/XL"}],
  "commits": [{"oid": "c87a817"}],
  "comments": [{"id": "1"}, {"id": "2"}],
  "reviews": [
    {"state": "PENDING", "body": "draft that only its author sees"},
    {"state": "COMMENTED", "body": "please add an e2e test"},
    {"state": "COMMENTED", "body": "  "},
    {"state": "APPROVED", "body": ""}
  ],
  "statusCheckRollup": [
    {"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS"},
    {"__typename": "CheckRun", "name": "fossa", "status": "COMPLETED", "conclusion": "SKIPPED"},
    {"__typename": "StatusContext", "context": "tide", "state": "PENDING"}
  ]
}`

const prRestJSON = `{"number": 6247, "commits": 1, "comments": 7, "review_comments": 3, "merged": false}`

func TestFetchPullRequest(t *testing.T) {
	f := &fakeGH{responses: map[string]string{
		"api repos/volcano-sh/volcano/issues/6247": `{"number": 6247, "pull_request": {"url": "x"}}`,
		"pr view 6247 --repo volcano-sh/volcano":   prViewJSON,
		"api repos/volcano-sh/volcano/pulls/6247":  prRestJSON,
	}}
	item, err := newFakeClient(f).Fetch(context.Background(), "volcano-sh/volcano", 6247, "")
	if err != nil {
		t.Fatal(err)
	}
	want := &Item{
		Repo: "volcano-sh/volcano", Number: 6247, Kind: KindPR,
		Title:  "fix(scheduler): enforce CSI volume attach limits",
		URL:    "https://github.com/volcano-sh/volcano/pull/6247",
		Author: "mohitvinchoo",
		Labels: []string{"kind/bug", "size/XL"},
		Snapshot: Snapshot{
			State: StateOpen, CI: CISuccess, ReviewDecision: ReviewRequired,
			HeadSHA: "c87a817ab8563627f4ccc8ad0d36e3e2b6e65b8d",
			// 7 conversation comments from REST; 3 inline review comments
			// plus the one submitted review that has a body.
			Commits: 1, Comments: 7, ReviewComments: 4,
			UpdatedAt: "2026-11-17T08:45:40Z",
		},
		UnresolvedThreads: -1,
	}
	if !reflect.DeepEqual(item, want) {
		t.Fatalf("item:\n got %+v\nwant %+v", item, want)
	}
	if f.called("api graphql") != 0 {
		t.Fatalf("review threads were queried for a PR that is not approved: %v", f.calls)
	}

	// A known kind skips the detection call.
	f.calls = nil
	if _, err := newFakeClient(f).Fetch(context.Background(), "volcano-sh/volcano", 6247, KindPR); err != nil {
		t.Fatal(err)
	}
	if f.called("api repos/volcano-sh/volcano/issues/") != 0 {
		t.Fatalf("kind was detected again: %v", f.calls)
	}
}

func TestFetchPullRequestStates(t *testing.T) {
	cases := []struct {
		name       string
		view, rest string
		want       string
	}{
		{"draft", `"state": "OPEN", "isDraft": true`, `"merged": false`, StateDraft},
		{"merged", `"state": "MERGED", "isDraft": false`, `"merged": true`, StateMerged},
		{"merged per REST only", `"state": "CLOSED", "isDraft": false`, `"merged": true`, StateMerged},
		{"closed", `"state": "CLOSED", "isDraft": true`, `"merged": false`, StateClosed},
	}
	for _, tc := range cases {
		f := &fakeGH{responses: map[string]string{
			"pr view":   `{"number": 1, ` + tc.view + `}`,
			"api repos": `{"number": 1, ` + tc.rest + `}`,
		}}
		item, err := newFakeClient(f).Fetch(context.Background(), "a/b", 1, KindPR)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if item.Snapshot.State != tc.want {
			t.Errorf("%s: state = %s, want %s", tc.name, item.Snapshot.State, tc.want)
		}
	}
}

func TestProwLabelsCountAsApproval(t *testing.T) {
	view := func(decision, labels string) string {
		return `{"number": 1, "state": "OPEN", "reviewDecision": "` + decision + `", "labels": [` + labels + `],
			"statusCheckRollup": [{"__typename": "CheckRun", "name": "e2e", "status": "COMPLETED", "conclusion": "SUCCESS"}]}`
	}
	const both = `{"name": "lgtm"}, {"name": "approved"}`
	cases := []struct {
		name, decision, labels string
		prow                   bool
		want                   string
		wantThreads            int
	}{
		{"lgtm and approved", "REVIEW_REQUIRED", both, true, ReviewApproved, 1},
		{"lgtm only", "REVIEW_REQUIRED", `{"name": "lgtm"}`, true, ReviewRequired, -1},
		{"labels ignored when disabled", "REVIEW_REQUIRED", both, false, ReviewRequired, -1},
		{"changes requested wins", "CHANGES_REQUESTED", both, true, ReviewChangesRequested, -1},
	}
	threads := `{"data": {"repository": {"pullRequest": {"reviewThreads": {"nodes": [
		{"isResolved": false, "isOutdated": false},
		{"isResolved": true, "isOutdated": false},
		{"isResolved": false, "isOutdated": true}]}}}}}`
	for _, tc := range cases {
		f := &fakeGH{responses: map[string]string{
			"pr view":     view(tc.decision, tc.labels),
			"api repos":   `{"number": 1}`,
			"api graphql": threads,
		}}
		c := newFakeClient(f)
		c.ProwLabels = tc.prow
		item, err := c.Fetch(context.Background(), "a/b", 1, KindPR)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if item.Snapshot.ReviewDecision != tc.want {
			t.Errorf("%s: decision = %q, want %q", tc.name, item.Snapshot.ReviewDecision, tc.want)
		}
		// Review threads are only looked up for a PR that would otherwise
		// qualify for Ready; resolved and outdated threads do not count.
		if item.UnresolvedThreads != tc.wantThreads {
			t.Errorf("%s: unresolved threads = %d, want %d", tc.name, item.UnresolvedThreads, tc.wantThreads)
		}
	}
}

func TestFetchIssue(t *testing.T) {
	f := &fakeGH{responses: map[string]string{
		"api repos/volcano-sh/volcano/issues/6100": `{"number": 6100}`,
		"issue view 6100 --repo volcano-sh/volcano": `{"number": 6100, "state": "CLOSED", "title": "scheduler panics",
			"url": "https://github.com/volcano-sh/volcano/issues/6100", "author": {"login": "reporter"},
			"comments": [{}, {}, {}], "updatedAt": "2026-11-01T00:00:00Z", "labels": [{"name": "kind/bug"}]}`,
	}}
	item, err := newFakeClient(f).Fetch(context.Background(), "volcano-sh/volcano", 6100, "")
	if err != nil {
		t.Fatal(err)
	}
	want := &Item{
		Repo: "volcano-sh/volcano", Number: 6100, Kind: KindIssue, Title: "scheduler panics",
		URL: "https://github.com/volcano-sh/volcano/issues/6100", Author: "reporter", Labels: []string{"kind/bug"},
		Snapshot:          Snapshot{State: StateClosed, CI: CIUnknown, Comments: 3, UpdatedAt: "2026-11-01T00:00:00Z"},
		UnresolvedThreads: -1,
	}
	if !reflect.DeepEqual(item, want) {
		t.Fatalf("item:\n got %+v\nwant %+v", item, want)
	}
	if f.called("pr view") != 0 {
		t.Fatalf("an issue was fetched as a pull request: %v", f.calls)
	}
}

func TestFetchErrors(t *testing.T) {
	notFound := fmt.Errorf("%w: gh: Not Found (HTTP 404)", ErrNotFound)
	offline := fmt.Errorf("%w: dial tcp: no route to host", ErrUnavailable)

	f := &fakeGH{errs: map[string]error{"api repos/a/b/issues/9": notFound}}
	if _, err := newFakeClient(f).Fetch(context.Background(), "a/b", 9, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing item: err = %v, want ErrNotFound", err)
	}

	// The REST payload carries the exact counts; without it the fetch fails
	// rather than reporting counts that would later look like new activity.
	f = &fakeGH{
		responses: map[string]string{"pr view": prViewJSON},
		errs:      map[string]error{"api repos/a/b/pulls/9": offline},
	}
	if _, err := newFakeClient(f).Fetch(context.Background(), "a/b", 9, KindPR); !errors.Is(err, ErrUnavailable) {
		t.Errorf("partial failure: err = %v, want ErrUnavailable", err)
	}

	f = &fakeGH{}
	for _, bad := range []struct {
		repo   string
		number int
	}{{"not a repo", 1}, {"a/b", 0}, {"--flag/x", 3}} {
		if _, err := newFakeClient(f).Fetch(context.Background(), bad.repo, bad.number, ""); !errors.Is(err, ErrBadRef) {
			t.Errorf("Fetch(%q, %d): err = %v, want ErrBadRef", bad.repo, bad.number, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("gh was invoked for invalid references: %v", f.calls)
	}

	f = &fakeGH{responses: map[string]string{"pr view": `not json`, "api repos": prRestJSON}}
	if _, err := newFakeClient(f).Fetch(context.Background(), "a/b", 9, KindPR); err == nil {
		t.Error("malformed gh output was accepted")
	}
}

func TestRollupCI(t *testing.T) {
	run := func(status, conclusion string) ghCheck {
		return ghCheck{TypeName: "CheckRun", Name: "job", Status: status, Conclusion: conclusion}
	}
	ctxState := func(name, state string) ghCheck {
		return ghCheck{TypeName: "StatusContext", Context: name, State: state}
	}
	ignore := map[string]bool{"tide": true}
	cases := []struct {
		name   string
		checks []ghCheck
		want   string
	}{
		{"no checks", nil, CIUnknown},
		{"only ignored", []ghCheck{ctxState("tide", "PENDING")}, CIUnknown},
		{"all green", []ghCheck{run("COMPLETED", "SUCCESS"), run("COMPLETED", "NEUTRAL"), run("COMPLETED", "SKIPPED"), ctxState("dco", "SUCCESS")}, CISuccess},
		{"green with ignored pending", []ghCheck{run("COMPLETED", "SUCCESS"), ctxState("tide", "PENDING")}, CISuccess},
		{"in progress", []ghCheck{run("COMPLETED", "SUCCESS"), run("IN_PROGRESS", "")}, CIPending},
		{"queued", []ghCheck{run("QUEUED", "")}, CIPending},
		{"pending context", []ghCheck{ctxState("ci/x", "PENDING")}, CIPending},
		{"expected context", []ghCheck{ctxState("ci/x", "EXPECTED")}, CIPending},
		{"failure beats pending", []ghCheck{run("IN_PROGRESS", ""), run("COMPLETED", "FAILURE")}, CIFailure},
		{"timed out", []ghCheck{run("COMPLETED", "TIMED_OUT")}, CIFailure},
		{"cancelled", []ghCheck{run("COMPLETED", "CANCELLED")}, CIFailure},
		{"error context", []ghCheck{ctxState("ci/x", "ERROR")}, CIFailure},
		{"ignored failure", []ghCheck{ctxState("tide", "FAILURE"), run("COMPLETED", "SUCCESS")}, CISuccess},
	}
	for _, tc := range cases {
		if got := RollupCI(tc.checks, ignore); got != tc.want {
			t.Errorf("%s: RollupCI = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestDetectUser(t *testing.T) {
	f := &fakeGH{responses: map[string]string{"api user --jq .login": "JesseStutler\n"}}
	c := newFakeClient(f)
	login, err := c.DetectUser(context.Background())
	if err != nil || login != "JesseStutler" || c.User() != "JesseStutler" {
		t.Fatalf("DetectUser = %q, %v; User() = %q", login, err, c.User())
	}

	// Offline, logged out or garbage output leaves the user unknown.
	for name, f := range map[string]*fakeGH{
		"offline": {errs: map[string]error{"api user": fmt.Errorf("%w: no route to host", ErrUnavailable)}},
		"empty":   {responses: map[string]string{"api user": "\n"}},
		"garbage": {responses: map[string]string{"api user": `{"message": "Bad credentials"}`}},
	} {
		c := newFakeClient(f)
		if login, err := c.DetectUser(context.Background()); err == nil || login != "" || c.User() != "" {
			t.Errorf("%s: DetectUser = %q, %v; User() = %q", name, login, err, c.User())
		}
	}

	c.SetUser(" @Someone ")
	if c.User() != "Someone" {
		t.Errorf("SetUser did not normalize the login: %q", c.User())
	}
}

// reviewedPRJSON is a pull request the current user reviewed, modelled on
// volcano-sh/volcano#6247: two reviews by another member, plain comments by
// the user, a Prow notifier comment and one review by the user on the head.
const reviewedPRJSON = `{
  "number": 6247, "state": "OPEN", "isDraft": false, "reviewDecision": "REVIEW_REQUIRED",
  "title": "fix(scheduler): enforce CSI volume attach limits",
  "url": "https://github.com/volcano-sh/volcano/pull/6247",
  "author": {"login": "mohitvinchoo"},
  "headRefOid": "c87a817a",
  "commits": [
    {"oid": "91d283c2", "committedDate": "2026-11-11T22:40:00Z"},
    {"oid": "c87a817a", "committedDate": "2026-11-17T04:54:40Z"}
  ],
  "comments": [
    {"author": {"login": "volcano-sh-bot"}, "createdAt": "2026-11-24T13:20:00Z", "body": "[APPROVALNOTIFIER] This PR is **NOT APPROVED**"},
    {"author": {"login": "JesseStutler"}, "createdAt": "2026-11-11T07:19:24Z", "body": "Thanks for raising this"},
    {"author": {"login": "mohitvinchoo"}, "createdAt": "2026-11-17T08:45:40Z", "body": "> /lgtm\nDone, PTAL"}
  ],
  "reviews": [
    {"author": {"login": "dafu-wu"}, "state": "COMMENTED", "submittedAt": "2026-11-17T05:58:16Z", "body": "Thanks for the quick turnaround", "commit": {"oid": "c87a817a"}},
    {"author": {"login": "jessestutler"}, "state": "COMMENTED", "submittedAt": "2026-11-24T13:15:12Z", "body": "", "commit": {"oid": "c87a817a"}}
  ],
  "statusCheckRollup": []
}`

func TestFetchReportsMyReview(t *testing.T) {
	fetch := func(user, view string) *Item {
		t.Helper()
		f := &fakeGH{responses: map[string]string{"pr view": view, "api repos": `{"number": 6247, "commits": 2}`}}
		c := newFakeClient(f)
		c.SetUser(user)
		item, err := c.Fetch(context.Background(), "volcano-sh/volcano", 6247, KindPR)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}

	// The login comparison ignores case; the bot comment that came after the
	// review is not somebody else's activity.
	item := fetch("JesseStutler", reviewedPRJSON)
	want := MyReview{State: MyReviewCommented, At: "2026-11-24T13:15:12Z", CommitSHA: "c87a817a"}
	if item.MyReview != want {
		t.Errorf("MyReview = %+v, want %+v", item.MyReview, want)
	}
	if item.LastCommitAt != "2026-11-17T04:54:40Z" || item.LastOtherActivityAt != "2026-11-17T08:45:40Z" {
		t.Errorf("LastCommitAt = %q, LastOtherActivityAt = %q", item.LastCommitAt, item.LastOtherActivityAt)
	}

	// Without a known user nothing is attributed, and everybody is "other".
	if item := fetch("", reviewedPRJSON); item.MyReview != (MyReview{}) || item.LastOtherActivityAt != "2026-11-24T13:15:12Z" {
		t.Errorf("unknown user: MyReview = %+v, LastOtherActivityAt = %q", item.MyReview, item.LastOtherActivityAt)
	}

	// The author's own review comments are replies, not a review.
	if item := fetch("mohitvinchoo", reviewedPRJSON); item.MyReview != (MyReview{}) {
		t.Errorf("author's own pull request: MyReview = %+v", item.MyReview)
	}

	issue := &fakeGH{responses: map[string]string{"issue view": `{"number": 7, "state": "OPEN", "author": {"login": "reporter"},
		"comments": [{"author": {"login": "JesseStutler"}, "createdAt": "2026-11-02T00:00:00Z", "body": "/lgtm"},
		             {"author": {"login": "reporter"}, "createdAt": "2026-11-03T00:00:00Z", "body": "more details"}]}`}}
	c := newFakeClient(issue)
	c.SetUser("JesseStutler")
	got, err := c.Fetch(context.Background(), "a/b", 7, KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	if got.MyReview != (MyReview{}) || got.LastOtherActivityAt != "2026-11-03T00:00:00Z" {
		t.Errorf("issue: MyReview = %+v, LastOtherActivityAt = %q", got.MyReview, got.LastOtherActivityAt)
	}
}

func TestDescription(t *testing.T) {
	f := &fakeGH{responses: map[string]string{
		"api repos/volcano-sh/volcano/issues/6247": `{"number": 6247, "pull_request": {"url": "x"}, "body": "\n## What this PR does\n\nRemoves the scalar CSI counting.\n"}`,
		"api repos/volcano-sh/volcano/issues/6100": `{"number": 6100, "body": null}`,
	}, errs: map[string]error{"api repos/volcano-sh/volcano/issues/9999": fmt.Errorf("%w: HTTP 404", ErrNotFound)}}
	c := newFakeClient(f)
	// The issues endpoint serves pull requests and issues alike.
	body, err := c.Description(context.Background(), "volcano-sh/volcano", 6247)
	if err != nil || body != "## What this PR does\n\nRemoves the scalar CSI counting." {
		t.Fatalf("Description = %q, %v", body, err)
	}
	if body, err := c.Description(context.Background(), "volcano-sh/volcano", 6100); err != nil || body != "" {
		t.Fatalf("an item without a body = %q, %v", body, err)
	}
	if _, err := c.Description(context.Background(), "volcano-sh/volcano", 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown item: %v", err)
	}
	if _, err := c.Description(context.Background(), "not a repo", 1); !errors.Is(err, ErrBadRef) {
		t.Fatalf("bad reference: %v", err)
	}
	if f.called("api") != 3 {
		t.Fatalf("gh calls = %v", f.calls)
	}
}

func TestParseReviewComments(t *testing.T) {
	single := `[{"user":{"login":"alice"},"created_at":"2026-11-25T08:00:00Z"}]`
	if got := parseReviewComments([]byte(single)); len(got) != 1 || got[0].User.Login != "alice" {
		t.Fatalf("single page = %+v", got)
	}
	// `gh api --paginate` writes one JSON array per page back-to-back.
	paginated := `[{"user":{"login":"alice"},"created_at":"2026-11-25T08:00:00Z"}][{"user":{"login":"bob"},"created_at":"2026-11-25T09:00:00Z"}]`
	if got := parseReviewComments([]byte(paginated)); len(got) != 2 || got[0].User.Login != "alice" || got[1].User.Login != "bob" {
		t.Fatalf("concatenated pages = %+v", got)
	}
	slurped := `[[{"user":{"login":"alice"},"created_at":"2026-11-25T08:00:00Z"}],[{"user":{"login":"bob"},"created_at":"2026-11-25T09:00:00Z"}]]`
	if got := parseReviewComments([]byte(slurped)); len(got) != 2 || got[1].User.Login != "bob" {
		t.Fatalf("slurped pages = %+v", got)
	}
}
