package db

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
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
