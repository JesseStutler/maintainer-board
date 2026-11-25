package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/history"
	"maintainer-board/internal/summary"
)

const (
	// candidateCacheTTL bounds how stale AO's import list may be for the
	// search-as-you-type endpoint; opening the dialog always asks AO.
	candidateCacheTTL = 15 * time.Second

	maxHistoryResults = 200
	previewRunes      = 240
	localSummaryRunes = 320
	// The AI search sees the most recent sessions plus the best keyword
	// matches among the older ones.
	aiCatalogRecent  = 60
	aiCatalogKeyword = 30
)

// Fields of an import candidate a search term can be found in.
const (
	fieldID           = "id"
	fieldTitle        = "title"
	fieldSummary      = "summary"
	fieldBranch       = "branch"
	fieldFolder       = "folder"
	fieldConversation = "conversation"
	// fieldFuzzy marks a match that needed typo tolerance.
	fieldFuzzy = "fuzzy"
)

var fieldWeights = map[string]float64{
	fieldID: 100, fieldTitle: 40, fieldSummary: 26, fieldBranch: 20, fieldFolder: 16, fieldConversation: 14,
}

// importCandidateView is an AO import candidate plus what the board and
// the local Copilot and Codex stores know about it.
type importCandidateView struct {
	ao.ImportCandidate
	// CardID is set when the already imported session has a card.
	CardID string `json:"cardId"`
	// NativeID is the Copilot session UUID or Codex thread UUID.
	NativeID string `json:"nativeId,omitempty"`
	// Summary is the stored summary of the session (its newest checkpoint).
	Summary string `json:"summary,omitempty"`
	// Preview is an excerpt of what the user asked the agent to do.
	Preview string `json:"preview,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// historyEntry pairs a candidate with its local session, when one matched.
type historyEntry struct {
	view  importCandidateView
	local *history.Session
}

// importCandidates returns AO's import list. With fresh false a list that
// is at most candidateCacheTTL old is reused, which keeps typing in the
// search box from scanning the history on every keystroke.
func (s *Server) importCandidates(ctx context.Context, fresh bool) ([]ao.ImportCandidate, error) {
	s.candidatesMu.Lock()
	defer s.candidatesMu.Unlock()
	if !fresh && s.candidates != nil && time.Since(s.candidatesAt) < candidateCacheTTL {
		return s.candidates, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	list, err := s.ao.ImportCandidates(cctx)
	if err != nil {
		return nil, err
	}
	s.candidates, s.candidatesAt = list, time.Now()
	return list, nil
}

func (s *Server) forgetImportCandidates() {
	s.candidatesMu.Lock()
	s.candidates = nil
	s.candidatesMu.Unlock()
}

// isInternalRun reports whether a history entry is one of the board's own
// AI summary or AI search runs rather than a real working session. The AO
// title is checked, and the locally stored title and first prompt, because
// either may have been shortened or renamed; a session that ran in the
// scratch directory of the AI CLIs (helperDir) is one whatever it says.
func isInternalRun(c ao.ImportCandidate, local *history.Session, helperDir string) bool {
	if summary.IsInternalPrompt(c.Title) {
		return true
	}
	if local == nil {
		return false
	}
	if helperDir != "" && local.CWD != "" {
		if cwd := filepath.Clean(local.CWD); cwd == helperDir || strings.HasPrefix(cwd, helperDir+string(filepath.Separator)) {
			return true
		}
	}
	if summary.IsInternalPrompt(local.Title) {
		return true
	}
	prompts := local.Prompts(1)
	return len(prompts) > 0 && summary.IsInternalPrompt(prompts[0])
}

// promptExcerpt joins the first user prompts of a session into one line.
func promptExcerpt(local *history.Session, prompts, maxRunes int) string {
	var parts []string
	for _, p := range local.Prompts(prompts) {
		if p = summary.Plain(p); p != "" {
			parts = append(parts, p)
		}
	}
	return summary.Truncate(strings.Join(parts, " / "), maxRunes)
}

// importHistory lists the importable sessions: AO's candidates without the
// board's own AI runs, each enriched from the local session stores.
func (s *Server) importHistory(ctx context.Context, fresh bool) ([]historyEntry, error) {
	candidates, err := s.importCandidates(ctx, fresh)
	if err != nil {
		return nil, err
	}
	cards, err := s.store.ListCards(ctx)
	if err != nil {
		return nil, err
	}
	linked := map[string]string{}
	for _, c := range cards {
		if c.AOSessionID != "" {
			linked[c.AOSessionID] = c.ID
		}
	}
	var local map[string]*history.Session
	if s.cfg.History != nil {
		local = s.cfg.History.Sessions(ctx)
	}
	helperDir := ""
	if s.cfg.HelperDir != "" {
		helperDir = filepath.Clean(s.cfg.HelperDir)
	}
	out := make([]historyEntry, 0, len(candidates))
	for _, c := range candidates {
		sess := local[c.ID]
		if isInternalRun(c, sess, helperDir) {
			continue
		}
		e := historyEntry{view: importCandidateView{ImportCandidate: c, CardID: linked[c.SessionID]}, local: sess}
		if sess != nil {
			e.view.NativeID = sess.NativeID
			e.view.Branch = sess.Branch
			e.view.Summary = summary.Truncate(summary.Plain(sess.Summary), localSummaryRunes)
			e.view.Preview = promptExcerpt(sess, 3, previewRunes)
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Server) handleImportCandidates(w http.ResponseWriter, r *http.Request) {
	entries, err := s.importHistory(r.Context(), true)
	if err != nil {
		s.writeHistoryError(w, err)
		return
	}
	out := make([]importCandidateView, len(entries))
	for i, e := range entries {
		out[i] = e.view
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
}

// writeHistoryError answers for a failed history listing: AO did not
// deliver the candidates, or the board database failed.
func (s *Server) writeHistoryError(w http.ResponseWriter, err error) {
	var apiErr *ao.APIError
	if errors.As(err, &apiErr) || errors.Is(err, ao.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		s.writeAOError(w, err)
		return
	}
	s.writeStoreError(w, err)
}

// Keyword and content search

// historyDoc is the searchable form of one history entry.
type historyDoc struct {
	entry *historyEntry
	// Lower-cased fields.
	title, summary, branch, folder, conversation string
	// words are the distinct ASCII words of the title, summary, branch and
	// first prompts, which typo-tolerant matching compares against.
	words []string
}

func newHistoryDoc(e *historyEntry) *historyDoc {
	c := e.view
	d := &historyDoc{
		entry:  e,
		title:  db.Fold(c.Title),
		folder: db.Fold(c.Folder + " " + c.ProjectID),
		branch: db.Fold(c.Branch),
	}
	fuzzy := []string{d.title, d.branch}
	if e.local != nil {
		d.summary = db.Fold(e.local.Summary)
		d.conversation = e.local.Folded()
		d.folder += " " + db.Fold(e.local.CWD+" "+e.local.Repository)
		fuzzy = append(fuzzy, d.summary)
		for _, p := range e.local.Prompts(3) {
			fuzzy = append(fuzzy, db.Fold(p))
		}
	}
	seen := map[string]bool{}
	for _, text := range fuzzy {
		for _, w := range strings.FieldsFunc(text, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
		}) {
			if len(w) >= 4 && len(w) <= 40 && !seen[w] {
				seen[w] = true
				d.words = append(d.words, w)
			}
		}
	}
	return d
}

// placeReason names where the session ran, for a term found in one of its
// places: the folder, the AO project or the repository that contains it.
func (d *historyDoc) placeReason(found string) string {
	c := d.entry.view
	places := []struct{ label, value string }{{"folder", c.Folder}, {"project", c.ProjectID}}
	if d.entry.local != nil {
		places = append(places, struct{ label, value string }{"folder", d.entry.local.CWD}, struct{ label, value string }{"repository", d.entry.local.Repository})
	}
	for _, p := range places {
		if p.value != "" && strings.Contains(db.Fold(p.value), found) {
			return "Matched " + p.label + ": " + p.value
		}
	}
	return "Matched folder"
}

// termHit says where one search term was found in a history entry.
type termHit struct {
	field string
	// quality is 1 for a literal match and lower for a tolerant one.
	quality float64
	// found is the text that matched: the term itself, the similar word,
	// or the full ID.
	found string
}

func hasDigit(s string) bool {
	return strings.ContainsFunc(s, unicode.IsDigit)
}

// matchID reports the ID of the entry that term refers to: the native
// Copilot or Codex UUID, the AO session ID of an imported session, or AO's
// candidate ID. Short terms only count as the beginning of an ID (or of
// one of the groups of a UUID), so that an ordinary word made of the
// letters a to f is not taken for one.
func matchID(c importCandidateView, term string) (id string, exact bool) {
	if sid := db.Fold(c.SessionID); sid != "" && len(term) >= 2 {
		if sid == term {
			return c.SessionID, true
		}
		if hasDigit(term) && strings.Contains(sid, term) {
			return c.SessionID, false
		}
	}
	if len(term) < 4 {
		return "", false
	}
	for _, id := range []string{c.NativeID, c.ID} {
		folded := db.Fold(id)
		switch {
		case folded == "":
		case folded == term:
			return id, true
		case strings.HasPrefix(folded, term) || strings.Contains(folded, "-"+term):
			return id, false
		case len(term) >= 6 && strings.Contains(folded, term):
			return id, false
		}
	}
	return "", false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// editDistance is the Levenshtein distance of two short ASCII words, or
// limit+1 as soon as it is known to exceed limit.
func editDistance(a, b string, limit int) int {
	if d := len(a) - len(b); d > limit || -d > limit {
		return limit + 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			best = min(best, cur[j])
		}
		if best > limit {
			return limit + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// isSubsequence reports whether the letters of term appear in word in the
// same order, as in "podgrp" and "podgroup".
func isSubsequence(term, word string) bool {
	i := 0
	for j := 0; j < len(word) && i < len(term); j++ {
		if term[i] == word[j] {
			i++
		}
	}
	return i == len(term)
}

// similarWord finds the word of the document closest to an ASCII term that
// is not contained literally: within one edit (two for long terms), or
// containing the term as an abbreviation.
func similarWord(words []string, term string) (word string, quality float64) {
	if len(term) < 4 || !isASCII(term) {
		return "", 0
	}
	limit := 1
	if len(term) >= 8 {
		limit = 2
	}
	bestDist := limit + 1
	for _, w := range words {
		if d := editDistance(term, w, limit); d < bestDist {
			bestDist, word = d, w
		}
	}
	if word != "" {
		return word, 0.7 - 0.1*float64(bestDist)
	}
	if len(term) >= 5 {
		for _, w := range words {
			if w[0] == term[0] && len(w) <= 2*len(term) && isSubsequence(term, w) {
				return w, 0.45
			}
		}
	}
	return "", 0
}

// bigramCoverage is the share of the adjacent character pairs of term that
// occur in text. It lets a Chinese phrase match text that words it a little
// differently, where there are no spaces to split terms at.
func bigramCoverage(term, text string) float64 {
	runes := []rune(term)
	if len(runes) < 3 || text == "" {
		return 0
	}
	found := 0
	for i := 0; i+1 < len(runes); i++ {
		if strings.Contains(text, string(runes[i:i+2])) {
			found++
		}
	}
	return float64(found) / float64(len(runes)-1)
}

// matchTerm finds term in the document: as an ID, literally in a field
// (the most telling field wins), or approximately.
func (d *historyDoc) matchTerm(term string) (termHit, bool) {
	if id, exact := matchID(d.entry.view, term); id != "" {
		quality := 1.0
		if exact {
			quality = 1.2
		}
		return termHit{field: fieldID, quality: quality, found: id}, true
	}
	for _, f := range []struct{ name, text string }{
		{fieldTitle, d.title}, {fieldSummary, d.summary}, {fieldBranch, d.branch}, {fieldFolder, d.folder}, {fieldConversation, d.conversation},
	} {
		if f.text != "" && strings.Contains(f.text, term) {
			return termHit{field: f.name, quality: 1, found: term}, true
		}
	}
	if word, quality := similarWord(d.words, term); word != "" {
		field := fieldConversation
		switch {
		case strings.Contains(d.title, word):
			field = fieldTitle
		case strings.Contains(d.summary, word):
			field = fieldSummary
		case strings.Contains(d.branch, word):
			field = fieldBranch
		}
		return termHit{field: field, quality: quality, found: word}, true
	}
	if !isASCII(term) {
		best, field := 0.0, ""
		for _, f := range []struct{ name, text string }{{fieldTitle, d.title}, {fieldSummary, d.summary}, {fieldConversation, d.conversation}} {
			if c := bigramCoverage(term, f.text); c > best {
				best, field = c, f.name
			}
		}
		if best >= 0.6 {
			return termHit{field: field, quality: 0.6 * best, found: ""}, true
		}
	}
	return termHit{}, false
}

// conversationSnippet returns an excerpt around needle from the first
// message that contains it, a prompt of the user if there is one.
func conversationSnippet(local *history.Session, needle string) string {
	if local == nil || needle == "" {
		return ""
	}
	var fallback string
	for _, m := range local.Messages {
		if !strings.Contains(db.Fold(m.Text), needle) {
			continue
		}
		if m.Role == "user" {
			return snippet(m.Text, needle)
		}
		if fallback == "" {
			fallback = m.Text
		}
	}
	if fallback == "" {
		return ""
	}
	return snippet(fallback, needle)
}

type historyMatch struct {
	Candidate importCandidateView `json:"candidate"`
	// MatchedBy lists the fields the terms were found in: "id", "title",
	// "summary", "branch", "folder", "conversation", and "fuzzy" when a
	// term only matched approximately.
	MatchedBy []string `json:"matchedBy"`
	Score     float64  `json:"score"`
	// Reason is a short explanation for the list row.
	Reason string `json:"reason"`
	// Snippet is the excerpt of the summary or conversation that matched.
	Snippet string `json:"snippet,omitempty"`
	// MatchedTerms is how many of the query terms were found; fewer than
	// all of them makes this a partial match.
	MatchedTerms int `json:"matchedTerms"`
}

type historySearchResponse struct {
	Query   string         `json:"query"`
	Terms   []string       `json:"terms"`
	Total   int            `json:"total"`
	Matches []historyMatch `json:"matches"`
	// Candidates is the number of sessions searched; Indexed is how many
	// of them had local conversation content.
	Candidates int `json:"candidates"`
	Indexed    int `json:"indexed"`
}

// matchHistoryEntry scores one entry against the terms. All terms found is
// a full match. A query of three or more terms may miss a third of them
// (lenient: all but one term may be missing): that is a partial match,
// ranked well below the full ones.
func matchHistoryEntry(d *historyDoc, terms []string, lenient bool) (historyMatch, bool) {
	m := historyMatch{Candidate: d.entry.view, MatchedBy: []string{}}
	hits := map[string]termHit{}
	fuzzy := false
	for _, term := range terms {
		hit, ok := d.matchTerm(term)
		if !ok {
			continue
		}
		m.MatchedTerms++
		m.Score += fieldWeights[hit.field] * hit.quality
		fuzzy = fuzzy || hit.quality < 1
		// Keep the first hit per field: it supplies the reason.
		if _, seen := hits[hit.field]; !seen {
			hits[hit.field] = hit
		}
	}
	if m.MatchedTerms == 0 {
		return m, false
	}
	if missing := len(terms) - m.MatchedTerms; missing > 0 {
		if !lenient && missing > len(terms)/3 {
			return m, false
		}
		m.Score *= 0.3 * float64(m.MatchedTerms) / float64(len(terms))
	}
	for _, field := range []string{fieldID, fieldTitle, fieldSummary, fieldBranch, fieldFolder, fieldConversation} {
		if _, ok := hits[field]; ok {
			m.MatchedBy = append(m.MatchedBy, field)
		}
	}
	if fuzzy {
		m.MatchedBy = append(m.MatchedBy, fieldFuzzy)
	}

	// The reason names the most specific evidence: an ID, then the text
	// the user cannot see in the row (conversation, summary), then the rest.
	if hit, ok := hits[fieldID]; ok {
		m.Reason = "Matched ID: " + hit.found
	} else if hit, ok := hits[fieldConversation]; ok {
		m.Snippet = conversationSnippet(d.entry.local, hit.found)
		if m.Snippet == "" {
			m.Snippet = d.entry.view.Preview
		}
		m.Reason = fmt.Sprintf("Matched conversation: %q", m.Snippet)
	} else if hit, ok := hits[fieldSummary]; ok {
		m.Snippet = snippet(d.entry.local.Summary, hit.found)
		m.Reason = fmt.Sprintf("Matched summary: %q", m.Snippet)
	} else if _, ok := hits[fieldTitle]; ok {
		m.Reason = "Matched title"
	} else if _, ok := hits[fieldBranch]; ok {
		m.Reason = "Matched branch: " + d.entry.view.Branch
	} else {
		m.Reason = d.placeReason(hits[fieldFolder].found)
	}
	if fuzzy {
		m.Reason = "Close match. " + m.Reason
	}
	if m.MatchedTerms < len(terms) {
		m.Reason = fmt.Sprintf("Partial match (%d of %d terms). %s", m.MatchedTerms, len(terms), m.Reason)
	}
	return m, true
}

// searchHistory ranks the entries that match q: by score, then by how
// recently the session was active. With lenient true a single matching
// term is enough, which is what picking sessions for the AI search needs.
func searchHistory(entries []historyEntry, q string, lenient bool) *historySearchResponse {
	res := &historySearchResponse{Query: q, Terms: searchTerms(q), Matches: []historyMatch{}, Candidates: len(entries)}
	if res.Terms == nil {
		res.Terms = []string{}
	}
	for i := range entries {
		if entries[i].local != nil {
			res.Indexed++
		}
	}
	if len(res.Terms) == 0 {
		return res
	}
	for i := range entries {
		if m, ok := matchHistoryEntry(newHistoryDoc(&entries[i]), res.Terms, lenient); ok {
			res.Matches = append(res.Matches, m)
		}
	}
	sort.SliceStable(res.Matches, func(i, j int) bool {
		a, b := res.Matches[i], res.Matches[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Candidate.LastActivity > b.Candidate.LastActivity
	})
	res.Total = len(res.Matches)
	if len(res.Matches) > maxHistoryResults {
		res.Matches = res.Matches[:maxHistoryResults]
	}
	return res
}

func (s *Server) handleImportSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > 1000 {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "The search query is too long.")
		return
	}
	entries, err := s.importHistory(r.Context(), false)
	if err != nil {
		s.writeHistoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, searchHistory(entries, q, false))
}

// AI search

// historyCatalog picks the sessions an AI search gets to see and describes
// each one compactly. The best keyword matches come first, then the most
// recent sessions, so that a prompt that has to be cut loses the least
// promising ones.
func historyCatalog(entries []historyEntry, query string) ([]summary.HistorySession, []*historyEntry) {
	recent := make([]*historyEntry, len(entries))
	for i := range entries {
		recent[i] = &entries[i]
	}
	sort.SliceStable(recent, func(i, j int) bool {
		return recent[i].view.LastActivity > recent[j].view.LastActivity
	})
	var picked []*historyEntry
	seen := map[string]bool{}
	if len(recent) > aiCatalogRecent {
		// The keyword search sees every session, however old.
		keyword := strings.Join(keywordTerms(query), " ")
		byID := map[string]*historyEntry{}
		for _, e := range recent {
			byID[e.view.ID] = e
		}
		for _, m := range searchHistory(entries, keyword, true).Matches {
			if len(picked) == aiCatalogKeyword {
				break
			}
			picked = append(picked, byID[m.Candidate.ID])
			seen[m.Candidate.ID] = true
		}
	}
	added := 0
	for _, e := range recent {
		if added == aiCatalogRecent {
			break
		}
		if !seen[e.view.ID] {
			picked = append(picked, e)
			added++
		}
	}
	catalog := make([]summary.HistorySession, len(picked))
	for i, e := range picked {
		c := e.view
		hs := summary.HistorySession{
			ID: c.NativeID, Harness: c.Harness, Title: c.Title, Folder: c.Folder, Branch: c.Branch, LastActivity: c.LastActivity,
		}
		if hs.ID == "" {
			hs.ID = truncateRunes(c.ID, 12)
		}
		if hs.Folder == "" {
			hs.Folder = c.ProjectID
		}
		if e.local != nil {
			if hs.Folder == "" {
				hs.Folder = e.local.CWD
			}
			hs.Excerpt = promptExcerpt(e.local, 3, 300)
			if c.Summary != "" {
				hs.Excerpt = strings.TrimSpace(hs.Excerpt + " Summary: " + summary.Truncate(c.Summary, 160))
			}
		}
		catalog[i] = hs
	}
	return catalog, picked
}

type historyAIMatch struct {
	Candidate importCandidateView `json:"candidate"`
	Reason    string              `json:"reason"`
}

type historyAIResponse struct {
	Query string `json:"query"`
	// Provider is the engine that answered: the summary model, "copilot"
	// or "codex".
	Provider string `json:"provider"`
	// Candidates is the number of sessions the model was shown.
	Candidates int              `json:"candidates"`
	Matches    []historyAIMatch `json:"matches"`
}

func (s *Server) handleImportAISearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Query string `json:"query"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	query := strings.Join(strings.Fields(req.Query), " ")
	if query == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Describe the session you are looking for.")
		return
	}
	if runes := []rune(query); len(runes) > maxAIQueryRunes {
		query = string(runes[:maxAIQueryRunes])
	}
	// One model run at a time: each one costs tokens.
	if !s.aiSearchMu.TryLock() {
		writeError(w, http.StatusConflict, "AI_SEARCH_BUSY", "An AI search is already running. Wait for it to finish.")
		return
	}
	defer s.aiSearchMu.Unlock()

	entries, err := s.importHistory(ctx, false)
	if err != nil {
		s.writeHistoryError(w, err)
		return
	}
	catalog, picked := historyCatalog(entries, query)
	out := historyAIResponse{Query: query, Candidates: len(catalog), Matches: []historyAIMatch{}}
	if len(catalog) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	matches, engine, err := s.ai.SearchHistory(ctx, summary.HistoryInput{Query: query, Sessions: catalog})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("ai history search failed", "err", err)
		writeError(w, http.StatusBadGateway, "AI_SEARCH_FAILED", capitalize(err.Error())+".")
		return
	}
	out.Provider = engine
	seen := map[int]bool{}
	for _, m := range matches {
		if m.Index < 0 || m.Index >= len(picked) || seen[m.Index] {
			continue
		}
		seen[m.Index] = true
		reason := m.Reason
		if reason == "" {
			reason = "Matches the query."
		}
		out.Matches = append(out.Matches, historyAIMatch{Candidate: picked[m.Index].view, Reason: reason})
	}
	writeJSON(w, http.StatusOK, out)
}
