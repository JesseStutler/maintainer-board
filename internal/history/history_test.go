package history

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	copilotID = "8dd57e72-e587-4995-a8d8-15426ebca116"
	codexID   = "01a1c998-e2d3-7c11-a59c-8d52c0f94482"
)

// writeCopilotStore creates a session store with the schema of the Copilot
// CLI (the columns the index reads) under home/.copilot.
func writeCopilotStore(t *testing.T, home string, stmts ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".copilot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-store.db")
	store, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	schema := []string{
		`CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, cwd TEXT, repository TEXT, host_type TEXT, branch TEXT, summary TEXT, created_at TEXT, updated_at TEXT)`,
		`CREATE TABLE IF NOT EXISTS turns (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, turn_index INTEGER NOT NULL, user_message TEXT, assistant_response TEXT, timestamp TEXT)`,
		`CREATE TABLE IF NOT EXISTS checkpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, checkpoint_number INTEGER NOT NULL, title TEXT, overview TEXT)`,
	}
	for _, stmt := range append(schema, stmts...) {
		if _, err := store.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return path
}

func writeRollout(t *testing.T, home, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "11", "11")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func meta(id, cwd, branch string) string {
	git := ""
	if branch != "" {
		git = fmt.Sprintf(`,"git":{"commit_hash":"abc","branch":%q,"repository_url":"https://github.com/volcano-sh/volcano.git"}`, branch)
	}
	return fmt.Sprintf(`{"timestamp":"2026-11-11T07:53:22Z","ordinal":0,"type":"session_meta","payload":{"session_id":%q,"id":%q,"cwd":%q,"base_instructions":{"text":"You are Codex."}%s}}`, id, id, cwd, git)
}

func userItem(text string) string {
	return fmt.Sprintf(`{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","thread_id":"x","item":{"type":"UserMessage","id":"i","content":[{"type":"text","text":%q}]}}}`, text)
}

func agentItem(text string) string {
	return fmt.Sprintf(`{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","thread_id":"x","item":{"type":"AgentMessage","id":"i","content":[{"type":"Text","text":%q}]}}}`, text)
}

func transcript(role, kind, text string) string {
	return fmt.Sprintf(`{"timestamp":"t","type":"response_item","payload":{"type":"message","id":"m","role":%q,"content":[{"type":%q,"text":%q}]}}`, role, kind, text)
}

func toolOutput(size int) string {
	// Tool output quotes JSON with escaped quotes, like the real files.
	return `{"timestamp":"t","type":"response_item","payload":{"type":"custom_tool_call_output","output":[{"type":"input_text","text":"{\"type\":\"UserMessage\",\"role\":\"user\"} ` + strings.Repeat("x", size) + `"}]}}`
}

func TestCandidateIDMatchesAO(t *testing.T) {
	// A pair observed on a real AO daemon.
	got := CandidateID("codex", "/root/.codex", codexID)
	if got != "cb8ceb50a897c211e6d97c0d21890d6c29642a3e0027e6872061bca7434d43ac" {
		t.Fatalf("CandidateID = %s", got)
	}
	if CandidateID("copilot", "/root/.codex", codexID) == got || CandidateID("codex", "/home/x/.codex", codexID) == got {
		t.Fatal("harness and config directory must be part of the ID")
	}
}

func TestCopilotStore(t *testing.T) {
	home := t.TempDir()
	writeCopilotStore(t, home,
		`INSERT INTO sessions (id, cwd, repository, branch, summary, updated_at) VALUES
			('`+copilotID+`', '/root/github/volcano', 'volcano-sh/volcano', 'feat/board', 'Tool for PR Review Tracking', '2026-11-25T03:07:00Z'),
			('11111111-2222-3333-4444-555555555555', NULL, NULL, NULL, NULL, NULL)`,
		`INSERT INTO turns (session_id, turn_index, user_message, assistant_response) VALUES
			('`+copilotID+`', 1, 'SECOND: add an author filter', 'Added the filter.'),
			('`+copilotID+`', 0, 'FIRST: build a kanban board for my PR reviews', 'I will build Maintainer Board.'),
			('`+copilotID+`', 2, NULL, 'A reply without a prompt')`,
		`INSERT INTO checkpoints (session_id, checkpoint_number, title, overview) VALUES
			('`+copilotID+`', 1, 'Deploying AO', 'Old overview'),
			('`+copilotID+`', 2, 'Building Maintainer Board UX', 'The user wanted a unified zero-emoji Kanban GUI.')`,
	)
	ix := New(home)
	all := ix.Sessions(context.Background())
	if len(all) != 2 {
		t.Fatalf("%d sessions, want 2", len(all))
	}
	s := all[CandidateID("copilot", filepath.Join(home, ".copilot"), copilotID)]
	if s == nil {
		t.Fatal("the session is not keyed by its AO candidate ID")
	}
	if s.NativeID != copilotID || s.Harness != "copilot" || s.CWD != "/root/github/volcano" || s.Branch != "feat/board" ||
		s.Repository != "volcano-sh/volcano" || s.Title != "Tool for PR Review Tracking" {
		t.Fatalf("session = %+v", s)
	}
	// The newest checkpoint is the summary; prompts come in turn order.
	if s.Summary != "The user wanted a unified zero-emoji Kanban GUI." {
		t.Errorf("summary = %q", s.Summary)
	}
	if p := s.Prompts(5); len(p) != 2 || !strings.HasPrefix(p[0], "FIRST:") || !strings.HasPrefix(p[1], "SECOND:") {
		t.Errorf("prompts = %q", p)
	}
	for _, want := range []string{"kanban board for my pr reviews", "added the filter.", "a reply without a prompt", "building maintainer board ux", "old overview"} {
		if !strings.Contains(s.Folded(), want) {
			t.Errorf("searchable text lacks %q", want)
		}
	}
	// A session with only NULL columns is still listed.
	if empty := all[CandidateID("copilot", filepath.Join(home, ".copilot"), "11111111-2222-3333-4444-555555555555")]; empty == nil || len(empty.Messages) != 0 {
		t.Errorf("empty session = %+v", empty)
	}
}

func TestCopilotStoreDegradesGracefully(t *testing.T) {
	ctx := context.Background()
	// No stores at all.
	if got := New(t.TempDir()).Sessions(ctx); len(got) != 0 {
		t.Fatalf("empty home: %d sessions", len(got))
	}
	// A file that is not a database.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".copilot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".copilot", "session-store.db"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRollout(t, home, "rollout-2026-11-11T07-53-22-"+codexID+".jsonl", meta(codexID, "/root", ""), userItem("still found"))
	if got := New(home).Sessions(ctx); len(got) != 1 {
		t.Fatalf("corrupt Copilot store: %d sessions, want the Codex one", len(got))
	}
	// A store of another schema: sessions without turns or checkpoints.
	home = t.TempDir()
	dir := filepath.Join(home, ".copilot")
	_ = os.MkdirAll(dir, 0o755)
	store, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "session-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, repository TEXT, branch TEXT, summary TEXT, updated_at TEXT)`,
		`INSERT INTO sessions (id, summary) VALUES ('` + copilotID + `', 'Only a title')`,
	} {
		if _, err := store.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	got := New(home).Sessions(ctx)
	if len(got) != 1 {
		t.Fatalf("store without turns: %d sessions", len(got))
	}
	for _, s := range got {
		if s.Title != "Only a title" {
			t.Errorf("title = %q", s.Title)
		}
	}
	// The store is opened read-only: a scan never creates or changes it.
	missing := filepath.Join(t.TempDir(), ".copilot", "session-store.db")
	New(filepath.Dir(filepath.Dir(missing))).Sessions(ctx)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("scanning created %s", missing)
	}
}

func TestCodexRollout(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	writeRollout(t, home, "rollout-2026-11-11T07-53-22-"+codexID+".jsonl",
		meta(codexID, "/root/github/volcano", "refactor/events"),
		transcript("developer", "input_text", "You are Codex."),
		transcript("user", "input_text", "# AGENTS.md instructions for /root/github/volcano"),
		transcript("user", "input_text", "<environment_context>cwd</environment_context>"),
		userItem("unify the pod and podgroup EVENT handling in #6081 and #6196"),
		transcript("user", "input_text", "unify the pod and podgroup EVENT handling in #6081 and #6196"),
		agentItem("I will read both issues first."),
		transcript("assistant", "output_text", "I will read both issues first."),
		toolOutput(2000),
		"this line is not JSON",
		userItem("now drop the unnecessary retries"),
		userItem("now drop the unnecessary retries"),
	)
	// An older CLI wrote plain user_message and agent_message events.
	const oldID = "01a1572e-c582-7433-ae9f-c435b4c1dc20"
	writeRollout(t, home, "rollout-2026-10-20T02-40-45-"+oldID+".jsonl",
		meta(oldID, "/root", ""),
		`{"timestamp":"t","type":"event_msg","payload":{"type":"user_message","message":"can you see my server\n","images":[]}}`,
		`{"timestamp":"t","type":"event_msg","payload":{"type":"agent_message","message":"Not directly."}}`,
	)
	// A file with only the raw model transcript falls back to it, without
	// the context the CLI injected.
	const rawID = "019f6a4a-8831-7873-bc46-5c790de323e1"
	writeRollout(t, home, "rollout-2026-07-16T09-38-17-"+rawID+".jsonl",
		transcript("user", "input_text", "<user_instructions>be brief</user_instructions>"),
		transcript("user", "input_text", "review PR 5638 for races"),
		transcript("assistant", "output_text", "Looking at the informer code."),
	)
	writeRollout(t, home, "notes.jsonl", userItem("a file without a session ID"))
	writeRollout(t, home, "readme.txt", "not a rollout")

	all := New(home).Sessions(context.Background())
	if len(all) != 3 {
		t.Fatalf("%d sessions, want 3: %v", len(all), all)
	}
	s := all[CandidateID("codex", codexDir, codexID)]
	if s == nil {
		t.Fatal("the rollout is not keyed by its AO candidate ID")
	}
	if s.NativeID != codexID || s.Harness != "codex" || s.CWD != "/root/github/volcano" || s.Branch != "refactor/events" || s.Repository != "volcano-sh/volcano" {
		t.Fatalf("session = %+v", s)
	}
	// What the user typed, once each, without the injected context; the
	// title is the first prompt.
	prompts := s.Prompts(9)
	if len(prompts) != 2 || prompts[0] != "unify the pod and podgroup EVENT handling in #6081 and #6196" || prompts[1] != "now drop the unnecessary retries" {
		t.Fatalf("prompts = %q", prompts)
	}
	if s.Title != prompts[0] {
		t.Errorf("title = %q", s.Title)
	}
	if n := strings.Count(s.Folded(), "i will read both issues first."); n != 1 {
		t.Errorf("the agent reply is indexed %d times", n)
	}
	if strings.Contains(s.Folded(), "agents.md") || strings.Contains(s.Folded(), "environment_context") || strings.Contains(s.Folded(), "xxxx") {
		t.Errorf("injected context or tool output was indexed: %q", s.Folded())
	}

	old := all[CandidateID("codex", codexDir, oldID)]
	if old == nil || len(old.Prompts(2)) != 1 || old.Prompts(1)[0] != "can you see my server" || !strings.Contains(old.Folded(), "not directly.") {
		t.Errorf("old format session = %+v", old)
	}
	raw := all[CandidateID("codex", codexDir, rawID)]
	if raw == nil || len(raw.Prompts(2)) != 1 || raw.Prompts(1)[0] != "review PR 5638 for races" || !strings.Contains(raw.Folded(), "informer code") {
		t.Errorf("transcript-only session = %+v", raw)
	}
}

func TestCodexRolloutReadsOnlyTheHeadAndTail(t *testing.T) {
	home := t.TempDir()
	lines := []string{meta(codexID, "/root", ""), userItem("HEAD prompt about preemption")}
	// Three megabytes of tool output push the middle out of both windows.
	for i := 0; i < 6; i++ {
		lines = append(lines, toolOutput(500000))
	}
	lines = append(lines[:5], append([]string{userItem("MIDDLE prompt that is skipped")}, lines[5:]...)...)
	lines = append(lines, agentItem("TAIL reply"), userItem("TAIL prompt about the release"))
	path := writeRollout(t, home, "rollout-2026-11-11T07-53-22-"+codexID+".jsonl", lines...)
	if st, _ := os.Stat(path); st.Size() < 2<<20 {
		t.Fatalf("fixture is only %d bytes", st.Size())
	}
	start := time.Now()
	all := New(home).Sessions(context.Background())
	elapsed := time.Since(start)
	s := all[CandidateID("codex", filepath.Join(home, ".codex"), codexID)]
	if s == nil {
		t.Fatal("no session")
	}
	prompts := s.Prompts(9)
	if len(prompts) != 2 || prompts[0] != "HEAD prompt about preemption" || prompts[1] != "TAIL prompt about the release" {
		t.Fatalf("prompts = %q", prompts)
	}
	if !strings.Contains(s.Folded(), "tail reply") {
		t.Error("the tail window was not read")
	}
	if elapsed > 2*time.Second {
		t.Errorf("scanning one large rollout took %v", elapsed)
	}
}

func TestIndexCachesAndRefreshes(t *testing.T) {
	home := t.TempDir()
	ctx := context.Background()
	path := writeRollout(t, home, "rollout-2026-11-11T07-53-22-"+codexID+".jsonl", meta(codexID, "/root", ""), userItem("first version"))
	ix := New(home)
	now := time.Unix(1_800_000_000, 0)
	ix.Now = func() time.Time { return now }
	id := CandidateID("codex", filepath.Join(home, ".codex"), codexID)

	first := ix.Sessions(ctx)[id]
	if first == nil || first.Prompts(1)[0] != "first version" {
		t.Fatalf("first scan = %+v", first)
	}
	// Within the TTL nothing is read again, even though the file changed.
	if err := os.WriteFile(path, []byte(meta(codexID, "/root", "")+"\n"+userItem("second version, longer")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(DefaultTTL - time.Second)
	if got := ix.Sessions(ctx)[id]; got != first {
		t.Fatal("the scan was repeated within the TTL")
	}
	// After the TTL the changed file is parsed again...
	now = now.Add(2 * time.Second)
	second := ix.Sessions(ctx)[id]
	if second == nil || second.Prompts(1)[0] != "second version, longer" {
		t.Fatalf("after the TTL = %+v", second)
	}
	// ...and an unchanged one is reused as it is.
	now = now.Add(DefaultTTL + time.Second)
	if got := ix.Sessions(ctx)[id]; got != second {
		t.Fatal("an unchanged rollout was parsed again")
	}
	// Invalidate forces a scan; a deleted file disappears.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ix.Invalidate()
	if got := ix.Sessions(ctx); len(got) != 0 {
		t.Fatalf("after the file was deleted: %d sessions", len(got))
	}

	// A new Copilot store row shows up once the file changed.
	db := writeCopilotStore(t, home, `INSERT INTO sessions (id, summary) VALUES ('`+copilotID+`', 'one')`)
	ix.Invalidate()
	if got := ix.Sessions(ctx); len(got) != 1 {
		t.Fatalf("copilot store: %d sessions", len(got))
	}
	writeCopilotStore(t, home, `INSERT INTO sessions (id, summary) VALUES ('22222222-2222-3333-4444-555555555555', 'two')`)
	_ = os.Chtimes(db, now, now.Add(time.Hour))
	ix.Invalidate()
	if got := ix.Sessions(ctx); len(got) != 2 {
		t.Fatalf("copilot store after an insert: %d sessions", len(got))
	}

	var nilIndex *Index
	if nilIndex.Sessions(ctx) != nil {
		t.Error("a nil index must report no sessions")
	}
	nilIndex.Invalidate()
}

func TestMessageBudget(t *testing.T) {
	var c collector
	c.add("user", strings.Repeat("u", maxUserRunes+500))
	c.add("assistant", strings.Repeat("a", maxAssistantRunes+500))
	c.add("user", "  ")
	c.add("checkpoint", "overview")
	msgs := c.messages()
	if len(msgs) != 3 || msgs[0].Role != "user" || msgs[1].Role != "checkpoint" || msgs[2].Role != "assistant" {
		t.Fatalf("messages = %+v", msgs)
	}
	if len(msgs[0].Text) != maxUserRunes || len(msgs[2].Text) != maxAssistantRunes {
		t.Errorf("lengths = %d / %d", len(msgs[0].Text), len(msgs[2].Text))
	}
	// The user prompts win when the session budget runs out.
	var big collector
	for i := 0; i < maxUserMessages; i++ {
		big.add("user", fmt.Sprintf("%d %s", i, strings.Repeat("p", maxUserRunes)))
	}
	big.add("assistant", "never fits")
	total := 0
	for _, m := range big.messages() {
		if m.Role != "user" {
			t.Fatalf("a %s message was kept over user prompts", m.Role)
		}
		total += len(m.Text)
	}
	if total > maxSessionRunes || total < maxSessionRunes-maxUserRunes {
		t.Errorf("kept %d runes, budget %d", total, maxSessionRunes)
	}
}
