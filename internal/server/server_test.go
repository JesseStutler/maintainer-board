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
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

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
	listErr       error
	resumeErr     error
	spawnErr      error
	sent          []string
	spawned       []ao.SpawnRequest
	imported      [][]string
	killed        []string
	renamed       []string
	nextID        int
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
	out := *conv
	out.LatestSequence = int64(q.Limit) // lets tests observe the forwarded limit
	out.OldestSequence = q.BeforeSequence
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

func (f *fakeAO) Rename(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renamed = append(f.renamed, id+"|"+name)
	return nil
}

func (f *fakeAO) ImportCandidates(context.Context) ([]ao.ImportCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

type fakeAI struct {
	text  string
	err   error
	input summary.Input
	calls int
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
		{ID: "s", Harness: "copilot", Title: summary.PromptMarker[:60] + "\u2026", MessageCount: 2},
	}
	hn.syncAO()

	list := hn.must(200, "GET", "/api/import/candidates", nil)["candidates"].([]any)
	if len(list) != 2 {
		t.Fatalf("candidates = %v; the board's own summarizer runs must be hidden", list)
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
