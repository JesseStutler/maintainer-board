package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrNotFound means the repository, pull request or issue does not exist
	// or is not visible to the authenticated gh account.
	ErrNotFound = errors.New("not found on GitHub")
	// ErrUnavailable means gh could not be executed or GitHub was unreachable.
	ErrUnavailable = errors.New("GitHub unavailable")
)

// Item is the live state of a pull request or issue.
type Item struct {
	Repo     string
	Number   int
	Kind     string
	Title    string
	URL      string
	Author   string
	Labels   []string
	Snapshot Snapshot
	// UnresolvedThreads is the number of unresolved review threads, or -1
	// when it was not queried.
	UnresolvedThreads int
}

// Runner executes gh with args and returns its standard output.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

type Client struct {
	Run Runner
	// Timeout bounds every single gh invocation.
	Timeout time.Duration
	// IgnoreContexts lists status contexts and check names excluded from the
	// CI rollup, such as Prow's always-pending "tide" context.
	IgnoreContexts map[string]bool
	// ProwLabels treats the "lgtm" plus "approved" label pair as an approval.
	ProwLabels bool
}

// NewClient returns a client that shells out to the gh binary.
func NewClient(ghPath string) *Client {
	if ghPath == "" {
		ghPath = "gh"
	}
	return &Client{
		Run:            execRunner(ghPath),
		Timeout:        30 * time.Second,
		IgnoreContexts: map[string]bool{"tide": true},
		ProwLabels:     true,
	}
}

func execRunner(ghPath string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, ghPath, args...)
		cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "CLICOLOR=0")
		cmd.WaitDelay = 2 * time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil {
			return stdout.Bytes(), nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: gh timed out", ErrUnavailable)
		}
		msg := strings.TrimSpace(stderr.String())
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%w: cannot run gh: %v", ErrUnavailable, err)
		}
		lower := strings.ToLower(msg + " " + stdout.String())
		if strings.Contains(lower, "http 404") || strings.Contains(lower, "could not resolve to a") {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, firstLine(msg))
		}
		return nil, fmt.Errorf("%w: gh %s: %s", ErrUnavailable, strings.Join(args[:min(2, len(args))], " "), firstLine(msg))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.Run(ctx, args...)
}

const prViewFields = "number,state,isDraft,reviewDecision,commits,comments,reviews,statusCheckRollup,headRefOid,updatedAt,title,url,author,labels"
const issueViewFields = "number,state,title,url,author,comments,updatedAt,labels"

// DetectKind asks the REST issues endpoint whether number is a pull request
// or an issue.
func (c *Client) DetectKind(ctx context.Context, repo string, number int) (string, error) {
	out, err := c.run(ctx, "api", fmt.Sprintf("repos/%s/issues/%d", repo, number))
	if err != nil {
		return "", err
	}
	return parseKind(out)
}

func parseKind(data []byte) (string, error) {
	var v struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("decode issue lookup: %w", err)
	}
	if v.Number == 0 {
		return "", ErrNotFound
	}
	if len(v.PullRequest) > 0 && string(v.PullRequest) != "null" {
		return KindPR, nil
	}
	return KindIssue, nil
}

// Fetch returns the live state of repo#number. knownKind is the kind stored
// from an earlier fetch; when empty the kind is detected first.
func (c *Client) Fetch(ctx context.Context, repo string, number int, knownKind string) (*Item, error) {
	if !ValidRepo(repo) || number <= 0 {
		return nil, fmt.Errorf("%w: %s#%d", ErrBadRef, repo, number)
	}
	kind := knownKind
	if kind != KindPR && kind != KindIssue {
		detected, err := c.DetectKind(ctx, repo, number)
		if err != nil {
			return nil, err
		}
		kind = detected
	}
	num := strconv.Itoa(number)
	if kind == KindIssue {
		out, err := c.run(ctx, "issue", "view", num, "--repo", repo, "--json", issueViewFields)
		if err != nil {
			return nil, err
		}
		item, err := parseIssueView(out)
		if err != nil {
			return nil, err
		}
		item.Repo, item.Number = repo, number
		return item, nil
	}

	view, err := c.run(ctx, "pr", "view", num, "--repo", repo, "--json", prViewFields)
	if err != nil {
		return nil, err
	}
	rest, err := c.run(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, number))
	if err != nil {
		return nil, err
	}
	item, err := c.parsePR(view, rest)
	if err != nil {
		return nil, err
	}
	item.Repo, item.Number = repo, number
	s := item.Snapshot
	if s.State == StateOpen && s.ReviewDecision == ReviewApproved && s.CI == CISuccess {
		// Only a PR that otherwise qualifies for the ready column needs the
		// extra query; a failure leaves the count unknown.
		if n, err := c.unresolvedThreads(ctx, repo, number); err == nil {
			item.UnresolvedThreads = n
		}
	}
	return item, nil
}

type ghAuthor struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghCheck struct {
	TypeName   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

type ghPRView struct {
	Number         int               `json:"number"`
	State          string            `json:"state"`
	IsDraft        bool              `json:"isDraft"`
	ReviewDecision string            `json:"reviewDecision"`
	Commits        []json.RawMessage `json:"commits"`
	Comments       []json.RawMessage `json:"comments"`
	Reviews        []struct {
		State string `json:"state"`
		Body  string `json:"body"`
	} `json:"reviews"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
	HeadRefOid        string    `json:"headRefOid"`
	UpdatedAt         string    `json:"updatedAt"`
	Title             string    `json:"title"`
	URL               string    `json:"url"`
	Author            ghAuthor  `json:"author"`
	Labels            []ghLabel `json:"labels"`
}

type ghPRRest struct {
	Number         int  `json:"number"`
	Commits        int  `json:"commits"`
	Comments       int  `json:"comments"`
	ReviewComments int  `json:"review_comments"`
	Merged         bool `json:"merged"`
}

func labelNames(labels []ghLabel) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// parsePR combines `gh pr view --json` output with the REST pulls payload,
// which supplies exact commit, comment and inline review comment counts.
func (c *Client) parsePR(view, rest []byte) (*Item, error) {
	var v ghPRView
	if err := json.Unmarshal(view, &v); err != nil {
		return nil, fmt.Errorf("decode gh pr view: %w", err)
	}
	var r ghPRRest
	if err := json.Unmarshal(rest, &r); err != nil {
		return nil, fmt.Errorf("decode pulls payload: %w", err)
	}
	if v.Number == 0 || r.Number == 0 {
		return nil, ErrNotFound
	}
	item := &Item{
		Kind:              KindPR,
		Title:             v.Title,
		URL:               v.URL,
		Author:            v.Author.Login,
		Labels:            labelNames(v.Labels),
		UnresolvedThreads: -1,
	}
	state := strings.ToUpper(v.State)
	switch {
	case state == StateMerged || r.Merged:
		state = StateMerged
	case state == StateClosed:
		state = StateClosed
	case v.IsDraft:
		state = StateDraft
	default:
		state = StateOpen
	}
	// A submitted review with a body is a comment the maintainer has to
	// read; inline review comments are counted by the REST payload.
	reviewBodies := 0
	for _, rv := range v.Reviews {
		if rv.State != "PENDING" && strings.TrimSpace(rv.Body) != "" {
			reviewBodies++
		}
	}
	decision := strings.ToUpper(v.ReviewDecision)
	if c.ProwLabels && decision != ReviewChangesRequested && hasLabel(item.Labels, "lgtm") && hasLabel(item.Labels, "approved") {
		decision = ReviewApproved
	}
	item.Snapshot = Snapshot{
		State:          state,
		CI:             RollupCI(v.StatusCheckRollup, c.IgnoreContexts),
		ReviewDecision: decision,
		HeadSHA:        v.HeadRefOid,
		Commits:        max(r.Commits, len(v.Commits)),
		Comments:       max(r.Comments, len(v.Comments)),
		ReviewComments: r.ReviewComments + reviewBodies,
		UpdatedAt:      v.UpdatedAt,
	}
	return item, nil
}

func parseIssueView(data []byte) (*Item, error) {
	var v struct {
		Number    int               `json:"number"`
		State     string            `json:"state"`
		Title     string            `json:"title"`
		URL       string            `json:"url"`
		Author    ghAuthor          `json:"author"`
		Comments  []json.RawMessage `json:"comments"`
		UpdatedAt string            `json:"updatedAt"`
		Labels    []ghLabel         `json:"labels"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("decode gh issue view: %w", err)
	}
	if v.Number == 0 {
		return nil, ErrNotFound
	}
	state := StateOpen
	if strings.EqualFold(v.State, StateClosed) {
		state = StateClosed
	}
	return &Item{
		Kind:              KindIssue,
		Title:             v.Title,
		URL:               v.URL,
		Author:            v.Author.Login,
		Labels:            labelNames(v.Labels),
		UnresolvedThreads: -1,
		Snapshot: Snapshot{
			State:     state,
			CI:        CIUnknown,
			Comments:  len(v.Comments),
			UpdatedAt: v.UpdatedAt,
		},
	}, nil
}

// RollupCI folds check runs and status contexts into one status. Any failure
// wins over pending, pending wins over success, and no checks is UNKNOWN.
func RollupCI(checks []ghCheck, ignore map[string]bool) string {
	failed, pending, passed := 0, 0, 0
	for _, ch := range checks {
		if ignore[ch.Name] || ignore[ch.Context] {
			continue
		}
		if ch.TypeName == "StatusContext" || (ch.Status == "" && ch.State != "") {
			switch strings.ToUpper(ch.State) {
			case "SUCCESS":
				passed++
			case "FAILURE", "ERROR":
				failed++
			default:
				pending++
			}
			continue
		}
		if strings.ToUpper(ch.Status) != "COMPLETED" {
			pending++
			continue
		}
		switch strings.ToUpper(ch.Conclusion) {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			passed++
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			failed++
		default:
			pending++
		}
	}
	switch {
	case failed > 0:
		return CIFailure
	case pending > 0:
		return CIPending
	case passed > 0:
		return CISuccess
	}
	return CIUnknown
}

const threadsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) { nodes { isResolved isOutdated } }
    }
  }
}`

func (c *Client) unresolvedThreads(ctx context.Context, repo string, number int) (int, error) {
	owner, name, _ := strings.Cut(repo, "/")
	out, err := c.run(ctx, "api", "graphql",
		"-f", "query="+threadsQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", "number="+strconv.Itoa(number))
	if err != nil {
		return -1, err
	}
	return parseUnresolvedThreads(out)
}

func parseUnresolvedThreads(data []byte) (int, error) {
	var v struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
							IsOutdated bool `json:"isOutdated"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return -1, fmt.Errorf("decode review threads: %w", err)
	}
	if v.Data.Repository.PullRequest == nil {
		return -1, ErrNotFound
	}
	n := 0
	for _, t := range v.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if !t.IsResolved && !t.IsOutdated {
			n++
		}
	}
	return n, nil
}
