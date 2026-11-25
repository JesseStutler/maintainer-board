package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
	"maintainer-board/internal/github"
	"maintainer-board/internal/summary"
)

// fakeAO is an in-memory AO daemon.
type fakeAO struct {
	mu            sync.Mutex
	sessions      []ao.Session
	projects      []ao.Project
	conversations map[string]*ao.Conversation
	candidates    []ao.ImportCandidate
	candidatesErr error
	listErr       error
	resumeErr     error
	spawnErr      error
	sent          []string
	spawned       []ao.SpawnRequest
	imported      [][]string
	killed        []string
	renamed       []string
	nextID        int
	// deleted lists the sessions removed with DeleteSession; deleteErr
	// makes it fail the way AO refuses a delete.
	deleted   []string
	deleteErr error
	// convQueries records every conversation request; latest overrides
	// the latest sequence a session reports.
	convQueries []ao.ConversationQuery
	latest      map[string]int64

	// Model catalogs per agent, model pickers per session, and every
	// settings change received.
	agentModels   map[string]*ao.AgentModels
	convModels    map[string]*ao.ConversationModels
	modelsErr     error
	settingsErr   error
	settingsCalls []string
}

func (f *fakeAO) AgentModels(_ context.Context, agent string) (*ao.AgentModels, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	if m, ok := f.agentModels[agent]; ok {
		return m, nil
	}
	return nil, &ao.APIError{Status: 404, Code: "AGENT_NOT_FOUND", Message: "Unknown agent adapter"}
}

func (f *fakeAO) ConversationModels(_ context.Context, id string) (*ao.ConversationModels, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.convModels[id]; ok {
		out := *m
		return &out, nil
	}
	return nil, &ao.APIError{Status: 404, Code: "SESSION_NOT_FOUND", Message: "session not found"}
}

// UpdateConversationSettings stores whatever it is given, like the real
// daemon: it does not check that the model exists.
func (f *fakeAO) UpdateConversationSettings(_ context.Context, id string, settings ao.ConversationTurnSettings) (*ao.ConversationSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.settingsErr != nil {
		return nil, f.settingsErr
	}
	m, ok := f.convModels[id]
	if !ok {
		return nil, &ao.APIError{Status: 404, Code: "SESSION_NOT_FOUND", Message: "session not found"}
	}
	show := func(v *string) string {
		if v == nil {
			return "-"
		}
		return "[" + *v + "]"
	}
	f.settingsCalls = append(f.settingsCalls, id+" model="+show(settings.Model)+" effort="+show(settings.ReasoningEffort)+" approval="+show(settings.ApprovalMode))
	if settings.Model != nil {
		m.Selected.Model = *settings.Model
	}
	if settings.ReasoningEffort != nil {
		m.Selected.ReasoningEffort = *settings.ReasoningEffort
	}
	out := m.Selected
	return &out, nil
}

func (f *fakeAO) ListSessions(context.Context) ([]ao.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]ao.Session(nil), f.sessions...), nil
}

func (f *fakeAO) ListProjects(context.Context) ([]ao.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects, nil
}

func (f *fakeAO) Spawn(_ context.Context, req ao.SpawnRequest) (*ao.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	f.spawned = append(f.spawned, req)
	f.nextID++
	sess := ao.Session{
		ID: fmt.Sprintf("%s-new-%d", req.ProjectID, f.nextID), ProjectID: req.ProjectID, Kind: req.Kind,
		Harness: req.Harness, Mode: req.Mode, DisplayName: req.DisplayName, Activity: ao.Activity{State: "active"},
	}
	f.sessions = append(f.sessions, sess)
	return &sess, nil
}

func (f *fakeAO) Conversation(_ context.Context, id string, q ao.ConversationQuery) (*ao.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	conv, ok := f.conversations[id]
	if !ok {
		return nil, &ao.APIError{Status: 404, Code: "SESSION_NOT_FOUND", Message: "session not found"}
	}
	f.convQueries = append(f.convQueries, q)
	out := *conv
	out.LatestSequence = int64(q.Limit) // lets tests observe the forwarded limit
	out.OldestSequence = q.BeforeSequence
	if v, ok := f.latest[id]; ok {
		out.LatestSequence = v
	}
	return &out, nil
}

func (f *fakeAO) SendMessage(_ context.Context, id, text, clientID string) (*ao.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, id+"|"+text+"|"+clientID)
	return &ao.SendResult{TurnID: "turn-1", State: "queued"}, nil
}

func (f *fakeAO) Interrupt(context.Context, string) error { return nil }

func (f *fakeAO) Resume(_ context.Context, id string) (*ao.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	for i := range f.sessions {
		if f.sessions[i].ID == id {
			f.sessions[i].NeedsResume = false
			s := f.sessions[i]
			return &s, nil
		}
	}
	return nil, &ao.APIError{Status: 404, Code: "SESSION_NOT_FOUND", Message: "session not found"}
}

func (f *fakeAO) Kill(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, id)
	return nil
}

// DeleteSession removes the session, like AO after a kill and a delete.
func (f *fakeAO) DeleteSession(_ context.Context, id string, cleanup bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, fmt.Sprintf("%s cleanup=%v", id, cleanup))
	kept := f.sessions[:0:0]
	for _, sess := range f.sessions {
		if sess.ID != id {
			kept = append(kept, sess)
		}
	}
	f.sessions = kept
	return nil
}

func (f *fakeAO) Rename(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renamed = append(f.renamed, id+"|"+name)
	return nil
}

func (f *fakeAO) ImportCandidates(context.Context) ([]ao.ImportCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}
	return f.candidates, nil
}

func (f *fakeAO) Import(_ context.Context, ids []string) ([]ao.ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imported = append(f.imported, ids)
	var out []ao.ImportResult
	for _, id := range ids {
		if id == "broken" {
			out = append(out, ao.ImportResult{ID: id, Status: "failed", Error: "unreadable history"})
			continue
		}
		sid := "imported-" + id
		f.sessions = append(f.sessions, ao.Session{ID: sid, Kind: "worker", Harness: "codex", DisplayName: "Imported " + id, NeedsResume: true, Activity: ao.Activity{State: "idle"}})
		out = append(out, ao.ImportResult{ID: id, Status: "created", SessionID: sid})
	}
	return out, nil
}

func (f *fakeAO) Events(ctx context.Context, _ string, _ func(ao.Event)) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeAO) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// fakeGitHub serves items by "repo#number".
type fakeGitHub struct {
	mu    sync.Mutex
	items map[string]*github.Item
	err   error
	kinds []string
	// bodies holds the description of an item by "repo#number".
	bodies map[string]string
}

func (f *fakeGitHub) Description(_ context.Context, repo string, number int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.bodies[fmt.Sprintf("%s#%d", repo, number)]
	if !ok {
		return "", github.ErrNotFound
	}
	return body, nil
}

func (f *fakeGitHub) Fetch(_ context.Context, repo string, number int, knownKind string) (*github.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kinds = append(f.kinds, knownKind)
	if f.err != nil {
		return nil, f.err
	}
	item, ok := f.items[fmt.Sprintf("%s#%d", repo, number)]
	if !ok {
		return nil, github.ErrNotFound
	}
	out := *item
	return &out, nil
}

func (f *fakeGitHub) update(key string, fn func(s *github.Snapshot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.items[key].Snapshot)
}

func (f *fakeGitHub) updateItem(key string, fn func(it *github.Item)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.items[key])
}

type fakeAI struct {
	text  string
	err   error
	input summary.Input
	calls int

	// AI search: the answer to give, and what was asked.
	searchMatches []summary.SearchMatch
	searchErr     error
	searchInput   summary.SearchInput
	searchCalls   int
	// searchGate, when set, blocks Search until it is closed.
	searchGate    chan struct{}
	searchStarted chan struct{}

	// AI history search: the answer to give, and what was asked.
	historyMatches []summary.HistoryMatch
	historyErr     error
	historyInput   summary.HistoryInput
	historyCalls   int
}

func (f *fakeAI) SearchHistory(_ context.Context, in summary.HistoryInput) ([]summary.HistoryMatch, string, error) {
	f.historyCalls++
	f.historyInput = in
	if f.searchStarted != nil {
		close(f.searchStarted)
	}
	if f.searchGate != nil {
		<-f.searchGate
	}
	return f.historyMatches, "gemini-3.8-flash", f.historyErr
}

func (f *fakeAI) Search(_ context.Context, in summary.SearchInput) ([]summary.SearchMatch, string, error) {
	f.searchCalls++
	f.searchInput = in
	if f.searchStarted != nil {
		close(f.searchStarted)
	}
	if f.searchGate != nil {
		<-f.searchGate
	}
	return f.searchMatches, "codex", f.searchErr
}

func (f *fakeAI) Summarize(_ context.Context, in summary.Input) (string, string, error) {
	f.calls++
	f.input = in
	return f.text, "copilot", f.err
}

type harness struct {
	t     *testing.T
	srv   *Server
	store *db.Store
	ao    *fakeAO
	gh    *fakeGitHub
	ai    *fakeAI
	h     http.Handler
}

func pr6247() *github.Item {
	return &github.Item{
		Repo: "volcano-sh/volcano", Number: 6247, Kind: github.KindPR,
		Title: "fix(scheduler): CSI limits", URL: "https://github.com/volcano-sh/volcano/pull/6247", Author: "mohit",
		Labels: []string{"kind/bug"},
		Snapshot: github.Snapshot{
			State: github.StateOpen, CI: github.CISuccess, ReviewDecision: github.ReviewRequired,
			HeadSHA: "aaa", Commits: 1, Comments: 7, ReviewComments: 1, UpdatedAt: "2026-11-17T08:45:40Z",
		},
		UnresolvedThreads: -1,
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	hn := &harness{
		t:     t,
		store: store,
		ao: &fakeAO{
			projects:      []ao.Project{{ID: "volcano", Name: "volcano"}},
			conversations: map[string]*ao.Conversation{},
		},
		gh: &fakeGitHub{items: map[string]*github.Item{
			"volcano-sh/volcano#6247": pr6247(),
			"volcano-sh/volcano#6100": {
				Repo: "volcano-sh/volcano", Number: 6100, Kind: github.KindIssue, Title: "scheduler panics",
				URL:      "https://github.com/volcano-sh/volcano/issues/6100",
				Snapshot: github.Snapshot{State: github.StateOpen, CI: github.CIUnknown, Comments: 2}, UnresolvedThreads: -1,
			},
		}},
		ai: &fakeAI{text: "Objective: Review.\nStatus: Waiting."},
	}
	web := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Maintainer Board</title>")}}
	hn.srv = New(Config{DefaultRepo: "volcano-sh/volcano", AOURL: "http://ao.test"}, store, hn.ao, hn.gh, hn.ai, web, nil)
	hn.h = hn.srv.Handler()
	return hn
}

// call performs a same-origin JSON request and decodes the response.
func (hn *harness) call(method, path string, body any) (int, map[string]any) {
	hn.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			hn.t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:3012"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	hn.h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			hn.t.Fatalf("%s %s: response is not a JSON object: %q", method, path, rec.Body.String())
		}
	}
	return rec.Code, out
}

// must is call plus a status assertion.
func (hn *harness) must(want int, method, path string, body any) map[string]any {
	hn.t.Helper()
	status, out := hn.call(method, path, body)
	if status != want {
		hn.t.Fatalf("%s %s = %d %v, want %d", method, path, status, out, want)
	}
	return out
}

func (hn *harness) errorCode(want int, method, path string, body any) string {
	hn.t.Helper()
	out := hn.must(want, method, path, body)
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	if msg, _ := e["message"].(string); code == "" || msg == "" {
		hn.t.Fatalf("%s %s: error response lacks code or message: %v", method, path, out)
	}
	return code
}

// syncAO runs one AO reconciliation and waits for the GitHub syncs it starts.
func (hn *harness) syncAO() {
	hn.t.Helper()
	hn.srv.syncAO(context.Background())
	hn.srv.bg.Wait()
}

func (hn *harness) card(id string) *db.Card {
	hn.t.Helper()
	c, err := hn.store.GetCard(context.Background(), id)
	if err != nil {
		hn.t.Fatal(err)
	}
	return c
}

func (hn *harness) cardBySession(id string) *db.Card {
	hn.t.Helper()
	c, err := hn.store.CardBySession(context.Background(), id)
	if err != nil {
		hn.t.Fatal(err)
	}
	return c
}

func (hn *harness) eventMessages(cardID string) []string {
	hn.t.Helper()
	events, err := hn.store.ListEvents(context.Background(), cardID, 0)
	if err != nil {
		hn.t.Fatal(err)
	}
	var out []string
	for i := len(events) - 1; i >= 0; i-- {
		out = append(out, events[i].Message)
	}
	return out
}

// newLinkedCard creates a card in column linked to volcano-sh/volcano#6247.
func (hn *harness) newLinkedCard(column string) string {
	hn.t.Helper()
	out := hn.must(201, "POST", "/api/cards", map[string]any{"title": "Review CSI", "columnId": column, "githubRef": "#6247"})
	return out["card"].(map[string]any)["id"].(string)
}

func worker(id, harnessName, name string) ao.Session {
	return ao.Session{ID: id, ProjectID: "volcano", Kind: "worker", Harness: harnessName, DisplayName: name, Mode: "chat", Activity: ao.Activity{State: "idle", LastActivityAt: "t1"}}
}

func TestBoardEndpoint(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "Design doc")}
	hn.syncAO()
	out := hn.must(200, "GET", "/api/board", nil)

	cols := out["columns"].([]any)
	if len(cols) != 6 || cols[3].(map[string]any)["role"] != "updates" {
		t.Fatalf("columns = %v", cols)
	}
	aoStatus := out["ao"].(map[string]any)
	if aoStatus["connected"] != true || aoStatus["defaultProject"] != "volcano" {
		t.Fatalf("ao status = %v", aoStatus)
	}
	settings := out["settings"].(map[string]any)
	if settings["defaultRepo"] != "volcano-sh/volcano" || settings["showOrchestrator"] != false {
		t.Fatalf("settings = %v", settings)
	}
	cards := out["cards"].([]any)
	if len(cards) != 1 {
		t.Fatalf("cards = %v", cards)
	}
	card := cards[0].(map[string]any)
	session := card["session"].(map[string]any)
	if card["aoSessionId"] != "volcano-2" || session["harness"] != "copilot" || card["sessionMissing"] != false {
		t.Fatalf("card = %v", card)
	}
	// Arrays are never null so the UI can iterate without guards.
	if _, ok := card["tags"].([]any); !ok {
		t.Fatalf("tags = %#v, want an array", card["tags"])
	}
}

func TestAOSyncCreatesCardsForWorkerSessions(t *testing.T) {
	hn := newHarness(t)
	terminated := worker("volcano-9", "copilot", "Old work")
	terminated.IsTerminated = true
	withPR := worker("volcano-5", "copilot", "Fix flaky e2e")
	withPR.PRs = []ao.SessionPR{{URL: "https://github.com/volcano-sh/volcano/pull/6247", Number: 6247}}
	hn.ao.sessions = []ao.Session{
		{ID: "volcano-1", Kind: "orchestrator", Harness: "copilot"},
		worker("volcano-2", "copilot", "[\u8bbe\u8ba1] Kubeflow Trainer"),
		worker("standalone-1", "codex", "[PR #6247][Review] CSI NodeVolumeLimits"),
		worker("volcano-4", "codex", "Look into issue #99999 crash"),
		withPR,
		terminated,
	}
	hn.syncAO()

	if c := hn.cardBySession("volcano-1"); c != nil {
		t.Fatal("an orchestrator session got a card by default")
	}
	if c := hn.cardBySession("volcano-9"); c != nil {
		t.Fatal("a terminated session got a card")
	}

	design := hn.cardBySession("volcano-2")
	if design == nil || design.ColumnID != "inbox" || design.Category != db.CategoryDesign || design.Origin != db.OriginAO || !design.AutoMove || design.GHNumber != 0 {
		t.Fatalf("design card = %+v", design)
	}

	review := hn.cardBySession("standalone-1")
	if review == nil || review.Category != db.CategoryPRReview {
		t.Fatalf("review card = %+v", review)
	}
	if review.GHRepo != "volcano-sh/volcano" || review.GHNumber != 6247 || review.GHKind != "pr" || review.GHState != "OPEN" || review.AckAt == "" || review.HasUnreadUpdates {
		t.Fatalf("review card was not linked with a baseline: %+v", review)
	}

	// A reference that does not exist on GitHub is dropped again.
	missing := hn.cardBySession("volcano-4")
	if missing == nil || missing.Category != db.CategoryIssue || missing.GHNumber != 0 {
		t.Fatalf("card with a dangling reference = %+v", missing)
	}
	if msgs := hn.eventMessages(missing.ID); !reflect.DeepEqual(msgs[len(msgs)-1:], []string{"Removed link to volcano-sh/volcano#99999: not found on GitHub"}) {
		t.Fatalf("events = %v", msgs)
	}

	// The PR AO attached to the session wins over title parsing.
	if c := hn.cardBySession("volcano-5"); c == nil || c.GHNumber != 6247 {
		t.Fatalf("session PR was not linked: %+v", c)
	}

	// A second sync creates nothing new.
	before, _ := hn.store.ListCards(context.Background())
	hn.syncAO()
	after, _ := hn.store.ListCards(context.Background())
	if len(before) != 4 || len(after) != 4 {
		t.Fatalf("card count went from %d to %d, want 4 both times", len(before), len(after))
	}
}

func TestShowOrchestratorSetting(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{{ID: "volcano-1", Kind: "orchestrator", Harness: "copilot"}}
	hn.syncAO()
	if hn.cardBySession("volcano-1") != nil {
		t.Fatal("orchestrator card created while the setting is off")
	}
	out := hn.must(200, "PATCH", "/api/settings", map[string]any{"showOrchestrator": true})
	if out["showOrchestrator"] != true {
		t.Fatalf("settings = %v", out)
	}
	hn.syncAO()
	c := hn.cardBySession("volcano-1")
	if c == nil || c.Title != "Orchestrator volcano-1" || c.Origin != db.OriginAO {
		t.Fatalf("orchestrator card = %+v", c)
	}
}

func TestDeletedAndUnlinkedSessionsDoNotComeBack(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "A"), worker("volcano-3", "codex", "B")}
	hn.syncAO()

	a := hn.cardBySession("volcano-2")
	hn.must(200, "DELETE", "/api/cards/"+a.ID, nil)
	b := hn.cardBySession("volcano-3")
	hn.must(200, "PATCH", "/api/cards/"+b.ID, map[string]any{"aoSessionId": ""})

	hn.syncAO()
	if hn.cardBySession("volcano-2") != nil || hn.cardBySession("volcano-3") != nil {
		t.Fatal("AO sync recreated a card the user removed or unlinked")
	}

	// Linking a session by hand is allowed again and lifts the block.
	hn.must(200, "PATCH", "/api/cards/"+b.ID, map[string]any{"aoSessionId": "volcano-2"})
	if c := hn.cardBySession("volcano-2"); c == nil || c.ID != b.ID {
		t.Fatalf("manual link failed: %+v", c)
	}
	other := hn.must(201, "POST", "/api/cards", map[string]any{"title": "other"})["card"].(map[string]any)["id"].(string)
	if code := hn.errorCode(409, "PATCH", "/api/cards/"+other, map[string]any{"aoSessionId": "volcano-2"}); code != "SESSION_ALREADY_LINKED" {
		t.Fatalf("code = %s", code)
	}
}

func TestCardTitleFollowsAORenameUntilEdited(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "Integrate Kubeflow Trainer")}
	hn.syncAO()
	id := hn.cardBySession("volcano-2").ID

	hn.ao.set(func() { hn.ao.sessions[0].DisplayName = "[Design] Kubeflow Trainer" })
	hn.syncAO()
	if got := hn.card(id).Title; got != "[Design] Kubeflow Trainer" {
		t.Fatalf("title = %q, want the AO rename", got)
	}

	hn.must(200, "PATCH", "/api/cards/"+id, map[string]any{"title": "My own title"})
	hn.ao.set(func() { hn.ao.sessions[0].DisplayName = "Renamed again in AO" })
	hn.syncAO()
	if got := hn.card(id).Title; got != "My own title" {
		t.Fatalf("title = %q, a manual title must not be overwritten", got)
	}
}

func TestAutoSummaryFromConversation(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "Design doc")}
	hn.ao.conversations["volcano-2"] = &ao.Conversation{Messages: []ao.Message{
		{Role: "user", Text: "compare the two designs"},
		{Role: "assistant", Text: "## Result\n\nBoth documents are assembled and pushed; the **diagrams** render correctly."},
		{Role: "assistant", Text: "still typing", Streaming: true},
	}}
	hn.syncAO()
	id := hn.cardBySession("volcano-2").ID
	c := hn.card(id)
	if c.Summary != "Result Both documents are assembled and pushed; the diagrams render correctly." || c.SummarySource != db.SummaryAuto {
		t.Fatalf("summary = %q (%s)", c.Summary, c.SummarySource)
	}

	// A manual summary is never replaced by extraction...
	hn.must(200, "PATCH", "/api/cards/"+id, map[string]any{"summary": "Mine"})
	hn.ao.set(func() {
		hn.ao.sessions[0].Activity.LastActivityAt = "t2"
		hn.ao.conversations["volcano-2"].Messages[1].Text = "A completely different and much newer final answer from the agent."
	})
	hn.srv.summaryStamp = map[string]summaryMark{}
	hn.syncAO()
	if c := hn.card(id); c.Summary != "Mine" || c.SummarySource != db.SummaryManual {
		t.Fatalf("manual summary was replaced: %q (%s)", c.Summary, c.SummarySource)
	}

	// ...until it is cleared, which hands control back.
	hn.must(200, "PATCH", "/api/cards/"+id, map[string]any{"summary": ""})
	hn.srv.summaryStamp = map[string]summaryMark{}
	hn.syncAO()
	if c := hn.card(id); c.Summary != "A completely different and much newer final answer from the agent." || c.SummarySource != db.SummaryAuto {
		t.Fatalf("summary after clearing = %q (%s)", c.Summary, c.SummarySource)
	}
}

func TestAOOfflineIsReported(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "A")}
	hn.syncAO()
	hn.ao.set(func() { hn.ao.listErr = fmt.Errorf("%w: connection refused", ao.ErrUnavailable) })
	hn.syncAO()

	out := hn.must(200, "GET", "/api/board", nil)
	status := out["ao"].(map[string]any)
	if status["connected"] != false || !strings.Contains(status["error"].(string), "http://ao.test") {
		t.Fatalf("ao status = %v", status)
	}
	// Cards and the last known session state stay visible while AO is down.
	if cards := out["cards"].([]any); len(cards) != 1 || cards[0].(map[string]any)["session"] == nil {
		t.Fatalf("cards while offline = %v", cards)
	}
}

func TestCreateCard(t *testing.T) {
	hn := newHarness(t)

	// The title and category default from the linked item.
	out := hn.must(201, "POST", "/api/cards", map[string]any{"githubRef": "https://github.com/volcano-sh/volcano/issues/6100", "tags": []string{"scheduler"}})
	card := out["card"].(map[string]any)
	if card["title"] != "scheduler panics" || card["category"] != "ISSUE" || card["ghKind"] != "issue" || card["columnId"] != "inbox" || card["origin"] != "manual" {
		t.Fatalf("card = %v", card)
	}
	if len(out["warnings"].([]any)) != 0 {
		t.Fatalf("warnings = %v", out["warnings"])
	}

	out = hn.must(201, "POST", "/api/cards", map[string]any{"title": "Design the queue API", "columnId": "waiting", "summary": "Mine", "autoMove": false})
	card = out["card"].(map[string]any)
	if card["category"] != "DESIGN" || card["columnId"] != "waiting" || card["summarySource"] != "manual" || card["autoMove"] != false {
		t.Fatalf("card = %v", card)
	}

	for name, tc := range map[string]struct {
		body map[string]any
		code int
		want string
	}{
		"no title":         {map[string]any{"tags": []string{"x"}}, 400, "INVALID_INPUT"},
		"bad category":     {map[string]any{"title": "x", "category": "BUG"}, 400, "INVALID_INPUT"},
		"bad reference":    {map[string]any{"title": "x", "githubRef": "not-a-ref"}, 400, "INVALID_GITHUB_REF"},
		"missing on GH":    {map[string]any{"title": "x", "githubRef": "#424242"}, 404, "GITHUB_NOT_FOUND"},
		"unknown column":   {map[string]any{"title": "x", "columnId": "nope"}, 404, "NOT_FOUND"},
		"bad agent":        {map[string]any{"title": "x", "agent": map[string]any{"harness": "claude"}}, 400, "INVALID_INPUT"},
		"agent and linked": {map[string]any{"title": "x", "aoSessionId": "s", "agent": map[string]any{"harness": "codex"}}, 400, "INVALID_INPUT"},
	} {
		if got := hn.errorCode(tc.code, "POST", "/api/cards", tc.body); got != tc.want {
			t.Errorf("%s: code = %s, want %s", name, got, tc.want)
		}
	}

	// GitHub being down does not block a titled card; the link is retried.
	hn.gh.err = fmt.Errorf("%w: network down", github.ErrUnavailable)
	out = hn.must(201, "POST", "/api/cards", map[string]any{"title": "Offline link", "githubRef": "#6247"})
	if card := out["card"].(map[string]any); card["ghNumber"] != float64(6247) || card["ghLastSyncedAt"] != "" || len(out["warnings"].([]any)) != 1 {
		t.Fatalf("offline link = %v", out)
	}
	if code := hn.errorCode(502, "POST", "/api/cards", map[string]any{"githubRef": "#6247"}); code != "GITHUB_UNAVAILABLE" {
		t.Errorf("untitled card while GitHub is down: code = %s", code)
	}
}

func TestCreateCardWithAgentSession(t *testing.T) {
	hn := newHarness(t)
	out := hn.must(201, "POST", "/api/cards", map[string]any{
		"title": "Fix flaky e2e", "agent": map[string]any{"harness": "codex", "prompt": "  investigate  "},
	})
	card := out["card"].(map[string]any)
	want := ao.SpawnRequest{ProjectID: "volcano", Harness: "codex", Mode: "chat", Kind: "worker", DisplayName: "Fix flaky e2e", DisplayNameExplicit: true, Prompt: "investigate"}
	if len(hn.ao.spawned) != 1 || hn.ao.spawned[0] != want {
		t.Fatalf("spawn requests = %+v, want %+v", hn.ao.spawned, want)
	}
	if card["aoSessionId"] != "volcano-new-1" || card["session"].(map[string]any)["harness"] != "codex" {
		t.Fatalf("card = %v", card)
	}
	id := card["id"].(string)

	// The sync that follows must not create a second card for the session.
	hn.syncAO()
	cards, _ := hn.store.ListCards(context.Background())
	if len(cards) != 1 {
		t.Fatalf("%d cards after sync, want 1", len(cards))
	}
	if code := hn.errorCode(409, "POST", "/api/cards/"+id+"/session", map[string]any{"harness": "copilot"}); code != "CONFLICT" {
		t.Fatalf("second session: code = %s", code)
	}

	// A failed spawn still leaves the card, with a warning.
	hn.ao.spawnErr = &ao.APIError{Status: 500, Code: "RUNTIME_CREATE_FAILED", Message: "worktree failed"}
	out = hn.must(201, "POST", "/api/cards", map[string]any{"title": "Second", "agent": map[string]any{"harness": "copilot"}})
	if warnings := out["warnings"].([]any); len(warnings) != 1 || !strings.Contains(warnings[0].(string), "Worktree failed") {
		t.Fatalf("warnings = %v", out["warnings"])
	}
	if out["card"].(map[string]any)["aoSessionId"] != "" {
		t.Fatalf("card claims a session: %v", out["card"])
	}
}

func TestPatchCard(t *testing.T) {
	hn := newHarness(t)
	id := hn.must(201, "POST", "/api/cards", map[string]any{"title": "Task"})["card"].(map[string]any)["id"].(string)

	out := hn.must(200, "PATCH", "/api/cards/"+id, map[string]any{
		"title": "  Renamed  ", "category": "TRIAGE", "tags": []string{"a", "a", "b"}, "notes": "## n",
		"summary": "Hand written", "autoMove": false, "columnId": "waiting",
	})
	if out["title"] != "Renamed" || out["category"] != "TRIAGE" || out["notes"] != "## n" || out["summary"] != "Hand written" ||
		out["summarySource"] != "manual" || out["autoMove"] != false || out["columnId"] != "waiting" ||
		!reflect.DeepEqual(out["tags"], []any{"a", "b"}) {
		t.Fatalf("patched card = %v", out)
	}
	msgs := hn.eventMessages(id)
	want := []string{"Card created in Inbox", "Auto-move disabled", "Summary edited manually", "Moved from Inbox to Waiting"}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("events = %v, want %v", msgs, want)
	}

	// An unchanged patch records nothing.
	hn.must(200, "PATCH", "/api/cards/"+id, map[string]any{"summary": "Hand written", "autoMove": false, "columnId": "waiting"})
	if got := hn.eventMessages(id); len(got) != len(want) {
		t.Fatalf("no-op patch added events: %v", got)
	}

	if code := hn.errorCode(400, "PATCH", "/api/cards/"+id, map[string]any{"title": "  "}); code != "INVALID_INPUT" {
		t.Errorf("empty title: code = %s", code)
	}
	if code := hn.errorCode(400, "PATCH", "/api/cards/"+id, map[string]any{"category": "NOPE"}); code != "INVALID_INPUT" {
		t.Errorf("bad category: code = %s", code)
	}
	if code := hn.errorCode(404, "PATCH", "/api/cards/"+id, map[string]any{"columnId": "nope"}); code != "NOT_FOUND" {
		t.Errorf("bad column: code = %s", code)
	}
	if code := hn.errorCode(404, "PATCH", "/api/cards/missing", map[string]any{"title": "x"}); code != "NOT_FOUND" {
		t.Errorf("missing card: code = %s", code)
	}
	if code := hn.errorCode(400, "PATCH", "/api/cards/"+id, "not an object"); code != "INVALID_JSON" {
		t.Errorf("bad body: code = %s", code)
	}
}

func TestMoveEndpoint(t *testing.T) {
	hn := newHarness(t)
	ids := map[string]string{}
	for _, title := range []string{"c", "b", "a"} {
		ids[title] = hn.must(201, "POST", "/api/cards", map[string]any{"title": title})["card"].(map[string]any)["id"].(string)
	}
	out := hn.must(200, "PATCH", "/api/cards/"+ids["a"]+"/move", map[string]any{"columnId": "inbox", "position": 2})
	if out["position"] != float64(2) {
		t.Fatalf("moved card = %v", out)
	}
	out = hn.must(200, "PATCH", "/api/cards/"+ids["c"]+"/move", map[string]any{"columnId": "waiting", "position": 0})
	if out["columnId"] != "waiting" || out["position"] != float64(0) {
		t.Fatalf("moved card = %v", out)
	}
	var inbox []string
	cards, _ := hn.store.ListCards(context.Background())
	for _, c := range cards {
		if c.ColumnID == "inbox" {
			inbox = append(inbox, c.Title)
		}
	}
	if !reflect.DeepEqual(inbox, []string{"b", "a"}) {
		t.Fatalf("inbox = %v", inbox)
	}
	if code := hn.errorCode(400, "PATCH", "/api/cards/"+ids["a"]+"/move", map[string]any{"position": 1}); code != "INVALID_INPUT" {
		t.Errorf("missing column: code = %s", code)
	}
	if code := hn.errorCode(404, "PATCH", "/api/cards/"+ids["a"]+"/move", map[string]any{"columnId": "nope"}); code != "NOT_FOUND" {
		t.Errorf("unknown column: code = %s", code)
	}
}

func TestColumnEndpoints(t *testing.T) {
	hn := newHarness(t)
	col := hn.must(201, "POST", "/api/columns", map[string]any{"title": "Backlog", "color": "#123456"})
	id := col["id"].(string)
	if col["position"] != float64(6) || col["role"] != "" {
		t.Fatalf("column = %v", col)
	}
	hn.must(200, "PATCH", "/api/columns/"+id, map[string]any{"title": "Later", "role": "waiting"})
	if c, _ := hn.store.GetColumn(context.Background(), "waiting"); c.Role != "" {
		t.Fatal("the waiting role was not handed over")
	}
	cardID := hn.must(201, "POST", "/api/cards", map[string]any{"title": "x", "columnId": id})["card"].(map[string]any)["id"].(string)

	out := hn.must(200, "POST", "/api/columns/reorder", map[string]any{"ids": []string{id, "inbox"}})
	if first := out["columns"].([]any)[0].(map[string]any); first["id"] != id {
		t.Fatalf("reordered columns = %v", out["columns"])
	}

	out = hn.must(200, "DELETE", "/api/columns/"+id, nil)
	if out["movedCardsTo"].(map[string]any)["id"] != "inbox" || hn.card(cardID).ColumnID != "inbox" {
		t.Fatalf("delete = %v, card in %s", out, hn.card(cardID).ColumnID)
	}

	if code := hn.errorCode(400, "POST", "/api/columns", map[string]any{"title": "x", "color": "blue"}); code != "INVALID_INPUT" {
		t.Errorf("bad color: code = %s", code)
	}
	for _, c := range []string{"in_progress", "waiting", "updates", "ready", "done"} {
		hn.must(200, "DELETE", "/api/columns/"+c, nil)
	}
	if code := hn.errorCode(409, "DELETE", "/api/columns/inbox", nil); code != "LAST_COLUMN" {
		t.Errorf("last column: code = %s", code)
	}
}

func TestGitHubUpdateLifecycle(t *testing.T) {
	hn := newHarness(t)
	const key = "volcano-sh/volcano#6247"
	id := hn.newLinkedCard("waiting")
	c := hn.card(id)
	if c.GHKind != "pr" || c.GHTitle != "fix(scheduler): CSI limits" || c.AckCommentCount != 7 || c.HasUnreadUpdates || c.ColumnID != "waiting" {
		t.Fatalf("freshly linked card = %+v", c)
	}

	sync := func() map[string]any {
		t.Helper()
		return hn.must(200, "POST", "/api/cards/"+id+"/sync-github", nil)
	}

	// Nothing changed: stays in Waiting, and the stored kind is reused.
	if out := sync(); out["newUpdate"] != false || out["moved"] != false {
		t.Fatalf("idle sync = %v", out)
	}
	if last := hn.gh.kinds[len(hn.gh.kinds)-1]; last != "pr" {
		t.Fatalf("sync passed kind %q, want the stored kind", last)
	}

	// New commit and comment: Waiting -> Updates.
	hn.gh.update(key, func(s *github.Snapshot) { s.Commits, s.HeadSHA, s.Comments, s.CI = 2, "bbb", 8, github.CIPending })
	out := sync()
	if out["newUpdate"] != true || out["moved"] != true || out["movedToRole"] != "updates" {
		t.Fatalf("first update = %v", out)
	}
	c = hn.card(id)
	if c.ColumnID != "updates" || !c.HasUnreadUpdates || c.UnreadUpdates != 2 || c.UpdateCycles != 1 || c.UpdateSummary != "+1 commit, +1 comment" {
		t.Fatalf("after first update = %+v", c)
	}

	// The same state polled again is not a new update.
	if out := sync(); out["newUpdate"] != false {
		t.Fatalf("repeat poll = %v", out)
	}
	if c = hn.card(id); c.UpdateCycles != 1 {
		t.Fatalf("cycles after repeat poll = %d", c.UpdateCycles)
	}

	// A second push is counted as another cycle.
	hn.gh.update(key, func(s *github.Snapshot) { s.Commits, s.HeadSHA = 3, "ccc" })
	sync()
	if c = hn.card(id); c.UpdateCycles != 2 || c.UnreadUpdates != 3 || c.UpdateSummary != "+2 commits, +1 comment" {
		t.Fatalf("after second push = %+v", c)
	}
	if n := hn.must(200, "GET", "/api/board", nil)["unreadCount"]; n != float64(1) {
		t.Fatalf("unreadCount = %v", n)
	}

	// Mark reviewed and send back to Waiting in one call.
	acked := hn.must(200, "POST", "/api/cards/"+id+"/ack", map[string]any{"moveToColumnId": "waiting"})
	if acked["hasUnreadUpdates"] != false || acked["unreadUpdates"] != float64(0) || acked["updateCycles"] != float64(0) ||
		acked["updateSummary"] != "" || acked["columnId"] != "waiting" || acked["ackCommitCount"] != float64(3) || acked["ackHeadSha"] != "ccc" {
		t.Fatalf("acked card = %v", acked)
	}
	if out := sync(); out["newUpdate"] != false || hn.card(id).ColumnID != "waiting" {
		t.Fatalf("sync right after ack = %v", out)
	}

	// Approved with green CI: Waiting -> Ready.
	hn.gh.update(key, func(s *github.Snapshot) { s.CI, s.ReviewDecision = github.CISuccess, github.ReviewApproved })
	sync()
	if c = hn.card(id); c.ColumnID != "ready" || c.UpdateSummary != "Approved, CI passed" {
		t.Fatalf("after approval = %+v", c)
	}

	// Merged: -> Done, with nothing left unread.
	hn.gh.update(key, func(s *github.Snapshot) { s.State = github.StateMerged })
	sync()
	if c = hn.card(id); c.ColumnID != "done" || c.HasUnreadUpdates || c.UpdateSummary != "Merged" || c.GHState != "MERGED" {
		t.Fatalf("after merge = %+v", c)
	}

	want := []string{
		"Card created in Waiting",
		"Linked pull request volcano-sh/volcano#6247 (open); baseline recorded",
		"Update detected: +1 commit, +1 comment",
		"Auto-moved from Waiting to Updates",
		"Update detected (2x since last review): +2 commits, +1 comment",
		"Marked caught up; moved from Updates to Waiting",
		"Update detected: Approved, CI passed",
		"Auto-moved from Waiting to Ready",
		"Merged",
		"Auto-moved from Ready to Done",
	}
	if got := hn.eventMessages(id); !reflect.DeepEqual(got, want) {
		t.Fatalf("timeline:\n got %q\nwant %q", got, want)
	}
}

func TestAutoMoveOffKeepsTheColumn(t *testing.T) {
	hn := newHarness(t)
	id := hn.newLinkedCard("waiting")
	hn.must(200, "PATCH", "/api/cards/"+id, map[string]any{"autoMove": false})
	hn.gh.update("volcano-sh/volcano#6247", func(s *github.Snapshot) { s.Comments = 9 })
	out := hn.must(200, "POST", "/api/cards/"+id+"/sync-github", nil)
	if out["newUpdate"] != true || out["moved"] != false {
		t.Fatalf("sync = %v", out)
	}
	if c := hn.card(id); c.ColumnID != "waiting" || !c.HasUnreadUpdates || c.UpdateSummary != "+2 comments" {
		t.Fatalf("card = %+v", c)
	}
}

func TestBoardWideGitHubSync(t *testing.T) {
	hn := newHarness(t)
	waiting := hn.newLinkedCard("waiting")
	issue := hn.must(201, "POST", "/api/cards", map[string]any{"githubRef": "#6100", "columnId": "in_progress"})["card"].(map[string]any)["id"].(string)
	hn.must(201, "POST", "/api/cards", map[string]any{"title": "unlinked"})

	hn.gh.update("volcano-sh/volcano#6247", func(s *github.Snapshot) { s.ReviewComments = 4 })
	hn.gh.update("volcano-sh/volcano#6100", func(s *github.Snapshot) { s.State = github.StateClosed })
	report := hn.must(200, "POST", "/api/github/sync", nil)
	if report["total"] != float64(2) || report["synced"] != float64(2) || report["updated"] != float64(2) || report["moved"] != float64(2) || report["failed"] != float64(0) {
		t.Fatalf("report = %v", report)
	}
	if hn.card(waiting).ColumnID != "updates" || hn.card(issue).ColumnID != "done" {
		t.Fatalf("columns = %s, %s", hn.card(waiting).ColumnID, hn.card(issue).ColumnID)
	}

	// The background loop leaves finished items alone; a manual sync does not.
	hn.gh.mu.Lock()
	hn.gh.kinds = nil
	hn.gh.mu.Unlock()
	bg := hn.srv.syncGitHub(context.Background(), false)
	if bg.Synced != 1 || bg.Skipped != 1 {
		t.Fatalf("background report = %+v, want the closed issue skipped", bg)
	}

	// Failures are reported per item and recorded on the card.
	hn.gh.err = fmt.Errorf("%w: gh api: rate limit exceeded", github.ErrUnavailable)
	report = hn.must(200, "POST", "/api/github/sync", nil)
	if report["failed"] != float64(2) || len(report["errors"].([]any)) != 2 {
		t.Fatalf("failing report = %v", report)
	}
	if c := hn.card(waiting); !strings.Contains(c.GHError, "rate limit exceeded") || c.ColumnID != "updates" {
		t.Fatalf("card after a failed sync = %+v", c)
	}
	status := hn.must(200, "GET", "/api/board", nil)["github"].(map[string]any)
	if status["syncing"] != false || !strings.Contains(status["error"].(string), "rate limit exceeded") {
		t.Fatalf("github status = %v", status)
	}
	hn.gh.err = nil
	hn.must(200, "POST", "/api/cards/"+waiting+"/sync-github", nil)
	if c := hn.card(waiting); c.GHError != "" {
		t.Fatalf("error not cleared after a good sync: %q", c.GHError)
	}
}

func TestLinkUnlinkAndAckErrors(t *testing.T) {
	hn := newHarness(t)
	id := hn.must(201, "POST", "/api/cards", map[string]any{"title": "Task"})["card"].(map[string]any)["id"].(string)

	for _, path := range []string{"/sync-github", "/ack"} {
		if code := hn.errorCode(409, "POST", "/api/cards/"+id+path, nil); code != "NOT_LINKED" {
			t.Errorf("%s on an unlinked card: code = %s", path, code)
		}
	}
	if code := hn.errorCode(400, "POST", "/api/cards/"+id+"/link-github", map[string]any{"ref": "nonsense"}); code != "INVALID_GITHUB_REF" {
		t.Errorf("bad ref: code = %s", code)
	}
	if code := hn.errorCode(404, "POST", "/api/cards/"+id+"/link-github", map[string]any{"ref": "#31337"}); code != "GITHUB_NOT_FOUND" {
		t.Errorf("missing item: code = %s", code)
	}
	if c := hn.card(id); c.GHNumber != 0 {
		t.Fatalf("a failed link left the card linked: %+v", c)
	}

	out := hn.must(200, "POST", "/api/cards/"+id+"/link-github", map[string]any{"ref": "volcano-sh/volcano#6247"})
	if out["ghNumber"] != float64(6247) || out["ghKind"] != "pr" || out["ackAt"] == "" {
		t.Fatalf("linked = %v", out)
	}

	// Relinking to another item starts from a clean baseline.
	hn.gh.update("volcano-sh/volcano#6247", func(s *github.Snapshot) { s.Comments = 20 })
	hn.must(200, "POST", "/api/cards/"+id+"/sync-github", nil)
	out = hn.must(200, "POST", "/api/cards/"+id+"/link-github", map[string]any{"ref": "#6100"})
	if out["ghNumber"] != float64(6100) || out["ghKind"] != "issue" || out["hasUnreadUpdates"] != false || out["ackCommentCount"] != float64(2) || out["ghHeadSha"] != "" {
		t.Fatalf("relinked = %v", out)
	}

	out = hn.must(200, "POST", "/api/cards/"+id+"/unlink-github", nil)
	if out["ghNumber"] != float64(0) || out["ghRepo"] != "" || out["ghState"] != "" || out["ackAt"] != "" {
		t.Fatalf("unlinked = %v", out)
	}
	if code := hn.errorCode(404, "POST", "/api/cards/"+id+"/ack", map[string]any{"moveToColumnId": "nope"}); code != "NOT_FOUND" {
		t.Errorf("ack with unknown column: code = %s", code)
	}

	// A link that has not synced yet has no state to acknowledge.
	hn.gh.err = fmt.Errorf("%w: offline", github.ErrUnavailable)
	pending := hn.must(201, "POST", "/api/cards", map[string]any{"title": "Pending", "githubRef": "#6247"})["card"].(map[string]any)["id"].(string)
	if code := hn.errorCode(409, "POST", "/api/cards/"+pending+"/ack", nil); code != "NOT_SYNCED" {
		t.Errorf("ack before the first sync: code = %s", code)
	}
}

// Times around the moment a test marks a card reviewed: the handler stamps
// the mark with the wall clock, so "later" has to be in the future.
const (
	longAgo   = "2026-11-17T04:54:40Z"
	reviewAt  = "2026-11-24T13:15:12Z"
	farFuture = "2099-01-01T00:00:00Z"
	evenLater = "2099-01-02T00:00:00Z"
)

func TestPersonalReviewFromGitHub(t *testing.T) {
	hn := newHarness(t)
	hn.srv.cfg.CurrentUser = "JesseStutler"
	const key = "volcano-sh/volcano#6247"
	hn.gh.updateItem(key, func(it *github.Item) {
		it.MyReview = github.MyReview{State: github.MyReviewCommented, At: reviewAt, CommitSHA: "aaa"}
		it.LastCommitAt, it.LastOtherActivityAt = longAgo, longAgo
	})
	id := hn.newLinkedCard("waiting")
	sync := func() *db.Card {
		t.Helper()
		hn.must(200, "POST", "/api/cards/"+id+"/sync-github", nil)
		return hn.card(id)
	}

	// The review is visible right after linking, although GitHub still says
	// the pull request as a whole needs review.
	c := hn.card(id)
	if c.GHMyReviewState != "COMMENTED" || c.GHMyReviewedAt != reviewAt || c.GHIsStaleReview || c.GHReviewDecision != github.ReviewRequired {
		t.Fatalf("linked card = %q at %q stale %v decision %q", c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview, c.GHReviewDecision)
	}
	board := hn.must(200, "GET", "/api/board", nil)
	if board["currentUser"] != "JesseStutler" {
		t.Fatalf("board currentUser = %v", board["currentUser"])
	}
	view := board["cards"].([]any)[0].(map[string]any)
	if view["ghMyReviewState"] != "COMMENTED" || view["ghMyReviewedAt"] != reviewAt || view["ghIsStaleReview"] != false || view["ghAuthor"] != "mohit" {
		t.Fatalf("card in the board payload = %v", view)
	}

	// The author pushes a commit: the review is out of date.
	hn.gh.updateItem(key, func(it *github.Item) {
		it.Snapshot.Commits, it.Snapshot.HeadSHA, it.LastCommitAt = 2, "bbb", farFuture
	})
	if c = sync(); !c.GHIsStaleReview || c.GHMyReviewState != "COMMENTED" || c.GHMyReviewedAt != reviewAt || !c.HasUnreadUpdates {
		t.Fatalf("after a push = %q at %q stale %v unread %v", c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview, c.HasUnreadUpdates)
	}

	// Looking at the push and marking the card caught up settles it; the
	// state stays the one from GitHub and the reference time moves.
	acked := hn.must(200, "POST", "/api/cards/"+id+"/ack", nil)
	if acked["ghIsStaleReview"] != false || acked["ghMyReviewState"] != "COMMENTED" || acked["ghMyReviewedAt"] == reviewAt {
		t.Fatalf("acked card = %v", acked)
	}
	markedAt := acked["ghMyReviewedAt"].(string)
	if c = sync(); c.GHIsStaleReview || c.GHMyReviewedAt != markedAt {
		t.Fatalf("sync after the ack = stale %v at %q, want %q", c.GHIsStaleReview, c.GHMyReviewedAt, markedAt)
	}

	// The author answers in a comment: out of date again.
	hn.gh.updateItem(key, func(it *github.Item) {
		it.Snapshot.Comments, it.LastOtherActivityAt = 8, farFuture
	})
	if c = sync(); !c.GHIsStaleReview {
		t.Fatalf("a reply after the review did not make it stale: %+v", c)
	}

	// A new review on GitHub replaces the state and is current again.
	hn.gh.updateItem(key, func(it *github.Item) {
		it.MyReview = github.MyReview{State: github.MyReviewApproved, At: evenLater, CommitSHA: "bbb"}
	})
	if c = sync(); c.GHMyReviewState != "APPROVED" || c.GHMyReviewedAt != evenLater || c.GHIsStaleReview {
		t.Fatalf("after approving = %q at %q stale %v", c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview)
	}
	// A review submitted on GitHub cannot be cleared from the board.
	if code := hn.errorCode(409, "POST", "/api/cards/"+id+"/unreview", nil); code != "REVIEW_FROM_GITHUB" {
		t.Fatalf("unreview of a GitHub review: code = %s", code)
	}

	// Unlinking drops the review along with the rest of the tracking state.
	out := hn.must(200, "POST", "/api/cards/"+id+"/unlink-github", nil)
	if out["ghMyReviewState"] != "" || out["ghMyReviewedAt"] != "" || out["ghIsStaleReview"] != false {
		t.Fatalf("unlinked card = %v", out)
	}
}

func TestMarkReviewedByHand(t *testing.T) {
	hn := newHarness(t)
	hn.srv.cfg.CurrentUser = "JesseStutler"
	const issueKey = "volcano-sh/volcano#6100"
	hn.gh.updateItem(issueKey, func(it *github.Item) { it.Author, it.LastOtherActivityAt = "reporter", longAgo })
	id := hn.must(201, "POST", "/api/cards", map[string]any{"githubRef": "#6100", "columnId": "waiting"})["card"].(map[string]any)["id"].(string)
	if c := hn.card(id); c.GHMyReviewState != "" || c.GHIsStaleReview {
		t.Fatalf("an item nobody reviewed = %q stale %v", c.GHMyReviewState, c.GHIsStaleReview)
	}

	// Nothing is unread, so the timeline calls it a review, not a catch-up.
	acked := hn.must(200, "POST", "/api/cards/"+id+"/ack", nil)
	if acked["ghMyReviewState"] != "REVIEWED" || acked["ghMyReviewedAt"] == "" || acked["ghIsStaleReview"] != false {
		t.Fatalf("marked card = %v", acked)
	}
	if got := hn.eventMessages(id); got[len(got)-1] != "Marked reviewed" {
		t.Fatalf("timeline = %q", got)
	}
	// GitHub has no review for an issue; the mark survives every sync.
	hn.must(200, "POST", "/api/cards/"+id+"/sync-github", nil)
	if c := hn.card(id); c.GHMyReviewState != "REVIEWED" || c.GHIsStaleReview {
		t.Fatalf("after a sync = %q stale %v", c.GHMyReviewState, c.GHIsStaleReview)
	}

	// Somebody comments later: the card asks for another look.
	hn.gh.updateItem(issueKey, func(it *github.Item) { it.Snapshot.Comments, it.LastOtherActivityAt = 3, farFuture })
	hn.must(200, "POST", "/api/cards/"+id+"/sync-github", nil)
	if c := hn.card(id); c.GHMyReviewState != "REVIEWED" || !c.GHIsStaleReview || !c.HasUnreadUpdates {
		t.Fatalf("after a new comment = %q stale %v unread %v", c.GHMyReviewState, c.GHIsStaleReview, c.HasUnreadUpdates)
	}

	// The mark can be taken back, and clearing twice is harmless.
	for range 2 {
		out := hn.must(200, "POST", "/api/cards/"+id+"/unreview", nil)
		if out["ghMyReviewState"] != "" || out["ghMyReviewedAt"] != "" || out["ghIsStaleReview"] != false {
			t.Fatalf("cleared card = %v", out)
		}
	}
	hn.must(200, "POST", "/api/cards/"+id+"/sync-github", nil)
	if c := hn.card(id); c.GHMyReviewState != "" || c.GHIsStaleReview {
		t.Fatalf("a cleared mark came back: %q stale %v", c.GHMyReviewState, c.GHIsStaleReview)
	}

	// A new head after a manual mark on a pull request is stale too.
	pr := hn.newLinkedCard("waiting")
	hn.must(200, "POST", "/api/cards/"+pr+"/ack", nil)
	hn.gh.update("volcano-sh/volcano#6247", func(s *github.Snapshot) { s.Commits, s.HeadSHA = 2, "bbb" })
	hn.must(200, "POST", "/api/cards/"+pr+"/sync-github", nil)
	if c := hn.card(pr); c.GHMyReviewState != "REVIEWED" || !c.GHIsStaleReview {
		t.Fatalf("manual mark after a push = %q stale %v", c.GHMyReviewState, c.GHIsStaleReview)
	}

	// Nobody reviews their own pull request: catching up leaves no mark.
	hn.srv.cfg.CurrentUser = "Mohit"
	own := hn.newLinkedCard("waiting")
	if out := hn.must(200, "POST", "/api/cards/"+own+"/ack", nil); out["ghMyReviewState"] != "" || out["ghMyReviewedAt"] != "" {
		t.Fatalf("own pull request after ack = %v", out)
	}
	// A mark made before the author was known is dropped on the next sync.
	hn.must(200, "POST", "/api/cards/"+pr+"/sync-github", nil)
	if c := hn.card(pr); c.GHMyReviewState != "" || c.GHIsStaleReview {
		t.Fatalf("own pull request kept a review mark: %q stale %v", c.GHMyReviewState, c.GHIsStaleReview)
	}
}

// fakeUserGitHub is a GitHub client that can also name its account.
type fakeUserGitHub struct {
	*fakeGitHub
	mu      sync.Mutex
	login   string
	pending string
	err     error
}

func (f *fakeUserGitHub) User() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.login
}

func (f *fakeUserGitHub) DetectUser(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.login = f.pending
	return f.login, nil
}

func TestCurrentUserComesFromTheGitHubClient(t *testing.T) {
	hn := newHarness(t)
	gh := &fakeUserGitHub{fakeGitHub: hn.gh, pending: "JesseStutler", err: fmt.Errorf("%w: offline", github.ErrUnavailable)}
	hn.srv.gh = gh

	// Offline at startup: the board works and simply does not know the user.
	hn.srv.resolveCurrentUser(context.Background())
	if got := hn.must(200, "GET", "/api/board", nil)["currentUser"]; got != "" {
		t.Fatalf("currentUser while gh is unreachable = %v", got)
	}
	// The next attempt succeeds and the board picks it up.
	gh.mu.Lock()
	gh.err = nil
	gh.mu.Unlock()
	hn.srv.resolveCurrentUser(context.Background())
	if got := hn.must(200, "GET", "/api/board", nil)["currentUser"]; got != "JesseStutler" {
		t.Fatalf("currentUser after detection = %v", got)
	}
	// An explicit --gh-user wins over the detected login.
	hn.srv.cfg.CurrentUser = "someone-else"
	if got := hn.must(200, "GET", "/api/board", nil)["currentUser"]; got != "someone-else" {
		t.Fatalf("currentUser with an explicit login = %v", got)
	}
}

// searchHarness is a board with three session cards whose conversations
// are indexed: a Codex review of PR #6247 and two Copilot sessions.
func searchHarness(t *testing.T) (*harness, map[string]string) {
	t.Helper()
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{
		worker("standalone-1", "codex", "[PR #6247][Review] CSI limits"),
		worker("volcano-2", "copilot", "Kubeflow Trainer design"),
		worker("volcano-3", "copilot", "PodGroup comparison"),
	}
	msg := func(id string, seq int64, role, text string) ao.Message {
		return ao.Message{Kind: "message", ID: id, Sequence: seq, Role: role, Text: text, CreatedAt: "2026-11-24T13:12:00Z"}
	}
	hn.ao.conversations = map[string]*ao.Conversation{
		"standalone-1": {SessionID: "standalone-1", Messages: []ao.Message{
			msg("a1", 1, "user", "Please review the change that removes the CSI scalar accounting."),
			msg("a2", 2, "assistant", "Removing the scalar changes preemption eligibility: HasSchedulingResourceRequests() now returns false, so the victim filter in preempt.go excludes lower-priority pods."),
			msg("a3", 3, "assistant", "Recommendation: hold the merge until the regression is covered by an end-to-end case."),
			msg("a4", 4, "assistant", "half written answer about hostpath"),
		}},
		"volcano-2": {SessionID: "volcano-2", Messages: []ao.Message{
			msg("b1", 1, "user", "\u8bbe\u8ba1 CompositePodGroup \u4e0e Kubeflow Trainer \u7684\u96c6\u6210\u65b9\u6848"),
			msg("b2", 2, "assistant", "The TrainJob controller creates one PodGroup per replicated job; minMember comes from numNodes."),
			msg("b3", 3, "assistant", "Next step: write the proposal document and send it to the maintainers for a first review."),
		}},
		"volcano-3": {SessionID: "volcano-3", Messages: []ao.Message{
			msg("c1", 1, "user", "Compare the upstream scheduling.k8s.io PodGroup API with ours."),
			msg("c2", 2, "assistant", "The two APIs differ mostly in how minimum membership and queues are expressed; a table follows."),
		}},
	}
	// The card summary is extracted from the last complete assistant
	// message, so the details of earlier messages exist only in the chat.
	hn.ao.conversations["standalone-1"].Messages[3].Streaming = true
	hn.syncAO()
	hn.srv.indexContent(context.Background())
	ids := map[string]string{}
	for _, sid := range []string{"standalone-1", "volcano-2", "volcano-3"} {
		c := hn.cardBySession(sid)
		if c == nil {
			t.Fatalf("no card for session %s", sid)
		}
		ids[sid] = c.ID
	}
	return hn, ids
}

// searchFor returns the matches of a keyword search by card ID.
func (hn *harness) searchFor(q string) map[string]map[string]any {
	hn.t.Helper()
	out := hn.must(200, "GET", "/api/search?q="+url.QueryEscape(q), nil)
	matches := map[string]map[string]any{}
	for _, m := range out["matches"].([]any) {
		m := m.(map[string]any)
		matches[m["cardId"].(string)] = m
	}
	return matches
}

func matchedBy(m map[string]any) string {
	var parts []string
	for _, v := range m["matchedBy"].([]any) {
		parts = append(parts, v.(string))
	}
	return strings.Join(parts, ",")
}

func TestSearchBySessionID(t *testing.T) {
	hn, ids := searchHarness(t)

	// The full ID, a part of it, and any letter case all find the card,
	// although the ID is not part of the title or summary.
	for _, q := range []string{"volcano-2", "VOLCANO-2", "cano-2"} {
		got := hn.searchFor(q)
		m := got[ids["volcano-2"]]
		if len(got) != 1 || m == nil || matchedBy(m) != "sessionId" || m["reason"] != "Matched ID: volcano-2" || m["sessionId"] != "volcano-2" {
			t.Errorf("search %q = %v", q, got)
		}
	}
	// A shared prefix matches every session carrying it.
	got := hn.searchFor("standalone")
	if m := got[ids["standalone-1"]]; len(got) != 1 || m == nil || m["reason"] != "Matched ID: standalone-1" {
		t.Errorf("search by ID prefix = %v", got)
	}
	// The repository name of the linked pull request contains "volcano",
	// so that card matches on its own fields and gets no ID badge.
	got = hn.searchFor("volcano")
	if len(got) != 3 || matchedBy(got[ids["standalone-1"]]) != "card" || got[ids["standalone-1"]]["reason"] != "" ||
		matchedBy(got[ids["volcano-2"]]) != "sessionId" || matchedBy(got[ids["volcano-3"]]) != "sessionId" {
		t.Errorf("search volcano = %v", got)
	}
	// Terms combine: one in the title, one in the ID.
	if got := hn.searchFor("kubeflow volcano-2"); len(got) != 1 || matchedBy(got[ids["volcano-2"]]) != "sessionId" {
		t.Errorf("title term plus ID term = %v", got)
	}
	if got := hn.searchFor("kubeflow volcano-3"); len(got) != 0 {
		t.Errorf("terms of two different cards matched %v", got)
	}

	// AO names the session branch after the ID. The ID still gets the
	// credit, and the branch and agent name stay searchable on their own.
	hn.ao.set(func() { hn.ao.sessions[1].Branch = "ao/volcano-2/import" })
	hn.syncAO()
	if got := hn.searchFor("volcano-2"); len(got) != 1 || got[ids["volcano-2"]]["reason"] != "Matched ID: volcano-2" {
		t.Errorf("ID that is also part of the branch = %v", got)
	}
	if got := hn.searchFor("ao/volcano-2/import"); len(got) != 1 || matchedBy(got[ids["volcano-2"]]) != "card" {
		t.Errorf("search by branch = %v", got)
	}
	if got := hn.searchFor("codex"); len(got) != 1 || matchedBy(got[ids["standalone-1"]]) != "card" {
		t.Errorf("search by agent = %v", got)
	}
}

func TestSearchConversationContent(t *testing.T) {
	hn, ids := searchHarness(t)

	// A function name that only appears inside the chat.
	got := hn.searchFor("hasschedulingresourcerequests")
	m := got[ids["standalone-1"]]
	if len(got) != 1 || m == nil || matchedBy(m) != "conversation" || m["role"] != "assistant" || m["messages"] != float64(1) {
		t.Fatalf("content search = %v", got)
	}
	snip := m["snippet"].(string)
	if !strings.Contains(snip, "HasSchedulingResourceRequests() now returns false") || !strings.HasPrefix(snip, "...") || m["reason"] != "In chat: "+snip {
		t.Fatalf("snippet = %q, reason = %q", snip, m["reason"])
	}
	if len([]rune(snip)) > 200 {
		t.Fatalf("snippet is not short: %d runes", len([]rune(snip)))
	}

	// Chinese text has no word boundaries; a substring still matches.
	if got := hn.searchFor("\u96c6\u6210\u65b9\u6848"); len(got) != 1 || matchedBy(got[ids["volcano-2"]]) != "conversation" {
		t.Errorf("CJK content search = %v", got)
	}
	// One term from the card, one from its conversation.
	if got := hn.searchFor("kubeflow minMember"); len(got) != 1 || matchedBy(got[ids["volcano-2"]]) != "conversation" {
		t.Errorf("card term plus chat term = %v", got)
	}
	// ID and conversation together report both sources, ID first.
	got = hn.searchFor("volcano-3 scheduling.k8s.io")
	if m := got[ids["volcano-3"]]; len(got) != 1 || matchedBy(m) != "sessionId,conversation" || m["reason"] != "Matched ID: volcano-3" || m["snippet"] == "" {
		t.Errorf("ID plus chat term = %v", got)
	}
	// A message that is still streaming is not indexed yet.
	if got := hn.searchFor("hostpath"); len(got) != 0 {
		t.Errorf("a streaming message was indexed: %v", got)
	}
	if got := hn.searchFor("nothing-says-this"); len(got) != 0 {
		t.Errorf("unexpected matches %v", got)
	}

	out := hn.must(200, "GET", "/api/search?q=", nil)
	index := out["index"].(map[string]any)
	if len(out["matches"].([]any)) != 0 || index["sessions"] != float64(3) || index["messages"] != float64(8) {
		t.Fatalf("empty query = %v", out)
	}

	// New messages are picked up by the next index pass, including the
	// one that finished streaming.
	hn.ao.set(func() {
		conv := hn.ao.conversations["standalone-1"]
		conv.Messages[3].Streaming = false
		conv.Messages = append(conv.Messages, ao.Message{Kind: "message", ID: "a5", Sequence: 5, Role: "user", Text: "What does install-csi-hostpath.sh do?"})
		hn.ao.sessions[0].Activity.LastActivityAt = "t2"
	})
	hn.syncAO()
	hn.srv.index.mu.Lock()
	hn.srv.index.seen = map[string]indexMark{}
	hn.srv.index.mu.Unlock()
	hn.srv.indexContent(context.Background())
	got = hn.searchFor("hostpath")
	if m := got[ids["standalone-1"]]; len(got) != 1 || m["messages"] != float64(2) || !strings.Contains(m["snippet"].(string), "install-csi-hostpath.sh") {
		t.Fatalf("after new messages = %v", got)
	}

	// A card without a session is searched by its own fields only.
	hn.must(201, "POST", "/api/cards", map[string]any{"title": "Write release notes", "notes": "mention preempt.go"})
	got = hn.searchFor("preempt.go")
	if len(got) != 2 {
		t.Fatalf("notes plus conversation = %v", got)
	}
}

func TestIndexFollowsARolledBackConversation(t *testing.T) {
	hn, ids := searchHarness(t)
	if got := hn.searchFor("minMember"); len(got) != 1 {
		t.Fatalf("before the rollback = %v", got)
	}
	// AO reports a conversation that ends before the indexed position.
	hn.ao.set(func() {
		hn.ao.conversations["volcano-2"] = &ao.Conversation{SessionID: "volcano-2", Messages: []ao.Message{
			{Kind: "message", ID: "b9", Sequence: 1, Role: "user", Text: "Start over with a queue design."},
		}}
	})
	if err := hn.srv.indexSession(context.Background(), "volcano-2", 500, "stamp", false); err != nil {
		t.Fatal(err)
	}
	if got := hn.searchFor("minMember"); len(got) != 0 {
		t.Fatalf("text of the dropped branch is still found: %v", got)
	}
	if got := hn.searchFor("queue design"); len(got) != 1 || got[ids["volcano-2"]] == nil {
		t.Fatalf("the new branch is not indexed: %v", got)
	}
}

func TestAISearch(t *testing.T) {
	hn, ids := searchHarness(t)
	hn.must(200, "PATCH", "/api/cards/"+ids["volcano-2"], map[string]any{"notes": "Waiting for the Kubeflow maintainers.", "tags": []string{"kubeflow"}})
	hn.ai.searchMatches = []summary.SearchMatch{
		{CardID: ids["standalone-1"], Reason: "Discusses CSI scalar removal and the preemption regression in PR #6247"},
		{CardID: ids["volcano-2"], Reason: "Designs the Kubeflow Trainer integration"},
	}
	out := hn.must(200, "POST", "/api/search/ai", map[string]any{"query": "  where did we talk about\n preemption  regressions? "})
	if out["query"] != "where did we talk about preemption regressions?" || out["engine"] != "codex" || out["cards"] != float64(3) {
		t.Fatalf("response = %v", out)
	}
	matches := out["matches"].([]any)
	first := matches[0].(map[string]any)
	if len(matches) != 2 || first["cardId"] != ids["standalone-1"] || first["reason"] != "Discusses CSI scalar removal and the preemption regression in PR #6247" {
		t.Fatalf("matches = %v", matches)
	}

	// The corpus describes every card, the one with keyword hits first,
	// with its session ID and excerpts of the conversation.
	in := hn.ai.searchInput
	if in.Query != "where did we talk about preemption regressions?" || len(in.Cards) != 3 || in.Cards[0].ID != ids["standalone-1"] {
		t.Fatalf("search input = %+v", in)
	}
	review := in.Cards[0]
	if review.SessionID != "standalone-1" || review.Agent != "codex" || review.Category != "PR Review" || review.Column != "Inbox" ||
		!strings.Contains(review.GitHub, "volcano-sh/volcano#6247") || !strings.Contains(review.GitHub, "mohit") {
		t.Fatalf("review card in the corpus = %+v", review)
	}
	joined := strings.Join(review.Excerpts, "\n")
	if !strings.Contains(joined, "[user, first message] Please review the change") || !strings.Contains(joined, "changes preemption eligibility") {
		t.Fatalf("excerpts = %q", review.Excerpts)
	}
	var design summary.SearchCard
	for _, c := range in.Cards {
		if c.ID == ids["volcano-2"] {
			design = c
		}
	}
	if design.Notes != "Waiting for the Kubeflow maintainers." || len(design.Tags) != 1 || design.SessionID != "volcano-2" || len(design.Excerpts) != 3 {
		t.Fatalf("design card in the corpus = %+v", design)
	}

	// No matches is a normal answer, with an empty list rather than null.
	hn.ai.searchMatches = nil
	out = hn.must(200, "POST", "/api/search/ai", map[string]any{"query": "anything about GPUs"})
	if list, ok := out["matches"].([]any); !ok || len(list) != 0 {
		t.Fatalf("empty answer = %v", out)
	}

	if code := hn.errorCode(400, "POST", "/api/search/ai", map[string]any{"query": "   "}); code != "INVALID_INPUT" {
		t.Errorf("blank query: code = %s", code)
	}
	calls := hn.ai.searchCalls
	hn.ai.searchErr = errors.New("AI search failed: copilot timed out; codex failed: quota")
	out = hn.must(502, "POST", "/api/search/ai", map[string]any{"query": "x"})
	if e := out["error"].(map[string]any); e["code"] != "AI_SEARCH_FAILED" || !strings.Contains(e["message"].(string), "copilot timed out; codex failed: quota") {
		t.Errorf("failure response = %v", out)
	}
	if hn.ai.searchCalls != calls+1 {
		t.Errorf("the model was called %d times for one request", hn.ai.searchCalls-calls)
	}
}

func TestAISearchRunsOneAtATime(t *testing.T) {
	hn, _ := searchHarness(t)
	hn.ai.searchGate, hn.ai.searchStarted = make(chan struct{}), make(chan struct{})
	done := make(chan int, 1)
	go func() {
		status, _ := hn.call("POST", "/api/search/ai", map[string]any{"query": "first"})
		done <- status
	}()
	<-hn.ai.searchStarted
	if code := hn.errorCode(409, "POST", "/api/search/ai", map[string]any{"query": "second"}); code != "AI_SEARCH_BUSY" {
		t.Errorf("concurrent search: code = %s", code)
	}
	close(hn.ai.searchGate)
	if status := <-done; status != 200 {
		t.Fatalf("first search = %d", status)
	}
	hn.ai.searchGate, hn.ai.searchStarted = nil, nil
	hn.must(200, "POST", "/api/search/ai", map[string]any{"query": "third"})
}

func TestAISearchOnAnEmptyBoardSkipsTheModel(t *testing.T) {
	hn := newHarness(t)
	out := hn.must(200, "POST", "/api/search/ai", map[string]any{"query": "anything"})
	if hn.ai.searchCalls != 0 || out["cards"] != float64(0) || len(out["matches"].([]any)) != 0 {
		t.Fatalf("empty board: calls = %d, response = %v", hn.ai.searchCalls, out)
	}
}

func TestSnippet(t *testing.T) {
	long := strings.Repeat("alpha beta gamma ", 20) + "the NEEDLE is here " + strings.Repeat("delta epsilon ", 30)
	cases := []struct {
		name, text, term, want string
	}{
		{"short text is returned whole", "A short  message\nabout   quotas.", "quotas", "A short message about quotas."},
		{"missing term shows the start", "Only this.", "absent", "Only this."},
		{"case-insensitive", "Check PreEmpt.go now", "preempt.go", "Check PreEmpt.go now"},
	}
	for _, tc := range cases {
		if got := snippet(tc.text, tc.term); got != tc.want {
			t.Errorf("%s: snippet = %q, want %q", tc.name, got, tc.want)
		}
	}
	got := snippet(long, "needle")
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "...") || !strings.Contains(got, "the NEEDLE is here") || len(got) > 190 {
		t.Errorf("windowed snippet = %q", got)
	}
	// Multi-byte text is cut on character boundaries.
	cjk := strings.Repeat("\u8c03\u5ea6\u5668", 60) + "\u62a2\u5360" + strings.Repeat("\u961f\u5217", 80)
	got = snippet(cjk, "\u62a2\u5360")
	if !utf8.ValidString(got) || !strings.Contains(got, "\u62a2\u5360") || len([]rune(got)) > 170 {
		t.Errorf("CJK snippet = %q", got)
	}
	// Emoji never reach the card face.
	if got := snippet("done \U0001F44D thanks", "thanks"); got != "done thanks" {
		t.Errorf("emoji snippet = %q", got)
	}
	// Markdown marks are dropped; the words stay.
	if got := snippet("Run **`install-csi-hostpath.sh`** from [the guide](https://example.com/guide) first:\n```sh\nmake e2e\n```", "hostpath"); got != "Run install-csi-hostpath.sh from the guide first: sh make e2e" {
		t.Errorf("markdown snippet = %q", got)
	}
}

func TestSearchTerms(t *testing.T) {
	if got := searchTerms("  CSI  csi Limits\tPR#6247 "); !reflect.DeepEqual(got, []string{"csi", "limits", "pr#6247"}) {
		t.Errorf("searchTerms = %q", got)
	}
	if got := searchTerms("a b c d e f g h i j"); len(got) != maxSearchTerms {
		t.Errorf("term count = %d, want the cap %d", len(got), maxSearchTerms)
	}
	if got := keywordTerms("Where did we talk about the preemption regression, and why?"); !reflect.DeepEqual(got, []string{"talk", "preemption", "regression"}) {
		t.Errorf("keywordTerms = %q", got)
	}
}

// modelHarness is a board with a running Codex session and an imported
// Copilot session that was not resumed, plus the model catalogs of both
// agents.
func modelHarness(t *testing.T) (hn *harness, running, sleeping string) {
	t.Helper()
	hn = newHarness(t)
	asleep := worker("volcano-2", "copilot", "Kubeflow design")
	asleep.NeedsResume = true
	hn.ao.sessions = []ao.Session{worker("standalone-1", "codex", "CSI review"), asleep}
	efforts := []string{"low", "medium", "high"}
	hn.ao.agentModels = map[string]*ao.AgentModels{
		"codex": {AgentID: "codex", Models: []ao.AgentModel{
			{ID: "gpt-6.2-sol", Label: "GPT-6.2-Sol", IsDefault: true, Efforts: efforts, DefaultEffort: "medium"},
			{ID: "gpt-6.2-astra", Label: "GPT-6.2-Astra", Efforts: []string{"low", "high"}, DefaultEffort: "low"},
		}},
		"copilot": {AgentID: "copilot", Models: []ao.AgentModel{
			{ID: "claude-sonnet-5", Label: "claude-sonnet-5", IsDefault: true},
			{ID: "gemini-4-neon"},
		}},
	}
	hn.ao.convModels = map[string]*ao.ConversationModels{
		"standalone-1": {
			Models: []ao.ConversationModel{
				{ID: "gpt-6.2-sol", DisplayName: "GPT-6.2-Sol", Description: "Fast, go-to model.", Default: true, Efforts: efforts, DefaultEffort: "medium"},
				{ID: "gpt-6.2-astra", DisplayName: "GPT-6.2-Astra", Efforts: []string{"low", "high"}, DefaultEffort: "low"},
				{ID: "plain", DisplayName: "Plain"},
			},
			Selected: ao.ConversationSettings{Model: "gpt-6.2-sol", ReasoningEffort: "medium", ApprovalMode: "on-request"},
		},
		// AO reports no models for a session whose agent is not running.
		"volcano-2": {Models: []ao.ConversationModel{}},
	}
	hn.syncAO()
	return hn, hn.cardBySession("standalone-1").ID, hn.cardBySession("volcano-2").ID
}

func modelIDs(out map[string]any) string {
	var ids []string
	for _, m := range out["models"].([]any) {
		ids = append(ids, m.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, ",")
}

func TestAgentModelsEndpoint(t *testing.T) {
	hn, _, _ := modelHarness(t)
	out := hn.must(200, "GET", "/api/ao/agents/codex/models", nil)
	first := out["models"].([]any)[0].(map[string]any)
	if out["agent"] != "codex" || modelIDs(out) != "gpt-6.2-sol,gpt-6.2-astra" || first["label"] != "GPT-6.2-Sol" || first["isDefault"] != true ||
		first["defaultEffort"] != "medium" || !reflect.DeepEqual(first["efforts"], []any{"low", "medium", "high"}) {
		t.Fatalf("codex models = %v", out)
	}
	// A model without a label is shown by its ID, and efforts is a list
	// even when the model has none.
	out = hn.must(200, "GET", "/api/ao/agents/copilot/models", nil)
	second := out["models"].([]any)[1].(map[string]any)
	if second["label"] != "gemini-4-neon" || second["isDefault"] != false || !reflect.DeepEqual(second["efforts"], []any{}) {
		t.Fatalf("copilot models = %v", out)
	}
	if code := hn.errorCode(400, "GET", "/api/ao/agents/gemini/models", nil); code != "INVALID_INPUT" {
		t.Errorf("unknown agent: code = %s", code)
	}
	hn.ao.set(func() { hn.ao.modelsErr = fmt.Errorf("%w: connection refused", ao.ErrUnavailable) })
	if code := hn.errorCode(503, "GET", "/api/ao/agents/codex/models", nil); code != "AO_OFFLINE" {
		t.Errorf("AO offline: code = %s", code)
	}
}

func TestCardModels(t *testing.T) {
	hn, running, sleeping := modelHarness(t)

	// A running session answers for itself.
	out := hn.must(200, "GET", "/api/cards/"+running+"/models", nil)
	selected := out["selected"].(map[string]any)
	first := out["models"].([]any)[0].(map[string]any)
	if out["sessionId"] != "standalone-1" || out["harness"] != "codex" || out["needsResume"] != false || out["source"] != "session" ||
		modelIDs(out) != "gpt-6.2-sol,gpt-6.2-astra,plain" || selected["model"] != "gpt-6.2-sol" || selected["reasoningEffort"] != "medium" ||
		first["label"] != "GPT-6.2-Sol" || first["description"] != "Fast, go-to model." || first["isDefault"] != true {
		t.Fatalf("running session models = %v", out)
	}

	// A session that was not resumed has no list of its own: the picker
	// is filled from the agent's catalog and the response says so.
	out = hn.must(200, "GET", "/api/cards/"+sleeping+"/models", nil)
	if out["harness"] != "copilot" || out["needsResume"] != true || out["source"] != "agent" || modelIDs(out) != "claude-sonnet-5,gemini-4-neon" ||
		out["selected"].(map[string]any)["model"] != "" {
		t.Fatalf("sleeping session models = %v", out)
	}
	// A selection made earlier survives the fallback.
	hn.ao.set(func() { hn.ao.convModels["volcano-2"].Selected.Model = "gemini-4-neon" })
	out = hn.must(200, "GET", "/api/cards/"+sleeping+"/models", nil)
	if out["selected"].(map[string]any)["model"] != "gemini-4-neon" || out["source"] != "agent" {
		t.Fatalf("selection of a sleeping session = %v", out)
	}
	// When the session cannot answer at all, its agent's catalog still does.
	hn.ao.set(func() { delete(hn.ao.convModels, "volcano-2") })
	out = hn.must(200, "GET", "/api/cards/"+sleeping+"/models", nil)
	if out["source"] != "agent" || modelIDs(out) != "claude-sonnet-5,gemini-4-neon" {
		t.Fatalf("fallback without a session answer = %v", out)
	}
	// Neither source: the AO error is passed on.
	hn.ao.set(func() {
		hn.ao.modelsErr = &ao.APIError{Status: 500, Code: "MODELS_UNAVAILABLE", Message: "catalog refresh failed"}
	})
	if code := hn.errorCode(404, "GET", "/api/cards/"+sleeping+"/models", nil); code != "SESSION_NOT_FOUND" {
		t.Errorf("no source at all: code = %s", code)
	}

	plain := hn.must(201, "POST", "/api/cards", map[string]any{"title": "No agent"})["card"].(map[string]any)["id"].(string)
	if code := hn.errorCode(409, "GET", "/api/cards/"+plain+"/models", nil); code != "NO_SESSION" {
		t.Errorf("card without a session: code = %s", code)
	}
	if code := hn.errorCode(404, "GET", "/api/cards/c_missing/models", nil); code != "NOT_FOUND" {
		t.Errorf("missing card: code = %s", code)
	}
}

func TestCardSettingsSwitchesTheModel(t *testing.T) {
	hn, running, sleeping := modelHarness(t)
	last := func() string {
		hn.ao.mu.Lock()
		defer hn.ao.mu.Unlock()
		if len(hn.ao.settingsCalls) == 0 {
			return ""
		}
		return hn.ao.settingsCalls[len(hn.ao.settingsCalls)-1]
	}

	// Model and effort together.
	out := hn.must(200, "PATCH", "/api/cards/"+running+"/settings", map[string]any{"model": "gpt-6.2-astra", "reasoningEffort": "high"})
	selected := out["selected"].(map[string]any)
	if out["sessionId"] != "standalone-1" || out["needsResume"] != false || selected["model"] != "gpt-6.2-astra" || selected["reasoningEffort"] != "high" {
		t.Fatalf("switch = %v", out)
	}
	// The approval mode is never sent: the board does not touch it.
	if got := last(); got != "standalone-1 model=[gpt-6.2-astra] effort=[high] approval=-" {
		t.Fatalf("AO received %q", got)
	}
	// Only the effort: the model field is left out, so AO keeps it.
	out = hn.must(200, "PATCH", "/api/cards/"+running+"/settings", map[string]any{"reasoningEffort": "low"})
	if got := last(); got != "standalone-1 model=- effort=[low] approval=-" || out["selected"].(map[string]any)["model"] != "gpt-6.2-astra" {
		t.Fatalf("effort only: AO received %q, response %v", got, out)
	}
	// An empty string resets to the agent's default.
	out = hn.must(200, "PATCH", "/api/cards/"+running+"/settings", map[string]any{"model": "", "reasoningEffort": ""})
	if got := last(); got != "standalone-1 model=[] effort=[] approval=-" || out["selected"].(map[string]any)["model"] != "" {
		t.Fatalf("reset: AO received %q, response %v", got, out)
	}
	if got := hn.eventMessages(running); !reflect.DeepEqual(got[len(got)-3:], []string{
		"Agent model set to gpt-6.2-astra (effort high)", "Agent model set to gpt-6.2-astra (effort low)", "Agent model set to the default model",
	}) {
		t.Fatalf("timeline = %q", got)
	}

	// AO would store any name; the board checks it first.
	calls := len(hn.ao.settingsCalls)
	for name, body := range map[string]map[string]any{
		"unknown model":                     {"model": "no-such-model"},
		"effort the model does not support": {"model": "gpt-6.2-astra", "reasoningEffort": "medium"},
		"effort on a model without efforts": {"model": "plain", "reasoningEffort": "low"},
		"effort unknown to the default":     {"reasoningEffort": "ultra"},
	} {
		if code := hn.errorCode(400, "PATCH", "/api/cards/"+running+"/settings", body); code != "UNKNOWN_MODEL" {
			t.Errorf("%s: code = %s", name, code)
		}
	}
	if code := hn.errorCode(400, "PATCH", "/api/cards/"+running+"/settings", map[string]any{}); code != "INVALID_INPUT" {
		t.Errorf("empty change: code = %s", code)
	}
	if len(hn.ao.settingsCalls) != calls {
		t.Fatalf("a rejected change reached AO: %q", hn.ao.settingsCalls[calls:])
	}

	// A session that was not resumed: validated against the agent's
	// catalog, stored for later, and the response says it waits for Resume.
	if code := hn.errorCode(400, "PATCH", "/api/cards/"+sleeping+"/settings", map[string]any{"model": "gpt-6.2-sol"}); code != "UNKNOWN_MODEL" {
		t.Errorf("a Codex model on a Copilot session: code = %s", code)
	}
	out = hn.must(200, "PATCH", "/api/cards/"+sleeping+"/settings", map[string]any{"model": " gemini-4-neon "})
	if out["needsResume"] != true || out["selected"].(map[string]any)["model"] != "gemini-4-neon" || last() != "volcano-2 model=[gemini-4-neon] effort=- approval=-" {
		t.Fatalf("sleeping session switch = %v, AO received %q", out, last())
	}
	if got := hn.eventMessages(sleeping); got[len(got)-1] != "Agent model set to gemini-4-neon; applies once the session is resumed" {
		t.Fatalf("timeline = %q", got)
	}

	// AO's own refusal is passed on with its code and a readable message.
	hn.ao.set(func() {
		hn.ao.settingsErr = &ao.APIError{Status: 409, Code: "SESSION_BUSY", Message: "session is busy"}
	})
	if code := hn.errorCode(409, "PATCH", "/api/cards/"+running+"/settings", map[string]any{"model": "plain"}); code != "SESSION_BUSY" {
		t.Errorf("busy session: code = %s", code)
	}
	plain := hn.must(201, "POST", "/api/cards", map[string]any{"title": "No agent"})["card"].(map[string]any)["id"].(string)
	if code := hn.errorCode(409, "PATCH", "/api/cards/"+plain+"/settings", map[string]any{"model": "plain"}); code != "NO_SESSION" {
		t.Errorf("card without a session: code = %s", code)
	}
}

func TestNewSessionWithModelAndEffort(t *testing.T) {
	hn, _, _ := modelHarness(t)
	spawned := func() ao.SpawnRequest {
		hn.ao.mu.Lock()
		defer hn.ao.mu.Unlock()
		return hn.ao.spawned[len(hn.ao.spawned)-1]
	}

	// POST /api/cards with the model inside the agent block.
	out := hn.must(201, "POST", "/api/cards", map[string]any{"title": "With a model", "agent": map[string]any{"harness": "codex", "prompt": "go", "model": "gpt-6.2-astra", "effort": "high"}})
	if req := spawned(); req.Harness != "codex" || req.Model != "gpt-6.2-astra" || req.Effort != "high" || req.Prompt != "go" {
		t.Fatalf("spawn request = %+v", req)
	}
	id := out["card"].(map[string]any)["id"].(string)
	if got := hn.eventMessages(id); !strings.HasSuffix(got[len(got)-1], " with model gpt-6.2-astra (effort high)") {
		t.Fatalf("timeline = %q", got)
	}
	// The top-level shorthand works too.
	hn.must(201, "POST", "/api/cards", map[string]any{"title": "Shorthand", "model": "gemini-4-neon", "agent": map[string]any{"harness": "copilot"}})
	if req := spawned(); req.Harness != "copilot" || req.Model != "gemini-4-neon" || req.Effort != "" {
		t.Fatalf("spawn request = %+v", req)
	}
	// Without a choice nothing is sent and AO uses its defaults.
	hn.must(201, "POST", "/api/cards", map[string]any{"title": "Defaults", "agent": map[string]any{"harness": "codex"}})
	if req := spawned(); req.Model != "" || req.Effort != "" {
		t.Fatalf("spawn request = %+v", req)
	}

	// A wrong model or effort is refused before anything is created.
	cards := len(hn.must(200, "GET", "/api/cards", nil)["cards"].([]any))
	count := len(hn.ao.spawned)
	for name, agent := range map[string]map[string]any{
		"unknown model":                {"harness": "codex", "model": "gemini-4-neon"},
		"unsupported effort":           {"harness": "codex", "model": "gpt-6.2-astra", "effort": "medium"},
		"effort without effort models": {"harness": "copilot", "model": "gemini-4-neon", "effort": "high"},
	} {
		if code := hn.errorCode(400, "POST", "/api/cards", map[string]any{"title": "Bad", "agent": agent}); code != "UNKNOWN_MODEL" {
			t.Errorf("%s: code = %s", name, code)
		}
	}
	if len(hn.ao.spawned) != count || len(hn.must(200, "GET", "/api/cards", nil)["cards"].([]any)) != cards {
		t.Fatalf("a refused request created a card or a session")
	}

	// Starting a session from an existing card takes the same fields.
	plain := hn.must(201, "POST", "/api/cards", map[string]any{"title": "Later"})["card"].(map[string]any)["id"].(string)
	if code := hn.errorCode(400, "POST", "/api/cards/"+plain+"/session", map[string]any{"harness": "codex", "model": "nope"}); code != "UNKNOWN_MODEL" {
		t.Errorf("start with an unknown model: code = %s", code)
	}
	hn.must(201, "POST", "/api/cards/"+plain+"/session", map[string]any{"harness": "codex", "model": "gpt-6.2-sol", "effort": "low"})
	if req := spawned(); req.Model != "gpt-6.2-sol" || req.Effort != "low" {
		t.Fatalf("spawn request = %+v", req)
	}

	// When the catalog cannot be read the choice is passed on to AO.
	hn.ao.set(func() { hn.ao.modelsErr = fmt.Errorf("%w: offline", ao.ErrUnavailable) })
	hn.must(201, "POST", "/api/cards", map[string]any{"title": "Unchecked", "agent": map[string]any{"harness": "codex", "model": "brand-new-model"}})
	if req := spawned(); req.Model != "brand-new-model" {
		t.Fatalf("spawn request = %+v", req)
	}
}

func TestSessionProxyEndpoints(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "A"), {ID: "standalone-1", Kind: "worker", Harness: "codex", NeedsResume: true}}
	hn.ao.conversations["volcano-2"] = &ao.Conversation{SessionID: "volcano-2", Controller: "ready", Messages: []ao.Message{{ID: "m1", Role: "user", Text: "hi"}}, Activities: []ao.ConversationActivity{}}
	hn.syncAO()

	conv := hn.must(200, "GET", "/api/sessions/volcano-2/conversation?limit=5000&beforeSequence=12", nil)
	if conv["latestSequence"] != float64(200) || conv["oldestSequence"] != float64(12) || len(conv["messages"].([]any)) != 1 {
		t.Fatalf("conversation = %v (limit must be capped at 200)", conv)
	}
	if code := hn.errorCode(404, "GET", "/api/sessions/ghost/conversation", nil); code != "SESSION_NOT_FOUND" {
		t.Errorf("unknown session: code = %s", code)
	}

	out := hn.must(202, "POST", "/api/sessions/volcano-2/messages", map[string]any{"text": "run tests", "clientMessageId": "uuid-1"})
	if out["turnId"] != "turn-1" || hn.ao.sent[0] != "volcano-2|run tests|uuid-1" {
		t.Fatalf("send = %v, forwarded %v", out, hn.ao.sent)
	}
	hn.must(202, "POST", "/api/sessions/volcano-2/messages", map[string]any{"text": "again"})
	if parts := strings.Split(hn.ao.sent[1], "|"); len(parts[2]) != 36 {
		t.Fatalf("a missing clientMessageId was not generated: %q", hn.ao.sent[1])
	}
	if code := hn.errorCode(400, "POST", "/api/sessions/volcano-2/messages", map[string]any{"text": "  "}); code != "INVALID_INPUT" {
		t.Errorf("empty message: code = %s", code)
	}

	hn.must(200, "POST", "/api/sessions/volcano-2/interrupt", nil)
	hn.must(200, "POST", "/api/sessions/volcano-2/kill", nil)
	hn.must(200, "PATCH", "/api/sessions/volcano-2", map[string]any{"displayName": " New name "})
	if !reflect.DeepEqual(hn.ao.killed, []string{"volcano-2"}) || !reflect.DeepEqual(hn.ao.renamed, []string{"volcano-2|New name"}) {
		t.Fatalf("killed %v, renamed %v", hn.ao.killed, hn.ao.renamed)
	}
	if code := hn.errorCode(400, "PATCH", "/api/sessions/volcano-2", map[string]any{"displayName": ""}); code != "INVALID_INPUT" {
		t.Errorf("empty name: code = %s", code)
	}

	list := hn.must(200, "GET", "/api/sessions", nil)["sessions"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["cardId"] == "" {
		t.Fatalf("sessions = %v", list)
	}
}

func TestResume(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{{ID: "standalone-1", Kind: "worker", Harness: "codex", DisplayName: "Review", NeedsResume: true}}
	hn.syncAO()
	cardID := hn.cardBySession("standalone-1").ID

	hn.ao.resumeErr = &ao.APIError{Status: 409, Code: "SESSION_OPEN_ELSEWHERE", Message: "session open elsewhere"}
	out := hn.must(409, "POST", "/api/sessions/standalone-1/resume", nil)
	e := out["error"].(map[string]any)
	if e["code"] != "SESSION_OPEN_ELSEWHERE" || !strings.Contains(e["message"].(string), "open in another terminal") {
		t.Fatalf("resume conflict = %v", out)
	}

	hn.ao.resumeErr = fmt.Errorf("%w: connection refused", ao.ErrUnavailable)
	if code := hn.errorCode(503, "POST", "/api/sessions/standalone-1/resume", nil); code != "AO_OFFLINE" {
		t.Errorf("offline: code = %s", code)
	}
	hn.ao.resumeErr = &ao.APIError{Status: 500, Code: "SESSION_IMPORT_RESUME_FAILED", Message: "agent crashed on start"}
	out = hn.must(502, "POST", "/api/sessions/standalone-1/resume", nil)
	if e := out["error"].(map[string]any); e["code"] != "SESSION_IMPORT_RESUME_FAILED" || e["message"] != "Agent crashed on start" {
		t.Fatalf("daemon failure = %v", out)
	}

	hn.ao.resumeErr = nil
	hn.must(200, "POST", "/api/sessions/standalone-1/resume", nil)
	board := hn.must(200, "GET", "/api/board", nil)
	if s := board["cards"].([]any)[0].(map[string]any)["session"].(map[string]any); s["needsResume"] != false {
		t.Fatalf("session after resume = %v", s)
	}
	if msgs := hn.eventMessages(cardID); msgs[len(msgs)-1] != "Resumed AO session standalone-1" {
		t.Fatalf("events = %v", msgs)
	}
}

func TestImport(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "Existing")}
	hn.ao.candidates = []ao.ImportCandidate{
		{ID: "a", Harness: "codex", Title: "review #6247", MessageCount: 30},
		{ID: "b", Harness: "copilot", Title: "Existing", SessionID: "volcano-2", MessageCount: 4},
		{ID: "s", Harness: "copilot", Title: summary.PromptMarker + " Describe what a card on a software\u2026", MessageCount: 2},
		{ID: "q", Harness: "copilot", Title: summary.SearchPromptMarker + " You are searching", MessageCount: 2},
		{ID: "old", Harness: "copilot", Title: "You are writing a status summary for a card on a software maintainer's\u2026", MessageCount: 2},
	}
	hn.syncAO()

	list := hn.must(200, "GET", "/api/ao/import-history", nil)["candidates"].([]any)
	if len(list) != 2 {
		t.Fatalf("candidates = %v; the board's own summarizer and search runs must be hidden", list)
	}
	// The path of the first release answers the same.
	if legacy := hn.must(200, "GET", "/api/import/candidates", nil)["candidates"].([]any); !reflect.DeepEqual(legacy, list) {
		t.Fatalf("legacy path = %v, want %v", legacy, list)
	}
	// Without a local history index the AO fields pass through unchanged.
	if first := list[0].(map[string]any); first["id"] != "a" || first["title"] != "review #6247" || first["messageCount"] != float64(30) || first["nativeId"] != nil {
		t.Fatalf("plain candidate = %v", first)
	}
	if imported := list[1].(map[string]any); imported["sessionId"] != "volcano-2" || imported["cardId"] != hn.cardBySession("volcano-2").ID {
		t.Fatalf("imported candidate = %v", imported)
	}

	out := hn.must(200, "POST", "/api/import", map[string]any{"ids": []string{"a", "broken"}})
	if out["imported"] != float64(1) || out["failed"] != float64(1) {
		t.Fatalf("import = %v", out)
	}
	results := out["results"].([]any)
	created := results[0].(map[string]any)
	card := hn.cardBySession("imported-a")
	if card == nil || created["cardId"] != card.ID || card.Title != "Imported a" || card.ColumnID != "inbox" {
		t.Fatalf("result %v, card %+v", created, card)
	}
	if failed := results[1].(map[string]any); failed["status"] != "failed" || failed["error"] != "unreadable history" || failed["cardId"] != "" {
		t.Fatalf("failed result = %v", failed)
	}
	if !reflect.DeepEqual(hn.ao.imported, [][]string{{"a", "broken"}}) {
		t.Fatalf("forwarded ids = %v", hn.ao.imported)
	}

	// Importing again after deleting the card brings the card back.
	hn.must(200, "DELETE", "/api/cards/"+card.ID, nil)
	hn.ao.set(func() { hn.ao.sessions = hn.ao.sessions[:1] })
	hn.must(200, "POST", "/api/import", map[string]any{"ids": []string{"a"}})
	if hn.cardBySession("imported-a") == nil {
		t.Fatal("re-import did not recreate the card")
	}

	if code := hn.errorCode(400, "POST", "/api/import", map[string]any{"ids": []string{}}); code != "INVALID_INPUT" {
		t.Errorf("empty import: code = %s", code)
	}
}

func TestSummarize(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("standalone-1", "codex", "[PR #6247] Review")}
	hn.ao.conversations["standalone-1"] = &ao.Conversation{Messages: []ao.Message{{Role: "user", Text: "review it"}, {Role: "assistant", Text: "ok"}}}
	hn.syncAO()
	id := hn.cardBySession("standalone-1").ID

	out := hn.must(200, "POST", "/api/cards/"+id+"/summarize", nil)
	if out["summary"] != "Objective: Review.\nStatus: Waiting." || out["engine"] != "copilot" {
		t.Fatalf("summarize = %v", out)
	}
	if c := hn.card(id); c.Summary != "Objective: Review.\nStatus: Waiting." || c.SummarySource != db.SummaryAI {
		t.Fatalf("card = %q (%s)", c.Summary, c.SummarySource)
	}
	in := hn.ai.input
	if in.Title != "[PR #6247] Review" || len(in.Turns) != 2 || !strings.Contains(in.GitHub, "volcano-sh/volcano#6247") || !strings.Contains(in.GitHub, "state OPEN") {
		t.Fatalf("summarizer input = %+v", in)
	}

	// The AI summary is not overwritten by automatic extraction.
	hn.srv.summaryStamp = map[string]summaryMark{}
	hn.syncAO()
	if c := hn.card(id); c.SummarySource != db.SummaryAI {
		t.Fatalf("AI summary was replaced by extraction: %q", c.Summary)
	}

	hn.ai.err = errors.New("AI summary failed: copilot timed out; codex failed: quota")
	if code := hn.errorCode(502, "POST", "/api/cards/"+id+"/summarize", nil); code != "SUMMARY_FAILED" {
		t.Errorf("failed summarize: code = %s", code)
	}
	if c := hn.card(id); c.Summary != "Objective: Review.\nStatus: Waiting." {
		t.Fatalf("a failed run changed the summary: %q", c.Summary)
	}

	hn.ai.err = nil
	empty := hn.must(201, "POST", "/api/cards", map[string]any{"title": "Bare"})["card"].(map[string]any)["id"].(string)
	calls := hn.ai.calls
	if code := hn.errorCode(409, "POST", "/api/cards/"+empty+"/summarize", nil); code != "NOTHING_TO_SUMMARIZE" {
		t.Errorf("bare card: code = %s", code)
	}
	if hn.ai.calls != calls {
		t.Error("the summarizer ran for a card with nothing to summarize")
	}
}

func TestSecurityMiddleware(t *testing.T) {
	hn := newHarness(t)
	do := func(method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		hn.h.ServeHTTP(rec, req)
		return rec
	}
	json := map[string]string{"Content-Type": "application/json"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{"Content-Type": "application/json"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	cases := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		want    int
	}{
		{"loopback ip", "GET", "http://127.0.0.1:3012/api/board", nil, 200},
		{"localhost", "GET", "http://localhost:3012/api/board", nil, 200},
		{"ipv6 loopback", "GET", "http://[::1]:3012/api/board", nil, 200},
		{"rebinding host", "GET", "http://evil.example:3012/api/board", nil, 403},
		{"same-origin post", "POST", "http://127.0.0.1:3012/api/columns", with(map[string]string{"Origin": "http://127.0.0.1:3012"}), 201},
		{"cross-origin post", "POST", "http://127.0.0.1:3012/api/columns", with(map[string]string{"Origin": "https://evil.example"}), 403},
		{"other local port", "POST", "http://127.0.0.1:3012/api/columns", with(map[string]string{"Origin": "http://127.0.0.1:8080"}), 403},
		{"cross-site fetch metadata", "POST", "http://127.0.0.1:3012/api/columns", with(map[string]string{"Sec-Fetch-Site": "cross-site"}), 403},
		{"form post", "POST", "http://127.0.0.1:3012/api/columns", map[string]string{"Content-Type": "text/plain"}, 415},
		{"cli post without origin", "POST", "http://127.0.0.1:3012/api/columns", json, 201},
	}
	for _, tc := range cases {
		rec := do(tc.method, tc.target, tc.headers, `{"title":"T","color":"#112233"}`)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}

	rec := do("GET", "http://127.0.0.1:3012/", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Maintainer Board") {
		t.Fatalf("index = %d %q", rec.Code, rec.Body.String())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("weak headers: CSP %q, nosniff %q", csp, rec.Header().Get("X-Content-Type-Options"))
	}
	if rec := do("GET", "http://127.0.0.1:3012/api/nope", nil, ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), "NOT_FOUND") {
		t.Errorf("unknown API route = %d %q", rec.Code, rec.Body.String())
	}

	big := `{"title":"` + strings.Repeat("x", 2<<20) + `"}`
	if rec := do("POST", "http://127.0.0.1:3012/api/cards", json, big); rec.Code != 413 {
		t.Errorf("oversized body: status = %d, want 413", rec.Code)
	}
}

func TestAllowedHosts(t *testing.T) {
	hn := newHarness(t)
	hn.srv.cfg.AllowedHosts = []string{"Board.Internal"}
	h := hn.srv.Handler()
	for host, want := range map[string]int{"board.internal:3012": 200, "other.internal:3012": 403} {
		req := httptest.NewRequest("GET", "http://"+host+"/healthz", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %s: status = %d, want %d", host, rec.Code, want)
		}
	}
}

func TestEventsStreamAnnouncesChanges(t *testing.T) {
	hn := newHarness(t)
	srv := httptest.NewServer(hn.h)
	defer srv.Close()
	// httptest listens on 127.0.0.1, which the host check accepts.
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	read := func() string {
		buf := make([]byte, 256)
		n, err := resp.Body.Read(buf)
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		return string(buf[:n])
	}
	if first := read(); !strings.Contains(first, "event: board") || !strings.Contains(first, `"version":0`) {
		t.Fatalf("initial event = %q", first)
	}
	hn.must(201, "POST", "/api/cards", map[string]any{"title": "x"})
	if next := read(); !strings.Contains(next, `"version":1`) {
		t.Fatalf("event after a change = %q", next)
	}
}

func TestInferCategory(t *testing.T) {
	cases := map[string]string{
		"[PR #6247][Review] CSI NodeVolumeLimits":      db.CategoryPRReview,
		"review\u4e00\u4e0b #6255":                     db.CategoryPRReview,
		"Reviewing the pull request for queue changes": db.CategoryPRReview,
		"[\u8bbe\u8ba1] Kubeflow Trainer":              db.CategoryDesign,
		"Design the capacity plugin":                   db.CategoryDesign,
		"Reproduce issue with gang scheduling":         db.CategoryIssue,
		"Triage incoming reports":                      db.CategoryTriage,
		"[\u8c03\u7814] K8s PodGroup":                  db.CategoryDev,
		"Improve preview rendering of proposals":       db.CategoryDev,
		"Tissue sample parser":                         db.CategoryDev,
	}
	for title, want := range cases {
		if got := InferCategory(title); got != want {
			t.Errorf("InferCategory(%q) = %s, want %s", title, got, want)
		}
	}
}
