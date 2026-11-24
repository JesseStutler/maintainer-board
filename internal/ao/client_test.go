package ao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recorded is one request seen by the fake daemon.
type recorded struct {
	Method  string
	Path    string
	Query   string
	Body    string
	Escaped string
}

// fakeDaemon serves canned responses for "METHOD /path" and records calls.
func fakeDaemon(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) (*Client, *[]recorded) {
	t.Helper()
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.URL.EscapedPath()})
		handler, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not_found","code":"ROUTE_NOT_FOUND","message":"no handler"}`)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL + "/"), &calls
}

func respond(status int, body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func jsonEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("request body is not JSON: %q", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("request body = %s, want %s", got, want)
	}
}

// A session payload shaped like the live daemon's response, including
// fields the board does not use.
const sessionJSON = `{
  "needsResume": true, "id": "standalone-1", "projectId": "volcano", "kind": "worker", "harness": "codex",
  "reviewerConfig": {}, "displayName": "[PR #6247][Review] CSI", "mode": "chat",
  "activity": {"state": "idle", "lastActivityAt": "2026-11-24T11:57:05.333Z"},
  "isTerminated": false, "status": "idle", "kanbanColumn": "building", "displayStatus": "Idle",
  "branch": "ao/standalone-1", "branchState": {"branch": "ao/standalone-1", "commits": 2, "unpushed": 1, "behind": 0, "conflicts": false},
  "createdAt": "2026-11-24T12:11:20Z", "updatedAt": "2026-11-24T12:21:09Z",
  "prs": [{"url": "https://github.com/volcano-sh/volcano/pull/6247", "number": 6247, "state": "open", "ci": "passing", "review": "review_required", "mergeability": "mergeable", "reviewComments": true, "updatedAt": "2026-11-24T12:00:00Z"}]
}`

func TestListSessionsDecodesLiveShape(t *testing.T) {
	c, _ := fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/sessions": respond(200, `{"sessions": [`+sessionJSON+`]}`),
	})
	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Session{{
		ID: "standalone-1", ProjectID: "volcano", Kind: "worker", Harness: "codex",
		DisplayName: "[PR #6247][Review] CSI", Mode: "chat",
		Activity:    Activity{State: "idle", LastActivityAt: "2026-11-24T11:57:05.333Z"},
		NeedsResume: true, Status: "idle", KanbanColumn: "building", DisplayStatus: "Idle",
		Branch:      "ao/standalone-1",
		BranchState: &BranchState{Branch: "ao/standalone-1", Commits: 2, Unpushed: 1},
		CreatedAt:   "2026-11-24T12:11:20Z", UpdatedAt: "2026-11-24T12:21:09Z",
		PRs: []SessionPR{{URL: "https://github.com/volcano-sh/volcano/pull/6247", Number: 6247, State: "open", CI: "passing", Review: "review_required", UpdatedAt: "2026-11-24T12:00:00Z"}},
	}}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("sessions:\n got %+v\nwant %+v", sessions, want)
	}
}

func TestSpawnSendsTheSpawnRequest(t *testing.T) {
	c, calls := fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/sessions": respond(201, `{"session": `+sessionJSON+`, "promptBytes": 5, "systemPromptBytes": 9}`),
	})
	sess, err := c.Spawn(context.Background(), SpawnRequest{
		ProjectID: "volcano", Harness: "copilot", Mode: "chat", Kind: "worker",
		DisplayName: "Fix flaky e2e", DisplayNameExplicit: true, Prompt: "start",
	})
	if err != nil || sess.ID != "standalone-1" {
		t.Fatalf("Spawn = %+v, %v", sess, err)
	}
	jsonEqual(t, (*calls)[0].Body, `{"projectId":"volcano","harness":"copilot","mode":"chat","kind":"worker","displayName":"Fix flaky e2e","displayNameExplicit":true,"prompt":"start"}`)

	c, _ = fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/sessions": respond(201, `{}`),
	})
	if _, err := c.Spawn(context.Background(), SpawnRequest{}); err == nil {
		t.Fatal("a spawn response without a session was accepted")
	}
}

func TestConversation(t *testing.T) {
	c, calls := fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/sessions/volcano-3/conversation": respond(200, `{
			"conversationId": "import:volcano-3", "sessionId": "volcano-3", "harness": "copilot", "mode": "chat",
			"controller": "stopped", "latestSequence": 0, "oldestSequence": 40, "hasMoreBefore": true, "turns": [],
			"messages": [
				{"kind": "message", "id": "import:volcano-3:40", "sequence": 40, "revision": 1, "role": "user", "origin": "human", "text": "hello", "editAvailable": true, "streaming": false, "clientMessageId": "abc", "createdAt": "2026-11-19T11:23:27Z"},
				{"kind": "message", "id": "import:volcano-3:41", "sequence": 41, "revision": 2, "role": "assistant", "origin": "provider", "text": "hi", "streaming": true, "createdAt": "2026-11-19T11:23:42Z"}
			],
			"activities": [{"kind": "activity", "id": "act-1", "sequence": 42, "revision": 0, "activityKind": "command", "status": "running", "summary": "go test ./...", "detail": {"cwd": "/x"}, "createdAt": "2026-11-19T11:24:00Z"}]
		}`),
		"GET /api/v1/sessions/empty-1/conversation": respond(200, `{"sessionId": "empty-1", "controller": "ready"}`),
	})
	conv, err := c.Conversation(context.Background(), "volcano-3", ConversationQuery{Limit: 50, BeforeSequence: 40})
	if err != nil {
		t.Fatal(err)
	}
	if q := (*calls)[0].Query; q != "beforeSequence=40&limit=50" {
		t.Fatalf("query = %q", q)
	}
	if conv.Controller != "stopped" || !conv.HasMoreBefore || conv.OldestSequence != 40 || len(conv.Messages) != 2 || len(conv.Activities) != 1 {
		t.Fatalf("conversation = %+v", conv)
	}
	if m := conv.Messages[0]; m.Role != "user" || m.Text != "hello" || m.Sequence != 40 || m.Revision != 1 || m.ClientMessageID != "abc" {
		t.Fatalf("first message = %+v", m)
	}
	if m := conv.Messages[1]; !m.Streaming || m.Role != "assistant" {
		t.Fatalf("second message = %+v", m)
	}
	if a := conv.Activities[0]; a.ActivityKind != "command" || a.Status != "running" || a.Summary != "go test ./..." {
		t.Fatalf("activity = %+v", a)
	}

	// Missing arrays decode to empty slices so the API never emits null.
	empty, err := c.Conversation(context.Background(), "empty-1", ConversationQuery{})
	if err != nil || empty.Messages == nil || empty.Activities == nil {
		t.Fatalf("empty conversation = %+v, %v", empty, err)
	}
	if q := (*calls)[1].Query; q != "" {
		t.Fatalf("default query = %q, want none", q)
	}
}

func TestSendInterruptKillRenameImport(t *testing.T) {
	c, calls := fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/sessions/volcano-2/conversation/messages":  respond(202, `{"turnId": "t-1", "state": "queued", "duplicate": false}`),
		"POST /api/v1/sessions/volcano-2/conversation/interrupt": respond(204, ``),
		"POST /api/v1/sessions/volcano-2/kill":                   respond(200, `{"ok": true, "sessionId": "volcano-2"}`),
		"PATCH /api/v1/sessions/volcano-2":                       respond(200, `{"ok": true}`),
		"GET /api/v1/session-import": respond(200, `{"candidates": [
			{"id": "aea7", "harness": "codex", "title": "review #6247", "lastActivity": "2026-11-24T12:58:54.383Z", "sessionId": "standalone-1", "messageCount": 30},
			{"id": "422e", "harness": "codex", "title": "logo", "lastActivity": "2026-11-16T08:09:04.911Z", "messageCount": 80, "folder": "/root/github/website"},
			{"id": "6f1d", "harness": "copilot", "title": "Tool", "projectId": "volcano", "lastActivity": "2026-11-24T13:04:05.922Z", "messageCount": 61}]}`),
		"POST /api/v1/session-import": respond(200, `{"results": [{"id": "422e", "status": "created", "sessionId": "standalone-2"}, {"id": "bad", "status": "failed", "error": "unreadable"}]}`),
	})
	ctx := context.Background()

	res, err := c.SendMessage(ctx, "volcano-2", "run the tests", "uuid-1")
	if err != nil || res.TurnID != "t-1" || res.State != "queued" || res.Duplicate {
		t.Fatalf("SendMessage = %+v, %v", res, err)
	}
	jsonEqual(t, (*calls)[0].Body, `{"text":"run the tests","clientMessageId":"uuid-1"}`)

	if err := c.Interrupt(ctx, "volcano-2"); err != nil {
		t.Fatalf("Interrupt (204, empty body): %v", err)
	}
	if err := c.Kill(ctx, "volcano-2"); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, (*calls)[2].Body, `{}`)
	if err := c.Rename(ctx, "volcano-2", "New name"); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, (*calls)[3].Body, `{"displayName":"New name"}`)

	candidates, err := c.ImportCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ImportCandidate{
		{ID: "aea7", Harness: "codex", Title: "review #6247", LastActivity: "2026-11-24T12:58:54.383Z", SessionID: "standalone-1", MessageCount: 30},
		{ID: "422e", Harness: "codex", Title: "logo", LastActivity: "2026-11-16T08:09:04.911Z", MessageCount: 80, Folder: "/root/github/website"},
		{ID: "6f1d", Harness: "copilot", Title: "Tool", ProjectID: "volcano", LastActivity: "2026-11-24T13:04:05.922Z", MessageCount: 61},
	}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("candidates:\n got %+v\nwant %+v", candidates, want)
	}

	results, err := c.Import(ctx, []string{"422e", "bad"})
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, (*calls)[5].Body, `{"ids":["422e","bad"]}`)
	wantResults := []ImportResult{{ID: "422e", Status: "created", SessionID: "standalone-2"}, {ID: "bad", Status: "failed", Error: "unreadable"}}
	if !reflect.DeepEqual(results, wantResults) {
		t.Fatalf("results = %+v", results)
	}
}

func TestSessionIDsAreEscapedInPaths(t *testing.T) {
	c, calls := fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){})
	_ = c.Interrupt(context.Background(), "../projects?x=1")
	got := (*calls)[0]
	if got.Escaped != "/api/v1/sessions/..%2Fprojects%3Fx=1/conversation/interrupt" || got.Query != "" {
		t.Fatalf("request went to %q with query %q; the ID must stay a single escaped path segment", got.Escaped, got.Query)
	}
}

func TestResumeSurfacesAPIErrors(t *testing.T) {
	c, _ := fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/sessions/standalone-1/resume-agent": respond(409, `{"error":"conflict","code":"SESSION_OPEN_ELSEWHERE","message":"session is open in another terminal","requestId":"r-1"}`),
		"POST /api/v1/sessions/volcano-3/resume-agent":    respond(200, `{"ok": true, "sessionId": "volcano-3", "resumeMode": "native", "session": `+sessionJSON+`}`),
		"POST /api/v1/sessions/broken-1/resume-agent":     respond(502, `upstream exploded`),
	})
	ctx := context.Background()

	_, err := c.Resume(ctx, "standalone-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != 409 || apiErr.Code != "SESSION_OPEN_ELSEWHERE" || apiErr.Message != "session is open in another terminal" || apiErr.RequestID != "r-1" {
		t.Fatalf("APIError = %+v", apiErr)
	}
	if !IsCode(err, "SESSION_OPEN_ELSEWHERE") || IsCode(err, "SESSION_NOT_FOUND") {
		t.Fatal("IsCode does not match the error code")
	}

	sess, err := c.Resume(ctx, "volcano-3")
	if err != nil || sess.ID != "standalone-1" {
		t.Fatalf("Resume = %+v, %v", sess, err)
	}

	_, err = c.Resume(ctx, "broken-1")
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || apiErr.Message != "upstream exploded" {
		t.Fatalf("non-JSON error = %v", err)
	}
}

func TestUnreachableDaemon(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := New(url).ListSessions(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(url).ListSessions(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: err = %v, want context.Canceled", err)
	}
}

func TestEventsStream(t *testing.T) {
	var lastEventID string
	c, _ := fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/events": func(w http.ResponseWriter, r *http.Request) {
			lastEventID = r.Header.Get("Last-Event-ID")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": hello\n\n")
			fmt.Fprint(w, "id: 7\nevent: session_updated\ndata: {\"sessionId\":\"volcano-1\"}\n\n")
			fmt.Fprint(w, "id: 8\nevent: session_created\ndata: line one\ndata: line two\n\n")
			w.(http.Flusher).Flush()
		},
	})
	var got []Event
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The fake closes the stream after two events, which ends the call.
	if err := c.Events(ctx, "6", func(ev Event) { got = append(got, ev) }); err != nil {
		t.Fatalf("Events: %v", err)
	}
	want := []Event{
		{ID: "7", Type: "session_updated", Data: `{"sessionId":"volcano-1"}`},
		{ID: "8", Type: "session_created", Data: "line one\nline two"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	if lastEventID != "6" {
		t.Fatalf("Last-Event-ID = %q, want 6", lastEventID)
	}

	c, _ = fakeDaemon(t, map[string]func(http.ResponseWriter, *http.Request){})
	err := c.Events(ctx, "", func(Event) {})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || !strings.Contains(apiErr.Message, "event stream") {
		t.Fatalf("missing stream: err = %v", err)
	}
}
