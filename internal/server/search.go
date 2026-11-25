package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/summary"
)

const (
	maxSearchTerms   = 8
	maxSearchTermLen = 80
	maxAIQueryRunes  = 400
	snippetBefore    = 50
	snippetAfter     = 110

	indexPageSize    = 200
	maxIndexPages    = 100
	indexMinInterval = 15 * time.Second
	indexRetryAfter  = time.Minute
)

// Sources a card can be found through.
const (
	matchCard         = "card"
	matchSessionID    = "sessionId"
	matchConversation = "conversation"
)

var categoryLabels = map[string]string{
	db.CategoryPRReview: "PR Review",
	db.CategoryIssue:    "Issue",
	db.CategoryDev:      "Dev",
	db.CategoryDesign:   "Design",
	db.CategoryTriage:   "Triage",
}

// Conversation index

type indexMark struct {
	stamp  string
	at     time.Time
	failed bool
}

// contentIndex tracks which sessions were indexed recently, so the frequent
// AO sync does not hit the database or AO for sessions that did not change.
type contentIndex struct {
	mu   sync.Mutex
	seen map[string]indexMark
	// run serializes index passes.
	run sync.Mutex
}

func (s *Server) requestContentIndex() {
	select {
	case s.indexRequests <- struct{}{}:
	default:
	}
}

func (s *Server) contentIndexLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.indexRequests:
			s.indexContent(ctx)
		}
	}
}

func sessionStamp(sess ao.Session) string {
	return sess.Activity.LastActivityAt + "|" + sess.Activity.State
}

// indexContent brings the local conversation index up to date for every
// session that has a card. Only sessions whose activity stamp changed are
// fetched, and only the part of the conversation that is new.
func (s *Server) indexContent(ctx context.Context) {
	s.index.run.Lock()
	defer s.index.run.Unlock()

	cards, err := s.store.ListCards(ctx)
	if err != nil {
		return
	}
	s.mu.RLock()
	var sessions []ao.Session
	for _, c := range cards {
		if sess, ok := s.sessions[c.AOSessionID]; ok && c.AOSessionID != "" {
			sessions = append(sessions, sess)
		}
	}
	s.mu.RUnlock()

	for _, sess := range sessions {
		if ctx.Err() != nil {
			return
		}
		stamp := sessionStamp(sess)
		s.index.mu.Lock()
		mark, seen := s.index.seen[sess.ID]
		s.index.mu.Unlock()
		if seen {
			wait := indexMinInterval
			if mark.failed {
				wait = indexRetryAfter
			}
			if (mark.stamp == stamp && !mark.failed) || time.Since(mark.at) < wait {
				continue
			}
		}
		lastSeq, stored, err := s.store.ContentState(ctx, sess.ID)
		if err == nil && stored != stamp {
			err = s.indexSession(ctx, sess.ID, lastSeq, stamp, false)
		}
		if err != nil {
			s.log.Debug("content index: session skipped", "session", sess.ID, "err", err)
		}
		s.index.mu.Lock()
		s.index.seen[sess.ID] = indexMark{stamp: stamp, at: time.Now(), failed: err != nil}
		s.index.mu.Unlock()
	}
}

// indexSession stores the messages of one session that are newer than
// lastSeq. AO pages a conversation backwards from its end, so the walk goes
// from the newest message down to the indexed part.
func (s *Server) indexSession(ctx context.Context, sessionID string, lastSeq int64, stamp string, reset bool) error {
	var msgs []db.ContentMessage
	newLast := lastSeq
	streamingAt := int64(0)
	before := int64(0)
	for page := 0; page < maxIndexPages; page++ {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		conv, err := s.ao.Conversation(cctx, sessionID, ao.ConversationQuery{Limit: indexPageSize, BeforeSequence: before, AfterSequence: lastSeq})
		cancel()
		if err != nil {
			return err
		}
		if page == 0 && !reset && lastSeq > 0 && conv.LatestSequence > 0 && conv.LatestSequence < lastSeq {
			// The conversation is shorter than what was indexed: it was
			// rolled back or replaced. Start over.
			return s.indexSession(ctx, sessionID, 0, stamp, true)
		}
		for _, m := range conv.Messages {
			if m.Streaming {
				if streamingAt == 0 || m.Sequence < streamingAt {
					streamingAt = m.Sequence
				}
				continue
			}
			msgs = append(msgs, db.ContentMessage{MessageID: m.ID, Sequence: m.Sequence, Role: m.Role, Text: m.Text, CreatedAt: m.CreatedAt})
			newLast = max(newLast, m.Sequence)
		}
		oldest := conv.OldestSequence
		if !conv.HasMoreBefore || oldest <= 0 || oldest <= lastSeq || (before != 0 && oldest >= before) {
			break
		}
		before = oldest
	}
	if streamingAt > 0 {
		// A message that is still being written is fetched again once it
		// is complete; an empty stamp keeps the session due.
		newLast, stamp = min(newLast, streamingAt-1), ""
	}
	return s.store.IndexContent(ctx, sessionID, msgs, newLast, stamp, reset)
}

// Keyword and content search

// searchTerms splits a query into lower-cased terms, as the board UI does.
func searchTerms(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.Fields(db.Fold(q)) {
		if r := []rune(t); len(r) > maxSearchTermLen {
			t = string(r[:maxSearchTermLen])
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == maxSearchTerms {
			break
		}
	}
	return out
}

// cardHaystack is the searchable text of a card itself: the same fields
// the board UI searches, except the AO session ID and the session branch.
// Those are matched separately, the ID first, so that a search for
// "volcano-2" is reported as a match on the ID even though AO also puts
// the ID into the branch name.
func cardHaystack(c *db.Card) string {
	parts := []string{c.Title, c.Summary, c.Notes, c.GHTitle, c.GHRepo, categoryLabels[c.Category],
		strings.Join(c.Tags, " "), strings.Join(c.GHLabels, " ")}
	if c.GHAuthor != "" {
		parts = append(parts, c.GHAuthor+" @"+c.GHAuthor)
	}
	if c.GHNumber != 0 {
		parts = append(parts, fmt.Sprintf("#%d %d", c.GHNumber, c.GHNumber))
	}
	return db.Fold(strings.Join(parts, "\n"))
}

// sessionHaystack is the searchable text of the live session of a card.
func sessionHaystack(sess *ao.Session) string {
	if sess == nil {
		return ""
	}
	return db.Fold(sess.Harness + " " + sess.Branch)
}

// snippetCleaner removes the Markdown marks that would clutter an excerpt.
var snippetCleaner = strings.NewReplacer("```", " ", "`", "", "**", "", "__", "")

// markdownLink matches "[text](target)"; only the text is worth showing.
var markdownLink = regexp.MustCompile(`\[([^\]\n]+)\]\([^)\n]*\)`)

// snippet returns a one-line excerpt of text around the first occurrence
// of term (already folded), with "..." where it was cut.
func snippet(text, term string) string {
	clean := markdownLink.ReplaceAllString(summary.StripEmoji(text), "$1")
	clean = strings.Join(strings.Fields(snippetCleaner.Replace(clean)), " ")
	folded := db.Fold(clean)
	at := -1
	if len(folded) == len(clean) {
		at = strings.Index(folded, term)
	}
	if at < 0 {
		// No position to center on: show the beginning.
		at = 0
		term = ""
	}
	start, end := at, at+len(term)
	for n := 0; start > 0 && n < snippetBefore; n++ {
		_, size := utf8.DecodeLastRuneInString(clean[:start])
		start -= size
	}
	for n := 0; end < len(clean) && n < snippetAfter; n++ {
		_, size := utf8.DecodeRuneInString(clean[end:])
		end += size
	}
	// Prefer to begin at a word.
	if start > 0 {
		if sp := strings.IndexByte(clean[start:at], ' '); sp >= 0 && start+sp+1 <= at {
			start += sp + 1
		}
	}
	out := clean[start:end]
	if start > 0 {
		out = "..." + out
	}
	if end < len(clean) {
		out += "..."
	}
	return out
}

type searchMatch struct {
	CardID string `json:"cardId"`
	// MatchedBy lists what was needed to find the card: "card" when its
	// own fields contain every term, otherwise "sessionId" and/or
	// "conversation".
	MatchedBy []string `json:"matchedBy"`
	// Reason is a short explanation for the card face, empty for a plain
	// match on the card's own fields.
	Reason    string `json:"reason"`
	SessionID string `json:"sessionId,omitempty"`
	// Snippet is the conversation excerpt around the matched term.
	Snippet string `json:"snippet,omitempty"`
	Role    string `json:"role,omitempty"`
	// Messages is the number of conversation messages containing the term
	// the snippet shows.
	Messages int `json:"messages,omitempty"`
}

type searchResponse struct {
	Query   string        `json:"query"`
	Terms   []string      `json:"terms"`
	Matches []searchMatch `json:"matches"`
	Index   struct {
		Sessions int `json:"sessions"`
		Messages int `json:"messages"`
	} `json:"index"`
}

// search finds the cards matching every term of q. A term may be found in
// the card's own fields, in its AO session ID, or in the indexed text of
// its session conversation.
func (s *Server) search(ctx context.Context, q string) (*searchResponse, error) {
	res := &searchResponse{Query: q, Terms: searchTerms(q), Matches: []searchMatch{}}
	if res.Terms == nil {
		res.Terms = []string{}
	}
	var err error
	if res.Index.Sessions, res.Index.Messages, err = s.store.ContentStats(ctx); err != nil {
		return nil, err
	}
	if len(res.Terms) == 0 {
		return res, nil
	}
	cards, err := s.store.ListCards(ctx)
	if err != nil {
		return nil, err
	}
	hits := map[string]map[string]db.ContentHit{}
	for _, term := range res.Terms {
		list, err := s.store.ContentHits(ctx, term)
		if err != nil {
			return nil, err
		}
		bySession := map[string]db.ContentHit{}
		for _, h := range list {
			bySession[h.SessionID] = h
		}
		hits[term] = bySession
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range cards {
		var sess *ao.Session
		if v, ok := s.sessions[c.AOSessionID]; ok && c.AOSessionID != "" {
			sess = &v
		}
		hay, sessHay := cardHaystack(c), sessionHaystack(sess)
		sid := db.Fold(c.AOSessionID)
		viaID := false
		var chat *db.ContentHit
		chatTerm := ""
		matched := true
		for _, term := range res.Terms {
			if strings.Contains(hay, term) {
				continue
			}
			if sid != "" && strings.Contains(sid, term) {
				viaID = true
				continue
			}
			if strings.Contains(sessHay, term) {
				continue
			}
			if h, ok := hits[term][c.AOSessionID]; ok && c.AOSessionID != "" {
				if chat == nil {
					chat, chatTerm = &h, term
				}
				continue
			}
			matched = false
			break
		}
		if !matched {
			continue
		}
		m := searchMatch{CardID: c.ID, MatchedBy: []string{}}
		if viaID {
			m.MatchedBy = append(m.MatchedBy, matchSessionID)
			m.SessionID = c.AOSessionID
			m.Reason = "Matched ID: " + c.AOSessionID
		}
		if chat != nil {
			m.MatchedBy = append(m.MatchedBy, matchConversation)
			m.SessionID = c.AOSessionID
			m.Snippet, m.Role, m.Messages = snippet(chat.Text, chatTerm), chat.Role, chat.Count
			if m.Reason == "" {
				m.Reason = "In chat: " + m.Snippet
			}
		}
		if len(m.MatchedBy) == 0 {
			m.MatchedBy = append(m.MatchedBy, matchCard)
		}
		res.Matches = append(res.Matches, m)
	}
	return res, nil
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > 1000 {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "The search query is too long.")
		return
	}
	res, err := s.search(r.Context(), q)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// AI search

// stopWords are too common to pick conversation excerpts by.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "about": true,
	"where": true, "which": true, "what": true, "when": true, "who": true, "how": true, "why": true, "was": true,
	"were": true, "are": true, "has": true, "have": true, "had": true, "did": true, "does": true, "not": true,
	"any": true, "all": true, "card": true, "cards": true, "session": true, "sessions": true, "find": true,
	"show": true, "one": true, "ones": true, "into": true, "over": true, "our": true, "you": true, "can": true,
}

// keywordTerms picks the query words worth looking up in conversations.
func keywordTerms(query string) []string {
	var out []string
	for _, t := range searchTerms(query) {
		t = strings.Trim(t, ".,;:!?\"'()[]{}<>")
		if utf8.RuneCountInString(t) < 3 || stopWords[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// searchCorpus describes every card for the AI search, cards that contain
// a query keyword first. Each card carries a few excerpts of its session
// conversation: the opening request, the keyword hits, the latest messages.
func (s *Server) searchCorpus(ctx context.Context, query string) ([]summary.SearchCard, error) {
	cards, err := s.store.ListCards(ctx)
	if err != nil {
		return nil, err
	}
	cols, err := s.columnIndex(ctx)
	if err != nil {
		return nil, err
	}
	terms := keywordTerms(query)
	hits := map[string][]db.ContentHit{}
	for _, term := range terms {
		list, err := s.store.ContentHits(ctx, term)
		if err != nil {
			return nil, err
		}
		for _, h := range list {
			h.Text = snippet(h.Text, term)
			hits[h.SessionID] = append(hits[h.SessionID], h)
		}
	}

	type entry struct {
		card  summary.SearchCard
		score int
		at    string
	}
	entries := make([]entry, 0, len(cards))
	for _, c := range cards {
		s.mu.RLock()
		sess, live := s.sessions[c.AOSessionID]
		s.mu.RUnlock()
		var sessPtr *ao.Session
		agent := ""
		if live && c.AOSessionID != "" {
			sessPtr, agent = &sess, sess.Harness
		}
		e := entry{at: c.UpdatedAt, card: summary.SearchCard{
			ID: c.ID, Title: c.Title, Category: categoryLabels[c.Category], Column: cols.title(c.ColumnID),
			GitHub: githubContext(c), Tags: c.Tags, Summary: c.Summary, Notes: c.Notes,
			SessionID: c.AOSessionID, Agent: agent,
		}}
		hay := cardHaystack(c) + "\n" + sessionHaystack(sessPtr)
		for _, term := range terms {
			if strings.Contains(hay, term) {
				e.score++
			}
		}
		if c.AOSessionID != "" {
			excerpts, err := s.store.ContentExcerpts(ctx, c.AOSessionID, 2)
			if err != nil {
				return nil, err
			}
			used := map[int64]bool{}
			add := func(role, text string) {
				e.card.Excerpts = append(e.card.Excerpts, "["+role+"] "+text)
			}
			if len(excerpts) > 0 && excerpts[0].Role == "user" {
				add("user, first message", excerpts[0].Text)
				used[excerpts[0].Sequence] = true
			}
			for i, h := range hits[c.AOSessionID] {
				e.score += 2
				if i < 3 && !used[h.Sequence] {
					add(h.Role, h.Text)
					used[h.Sequence] = true
				}
			}
			for _, m := range excerpts {
				if !used[m.Sequence] {
					add(m.Role+", latest", m.Text)
					used[m.Sequence] = true
				}
			}
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		return entries[i].at > entries[j].at
	})
	out := make([]summary.SearchCard, len(entries))
	for i, e := range entries {
		out[i] = e.card
	}
	return out, nil
}

type aiSearchResponse struct {
	Query   string                `json:"query"`
	Engine  string                `json:"engine"`
	Cards   int                   `json:"cards"`
	Matches []summary.SearchMatch `json:"matches"`
}

func (s *Server) handleAISearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Query string `json:"query"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	query := strings.Join(strings.Fields(req.Query), " ")
	if query == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "Describe what you are looking for.")
		return
	}
	if runes := []rune(query); len(runes) > maxAIQueryRunes {
		query = string(runes[:maxAIQueryRunes])
	}
	// One model run at a time: each one is slow and costs tokens.
	if !s.aiSearchMu.TryLock() {
		writeError(w, http.StatusConflict, "AI_SEARCH_BUSY", "An AI search is already running. Wait for it to finish.")
		return
	}
	defer s.aiSearchMu.Unlock()

	corpus, err := s.searchCorpus(ctx, query)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	out := aiSearchResponse{Query: query, Cards: len(corpus), Matches: []summary.SearchMatch{}}
	if len(corpus) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	matches, engine, err := s.ai.Search(ctx, summary.SearchInput{Query: query, Cards: corpus})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("ai search failed", "err", err)
		writeError(w, http.StatusBadGateway, "AI_SEARCH_FAILED", capitalize(err.Error())+".")
		return
	}
	out.Engine = engine
	if matches != nil {
		out.Matches = matches
	}
	writeJSON(w, http.StatusOK, out)
}
