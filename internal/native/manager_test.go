package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maintainer-board/internal/ao"
)

func TestStorePersistenceAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "native_sessions.json")

	st, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rec := SessionRecord{
		ID:           "01a215dd-f08e-7013-bdad-7bddb872957a",
		Harness:      "codex",
		ProjectID:    "volcano",
		CWD:          "/repo/volcano",
		DisplayName:  "PR #6291 review",
		ApprovalMode: "bypass-permissions",
	}
	if err := st.Upsert(rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	reloaded, err := openSessionStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, ok := reloaded.Get(rec.ID)
	if !ok {
		t.Fatalf("expected persisted session %s", rec.ID)
	}
	if got.DisplayName != rec.DisplayName || got.CWD != rec.CWD {
		t.Fatalf("unexpected record: %+v", got)
	}

	if err := reloaded.Delete(rec.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reloaded.Get(rec.ID); ok {
		t.Fatalf("session should be deleted")
	}
}

func TestReadCodexRolloutAndPaging(t *testing.T) {
	home := t.TempDir()
	sid := "01a215dd-0000-7000-8000-000000000001"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "11", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "" +
		`{"timestamp":"2026-11-26T01:00:00Z","type":"session_meta","payload":{"id":"` + sid + `","cwd":"/repo/volcano"}}` + "\n" +
		`{"timestamp":"2026-11-26T01:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"run unit tests"}}` + "\n" +
		`{"timestamp":"2026-11-26T01:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"go test ./...\"}"}}` + "\n" +
		`{"timestamp":"2026-11-26T01:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"Chunk ID: 1\nOutput:\nok  pkg/scheduler 0.42s"}}` + "\n" +
		`{"timestamp":"2026-11-26T01:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"All tests passed."}}` + "\n"

	rolloutPath := filepath.Join(dir, "rollout-2026-11-26T01-00-00-"+sid+".jsonl")
	if err := os.WriteFile(rolloutPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	buf, err := readCodexRollout(home, sid)
	if err != nil {
		t.Fatalf("readCodexRollout: %v", err)
	}
	conv := buf.snapshot(sid, "codex", "native", "Test", ao.ConversationQuery{Limit: 50})
	if len(conv.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(conv.Messages), conv.Messages)
	}
	if conv.Messages[0].Role != "user" || conv.Messages[0].Text != "run unit tests" {
		t.Fatalf("unexpected first message: %+v", conv.Messages[0])
	}
	if conv.Messages[1].Role != "assistant" || conv.Messages[1].Text != "All tests passed." {
		t.Fatalf("unexpected second message: %+v", conv.Messages[1])
	}
	if len(conv.Activities) != 1 || conv.Activities[0].ActivityKind != "command" {
		t.Fatalf("expected 1 command activity, got %+v", conv.Activities)
	}
	if string(conv.Activities[0].Detail) == "" || !strings.Contains(string(conv.Activities[0].Detail), "ok  pkg/scheduler 0.42s") {
		t.Fatalf("expected command output in activity detail, got %s", string(conv.Activities[0].Detail))
	}

	// Verify pagination returns the newest window and marks HasMoreBefore.
	paged := buf.snapshot(sid, "codex", "native", "Test", ao.ConversationQuery{Limit: 1})
	if len(paged.Messages) != 1 || paged.Messages[0].Text != "All tests passed." || !paged.HasMoreBefore {
		t.Fatalf("unexpected paged snapshot: %+v", paged)
	}
}

func TestReadCopilotEvents(t *testing.T) {
	home := t.TempDir()
	sid := "07081b91-0aee-421b-9d18-4d2a62ca81de"
	dir := filepath.Join(home, ".copilot", "session-state", sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "" +
		`{"type":"user.message","id":"u1","timestamp":"2026-11-26T01:00:01Z","data":{"content":"check PR 6291"}}` + "\n" +
		`{"type":"tool.execution_start","id":"t1","timestamp":"2026-11-26T01:00:02Z","data":{"toolCallId":"tc1","toolName":"bash","arguments":{"command":"gh pr view 6291"}}}` + "\n" +
		`{"type":"tool.execution_complete","id":"t2","timestamp":"2026-11-26T01:00:03Z","data":{"toolCallId":"tc1","success":true}}` + "\n" +
		`{"type":"assistant.message","id":"a1","timestamp":"2026-11-26T01:00:04Z","data":{"messageId":"m1","content":"Looks clean."}}` + "\n"

	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	buf, err := readCopilotEvents(home, sid)
	if err != nil {
		t.Fatalf("readCopilotEvents: %v", err)
	}
	conv := buf.snapshot(sid, "copilot", "native", "PR", ao.ConversationQuery{})
	if len(conv.Messages) != 2 || len(conv.Activities) != 1 {
		t.Fatalf("expected 2 messages and 1 activity, got %d / %d", len(conv.Messages), len(conv.Activities))
	}
	if conv.Activities[0].Status != "completed" || conv.Activities[0].ActivityKind != "command" {
		t.Fatalf("unexpected activity: %+v", conv.Activities[0])
	}
}

func TestApprovalModeMappings(t *testing.T) {
	pol, sb, turnSB := codexApprovalSettings("bypass-permissions")
	if pol != "never" || sb != "danger-full-access" || turnSB["type"] != "dangerFullAccess" {
		t.Fatalf("unexpected codex yolo settings: %q %q %+v", pol, sb, turnSB)
	}
	if got := copilotAllowAllValue("bypass-permissions"); got != "on" {
		t.Fatalf("expected copilot allow_all=on, got %q", got)
	}
	if got := copilotAllowAllValue("auto"); got != "auto" {
		t.Fatalf("expected copilot allow_all=auto, got %q", got)
	}
	if got := copilotAllowAllValue("default"); got != "off" {
		t.Fatalf("expected copilot allow_all=off, got %q", got)
	}
}

func TestImportAndConversationFromLocalHistory(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	repoDir := filepath.Join(t.TempDir(), "volcano")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sid := "01a215dd-0000-7000-8000-000000000099"
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "11", "26")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := "" +
		`{"timestamp":"2026-11-26T02:00:00Z","type":"session_meta","payload":{"id":"` + sid + `","cwd":"` + repoDir + `"}}` + "\n" +
		`{"timestamp":"2026-11-26T02:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"investigate scheduler race"}}` + "\n" +
		`{"timestamp":"2026-11-26T02:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"Found the lock ordering issue."}}` + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "rollout-2026-11-26T02-00-00-"+sid+".jsonl"), []byte(rollout), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := New(Config{
		DataDir:             dataDir,
		HomeDir:             home,
		Workspaces:          []string{repoDir},
		DefaultApprovalMode: "bypass-permissions",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cands, err := mgr.ImportCandidates(ctx)
	if err != nil {
		t.Fatalf("ImportCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ProjectID != "volcano" {
		t.Fatalf("expected 1 candidate in project volcano, got %+v", cands)
	}

	res, err := mgr.Import(ctx, []string{cands[0].ID})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res) != 1 || res[0].Status != "imported" || res[0].SessionID != sid {
		t.Fatalf("unexpected import result: %+v", res)
	}

	sessions, err := mgr.ListSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("ListSessions: %v %+v", err, sessions)
	}
	if sessions[0].ID != sid || sessions[0].NativeSessionID != sid || sessions[0].CWD != repoDir {
		t.Fatalf("unexpected session metadata: %+v", sessions[0])
	}

	conv, err := mgr.Conversation(ctx, sid, ao.ConversationQuery{})
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Text != "Found the lock ordering issue." {
		t.Fatalf("unexpected conversation: %+v", conv)
	}
}

func TestResolveWorkspaceAcceptsDynamicDirectory(t *testing.T) {
	defaultDir := filepath.Join(t.TempDir(), "volcano")
	dynamicDir := filepath.Join(t.TempDir(), "kubernetes")
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dynamicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, err := New(Config{
		DataDir:    t.TempDir(),
		HomeDir:    t.TempDir(),
		Workspaces: []string{defaultDir},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	id, cwd := mgr.resolveWorkspace(dynamicDir)
	if id != "kubernetes" || cwd != dynamicDir {
		t.Fatalf("resolveWorkspace(dynamicDir) = (%q, %q), want (kubernetes, %q)", id, cwd, dynamicDir)
	}
	fallbackID, fallbackCWD := mgr.resolveWorkspace(filepath.Join(t.TempDir(), "does-not-exist"))
	if fallbackID != "volcano" || fallbackCWD != defaultDir {
		t.Fatalf("resolveWorkspace(missing) = (%q, %q), want (volcano, %q)", fallbackID, fallbackCWD, defaultDir)
	}
}

