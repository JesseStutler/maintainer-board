package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCard(t *testing.T, s *Store, title, column string) *Card {
	t.Helper()
	c, err := s.CreateCard(context.Background(), &Card{Title: title, ColumnID: column, AutoMove: true})
	if err != nil {
		t.Fatalf("create card %q: %v", title, err)
	}
	return c
}

// order returns the card titles of a column in board order and fails when
// positions are not contiguous from zero.
func order(t *testing.T, s *Store, column string) []string {
	t.Helper()
	cards, err := s.ListCards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, c := range cards {
		if c.ColumnID != column {
			continue
		}
		if c.Position != len(titles) {
			t.Fatalf("column %s: card %q has position %d, want %d", column, c.Title, c.Position, len(titles))
		}
		titles = append(titles, c.Title)
	}
	return titles
}

func TestDefaultColumns(t *testing.T) {
	s := newStore(t)
	cols, err := s.ListColumns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Column{
		{"inbox", "Inbox", 0, "#6e7681", RoleInbox},
		{"in_progress", "In Progress", 1, "#3b82f6", RoleInProgress},
		{"waiting", "Waiting", 2, "#a855f7", RoleWaiting},
		{"updates", "Updates", 3, "#f59e0b", RoleUpdates},
		{"ready", "Ready", 4, "#10b981", RoleReady},
		{"done", "Done", 5, "#52525b", RoleDone},
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("default columns:\n got %+v\nwant %+v", cols, want)
	}
}

func TestReopenKeepsDataAndDoesNotReseed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "board.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.DeleteColumn(ctx, "ready"); err != nil {
		t.Fatal(err)
	}
	card, err := s.CreateCard(ctx, &Card{Title: "persisted", ColumnID: "inbox", Tags: StringList{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cols, _ := s.ListColumns(ctx)
	if len(cols) != 5 {
		t.Fatalf("reopen re-seeded columns: got %d, want 5", len(cols))
	}
	got, err := s.GetCard(ctx, card.ID)
	if err != nil || got.Title != "persisted" || !reflect.DeepEqual(got.Tags, StringList{"a"}) {
		t.Fatalf("card after reopen = %+v, err %v", got, err)
	}
}

// writeV1Database creates a database exactly as the first release left it:
// schema version 1, one card with GitHub tracking state, one timeline event.
func writeV1Database(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, stmt := range []string{
		migrations[0],
		`PRAGMA user_version = 1`,
		`INSERT INTO cards (id, title, column_id, category, tags, ao_session_id, gh_repo, gh_kind, gh_number, gh_author,
			gh_state, gh_review_decision, ack_at, unread_updates, has_unread_updates, created_at, updated_at)
		 VALUES ('c_old', 'Review CSI limits', 'waiting', 'PR_REVIEW', '["scheduler"]', 'standalone-1', 'volcano-sh/volcano', 'pr', 6247, 'mohit',
			'OPEN', 'REVIEW_REQUIRED', '2026-11-24T14:05:08Z', 2, 1, '2026-11-24T14:05:06Z', '2026-11-24T14:05:08Z')`,
		`INSERT INTO card_events (card_id, kind, message, created_at) VALUES ('c_old', 'created', 'Card created in Waiting', '2026-11-24T14:05:06Z')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("prepare v1 database: %v", err)
		}
	}
}

func cardColumnNames(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('cards')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names[name] = true
	}
	return names
}

func TestMigrationAddsReviewColumnsToExistingDatabase(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, path string) {
		t.Helper()
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open a version 1 database: %v", err)
		}
		defer s.Close()
		var version int
		if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
			t.Fatalf("user_version = %d, err %v, want %d", version, err, len(migrations))
		}
		names := cardColumnNames(t, s)
		for _, col := range []string{"gh_my_review_state", "gh_my_reviewed_at", "gh_is_stale_review"} {
			if !names[col] {
				t.Errorf("cards.%s is missing after migration", col)
			}
		}
		// The existing row survives untouched and reads the new fields as
		// their defaults.
		got, err := s.GetCard(ctx, "c_old")
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "Review CSI limits" || got.ColumnID != "waiting" || got.AOSessionID != "standalone-1" || got.GHNumber != 6247 ||
			got.GHAuthor != "mohit" || got.GHReviewDecision != "REVIEW_REQUIRED" || !got.HasUnreadUpdates || got.UnreadUpdates != 2 ||
			!reflect.DeepEqual(got.Tags, StringList{"scheduler"}) {
			t.Fatalf("existing card changed during migration: %+v", got)
		}
		if got.GHMyReviewState != "" || got.GHMyReviewedAt != "" || got.GHIsStaleReview {
			t.Fatalf("new fields are not empty on an existing card: %q %q %v", got.GHMyReviewState, got.GHMyReviewedAt, got.GHIsStaleReview)
		}
		if events, err := s.ListEvents(ctx, "c_old", 0); err != nil || len(events) != 1 {
			t.Fatalf("timeline after migration = %v, err %v", events, err)
		}
		if cols, _ := s.ListColumns(ctx); len(cols) != 6 {
			t.Fatalf("migration re-seeded or dropped columns: %d", len(cols))
		}
		// The conversation index of the same upgrade is usable too.
		if err := s.IndexContent(ctx, "standalone-1", []ContentMessage{{MessageID: "m1", Sequence: 1, Role: "user", Text: "CSI limits"}}, 1, "", false); err != nil {
			t.Fatalf("content index after migration: %v", err)
		}
		if hits, err := s.ContentHits(ctx, "csi"); err != nil || len(hits) != 1 {
			t.Fatalf("content hits after migration = %v, %v", hits, err)
		}
		// The migrated row accepts the new fields.
		if _, err := s.UpdateCard(ctx, "c_old", func(c *Card, m *Mutation) error {
			c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview = "APPROVED", "2026-11-24T13:15:12Z", true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetCard(ctx, "c_old"); got.GHMyReviewState != "APPROVED" || !got.GHIsStaleReview {
			t.Fatalf("new fields did not persist: %+v", got)
		}
	}

	t.Run("version 1 database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "board.db")
		writeV1Database(t, path)
		check(t, path)
		// Opening again is a no-op: nothing is added twice, data stays.
		s, err := Open(path)
		if err != nil {
			t.Fatalf("second open: %v", err)
		}
		defer s.Close()
		if got, err := s.GetCard(ctx, "c_old"); err != nil || got.GHMyReviewState != "APPROVED" {
			t.Fatalf("card after second open = %+v, err %v", got, err)
		}
	})

	// A column that already exists (an interrupted or hand-applied upgrade)
	// must not make the migration fail.
	t.Run("column already present", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "board.db")
		writeV1Database(t, path)
		raw, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`ALTER TABLE cards ADD COLUMN gh_my_review_state TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatal(err)
		}
		raw.Close()
		check(t, path)
	})
}

func TestFreshDatabaseHasReviewColumns(t *testing.T) {
	s := newStore(t)
	names := cardColumnNames(t, s)
	for _, col := range []string{"gh_my_review_state", "gh_my_reviewed_at", "gh_is_stale_review"} {
		if !names[col] {
			t.Errorf("cards.%s is missing in a fresh database", col)
		}
	}
	c := mustCard(t, s, "fresh", "inbox")
	if c.GHMyReviewState != "" || c.GHIsStaleReview {
		t.Fatalf("fresh card = %+v", c)
	}
}

func TestColumnRoleIsExclusive(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	col, err := s.CreateColumn(ctx, "  Needs Re-review ", "#FF0000", RoleUpdates)
	if err != nil {
		t.Fatal(err)
	}
	if col.Title != "Needs Re-review" || col.Color != "#ff0000" || col.Position != 6 {
		t.Fatalf("created column = %+v", col)
	}
	old, _ := s.GetColumn(ctx, "updates")
	if old.Role != "" {
		t.Fatalf("old updates column kept role %q", old.Role)
	}
	byRole, _ := s.ColumnByRole(ctx, RoleUpdates)
	if byRole == nil || byRole.ID != col.ID {
		t.Fatalf("ColumnByRole(updates) = %+v, want %s", byRole, col.ID)
	}

	role := RoleUpdates
	if _, err := s.UpdateColumn(ctx, "updates", ColumnPatch{Role: &role}); err != nil {
		t.Fatal(err)
	}
	moved, _ := s.GetColumn(ctx, col.ID)
	if moved.Role != "" {
		t.Fatalf("role was not taken back from %s", col.ID)
	}
}

func TestColumnValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	empty, badColor, badRole := "  ", "red", "archive"
	cases := map[string]ColumnPatch{
		"empty title": {Title: &empty},
		"bad color":   {Color: &badColor},
		"bad role":    {Role: &badRole},
	}
	for name, patch := range cases {
		if _, err := s.UpdateColumn(ctx, "inbox", patch); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.UpdateColumn(ctx, "missing", ColumnPatch{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing column: err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateColumn(ctx, "", "#000000", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("create without title: err = %v, want ErrInvalid", err)
	}
}

func TestReorderColumns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.ReorderColumns(ctx, []string{"done", "inbox"}); err != nil {
		t.Fatal(err)
	}
	cols, _ := s.ListColumns(ctx)
	var ids []string
	for i, c := range cols {
		if c.Position != i {
			t.Fatalf("column %s position %d, want %d", c.ID, c.Position, i)
		}
		ids = append(ids, c.ID)
	}
	want := []string{"done", "inbox", "in_progress", "waiting", "updates", "ready"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
	if err := s.ReorderColumns(ctx, []string{"inbox", "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
	if err := s.ReorderColumns(ctx, []string{"inbox", "inbox"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate id: err = %v, want ErrInvalid", err)
	}
}

func TestDeleteColumnMovesCards(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCard(t, s, "in inbox", "inbox")
	mustCard(t, s, "w2", "waiting")
	mustCard(t, s, "w1", "waiting")

	target, err := s.DeleteColumn(ctx, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "inbox" {
		t.Fatalf("cards moved to %s, want inbox", target.ID)
	}
	if got, want := order(t, s, "inbox"), []string{"in inbox", "w1", "w2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inbox order = %v, want %v", got, want)
	}

	// Without an inbox-role column the first remaining column receives them.
	target, err = s.DeleteColumn(ctx, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "in_progress" {
		t.Fatalf("cards moved to %s, want in_progress", target.ID)
	}
	if got := order(t, s, "in_progress"); len(got) != 3 {
		t.Fatalf("in_progress has %v", got)
	}

	for _, id := range []string{"updates", "ready", "done"} {
		if _, err := s.DeleteColumn(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteColumn(ctx, "in_progress"); !errors.Is(err, ErrLastColumn) {
		t.Fatalf("deleting the last column: err = %v, want ErrLastColumn", err)
	}
}

func TestCreateCardDefaultsAndValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c, err := s.CreateCard(ctx, &Card{Title: "  task  ", ColumnID: "inbox", Tags: StringList{" a ", "A", "", "b"}},
		EventInput{Kind: EventCreated, Message: "made"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.Title != "task" || c.Category != CategoryDev || c.SummarySource != SummaryAuto || c.Origin != OriginManual {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if !reflect.DeepEqual(c.Tags, StringList{"a", "b"}) {
		t.Fatalf("tags = %v, want [a b]", c.Tags)
	}
	events, _ := s.ListEvents(ctx, c.ID, 0)
	if len(events) != 1 || events[0].Message != "made" {
		t.Fatalf("events = %+v", events)
	}

	bad := []*Card{
		{Title: " ", ColumnID: "inbox"},
		{Title: "x", ColumnID: "inbox", Category: "BUG"},
		{Title: "x", ColumnID: "inbox", SummarySource: "robot"},
	}
	for _, card := range bad {
		if _, err := s.CreateCard(ctx, card); !errors.Is(err, ErrInvalid) {
			t.Errorf("card %+v: err = %v, want ErrInvalid", card, err)
		}
	}
	if _, err := s.CreateCard(ctx, &Card{Title: "x", ColumnID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown column: err = %v, want ErrNotFound", err)
	}
}

func TestCardFieldsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := mustCard(t, s, "round trip", "waiting")
	want, err := s.UpdateCard(ctx, c.ID, func(c *Card, m *Mutation) error {
		c.Category, c.Tags = CategoryPRReview, StringList{"scheduler"}
		c.AOSessionID, c.AOTitle = "volcano-2", "ao title"
		c.Summary, c.SummarySource, c.Notes, c.AutoMove, c.Origin = "sum", SummaryAI, "notes", false, OriginAO
		c.GHRepo, c.GHKind, c.GHNumber = "volcano-sh/volcano", "pr", 6247
		c.GHURL, c.GHTitle, c.GHAuthor = "https://example/pr", "title", "someone"
		c.GHState, c.GHCIStatus, c.GHReviewDecision, c.GHHeadSHA = "OPEN", "SUCCESS", "APPROVED", "abc"
		c.GHCommitCount, c.GHCommentCount, c.GHReviewCommentCount = 3, 4, 5
		c.GHLabels, c.GHUpdatedAt, c.GHLastSyncedAt, c.GHError = StringList{"lgtm"}, "t1", "t2", "boom"
		c.AckHeadSHA, c.AckCommitCount, c.AckCommentCount, c.AckReviewCommentCount = "ack", 1, 2, 3
		c.AckState, c.AckCIStatus, c.AckReviewDecision, c.AckUpdatedAt, c.AckAt = "DRAFT", "PENDING", "REVIEW_REQUIRED", "t3", "t4"
		c.PrevHeadSHA, c.PrevCommitCount, c.PrevCommentCount, c.PrevReviewCommentCount = "prev", 6, 7, 8
		c.PrevState, c.PrevCIStatus, c.PrevReviewDecision, c.PrevUpdatedAt = "OPEN", "FAILURE", "CHANGES_REQUESTED", "t5"
		c.UnreadUpdates, c.UpdateCycles, c.UpdateSummary, c.HasUnreadUpdates = 9, 2, "+2 commits", true
		c.GHMyReviewState, c.GHMyReviewedAt, c.GHIsStaleReview = "LGTM", "t6", true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCard(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored card differs:\n got %+v\nwant %+v", got, want)
	}
	bySession, _ := s.CardBySession(ctx, "volcano-2")
	if bySession == nil || bySession.ID != c.ID {
		t.Fatalf("CardBySession = %+v", bySession)
	}
}

func TestNewCardsGoToTheTop(t *testing.T) {
	s := newStore(t)
	mustCard(t, s, "first", "inbox")
	mustCard(t, s, "second", "inbox")
	mustCard(t, s, "third", "inbox")
	if got, want := order(t, s, "inbox"), []string{"third", "second", "first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestMoveCardReorders(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := mustCard(t, s, "c", "inbox")
	mustCard(t, s, "b", "inbox")
	a := mustCard(t, s, "a", "inbox")
	x := mustCard(t, s, "x", "waiting")

	cases := []struct {
		name          string
		card          *Card
		column        string
		position      int
		inbox, waitng []string
	}{
		{"down within column", a, "inbox", 2, []string{"b", "c", "a"}, []string{"x"}},
		{"up within column", a, "inbox", 0, []string{"a", "b", "c"}, []string{"x"}},
		{"same place", a, "inbox", 0, []string{"a", "b", "c"}, []string{"x"}},
		{"across to the end", a, "waiting", 99, []string{"b", "c"}, []string{"x", "a"}},
		{"across to the top", c, "waiting", -5, []string{"b"}, []string{"c", "x", "a"}},
		{"across to the middle", x, "inbox", 1, []string{"b", "x"}, []string{"c", "a"}},
	}
	for _, tc := range cases {
		moved, err := s.MoveCard(ctx, tc.card.ID, tc.column, tc.position)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if moved.ColumnID != tc.column {
			t.Fatalf("%s: returned column %s", tc.name, moved.ColumnID)
		}
		if got := order(t, s, "inbox"); !reflect.DeepEqual(got, tc.inbox) {
			t.Fatalf("%s: inbox = %v, want %v", tc.name, got, tc.inbox)
		}
		if got := order(t, s, "waiting"); !reflect.DeepEqual(got, tc.waitng) {
			t.Fatalf("%s: waiting = %v, want %v", tc.name, got, tc.waitng)
		}
	}

	// Only moves across columns are recorded in the timeline.
	events, _ := s.ListEvents(ctx, a.ID, 0)
	if len(events) != 1 || events[0].Kind != EventMove || events[0].Message != "Moved from Inbox to Waiting" {
		t.Fatalf("events for a = %+v", events)
	}
	if _, err := s.MoveCard(ctx, a.ID, "nope", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown column: err = %v, want ErrNotFound", err)
	}
	if _, err := s.MoveCard(ctx, "nope", "inbox", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown card: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateCardColumnChangePlacesOnTop(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCard(t, s, "u1", "updates")
	mustCard(t, s, "w2", "waiting")
	w1 := mustCard(t, s, "w1", "waiting")
	mustCard(t, s, "w0", "waiting")

	updated, err := s.UpdateCard(ctx, w1.ID, func(c *Card, m *Mutation) error {
		c.ColumnID = "updates"
		c.ID, c.Position = "hijacked", 42
		m.Event(EventMove, "auto")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != w1.ID || updated.Position != 0 {
		t.Fatalf("identity fields changed: %+v", updated)
	}
	if got, want := order(t, s, "updates"), []string{"w1", "u1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %v, want %v", got, want)
	}
	if got, want := order(t, s, "waiting"), []string{"w0", "w2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting = %v, want %v", got, want)
	}

	// A failing callback rolls everything back, including its events.
	boom := errors.New("boom")
	_, err = s.UpdateCard(ctx, w1.ID, func(c *Card, m *Mutation) error {
		c.Title = "changed"
		m.Event(EventUpdate, "never stored")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	again, _ := s.GetCard(ctx, w1.ID)
	events, _ := s.ListEvents(ctx, w1.ID, 0)
	if again.Title != "w1" || len(events) != 1 {
		t.Fatalf("rollback failed: title %q, events %+v", again.Title, events)
	}
}

func TestDeleteCardRenumbersAndDropsEvents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustCard(t, s, "c", "inbox")
	b := mustCard(t, s, "b", "inbox")
	mustCard(t, s, "a", "inbox")
	if err := s.AddEvent(ctx, b.ID, EventUpdate, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteCard(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := order(t, s, "inbox"), []string{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inbox = %v, want %v", got, want)
	}
	if events, _ := s.ListEvents(ctx, b.ID, 0); len(events) != 0 {
		t.Fatalf("events survived the card: %+v", events)
	}
	if _, err := s.DeleteCard(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestSettingsAndIgnoredSessions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if v, _ := s.GetSetting(ctx, "k", "fallback"); v != "fallback" {
		t.Fatalf("missing setting = %q", v)
	}
	_ = s.SetSetting(ctx, "k", "1")
	_ = s.SetSetting(ctx, "k", "2")
	if v, _ := s.GetSetting(ctx, "k", ""); v != "2" {
		t.Fatalf("setting = %q, want 2", v)
	}

	_ = s.IgnoreSession(ctx, "volcano-9")
	_ = s.IgnoreSession(ctx, "volcano-9")
	_ = s.IgnoreSession(ctx, "")
	ignored, _ := s.IgnoredSessions(ctx)
	if len(ignored) != 1 || !ignored["volcano-9"] {
		t.Fatalf("ignored = %v", ignored)
	}
	_ = s.UnignoreSession(ctx, "volcano-9")
	if ignored, _ = s.IgnoredSessions(ctx); len(ignored) != 0 {
		t.Fatalf("ignored after unignore = %v", ignored)
	}
}

func TestContentIndex(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if seq, stamp, err := s.ContentState(ctx, "volcano-2"); err != nil || seq != 0 || stamp != "" {
		t.Fatalf("state of an unknown session = %d %q %v", seq, stamp, err)
	}
	msgs := []ContentMessage{
		{MessageID: "m1", Sequence: 1, Role: "user", Text: "Please review the CSI NodeVolumeLimits removal", CreatedAt: "t1"},
		{MessageID: "m2", Sequence: 2, Role: "assistant", Text: "The preempt.go victim filter changes eligibility.", CreatedAt: "t2"},
		{MessageID: "m3", Sequence: 3, Role: "user", Text: "  ", CreatedAt: "t3"},
		{MessageID: "", Sequence: 4, Role: "user", Text: "no id"},
	}
	if err := s.IndexContent(ctx, "volcano-2", msgs, 3, "stamp-1", false); err != nil {
		t.Fatal(err)
	}
	if err := s.IndexContent(ctx, "volcano-3", []ContentMessage{{MessageID: "x1", Sequence: 1, Role: "user", Text: "CompositePodGroup 设计讨论"}}, 1, "s", false); err != nil {
		t.Fatal(err)
	}
	if seq, stamp, _ := s.ContentState(ctx, "volcano-2"); seq != 3 || stamp != "stamp-1" {
		t.Fatalf("state = %d %q", seq, stamp)
	}
	// Blank messages and messages without an ID are not stored.
	if sessions, messages, err := s.ContentStats(ctx); err != nil || sessions != 2 || messages != 3 {
		t.Fatalf("stats = %d sessions, %d messages, %v", sessions, messages, err)
	}

	// Case-insensitive substring match; the newest matching message is returned.
	hits, err := s.ContentHits(ctx, "NODEVOLUME")
	if err != nil || len(hits) != 1 || hits[0].SessionID != "volcano-2" || hits[0].Count != 1 || hits[0].Sequence != 1 || hits[0].Role != "user" {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if hits, _ := s.ContentHits(ctx, "e"); len(hits) != 2 || hits[0].SessionID != "volcano-2" || hits[0].Count != 2 || hits[0].Sequence != 2 {
		t.Fatalf("hits across sessions = %+v", hits)
	}
	// Text without word boundaries is found by substring too.
	if hits, _ := s.ContentHits(ctx, "设计"); len(hits) != 1 || hits[0].SessionID != "volcano-3" {
		t.Fatalf("CJK hits = %+v", hits)
	}
	// LIKE wildcards in the term are plain characters.
	if hits, _ := s.ContentHits(ctx, "%"); len(hits) != 0 {
		t.Fatalf("a percent sign matched %+v", hits)
	}
	if hits, _ := s.ContentHits(ctx, "  "); hits != nil {
		t.Fatalf("an empty term matched %+v", hits)
	}

	// A revised message replaces the stored text instead of adding a row.
	revised := []ContentMessage{{MessageID: "m2", Sequence: 2, Role: "assistant", Text: "Rewritten answer about quotas."}}
	if err := s.IndexContent(ctx, "volcano-2", revised, 3, "stamp-2", false); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.ContentHits(ctx, "preempt.go"); len(hits) != 0 {
		t.Fatalf("the old text of a revised message is still indexed: %+v", hits)
	}
	if hits, _ := s.ContentHits(ctx, "quotas"); len(hits) != 1 {
		t.Fatalf("the revised text is not indexed: %+v", hits)
	}

	// Excerpts: the first user message, then the newest messages in order.
	more := []ContentMessage{
		{MessageID: "m5", Sequence: 5, Role: "user", Text: "five"},
		{MessageID: "m6", Sequence: 6, Role: "assistant", Text: "six"},
	}
	if err := s.IndexContent(ctx, "volcano-2", more, 6, "stamp-3", false); err != nil {
		t.Fatal(err)
	}
	excerpts, err := s.ContentExcerpts(ctx, "volcano-2", 2)
	if err != nil || len(excerpts) != 3 || excerpts[0].MessageID != "m1" || excerpts[1].MessageID != "m5" || excerpts[2].MessageID != "m6" {
		t.Fatalf("excerpts = %+v, %v", excerpts, err)
	}
	if excerpts, _ := s.ContentExcerpts(ctx, "volcano-2", 10); len(excerpts) != 4 || excerpts[0].MessageID != "m1" || excerpts[1].MessageID != "m2" {
		t.Fatalf("the first message was repeated: %+v", excerpts)
	}

	// A reset drops what a rolled-back conversation no longer contains.
	if err := s.IndexContent(ctx, "volcano-2", []ContentMessage{{MessageID: "n1", Sequence: 1, Role: "user", Text: "fresh start"}}, 1, "stamp-4", true); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.ContentHits(ctx, "NodeVolumeLimits"); len(hits) != 0 {
		t.Fatalf("reset kept old messages: %+v", hits)
	}
	if sessions, messages, _ := s.ContentStats(ctx); sessions != 2 || messages != 2 {
		t.Fatalf("stats after reset = %d sessions, %d messages", sessions, messages)
	}

	// Very long messages are cut, not rejected.
	long := strings.Repeat("a", MaxIndexedRunes+500) + " needle"
	if err := s.IndexContent(ctx, "big", []ContentMessage{{MessageID: "b1", Sequence: 1, Role: "assistant", Text: long}}, 1, "", false); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.ContentHits(ctx, "needle"); len(hits) != 0 {
		t.Fatalf("text beyond the limit was indexed")
	}
	if hits, _ := s.ContentHits(ctx, "aaaa"); len(hits) != 1 || len(hits[0].Text) != MaxIndexedRunes {
		t.Fatalf("long message hit = %d sessions", len(hits))
	}
}
