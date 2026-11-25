package github

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// Personal review states: where the maintainer running the board stands on
// a pull request or issue, as opposed to the overall review decision.
const (
	MyReviewApproved         = "APPROVED"
	MyReviewLGTM             = "LGTM"
	MyReviewChangesRequested = "CHANGES_REQUESTED"
	MyReviewCommented        = "COMMENTED"
	// MyReviewReviewed is recorded by the board when the maintainer marks an
	// item reviewed by hand; GitHub never reports it.
	MyReviewReviewed = "REVIEWED"
)

// MyReview is what GitHub knows about the current user's own review.
type MyReview struct {
	// State is empty when the user left no review, or one of the MyReview
	// constants except MyReviewReviewed.
	State string
	// At is the time of the user's latest review activity (RFC 3339).
	At string
	// CommitSHA is the head the latest review was submitted against. It is
	// empty when the latest activity was a Prow command in a comment.
	CommitSHA string
}

type ghReview struct {
	Author      ghAuthor `json:"author"`
	State       string   `json:"state"`
	Body        string   `json:"body"`
	SubmittedAt string   `json:"submittedAt"`
	Commit      struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

type ghComment struct {
	Author    ghAuthor `json:"author"`
	Body      string   `json:"body"`
	CreatedAt string   `json:"createdAt"`
}

type ghCommit struct {
	OID           string `json:"oid"`
	CommittedDate string `json:"committedDate"`
}

func sameUser(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}

// botLogin matches accounts whose comments are automation, not a reply the
// maintainer has to read again: Prow robots, GitHub Apps (gh reports some of
// them as "app/name"), CI reporters.
var botLogin = regexp.MustCompile(`(?i)(\[bot\]|-bot|-robot)$|^app/|^(github-actions|dependabot|renovate|codecov|codecov-commenter|copilot|copilot-pull-request-reviewer|copilot-swe-agent)$`)

func isBot(login string) bool { return botLogin.MatchString(login) }

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// After reports whether RFC 3339 time a is strictly later than b. An
// unparseable a is never later; an unparseable b is earlier than any time.
func After(a, b string) bool {
	ta, ok := parseTime(a)
	if !ok {
		return false
	}
	tb, ok := parseTime(b)
	return !ok || ta.After(tb)
}

// prowCommand matches the Prow review commands. Like Prow, it only accepts a
// command that stands alone on its line.
var prowCommand = regexp.MustCompile(`(?im)^[ \t]*/(lgtm|approve|remove-lgtm|remove-approve)(?:[ \t]+(cancel|no-issue))?[ \t]*\r?$`)

const (
	evLGTM          = "prow:lgtm"
	evLGTMCancel    = "prow:lgtm-cancel"
	evApprove       = "prow:approve"
	evApproveCancel = "prow:approve-cancel"
)

// prowEvents returns the Prow review commands found in a comment body, in
// the order they appear.
func prowEvents(body string) []string {
	var out []string
	for _, m := range prowCommand.FindAllStringSubmatch(body, -1) {
		cancel := strings.HasPrefix(strings.ToLower(m[1]), "remove-") || strings.EqualFold(m[2], "cancel")
		switch name := strings.TrimPrefix(strings.ToLower(m[1]), "remove-"); {
		case name == "lgtm" && cancel:
			out = append(out, evLGTMCancel)
		case name == "lgtm":
			out = append(out, evLGTM)
		case cancel:
			out = append(out, evApproveCancel)
		default:
			out = append(out, evApprove)
		}
	}
	return out
}

type reviewEvent struct {
	at   time.Time
	raw  string
	kind string
	sha  string
}

// myReview folds the user's submitted reviews and Prow commands into one
// state. A GitHub review that requests changes or approves stays in force
// until a later one replaces it or it is dismissed, exactly as GitHub counts
// it; /lgtm and /approve stay in force until cancelled. A plain review
// comment never cancels an earlier approval.
func myReview(user string, reviews []ghReview, comments []ghComment) MyReview {
	if user == "" {
		return MyReview{}
	}
	var events []reviewEvent
	for _, rv := range reviews {
		state := strings.ToUpper(rv.State)
		at, ok := parseTime(rv.SubmittedAt)
		if !ok || state == "PENDING" || !sameUser(rv.Author.Login, user) {
			continue
		}
		events = append(events, reviewEvent{at: at, raw: rv.SubmittedAt, kind: state, sha: rv.Commit.OID})
		for _, kind := range prowEvents(rv.Body) {
			events = append(events, reviewEvent{at: at, raw: rv.SubmittedAt, kind: kind, sha: rv.Commit.OID})
		}
	}
	for _, cm := range comments {
		at, ok := parseTime(cm.CreatedAt)
		if !ok || !sameUser(cm.Author.Login, user) {
			continue
		}
		for _, kind := range prowEvents(cm.Body) {
			events = append(events, reviewEvent{at: at, raw: cm.CreatedAt, kind: kind})
		}
	}
	if len(events) == 0 {
		return MyReview{}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })

	decision := ""
	lgtm, approve := false, false
	for _, ev := range events {
		switch ev.kind {
		case ReviewApproved, ReviewChangesRequested:
			decision = ev.kind
		case "DISMISSED":
			decision = ""
		case evLGTM:
			lgtm = true
		case evLGTMCancel:
			lgtm = false
		case evApprove:
			approve = true
		case evApproveCancel:
			approve = false
		}
	}
	last := events[len(events)-1]
	out := MyReview{At: last.raw, CommitSHA: last.sha}
	switch {
	case decision == ReviewChangesRequested:
		out.State = MyReviewChangesRequested
	case decision == ReviewApproved || approve:
		out.State = MyReviewApproved
	case lgtm:
		out.State = MyReviewLGTM
	default:
		out.State = MyReviewCommented
	}
	return out
}

// lastOtherActivity returns the time of the newest comment or submitted
// review written by a person other than user, or "" when there is none.
func lastOtherActivity(user string, reviews []ghReview, comments []ghComment) string {
	var latest time.Time
	raw := ""
	consider := func(login, at string) {
		if sameUser(login, user) || isBot(login) {
			return
		}
		if t, ok := parseTime(at); ok && t.After(latest) {
			latest, raw = t, at
		}
	}
	for _, rv := range reviews {
		if strings.ToUpper(rv.State) != "PENDING" {
			consider(rv.Author.Login, rv.SubmittedAt)
		}
	}
	for _, cm := range comments {
		consider(cm.Author.Login, cm.CreatedAt)
	}
	return raw
}

func lastCommitTime(commits []ghCommit) string {
	var latest time.Time
	raw := ""
	for _, c := range commits {
		if t, ok := parseTime(c.CommittedDate); ok && t.After(latest) {
			latest, raw = t, c.CommittedDate
		}
	}
	return raw
}

// ReviewInput is everything that decides the personal review status of a
// card after a sync.
type ReviewInput struct {
	// GitHub is the user's review as GitHub reports it.
	GitHub MyReview
	// StoredState and StoredAt are the values persisted on the card. They
	// carry a review the maintainer marked by hand on the board.
	StoredState string
	StoredAt    string
	// AckHeadSHA is the head commit recorded when the maintainer last marked
	// the card caught up.
	AckHeadSHA string
	// Own is true when the current user wrote the pull request or issue;
	// nobody reviews their own work.
	Own bool
	// Live is the current state of the item.
	Live Snapshot
	// LastCommitAt and LastOtherActivityAt come from the fetched item.
	LastCommitAt        string
	LastOtherActivityAt string
}

// ReviewStatus is the personal review status stored on a card.
type ReviewStatus struct {
	State string
	At    string
	// Stale is true when the item changed after the review: a commit was
	// pushed, or somebody else commented or reviewed.
	Stale bool
}

// EvaluateReview merges the review GitHub reports with the mark the
// maintainer set by hand, and decides whether that review is out of date.
//
// A review on GitHub always names the state. Marking the card reviewed on
// the board afterwards only moves the reference point forward: it means
// "I have seen everything up to now". Without a review on GitHub a manual
// mark is kept as REVIEWED.
//
// The review is stale when the head commit moved past the reviewed one or
// when someone other than the user (bots excluded) commented or reviewed
// later. The user's own later comments and CI results never make it stale.
func EvaluateReview(in ReviewInput) ReviewStatus {
	if in.Own {
		return ReviewStatus{}
	}
	state, at, head := in.GitHub.State, in.GitHub.At, in.GitHub.CommitSHA
	switch {
	case state == "" && in.StoredState == MyReviewReviewed:
		state, at, head = MyReviewReviewed, in.StoredAt, in.AckHeadSHA
	case state == "":
		return ReviewStatus{}
	case After(in.StoredAt, at):
		// Marked caught up on the board after the review on GitHub.
		at, head = in.StoredAt, in.AckHeadSHA
	}
	out := ReviewStatus{State: state, At: at}
	if IsTerminal(in.Live.State) {
		return out
	}
	pushed := After(in.LastCommitAt, at)
	if head != "" && in.Live.HeadSHA != "" {
		// The reviewed commit is known: any other head means new commits or
		// a force-push, whatever the commit dates say.
		pushed = head != in.Live.HeadSHA
	}
	out.Stale = pushed || After(in.LastOtherActivityAt, at)
	return out
}
