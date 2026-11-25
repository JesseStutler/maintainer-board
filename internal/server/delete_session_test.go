package server

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/db"
)

func (hn *harness) ignored(sessionID string) bool {
	hn.t.Helper()
	marks, err := hn.store.IgnoredSessions(context.Background())
	if err != nil {
		hn.t.Fatal(err)
	}
	return marks[sessionID]
}

func (hn *harness) sessionCached(sessionID string) bool {
	hn.srv.mu.RLock()
	defer hn.srv.mu.RUnlock()
	_, ok := hn.srv.sessions[sessionID]
	return ok
}

func TestDeleteCardAndItsAOSession(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "A"), worker("volcano-3", "codex", "B"), worker("volcano-4", "codex", "C")}
	hn.ao.conversations["volcano-2"] = &ao.Conversation{Messages: []ao.Message{{ID: "m1", Sequence: 1, Role: "user", Text: "indexed text"}}}
	hn.syncAO()
	if err := hn.store.IndexContent(context.Background(), "volcano-2", []db.ContentMessage{{MessageID: "m1", Sequence: 1, Role: "user", Text: "indexed text"}}, 1, "s", false); err != nil {
		t.Fatal(err)
	}

	// Query parameter: the session is killed and deleted in AO, then the card goes.
	a := hn.cardBySession("volcano-2")
	out := hn.must(200, "DELETE", "/api/cards/"+a.ID+"?deleteAoSession=true", nil)
	if out["deleted"] != a.ID || out["aoSessionDeleted"] != true || out["aoSessionId"] != "volcano-2" {
		t.Fatalf("response = %v", out)
	}
	if !reflect.DeepEqual(hn.ao.deleted, []string{"volcano-2 cleanup=true"}) {
		t.Fatalf("AO deletes = %v", hn.ao.deleted)
	}
	if hn.cardBySession("volcano-2") != nil || hn.sessionCached("volcano-2") || hn.ignored("volcano-2") {
		t.Fatalf("after the delete: card %v, cached %v, dismissed %v", hn.cardBySession("volcano-2"), hn.sessionCached("volcano-2"), hn.ignored("volcano-2"))
	}
	if n, _ := hn.store.ContentHits(context.Background(), "indexed"); len(n) != 0 {
		t.Errorf("the conversation index of the deleted session was kept: %v", n)
	}
	// AO no longer reports the session, so the sync has nothing to bring back.
	hn.syncAO()
	if hn.cardBySession("volcano-2") != nil {
		t.Fatal("AO sync recreated the card of a deleted session")
	}

	// JSON body works as well.
	b := hn.cardBySession("volcano-3")
	hn.must(200, "DELETE", "/api/cards/"+b.ID, map[string]any{"deleteAoSession": true})
	if !reflect.DeepEqual(hn.ao.deleted, []string{"volcano-2 cleanup=true", "volcano-3 cleanup=true"}) || hn.cardBySession("volcano-3") != nil {
		t.Fatalf("AO deletes = %v", hn.ao.deleted)
	}

	// A card without a session is simply deleted, whatever the flag says.
	plain := hn.must(201, "POST", "/api/cards", map[string]any{"title": "No session"})["card"].(map[string]any)["id"].(string)
	out = hn.must(200, "DELETE", "/api/cards/"+plain+"?deleteAoSession=true", nil)
	if out["aoSessionDeleted"] != false || out["aoSessionId"] != "" || len(hn.ao.deleted) != 2 {
		t.Fatalf("card without a session: %v, AO deletes %v", out, hn.ao.deleted)
	}
	if code := hn.errorCode(404, "DELETE", "/api/cards/"+plain+"?deleteAoSession=true", nil); code != "NOT_FOUND" {
		t.Errorf("deleting twice: code = %s", code)
	}
}

func TestDeleteCardOnlyKeepsTheAOSession(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "A")}
	hn.syncAO()
	a := hn.cardBySession("volcano-2")

	out := hn.must(200, "DELETE", "/api/cards/"+a.ID+"?deleteAoSession=false", nil)
	if out["aoSessionDeleted"] != false || out["aoSessionId"] != "volcano-2" {
		t.Fatalf("response = %v", out)
	}
	if len(hn.ao.deleted) != 0 || len(hn.ao.killed) != 0 {
		t.Fatalf("the AO session was touched: deleted %v, killed %v", hn.ao.deleted, hn.ao.killed)
	}
	// The session is dismissed: every later sync leaves it without a card.
	if !hn.ignored("volcano-2") {
		t.Fatal("the session was not remembered as dismissed")
	}
	for i := 0; i < 3; i++ {
		hn.ao.set(func() { hn.ao.sessions[0].Activity.LastActivityAt = fmt.Sprintf("t%d", i+2) })
		hn.syncAO()
	}
	if hn.cardBySession("volcano-2") != nil {
		t.Fatal("AO sync resurrected the card of a session the user kept only in AO")
	}
	// It is still listed as a session that can be linked by hand...
	sessions := hn.must(200, "GET", "/api/sessions", nil)["sessions"].([]any)
	if len(sessions) != 1 || sessions[0].(map[string]any)["cardId"] != "" {
		t.Fatalf("sessions = %v", sessions)
	}
	// ...and linking it again on purpose lifts the dismissal.
	card := hn.must(201, "POST", "/api/cards", map[string]any{"title": "Linked again", "aoSessionId": "volcano-2"})["card"].(map[string]any)
	if card["aoSessionId"] != "volcano-2" || hn.ignored("volcano-2") {
		t.Fatalf("manual link after a dismissal: %v, dismissed %v", card, hn.ignored("volcano-2"))
	}
}

func TestDeleteCardReportsAORefusal(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "A")}
	hn.syncAO()
	a := hn.cardBySession("volcano-2")

	cases := []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{&ao.APIError{Status: 409, Code: "WORKSPACE_DIRTY", Message: "worktree has uncommitted changes"}, 409, "WORKSPACE_DIRTY",
			"AO did not delete session volcano-2: The worktree of this session has uncommitted changes, so AO refused to delete it. Commit or discard them (or run 'ao session cleanup'), then delete again. Nothing was removed from the board."},
		{&ao.APIError{Status: 409, Code: "SOMETHING_NEW", Message: "session is pinned"}, 409, "SOMETHING_NEW",
			"AO did not delete session volcano-2: Session is pinned. Nothing was removed from the board."},
		{fmt.Errorf("%w: connection refused", ao.ErrUnavailable), 503, "AO_OFFLINE",
			"AO did not delete session volcano-2: The AO daemon is not reachable at http://ao.test. Nothing was removed from the board."},
	}
	for _, tc := range cases {
		hn.ao.set(func() { hn.ao.deleteErr = tc.err })
		status, out := hn.call("DELETE", "/api/cards/"+a.ID+"?deleteAoSession=true", nil)
		e, _ := out["error"].(map[string]any)
		if status != tc.status || e["code"] != tc.code || e["message"] != tc.message {
			t.Errorf("%v: got %d %v, want %d %s %q", tc.err, status, e, tc.status, tc.code, tc.message)
		}
		// Nothing is claimed deleted: the card is there, still linked.
		if c := hn.cardBySession("volcano-2"); c == nil || c.ID != a.ID || hn.ignored("volcano-2") {
			t.Fatalf("%v: card %+v, dismissed %v", tc.err, c, hn.ignored("volcano-2"))
		}
		if code := hn.errorCode(tc.status, "DELETE", "/api/cards/"+a.ID+"/session", nil); code != tc.code {
			t.Errorf("%v: session delete code = %s", tc.err, code)
		}
		if c := hn.card(a.ID); c.AOSessionID != "volcano-2" {
			t.Fatalf("%v: the session was unlinked although AO kept it", tc.err)
		}
	}
	// Once AO agrees, the same request goes through.
	hn.ao.set(func() { hn.ao.deleteErr = nil })
	hn.must(200, "DELETE", "/api/cards/"+a.ID+"?deleteAoSession=true", nil)
	if hn.cardBySession("volcano-2") != nil || len(hn.ao.deleted) != 1 {
		t.Fatalf("retry: deleted %v", hn.ao.deleted)
	}
}

func TestDeleteAOSessionKeepsTheCard(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "Design doc")}
	hn.syncAO()
	a := hn.cardBySession("volcano-2")
	hn.must(200, "PATCH", "/api/cards/"+a.ID, map[string]any{"notes": "keep these notes"})

	out := hn.must(200, "DELETE", "/api/cards/"+a.ID+"/session", nil)
	card := out["card"].(map[string]any)
	if out["aoSessionDeleted"] != true || out["aoSessionId"] != "volcano-2" || card["aoSessionId"] != "" || card["session"] != nil || card["sessionMissing"] != false {
		t.Fatalf("response = %v", out)
	}
	if !reflect.DeepEqual(hn.ao.deleted, []string{"volcano-2 cleanup=true"}) {
		t.Fatalf("AO deletes = %v", hn.ao.deleted)
	}
	c := hn.card(a.ID)
	if c.AOSessionID != "" || c.AOTitle != "" || c.Notes != "keep these notes" || c.Title != "Design doc" {
		t.Fatalf("card = %+v", c)
	}
	events := hn.eventMessages(a.ID)
	if last := events[len(events)-1]; last != "Deleted AO session volcano-2 from Agent Orchestrator" {
		t.Fatalf("timeline = %v", events)
	}
	hn.syncAO()
	if hn.cardBySession("volcano-2") != nil || hn.ignored("volcano-2") {
		t.Fatal("a deleted session came back or stayed dismissed")
	}
	// The card can take a new session now.
	if code := hn.errorCode(409, "DELETE", "/api/cards/"+a.ID+"/session", nil); code != "NO_SESSION" {
		t.Errorf("no session left: code = %s", code)
	}
	if code := hn.errorCode(404, "DELETE", "/api/cards/c_missing/session", nil); code != "NOT_FOUND" {
		t.Errorf("unknown card: code = %s", code)
	}
}

func TestUnlinkEndpointDetachesOnly(t *testing.T) {
	hn := newHarness(t)
	hn.ao.sessions = []ao.Session{worker("volcano-2", "copilot", "A")}
	hn.syncAO()
	a := hn.cardBySession("volcano-2")

	out := hn.must(200, "POST", "/api/cards/"+a.ID+"/unlink", nil)
	if out["aoSessionId"] != "volcano-2" || out["card"].(map[string]any)["aoSessionId"] != "" {
		t.Fatalf("response = %v", out)
	}
	if len(hn.ao.deleted) != 0 || len(hn.ao.killed) != 0 {
		t.Fatalf("unlink touched the AO session: deleted %v, killed %v", hn.ao.deleted, hn.ao.killed)
	}
	if events := hn.eventMessages(a.ID); !strings.Contains(strings.Join(events, "|"), "Unlinked AO session volcano-2") {
		t.Fatalf("timeline = %v", events)
	}
	// The session stays in AO without getting a second card.
	hn.syncAO()
	if hn.cardBySession("volcano-2") != nil || !hn.sessionCached("volcano-2") || !hn.ignored("volcano-2") {
		t.Fatalf("after unlink: card %v, cached %v, dismissed %v", hn.cardBySession("volcano-2"), hn.sessionCached("volcano-2"), hn.ignored("volcano-2"))
	}
	if code := hn.errorCode(409, "POST", "/api/cards/"+a.ID+"/unlink", nil); code != "NO_SESSION" {
		t.Errorf("second unlink: code = %s", code)
	}
	// Linking it again by hand lifts the dismissal.
	hn.must(200, "PATCH", "/api/cards/"+a.ID, map[string]any{"aoSessionId": "volcano-2"})
	if hn.ignored("volcano-2") || hn.cardBySession("volcano-2") == nil {
		t.Fatal("re-linking did not lift the dismissal")
	}
}
