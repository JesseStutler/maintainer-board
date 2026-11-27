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

var repoURLRe = regexp.MustCompile(`^(?:https?://)?(?:www\.)?github\.com/([^/\s]+/[^/\s?#]+)(?:[/?#].*)?$`)

// NormalizeRepo accepts "owner/name" or a GitHub repository/PR/issue URL and
// returns the canonical "owner/name" form.
func NormalizeRepo(input string) (string, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return "", fmt.Errorf("%w: empty repository", ErrBadRef)
	}
	if m := repoURLRe.FindStringSubmatch(in); m != nil {
		in = strings.TrimSuffix(m[1], ".git")
	}
	in = strings.Trim(in, "/")
	if !ValidRepo(in) {
		return "", fmt.Errorf("%w: invalid repository %q (expected owner/name or GitHub URL)", ErrBadRef, input)
	}
	return in, nil
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

// ParseRefs parses one or more GitHub references separated by whitespace,
// commas, or semicolons (for example "#6081 #6196" or "owner/repo#1, #2").
// Duplicate (repo, number) entries are removed while preserving order.
func ParseRefs(input, defaultRepo string) ([]Ref, error) {
	fields := strings.FieldsFunc(strings.TrimSpace(input), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrBadRef)
	}
	out := make([]Ref, 0, len(fields))
	seen := map[string]bool{}
	for _, token := range fields {
		ref, err := ParseRef(token, defaultRepo)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s#%d", strings.ToLower(ref.Repo), ref.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out, nil
}

// FindRefs extracts distinct PR or issue references mentioned in free text,
// ordered by first appearance and capped to keep noisy prompts bounded.
func FindRefs(text, defaultRepo string) []Ref {
	const maxRefs = 5
	type hit struct {
		pos int
		ref Ref
	}
	var hits []hit
	add := func(pos int, raw string) {
		if ref, err := ParseRef(raw, defaultRepo); err == nil {
			hits = append(hits, hit{pos: pos, ref: ref})
		}
	}
	for _, loc := range textURLRe.FindAllStringIndex(text, -1) {
		add(loc[0], text[loc[0]:loc[1]])
	}
	for _, m := range textShortRe.FindAllStringSubmatchIndex(text, -1) {
		add(m[0], text[m[2]:m[3]]+"#"+text[m[4]:m[5]])
	}
	for _, m := range textRefRe.FindAllStringSubmatchIndex(text, -1) {
		add(m[2], text[m[2]:m[3]])
	}
	if len(hits) == 0 {
		return nil
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].pos < hits[j-1].pos; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]Ref, 0, min(len(hits), maxRefs))
	seen := map[string]bool{}
	for _, h := range hits {
		key := fmt.Sprintf("%s#%d", strings.ToLower(h.ref.Repo), h.ref.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, h.ref)
		if len(out) == maxRefs {
			break
		}
	}
	return out
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
