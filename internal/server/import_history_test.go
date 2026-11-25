package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"maintainer-board/internal/ao"
	"maintainer-board/internal/history"
	"maintainer-board/internal/summary"
)

const (
	boardSessionID  = "8dd57e72-e587-4995-a8d8-15426ebca116"
	helperSessionID = "3425333e-2b9d-4f05-80a7-6986642cdd8d"
	eventsSessionID = "01a1c998-e2d3-7c11-a59c-8d52c0f94482"
	csiSessionID    = "01a1c02a-7f76-7dc0-97f0-5a417ba188b7"
	// "As a Volcano maintainer I often review all kinds of PR issues and
	// want a kanban tool", the way the user wrote it.
	boardPrompt = "\u6211\u4f5c\u4e3a Volcano Maintainers\uff0c\u7ecf\u5e38\u8981 review \u5404\u79cd PR issues\uff0c\u60f3\u8981\u4e00\u4e2a\u770b\u677f\u5de5\u5177"
)

func rolloutLine(kind, text string) string {
	return fmt.Sprintf(`{"timestamp":"t","type":"event_msg","payload":{"type":"item_completed","item":{"type":%q,"content":[{"type":"text","text":%q}]}}}`, kind, text)
}

// historyHarness serves an import history of four real sessions and one
// AI helper run. Three of the real ones and the helper run exist in local
// Copilot and Codex stores under a temporary home directory; the fourth is
// known to AO only. It returns the AO candidate ID of each session by name.
func historyHarness(t *testing.T) (*harness, map[string]string) {
	t.Helper()
	hn := newHarness(t)
	home := t.TempDir()
	copilotDir, codexDir := filepath.Join(home, ".copilot"), filepath.Join(home, ".codex")
	if err := os.MkdirAll(copilotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := sql.Open("sqlite", "file:"+filepath.Join(copilotDir, "session-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, repository TEXT, host_type TEXT, branch TEXT, summary TEXT, created_at TEXT, updated_at TEXT)`,
		`CREATE TABLE turns (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, turn_index INTEGER NOT NULL, user_message TEXT, assistant_response TEXT)`,
		`CREATE TABLE checkpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, checkpoint_number INTEGER NOT NULL, title TEXT, overview TEXT)`,
		`INSERT INTO sessions (id, cwd, repository, branch, summary) VALUES
			('` + boardSessionID + `', '/root/tools/maintainer-board', 'volcano-sh/volcano', 'feat/board', 'Tool for PR Review Tracking'),
			('` + helperSessionID + `', '/root/tools/maintainer-board/data/summarizer', NULL, 'main', 'Renamed by hand')`,
		`INSERT INTO turns (session_id, turn_index, user_message, assistant_response) VALUES
			('` + boardSessionID + `', 0, '` + boardPrompt + `', 'I will build it on top of agent-orchestrator and the gh CLI.'),
			('` + boardSessionID + `', 1, 'Add an author filter and brand logos', 'Done, both are on the card face now.'),
			('` + helperSessionID + `', 0, '` + summary.PromptMarker + ` Describe what a card is about.', 'A summary.')`,
		`INSERT INTO checkpoints (session_id, checkpoint_number, title, overview) VALUES
			('` + boardSessionID + `', 1, 'Building Maintainer Board', 'The user wanted a unified zero-emoji Kanban GUI for review sessions.')`,
	} {
		if _, err := store.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	store.Close()

	rollouts := filepath.Join(codexDir, "sessions", "2026", "11", "11")
	if err := os.MkdirAll(rollouts, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(id, cwd, branch string, lines ...string) {
		metaLine := fmt.Sprintf(`{"timestamp":"t","type":"session_meta","payload":{"id":%q,"cwd":%q,"git":{"branch":%q}}}`, id, cwd, branch)
		body := strings.Join(append([]string{metaLine}, lines...), "\n") + "\n"
		if err := os.WriteFile(filepath.Join(rollouts, "rollout-2026-11-11T07-53-22-"+id+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(eventsSessionID, "/root/github/volcano", "refactor/events",
		rolloutLine("UserMessage", "#6081 #6196 unify the pod and podgroup event/condition refactor, drop the unnecessary retries and rollback"),
		rolloutLine("AgentMessage", "The scheduler cache updates PodGroup conditions twice per cycle."))
	write(csiSessionID, "/root/github/volcano", "master",
		rolloutLine("UserMessage", "Investigate why preemption regresses for CSI volume limits in PR 6247"),
		rolloutLine("AgentMessage", "Reproduced on a kind cluster: the high priority pod stays pending."))

	ids := map[string]string{
		"board":  history.CandidateID("copilot", copilotDir, boardSessionID),
		"helper": history.CandidateID("copilot", copilotDir, helperSessionID),
		"events": history.CandidateID("codex", codexDir, eventsSessionID),
		"csi":    history.CandidateID("codex", codexDir, csiSessionID),
		"design": "aoonly00000000000000000000000000000000000000000000000000000000ff",
	}
	hn.ao.sessions = []ao.Session{worker("standalone-2", "codex", "Event refactor")}
	hn.ao.candidates = []ao.ImportCandidate{
		{ID: ids["board"], Harness: "copilot", Title: "Tool for PR Review Tracking", ProjectID: "volcano", LastActivity: "2026-11-25T03:07:00Z", MessageCount: 92},
		// AO shows the renamed title; only the local first prompt gives the run away.
		{ID: ids["helper"], Harness: "copilot", Title: "Renamed by hand", LastActivity: "2026-11-25T03:01:00Z", MessageCount: 2},
		{ID: ids["events"], Harness: "codex", Title: "#6081 #6196 pod podgroup event refactor", Folder: "/root/github/volcano", LastActivity: "2026-11-11T07:53:22Z", MessageCount: 240, SessionID: "standalone-2"},
		{ID: ids["csi"], Harness: "codex", Title: "Investigate why preemption regresses", Folder: "/root/github/volcano", LastActivity: "2026-10-21T10:16:38Z", MessageCount: 61},
		{ID: ids["design"], Harness: "copilot", Title: "Kubeflow Trainer integration design", Folder: "/root/kubeflow", LastActivity: "2026-09-01T00:00:00Z", MessageCount: 12},
	}
	hn.srv.cfg.History = history.New(home)
	hn.syncAO()
	return hn, ids
}

func candidateIDs(list []any) []string {
	out := []string{}
	for _, item := range list {
		m := item.(map[string]any)
		if c, ok := m["candidate"].(map[string]any); ok {
			m = c
		}
		out = append(out, m["id"].(string))
	}
	return out
}

func TestImportHistoryIsEnrichedFromLocalStores(t *testing.T) {
	hn, ids := historyHarness(t)
	list := hn.must(200, "GET", "/api/ao/import-history", nil)["candidates"].([]any)
	if got, want := candidateIDs(list), []string{ids["board"], ids["events"], ids["csi"], ids["design"]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v (the AI helper run must be hidden)", got, want)
	}
	board := list[0].(map[string]any)
	if board["nativeId"] != boardSessionID || board["branch"] != "feat/board" ||
		board["summary"] != "The user wanted a unified zero-emoji Kanban GUI for review sessions." ||
		board["preview"] != boardPrompt+" / Add an author filter and brand logos" {
		t.Fatalf("copilot candidate = %v", board)
	}
	// The AO fields stay as they were.
	if board["harness"] != "copilot" || board["title"] != "Tool for PR Review Tracking" || board["projectId"] != "volcano" ||
		board["messageCount"] != float64(92) || board["lastActivity"] != "2026-11-25T03:07:00Z" || board["cardId"] != "" {
		t.Fatalf("AO fields of the copilot candidate = %v", board)
	}
	events := list[1].(map[string]any)
	if events["nativeId"] != eventsSessionID || events["branch"] != "refactor/events" || events["sessionId"] != "standalone-2" ||
		events["cardId"] != hn.cardBySession("standalone-2").ID || events["folder"] != "/root/github/volcano" {
		t.Fatalf("codex candidate = %v", events)
	}
	if !strings.HasPrefix(events["preview"].(string), "#6081 #6196 unify the pod and podgroup") {
		t.Fatalf("codex preview = %v", events["preview"])
	}
	// A candidate without local data is listed as AO reports it.
	design := list[3].(map[string]any)
	for _, key := range []string{"nativeId", "summary", "preview", "branch"} {
		if _, ok := design[key]; ok {
			t.Errorf("AO-only candidate has %s = %v", key, design[key])
		}
	}
	if design["title"] != "Kubeflow Trainer integration design" {
		t.Fatalf("AO-only candidate = %v", design)
	}
}

func TestImportHistorySearch(t *testing.T) {
	hn, ids := historyHarness(t)
	search := func(q string) (map[string]any, []any) {
		t.Helper()
		out := hn.must(200, "GET", "/api/ao/import-history/search?q="+strings.ReplaceAll(q, " ", "+"), nil)
		return out, out["matches"].([]any)
	}
	only := func(q, name string) map[string]any {
		t.Helper()
		out, matches := search(q)
		if got := candidateIDs(matches); len(got) != 1 || got[0] != ids[name] || out["total"] != float64(1) {
			t.Fatalf("search %q = %v (total %v), want only %s", q, got, out["total"], name)
		}
		return matches[0].(map[string]any)
	}
	by := func(m map[string]any) string {
		var parts []string
		for _, v := range m["matchedBy"].([]any) {
			parts = append(parts, v.(string))
		}
		return strings.Join(parts, ",")
	}

	// Session IDs: native UUID prefixes in any case, the AO session ID of
	// an imported session, and AO's candidate ID.
	if m := only("01a1c02a", "csi"); by(m) != "id" || m["reason"] != "Matched ID: "+csiSessionID {
		t.Fatalf("native ID prefix: %v", m)
	}
	if m := only("8DD57E72-e587", "board"); by(m) != "id" || m["reason"] != "Matched ID: "+boardSessionID {
		t.Fatalf("upper-case native ID: %v", m)
	}
	if m := only("8d52c0f94482", "events"); m["reason"] != "Matched ID: "+eventsSessionID {
		t.Fatalf("last group of a UUID: %v", m)
	}
	if m := only("standalone-2", "events"); by(m) != "id" || m["reason"] != "Matched ID: standalone-2" {
		t.Fatalf("AO session ID: %v", m)
	}
	if m := only(ids["csi"][:10], "csi"); m["reason"] != "Matched ID: "+ids["csi"] {
		t.Fatalf("AO candidate ID: %v", m)
	}
	if m := only("01a1c02a preemption", "csi"); by(m) != "id,title" {
		t.Fatalf("ID plus a word: %v", m)
	}
	// The enriched candidate comes back with every match.
	if c := only("01a1c02a", "csi")["candidate"].(map[string]any); c["nativeId"] != csiSessionID || c["harness"] != "codex" || c["messageCount"] != float64(61) {
		t.Fatalf("candidate of a match = %v", c)
	}

	// Conversation content that is in no title: what the agent answered,
	// with an excerpt around the term.
	m := only("agent-orchestrator", "board")
	if by(m) != "conversation" || !strings.Contains(m["snippet"].(string), "on top of agent-orchestrator") ||
		m["reason"] != fmt.Sprintf("Matched conversation: %q", m["snippet"]) {
		t.Fatalf("conversation match: %v", m)
	}
	// Several terms may be spread over title, summary and conversation.
	if m := only("tracking kanban logos", "board"); by(m) != "title,summary,conversation" {
		t.Fatalf("terms across fields: %v", m)
	}
	if m := only("zero-emoji", "board"); by(m) != "summary" || !strings.HasPrefix(m["reason"].(string), `Matched summary: "`) {
		t.Fatalf("summary match: %v", m)
	}
	if m := only("refactor/events", "events"); m["reason"] != "Matched branch: refactor/events" {
		t.Fatalf("branch match: %v", m)
	}
	if m := only("kubeflow", "design"); by(m) != "title" || m["reason"] != "Matched title" {
		t.Fatalf("AO-only candidate: %v", m)
	}

	// Typos and abbreviations still find the session, marked as close.
	if m := only("premption", "csi"); by(m) != "title,fuzzy" || !strings.HasPrefix(m["reason"].(string), "Close match. ") {
		t.Fatalf("typo: %v", m)
	}
	if m := only("podgrp retries", "events"); !strings.Contains(by(m), "fuzzy") {
		t.Fatalf("abbreviation: %v", m)
	}
	// Chinese: a literal phrase, a phrase worded slightly differently, and
	// a query that mixes both languages.
	if m := only("\u770b\u677f\u5de5\u5177", "board"); by(m) != "conversation" || !strings.Contains(m["snippet"].(string), "\u770b\u677f\u5de5\u5177") {
		t.Fatalf("Chinese phrase: %v", m)
	}
	if m := only("\u60f3\u8981\u770b\u677f\u5de5\u5177", "board"); by(m) != "conversation,fuzzy" {
		t.Fatalf("Chinese phrase worded differently: %v", m)
	}
	if m := only("review \u770b\u677f", "board"); by(m) != "title,conversation" || !strings.Contains(m["snippet"].(string), "\u770b\u677f") {
		t.Fatalf("mixed query: %v", m)
	}

	// Two terms must both match; of several, one may be missing, which is
	// reported as a partial match.
	if _, matches := search("preemption kubeflow"); len(matches) != 0 {
		t.Fatalf("two unrelated terms matched %v", candidateIDs(matches))
	}
	if m := only("csi preemption limits kubeflow", "csi"); m["matchedTerms"] != float64(3) || !strings.HasPrefix(m["reason"].(string), "Partial match (3 of 4 terms). ") {
		t.Fatalf("partial match: %v", m)
	}

	// Ranking: the title beats the conversation, then the newer session.
	_, matches := search("podgroup")
	if got := candidateIDs(matches); len(got) != 1 || got[0] != ids["events"] {
		t.Fatalf("podgroup = %v", got)
	}
	_, matches = search("volcano")
	if got, want := candidateIDs(matches), []string{ids["board"], ids["events"], ids["csi"]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ranking for volcano = %v, want %v", got, want)
	}
	// The reason names the place that contains the term.
	if got := []any{matches[0].(map[string]any)["reason"], matches[1].(map[string]any)["reason"]}; !reflect.DeepEqual(got, []any{"Matched project: volcano", "Matched folder: /root/github/volcano"}) {
		t.Fatalf("place reasons = %v", got)
	}
	first, last := matches[0].(map[string]any), matches[2].(map[string]any)
	if first["score"].(float64) < last["score"].(float64) {
		t.Fatalf("scores are not descending: %v, %v", first["score"], last["score"])
	}

	// The helper run is never found, not even by its own prompt.
	if _, matches := search("Describe what a card is about"); len(matches) != 0 {
		t.Fatalf("the AI helper run was found: %v", candidateIDs(matches))
	}
	out, matches := search("")
	if len(matches) != 0 || out["total"] != float64(0) || out["candidates"] != float64(4) || out["indexed"] != float64(3) {
		t.Fatalf("empty query = %v", out)
	}
	if code := hn.errorCode(400, "GET", "/api/ao/import-history/search?q="+strings.Repeat("x", 1001), nil); code != "INVALID_INPUT" {
		t.Fatalf("long query: code = %s", code)
	}
}

func TestHelperDirectoryMarksAIRuns(t *testing.T) {
	hn, ids := historyHarness(t)
	// A prompt of some other release, not recognizable by its text: the
	// session ran in the scratch directory of the AI CLIs, which settles it.
	hn.ao.set(func() {
		hn.ao.candidates[0].Title = "Reply with exactly two short lines of plain English"
	})
	hn.srv.forgetImportCandidates()
	list := hn.must(200, "GET", "/api/ao/import-history", nil)["candidates"].([]any)
	if got := candidateIDs(list); len(got) != 4 || got[0] != ids["board"] {
		t.Fatalf("without a helper directory: %v", got)
	}
	for _, dir := range []string{"/root/tools/maintainer-board", "/root/tools/maintainer-board/"} {
		hn.srv.cfg.HelperDir = dir
		list = hn.must(200, "GET", "/api/ao/import-history", nil)["candidates"].([]any)
		if got, want := candidateIDs(list), []string{ids["events"], ids["csi"], ids["design"]}; !reflect.DeepEqual(got, want) {
			t.Fatalf("helper directory %q: candidates = %v, want %v", dir, got, want)
		}
	}
	// A directory that merely starts with the same characters is not it.
	hn.srv.cfg.HelperDir = "/root/tools/maintainer"
	if list = hn.must(200, "GET", "/api/ao/import-history", nil)["candidates"].([]any); len(list) != 4 {
		t.Fatalf("a sibling directory hid %d candidates", 4-len(list))
	}
}

func TestImportHistorySearchWithoutLocalStores(t *testing.T) {
	hn := newHarness(t)
	hn.ao.candidates = []ao.ImportCandidate{
		{ID: "abcdef0123456789", Harness: "codex", Title: "review #6247", Folder: "/root/github/volcano"},
		{ID: "s", Harness: "copilot", Title: "Reply in 1 concise Simplified Chinese sentence describing what this PR"},
	}
	out := hn.must(200, "GET", "/api/ao/import-history/search?q=6247", nil)
	if got := candidateIDs(out["matches"].([]any)); len(got) != 1 || got[0] != "abcdef0123456789" || out["indexed"] != float64(0) {
		t.Fatalf("search without an index = %v", out)
	}
	// AO offline is reported as such.
	hn.srv.forgetImportCandidates()
	hn.ao.set(func() { hn.ao.candidatesErr = fmt.Errorf("%w: connection refused", ao.ErrUnavailable) })
	if code := hn.errorCode(503, "GET", "/api/ao/import-history/search?q=6247", nil); code != "AO_OFFLINE" {
		t.Fatalf("offline: code = %s", code)
	}
	if code := hn.errorCode(503, "GET", "/api/ao/import-history", nil); code != "AO_OFFLINE" {
		t.Fatalf("offline list: code = %s", code)
	}
}

func TestMatchID(t *testing.T) {
	c := importCandidateView{ImportCandidate: ao.ImportCandidate{ID: "cb8ceb50a897c211e6d97c0d21890d6c", SessionID: "volcano-12"}, NativeID: "01a1c998-e2d3-7c11-a59c-8d52c0f94482"}
	cases := []struct {
		term  string
		want  string
		exact bool
	}{
		{"01a1c998", c.NativeID, false},
		{"01a1c998-e2d3-7c11-a59c-8d52c0f94482", c.NativeID, true},
		{"e2d3", c.NativeID, false},      // the beginning of a UUID group
		{"7c11-a59c", c.NativeID, false}, // spanning groups
		{"c0f94482", c.NativeID, false},  // six or more characters anywhere
		{"2d3-7", "", false},             // short and not at the start of a group
		{"cb8c", c.ID, false},
		{"e6d97c0d", c.ID, false},
		{"volcano-12", "volcano-12", true},
		{"volcano-1", "volcano-12", false},
		{"12", "volcano-12", false},
		{"volcano", "", false}, // a word without a digit is not an ID
		{"cano", "", false},
		{"a59", "", false}, // too short for a UUID
		{"dead", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if id, exact := matchID(c, tc.term); id != tc.want || exact != tc.exact {
			t.Errorf("matchID(%q) = %q, %v; want %q, %v", tc.term, id, exact, tc.want, tc.exact)
		}
	}
	if id, _ := matchID(importCandidateView{}, "abcd"); id != "" {
		t.Errorf("an empty candidate matched %q", id)
	}
}

func TestFuzzyHelpers(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		limit int
		want  int
	}{
		{"preempt", "preempt", 1, 0}, {"premption", "preemption", 1, 1}, {"schedulr", "scheduler", 2, 1},
		{"kubeflwo", "kubeflow", 2, 2}, {"cache", "queue", 1, 2}, {"a", "abcdef", 1, 2}, {"volcano", "vulcan", 2, 2},
	} {
		if got := editDistance(tc.a, tc.b, tc.limit); got != tc.want {
			t.Errorf("editDistance(%q, %q, %d) = %d, want %d", tc.a, tc.b, tc.limit, got, tc.want)
		}
	}
	words := []string{"preemption", "podgroup", "scheduler", "retries"}
	for _, tc := range []struct{ term, want string }{
		{"premption", "preemption"}, {"schedulers", "scheduler"}, {"podgrp", "podgroup"}, {"shedulerr", "scheduler"},
		{"pod", ""}, {"retr", ""}, {"kubeflow", ""}, {"\u8c03\u5ea6\u5668\u7f13\u5b58", ""},
	} {
		if got, _ := similarWord(words, tc.term); got != tc.want {
			t.Errorf("similarWord(%q) = %q, want %q", tc.term, got, tc.want)
		}
	}
	text := "\u60f3\u8981\u4e00\u4e2a\u770b\u677f\u5de5\u5177" // "want a kanban tool"
	if got := bigramCoverage("\u60f3\u8981\u770b\u677f\u5de5\u5177", text); got != 0.8 {
		t.Errorf("coverage of a reworded phrase = %v, want 0.8", got)
	}
	if got := bigramCoverage("\u8c03\u5ea6\u5668\u7f13\u5b58", text); got != 0 {
		t.Errorf("coverage of an unrelated phrase = %v", got)
	}
	if got := bigramCoverage("\u770b\u677f", text); got != 0 {
		t.Errorf("a two-character term must match literally, got coverage %v", got)
	}
}

func TestImportHistoryAISearch(t *testing.T) {
	hn, ids := historyHarness(t)
	// The catalog lists the sessions newest first: board, events, csi, design.
	hn.ai.historyMatches = []summary.HistoryMatch{
		{Index: 2, Reason: "\u6392\u67e5 CSI \u62a2\u5360\u56de\u5f52"},
		{Index: 0, Reason: ""},
		{Index: 9, Reason: "invented"},
		{Index: 2, Reason: "again"},
	}
	out := hn.must(200, "POST", "/api/ao/import-history/ai-search", map[string]any{"query": "  the session about\n CSI preemption  "})
	if out["query"] != "the session about CSI preemption" || out["provider"] != "gemini-3.8-flash" || out["candidates"] != float64(4) {
		t.Fatalf("response = %v", out)
	}
	matches := out["matches"].([]any)
	if got, want := candidateIDs(matches), []string{ids["csi"], ids["board"]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
	first := matches[0].(map[string]any)
	if first["reason"] != "\u6392\u67e5 CSI \u62a2\u5360\u56de\u5f52" || first["candidate"].(map[string]any)["nativeId"] != csiSessionID {
		t.Fatalf("first match = %v", first)
	}
	if matches[1].(map[string]any)["reason"] != "Matches the query." {
		t.Fatalf("a match without a reason = %v", matches[1])
	}

	// What the model was shown: every real session with its native ID,
	// where it ran and what the user asked, but not the helper run.
	in := hn.ai.historyInput
	if in.Query != "the session about CSI preemption" || len(in.Sessions) != 4 {
		t.Fatalf("history input = %+v", in)
	}
	board, csi, design := in.Sessions[0], in.Sessions[2], in.Sessions[3]
	if board.ID != boardSessionID || board.Harness != "copilot" || board.Branch != "feat/board" || board.Folder != "volcano" ||
		!strings.HasPrefix(board.Excerpt, boardPrompt+" / Add an author filter") || !strings.Contains(board.Excerpt, "Summary: The user wanted a unified zero-emoji Kanban GUI") {
		t.Fatalf("copilot session in the catalog = %+v", board)
	}
	if csi.ID != csiSessionID || csi.Folder != "/root/github/volcano" || csi.LastActivity != "2026-10-21T10:16:38Z" ||
		csi.Excerpt != "Investigate why preemption regresses for CSI volume limits in PR 6247" {
		t.Fatalf("codex session in the catalog = %+v", csi)
	}
	if design.ID != ids["design"][:12] || design.Excerpt != "" || design.Title != "Kubeflow Trainer integration design" {
		t.Fatalf("AO-only session in the catalog = %+v", design)
	}
	for _, s := range in.Sessions {
		if s.ID == helperSessionID || strings.Contains(s.Excerpt, summary.PromptMarker) {
			t.Fatalf("the AI helper run was sent to the model: %+v", s)
		}
	}

	// No matches is a valid answer.
	hn.ai.historyMatches = nil
	out = hn.must(200, "POST", "/api/ao/import-history/ai-search", map[string]any{"query": "nothing like this"})
	if len(out["matches"].([]any)) != 0 {
		t.Fatalf("no matches = %v", out)
	}
	if code := hn.errorCode(400, "POST", "/api/ao/import-history/ai-search", map[string]any{"query": "  "}); code != "INVALID_INPUT" {
		t.Errorf("empty query: code = %s", code)
	}
	hn.ai.historyErr = errors.New("AI search failed: copilot timed out; codex failed: quota")
	if code := hn.errorCode(502, "POST", "/api/ao/import-history/ai-search", map[string]any{"query": "x"}); code != "AI_SEARCH_FAILED" {
		t.Errorf("failed search: code = %s", code)
	}

	// With nothing to search the model is not called.
	empty := newHarness(t)
	out = empty.must(200, "POST", "/api/ao/import-history/ai-search", map[string]any{"query": "anything"})
	if empty.ai.historyCalls != 0 || len(out["matches"].([]any)) != 0 || out["candidates"] != float64(0) {
		t.Fatalf("empty history: %v after %d model calls", out, empty.ai.historyCalls)
	}
}

func TestImportHistoryAISearchRunsOneAtATime(t *testing.T) {
	hn, _ := historyHarness(t)
	hn.ai.searchGate, hn.ai.searchStarted = make(chan struct{}), make(chan struct{})
	done := make(chan int, 1)
	go func() {
		status, _ := hn.call("POST", "/api/ao/import-history/ai-search", map[string]any{"query": "first"})
		done <- status
	}()
	<-hn.ai.searchStarted
	if code := hn.errorCode(409, "POST", "/api/ao/import-history/ai-search", map[string]any{"query": "second"}); code != "AI_SEARCH_BUSY" {
		t.Errorf("concurrent search: code = %s", code)
	}
	close(hn.ai.searchGate)
	if status := <-done; status != 200 {
		t.Fatalf("first search = %d", status)
	}
}

func TestHistoryCatalogForALongHistory(t *testing.T) {
	var entries []historyEntry
	for i := 0; i < 100; i++ {
		entries = append(entries, historyEntry{view: importCandidateView{ImportCandidate: ao.ImportCandidate{
			ID: fmt.Sprintf("id-%03d", i), Harness: "codex", Title: fmt.Sprintf("routine session %d", i),
			LastActivity: fmt.Sprintf("2026-%02d-%02dT00:00:00Z", 1+i/28, 1+i%28),
		}}})
	}
	// Two old sessions the query is about, and the oldest of all.
	entries[3].view.Title = "debug the hierarchical queue capacity plugin"
	entries[7].view.Title = "queue capacity e2e flake"
	catalog, picked := historyCatalog(entries, "where did I debug the capacity plugin of the queue?")
	if len(catalog) != aiCatalogRecent+2 || len(picked) != len(catalog) {
		t.Fatalf("catalog has %d sessions, want %d", len(catalog), aiCatalogRecent+2)
	}
	// The keyword matches lead, the best one first; the newest follow.
	if catalog[0].Title != entries[3].view.Title || catalog[1].Title != entries[7].view.Title {
		t.Fatalf("catalog starts with %q, %q", catalog[0].Title, catalog[1].Title)
	}
	if catalog[2].Title != "routine session 99" || catalog[len(catalog)-1].Title != "routine session 40" {
		t.Fatalf("recent part runs from %q to %q", catalog[2].Title, catalog[len(catalog)-1].Title)
	}
	for i, e := range picked {
		if e.view.Title != catalog[i].Title {
			t.Fatalf("index %d maps to %q, catalog says %q", i, e.view.Title, catalog[i].Title)
		}
	}
	// A short history is sent whole, newest first, whatever the query.
	catalog, _ = historyCatalog(entries[:5], "queue capacity")
	if len(catalog) != 5 || catalog[0].Title != "routine session 4" {
		t.Fatalf("short history = %d sessions, first %q", len(catalog), catalog[0].Title)
	}
}

func TestSummarizeAsksWhatTheCardIsAbout(t *testing.T) {
	hn := newHarness(t)
	hn.gh.bodies = map[string]string{"volcano-sh/volcano#6247": "Removes the scalar CSI volume counting."}
	hn.ao.sessions = []ao.Session{worker("standalone-1", "codex", "[PR #6247] Review")}
	hn.ao.conversations["standalone-1"] = &ao.Conversation{Messages: []ao.Message{
		{Role: "user", Text: "review #6247 and tell me why it deletes so much code"}, {Role: "assistant", Text: "Reading the diff."},
	}}
	hn.syncAO()
	id := hn.cardBySession("standalone-1").ID
	hn.ao.set(func() { hn.ao.convQueries = nil })

	hn.must(200, "POST", "/api/cards/"+id+"/summarize", nil)
	in := hn.ai.input
	if in.Description != "Removes the scalar CSI volume counting." || !strings.Contains(in.GitHub, "volcano-sh/volcano#6247") || len(in.Turns) != 2 || len(in.Recent) != 0 {
		t.Fatalf("summarizer input = %+v", in)
	}
	// The opening of the conversation is read, not its end: AO pages
	// backwards, so this asks for what precedes item 41.
	if want := []ao.ConversationQuery{{Limit: 40, BeforeSequence: 41}}; !reflect.DeepEqual(hn.ao.convQueries, want) {
		t.Fatalf("conversation queries = %+v, want %+v", hn.ao.convQueries, want)
	}

	// A long conversation whose opening says nothing, on a card without a
	// GitHub link: the latest turns are fetched as well.
	hn.ao.set(func() {
		hn.ao.sessions = append(hn.ao.sessions, worker("volcano-5", "copilot", "Untitled"))
		hn.ao.conversations["volcano-5"] = &ao.Conversation{Messages: []ao.Message{{Role: "user", Text: "continue"}, {Role: "assistant", Text: "Fixed the cache race."}}}
		hn.ao.latest = map[string]int64{"volcano-5": 300}
	})
	hn.syncAO()
	sparse := hn.cardBySession("volcano-5").ID
	hn.ao.set(func() { hn.ao.convQueries = nil })
	hn.must(200, "POST", "/api/cards/"+sparse+"/summarize", nil)
	if want := []ao.ConversationQuery{{Limit: 40, BeforeSequence: 41}, {Limit: 12}}; !reflect.DeepEqual(hn.ao.convQueries, want) {
		t.Fatalf("conversation queries for a sparse card = %+v, want %+v", hn.ao.convQueries, want)
	}
	if in := hn.ai.input; in.Description != "" || len(in.Turns) != 2 || len(in.Recent) != 2 {
		t.Fatalf("sparse summarizer input = %+v", in)
	}

	// An unreachable description does not fail the summary.
	hn.gh.bodies = nil
	hn.must(200, "POST", "/api/cards/"+id+"/summarize", nil)
	if hn.ai.input.Description != "" {
		t.Fatalf("description = %q", hn.ai.input.Description)
	}
}
