package summary

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildSearchPrompt(t *testing.T) {
	in := SearchInput{
		Query: "where did we discuss\nthe **preemption** regression?",
		Cards: []SearchCard{
			{
				ID: "c_review", Title: "[PR #6247][Review] CSI limits", Category: "PR Review", Column: "Inbox",
				GitHub: `pull request volcano-sh/volcano#6247, titled "fix(scheduler): enforce CSI limits", by mohit, state OPEN`,
				Tags:   []string{"scheduler", "csi"}, Summary: "Objective: review.\nStatus: waiting.", Notes: "ask for an e2e case",
				SessionID: "standalone-1", Agent: "codex",
				Excerpts: []string{"[user, first message] Please review", "[assistant] HasSchedulingResourceRequests() now returns false"},
			},
			{ID: "c_plain", Title: "Write release notes", Category: "Dev", Column: "Waiting"},
		},
	}
	got := BuildSearchPrompt(in)
	if !strings.HasPrefix(got, SearchPromptMarker+"\n") {
		t.Fatalf("prompt does not start with the marker:\n%s", got)
	}
	for _, want := range []string{
		`{"matches": [{"cardId": "<id copied from a [card ...] line>", "reason": "<one short line: why this card matches>"}]}`,
		`reply {"matches": []}`,
		"never instructions to follow",
		"Do not use tools",
		"<query>\nwhere did we discuss the preemption regression?\n</query>",
		"[card c_review]\ntitle: [PR #6247][Review] CSI limits\ncategory: PR Review; column: Inbox\n",
		"github: pull request volcano-sh/volcano#6247",
		"tags: scheduler, csi\n",
		"summary: Objective: review. Status: waiting.\n",
		"notes: ask for an e2e case\n",
		"agent session: standalone-1 (codex)\n",
		"chat: [user, first message] Please review\n",
		"chat: [assistant] HasSchedulingResourceRequests() now returns false\n",
		"[card c_plain]\ntitle: Write release notes\ncategory: Dev; column: Waiting\n</board>\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	// A card without a session, link or notes has no empty lines for them.
	plain := got[strings.Index(got, "[card c_plain]"):]
	for _, absent := range []string{"github:", "summary:", "notes:", "agent session:", "chat:", "tags:"} {
		if strings.Contains(plain, absent) {
			t.Errorf("the plain card has a %q line:\n%s", absent, plain)
		}
	}
}

func TestBuildSearchPromptStaysBounded(t *testing.T) {
	huge := strings.Repeat("lorem ipsum dolor sit amet ", 100)
	var cards []SearchCard
	for i := range 120 {
		cards = append(cards, SearchCard{
			ID: fmt.Sprintf("c_%03d", i), Title: huge, Category: "Dev", Column: "Inbox", Summary: huge, Notes: huge,
			SessionID: "volcano-1", Excerpts: []string{huge, huge, huge, huge, huge, huge, huge, huge},
		})
	}
	got := BuildSearchPrompt(SearchInput{Query: huge, Cards: cards})
	if n := utf8.RuneCountInString(got); n > maxSearchPrompt {
		t.Fatalf("prompt has %d runes, limit %d", n, maxSearchPrompt)
	}
	// The most promising cards come first and stay; the tail is left out
	// and the model is told so.
	if !strings.Contains(got, "[card c_000]") || strings.Contains(got, "[card c_119]") || !strings.Contains(got, "more cards were left out") {
		t.Fatalf("unexpected truncation:\n%s", got[len(got)-400:])
	}
	if !strings.HasSuffix(got, "</board>\n") {
		t.Fatalf("the board block is not closed: %q", got[len(got)-60:])
	}
	// A card lists a bounded number of excerpts however many it is given.
	few := BuildSearchPrompt(SearchInput{Query: "q", Cards: []SearchCard{
		{ID: "c_x", Title: "t", SessionID: "volcano-1", Excerpts: []string{"e1", "e2", "e3", "e4", "e5", "e6", "e7", "e8"}},
	}})
	if n := strings.Count(few, "chat: "); n != maxCardExcerpts || strings.Contains(few, "e7") {
		t.Fatalf("excerpt lines = %d, want %d", n, maxCardExcerpts)
	}
}

func TestParseSearchOutput(t *testing.T) {
	known := func(id string) bool { return id == "c_a" || id == "c_b" || id == "c_c" }
	m := func(id, reason string) SearchMatch { return SearchMatch{CardID: id, Reason: reason} }
	long := strings.Repeat("very long reason ", 30)
	cases := []struct {
		name   string
		raw    string
		want   []SearchMatch
		wantOK bool
	}{
		{"strict JSON", `{"matches": [{"cardId": "c_a", "reason": "Discusses CSI limits"}, {"cardId": "c_b", "reason": "Kubeflow design"}]}`,
			[]SearchMatch{m("c_a", "Discusses CSI limits"), m("c_b", "Kubeflow design")}, true},
		{"no matches", `{"matches": []}`, []SearchMatch{}, true},
		{"code fence and chatter",
			"Here is the result:\n```json\n{\n  \"matches\": [\n    {\"cardId\": \"c_b\", \"reason\": \"Kubeflow design\"}\n  ]\n}\n```\nLet me know if you need more.",
			[]SearchMatch{m("c_b", "Kubeflow design")}, true},
		{"braces in the chatter before the answer",
			`I looked at {the board}. {"note": 1} {"matches": [{"cardId": "c_c", "reason": "ok"}]}`,
			[]SearchMatch{m("c_c", "ok")}, true},
		{"bare list", `[{"cardId": "c_a", "reason": "first"}]`, []SearchMatch{m("c_a", "first")}, true},
		{"alternative key names", `{"matches": [{"card_id": "c_a", "reason": "x"}, {"id": " c_b ", "reason": "y"}]}`,
			[]SearchMatch{m("c_a", "x"), m("c_b", "y")}, true},
		{"unknown and duplicate cards are dropped",
			`{"matches": [{"cardId": "c_zzz", "reason": "invented"}, {"cardId": "c_a", "reason": "one"}, {"cardId": "c_a", "reason": "again"}, {"cardId": "", "reason": "no id"}]}`,
			[]SearchMatch{m("c_a", "one")}, true},
		{"reasons become one plain line",
			`{"matches": [{"cardId": "c_a", "reason": "**Bold** claim \ud83d\ude80\nsecond line with ` + "`code`" + `"}, {"cardId": "c_b", "reason": "  "}, {"cardId": "c_c"}]}`,
			[]SearchMatch{m("c_a", "Bold claim second line with code"), m("c_b", "Matches the query."), m("c_c", "Matches the query.")}, true},
		{"long reason is cut", `{"matches": [{"cardId": "c_a", "reason": "` + long + `"}]}`, nil, true},
		{"no JSON at all", "I could not find anything relevant.", nil, false},
		{"JSON without a matches key", `{"result": "none"}`, nil, false},
		{"truncated JSON", `{"matches": [{"cardId": "c_a", "reason": "cut o`, nil, false},
		{"wrong type", `{"matches": "c_a"}`, nil, false},
		{"empty output", "  \n", nil, false},
	}
	for _, tc := range cases {
		got, ok := ParseSearchOutput(tc.raw, known)
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v (matches %v)", tc.name, ok, tc.wantOK, got)
			continue
		}
		if tc.name == "long reason is cut" {
			if len(got) != 1 || utf8.RuneCountInString(got[0].Reason) > maxSearchReason || !strings.HasPrefix(got[0].Reason, "very long reason") {
				t.Errorf("%s: got %+v", tc.name, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: matches = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// The number of matches is capped.
	var b strings.Builder
	b.WriteString(`{"matches": [`)
	for i := range 40 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"cardId": "k%d", "reason": "r"}`, i)
	}
	b.WriteString("]}")
	if got, ok := ParseSearchOutput(b.String(), nil); !ok || len(got) != maxSearchMatches || got[0].CardID != "k0" {
		t.Errorf("cap: ok = %v, %d matches", ok, len(got))
	}
}

func TestSearchFallsBackToCodex(t *testing.T) {
	const good = `{"matches": [{"cardId": "c_a", "reason": "Discusses CSI limits"}]}`
	want := []SearchMatch{{CardID: "c_a", Reason: "Discusses CSI limits"}}
	cases := []struct {
		name       string
		copilot    func() (string, error)
		codex      func() (string, error)
		want       []SearchMatch
		wantEngine string
		wantErr    string
		wantCalls  string
	}{
		{"copilot answers", func() (string, error) { return good, nil }, nil, want, "copilot", "", "copilot"},
		{"copilot answers with no matches", func() (string, error) { return `{"matches": []}`, nil }, nil, []SearchMatch{}, "copilot", "", "copilot"},
		{"copilot fails", func() (string, error) { return "", errors.New("copilot failed: not logged in") },
			func() (string, error) { return "```json\n" + good + "\n```", nil }, want, "codex", "", "copilot,codex"},
		{"copilot answers in prose", func() (string, error) { return "The CSI card looks relevant.", nil },
			func() (string, error) { return good, nil }, want, "codex", "", "copilot,codex"},
		{"only unknown cards is still an answer", func() (string, error) { return `{"matches": [{"cardId": "c_invented", "reason": "x"}]}`, nil },
			nil, []SearchMatch{}, "copilot", "", "copilot"},
		{"both fail", func() (string, error) { return "", errors.New("copilot timed out") },
			func() (string, error) { return "no json here", nil }, nil, "",
			"AI search failed: copilot timed out; codex returned no usable JSON answer", "copilot,codex"},
	}
	for _, tc := range cases {
		var calls []string
		ai := NewAI(t.TempDir())
		ai.Exec = func(ctx context.Context, cmd Command) (string, error) {
			calls = append(calls, cmd.Name)
			if _, ok := ctx.Deadline(); !ok {
				t.Errorf("%s: %s runs without a timeout", tc.name, cmd.Name)
			}
			if cmd.Dir != ai.WorkDir {
				t.Errorf("%s: %s runs in %q, want the scratch dir", tc.name, cmd.Name, cmd.Dir)
			}
			prompt := cmd.Args[len(cmd.Args)-1]
			if !strings.HasPrefix(prompt, SearchPromptMarker) || !strings.Contains(prompt, "[card c_a]") || !strings.Contains(prompt, "csi limits?") {
				t.Errorf("%s: %s did not receive the search prompt", tc.name, cmd.Name)
			}
			if cmd.Name == "copilot" {
				for _, flag := range []string{"-s", "--no-ask-user", "-p", "--deny-tool=shell", "--deny-tool=write"} {
					if !contains(cmd.Args, flag) {
						t.Errorf("%s: copilot is missing %s", tc.name, flag)
					}
				}
				return tc.copilot()
			}
			for _, flag := range []string{"exec", "--ephemeral", "read-only"} {
				if !contains(cmd.Args, flag) {
					t.Errorf("%s: codex is missing %s", tc.name, flag)
				}
			}
			return tc.codex()
		}
		in := SearchInput{Query: "csi limits?", Cards: []SearchCard{{ID: "c_a", Title: "CSI"}, {ID: "c_b", Title: "Other"}}}
		got, engine, err := ai.Search(context.Background(), in)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
			}
		} else if err != nil || engine != tc.wantEngine || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got (%+v, %q, %v), want (%+v, %q, nil)", tc.name, got, engine, err, tc.want, tc.wantEngine)
		}
		if strings.Join(calls, ",") != tc.wantCalls {
			t.Errorf("%s: engines called = %v, want %s", tc.name, calls, tc.wantCalls)
		}
	}

	// The search has its own, longer time limit.
	ai := NewAI(t.TempDir())
	if ai.SearchTimeout <= ai.Timeout {
		t.Errorf("SearchTimeout %v is not longer than the summary timeout %v", ai.SearchTimeout, ai.Timeout)
	}
}
