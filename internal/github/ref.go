// Package github wraps the gh CLI to track pull requests and issues, and
// contains the incremental-update and auto-move decision engine.
package github

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	KindPR    = "pr"
	KindIssue = "issue"
)

// Ref identifies a pull request or issue. Kind is only a hint taken from a
// URL; GitHub redirects between /issues/N and /pull/N, so callers detect the
// real kind through the API.
type Ref struct {
	Repo   string
	Number int
	Kind   string
}

func (r Ref) String() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

var ErrBadRef = errors.New("unrecognized GitHub reference")

var (
	repoRe      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)
	numberRe    = regexp.MustCompile(`^#?(\d{1,9})$`)
	shortRefRe  = regexp.MustCompile(`^([^\s#]+)#(\d{1,9})$`)
	urlRefRe    = regexp.MustCompile(`^(?:https?://)?(?:www\.)?github\.com/([^/\s]+/[^/\s]+)/(pull|pulls|issues)/(\d{1,9})(?:[/?#].*)?$`)
	textRefRe   = regexp.MustCompile(`(?:^|[^\w&/])#(\d{1,7})\b`)
	textURLRe   = regexp.MustCompile(`https?://github\.com/[^/\s]+/[^/\s]+/(?:pull|issues)/\d{1,9}`)
	textShortRe = regexp.MustCompile(`\b([A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+)#(\d{1,7})\b`)
)

// ValidRepo reports whether repo looks like "owner/name".
func ValidRepo(repo string) bool {
	return repoRe.MatchString(repo) && !strings.Contains(repo, "..")
}

// ParseRef parses "#6247", "6247", "owner/repo#123" or a GitHub pull request
// or issue URL. Bare numbers resolve against defaultRepo.
func ParseRef(input, defaultRepo string) (Ref, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return Ref{}, fmt.Errorf("%w: empty input", ErrBadRef)
	}
	var ref Ref
	switch {
	case numberRe.MatchString(in):
		ref.Repo = defaultRepo
		ref.Number, _ = strconv.Atoi(numberRe.FindStringSubmatch(in)[1])
	case urlRefRe.MatchString(in):
		m := urlRefRe.FindStringSubmatch(in)
		ref.Repo = m[1]
		ref.Number, _ = strconv.Atoi(m[3])
		if m[2] == "issues" {
			ref.Kind = KindIssue
		} else {
			ref.Kind = KindPR
		}
	case shortRefRe.MatchString(in):
		m := shortRefRe.FindStringSubmatch(in)
		ref.Repo = m[1]
		ref.Number, _ = strconv.Atoi(m[2])
	default:
		return Ref{}, fmt.Errorf("%w: %q (use #123, owner/repo#123 or a GitHub URL)", ErrBadRef, input)
	}
	if !ValidRepo(ref.Repo) {
		return Ref{}, fmt.Errorf("%w: invalid repository %q", ErrBadRef, ref.Repo)
	}
	if ref.Number <= 0 {
		return Ref{}, fmt.Errorf("%w: invalid number in %q", ErrBadRef, input)
	}
	return ref, nil
}

// FindRef extracts the first PR or issue reference mentioned in free text,
// such as an AO session display name ("[PR #6247][Review] ...").
func FindRef(text, defaultRepo string) (Ref, bool) {
	if u := textURLRe.FindString(text); u != "" {
		if ref, err := ParseRef(u, defaultRepo); err == nil {
			return ref, true
		}
	}
	if m := textShortRe.FindStringSubmatch(text); m != nil {
		if ref, err := ParseRef(m[1]+"#"+m[2], defaultRepo); err == nil {
			return ref, true
		}
	}
	if m := textRefRe.FindStringSubmatch(text); m != nil {
		if ref, err := ParseRef(m[1], defaultRepo); err == nil {
			return ref, true
		}
	}
	return Ref{}, false
}
