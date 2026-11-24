package summary

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPlain(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"headings and emphasis", "## Result\n\nThe **fix** is `ready` and __tested__.", "Result The fix is ready and tested."},
		{"lists", "- first item\n- second item\n1. numbered", "first item second item numbered"},
		{"links and images", "See [the PR](https://github.com/a/b/pull/1) and ![diagram](x.png).", "See the PR and diagram."},
		{"code block removed", "Before\n```go\nfmt.Println(1)\n```\nAfter", "Before After"},
		{"unterminated code block", "Before\n```\nstill code", "Before"},
		{"table", "| Case | Result |\n|------|--------|\n| preempt | ok |", "Case Result preempt ok"},
		{"blockquote and html", "> quoted <b>text</b>", "quoted text"},
		{"snake_case kept", "call node_volume_limits now", "call node_volume_limits now"},
		{"emoji stripped", "Done \u2705 shipped \U0001F680 ok", "Done shipped ok"},
		{"cjk kept", "\u5df2\u63a8\u5230 PR\uff0c\u7b49\u5f85\u8bc4\u5ba1\u3002", "\u5df2\u63a8\u5230 PR\uff0c\u7b49\u5f85\u8bc4\u5ba1\u3002"},
		{"whitespace collapsed", "a\t\tb\r\n\r\n  c", "a b c"},
	}
	for _, tc := range cases {
		if got := Plain(tc.in); got != tc.want {
			t.Errorf("%s: Plain(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 180); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
	sentences := "First sentence is here. Second sentence follows it. Third sentence is the one that gets cut off somewhere."
	if got := Truncate(sentences, 60); got != "First sentence is here. Second sentence follows it." {
		t.Errorf("sentence cut = %q", got)
	}
	words := "one two three four five six seven eight nine ten eleven twelve"
	if got := Truncate(words, 30); got != "one two three four five..." {
		t.Errorf("word cut = %q", got)
	}
	cjk := strings.Repeat("\u8fd9\u662f\u4e00\u6bb5\u5f88\u957f\u7684\u4e2d\u6587\u6458\u8981", 30) // no spaces or sentence ends
	got := Truncate(cjk, AutoMaxLen)
	if n := utf8.RuneCountInString(got); n != AutoMaxLen || !strings.HasSuffix(got, "...") || !utf8.ValidString(got) {
		t.Errorf("cjk cut has %d runes, valid %v: %q", n, utf8.ValidString(got), got)
	}
	cjkSentence := "\u7b2c\u4e00\u53e5\u8bdd\u5728\u8fd9\u91cc\u7ed3\u675f\u3002\u7b2c\u4e8c\u53e5\u8bdd\u6bd4\u8f83\u957f\u4f1a\u88ab\u622a\u65ad\u6389"
	if got := Truncate(cjkSentence, 16); got != "\u7b2c\u4e00\u53e5\u8bdd\u5728\u8fd9\u91cc\u7ed3\u675f\u3002" {
		t.Errorf("cjk sentence cut = %q", got)
	}
}

func TestExtract(t *testing.T) {
	long := "The regression is reproduced on a kind cluster: the high priority pod stays pending under the PR build and preempts on base."
	cases := []struct {
		name  string
		turns []Turn
		want  string
	}{
		{"latest substantial assistant message", []Turn{
			{"user", "review #6247"},
			{"assistant", "## Findings\n\n" + long},
			{"assistant", "Done."},
		}, "Findings " + long},
		{"short reply falls back to request and reply", []Turn{
			{"user", "please rebase the branch"},
			{"assistant", "Done."},
		}, "Asked: please rebase the branch Reply: Done."},
		{"only a user request", []Turn{{"user", "analyze the scheduler cache"}}, "Asked: analyze the scheduler cache"},
		{"empty turns ignored", []Turn{{"assistant", "```\ncode only\n```"}, {"user", "  "}}, "fallback title"},
		{"no turns", nil, "fallback title"},
	}
	for _, tc := range cases {
		got := Extract(tc.turns, "fallback title")
		if got != Truncate(tc.want, AutoMaxLen) {
			t.Errorf("%s: Extract = %q, want %q", tc.name, got, Truncate(tc.want, AutoMaxLen))
		}
		if utf8.RuneCountInString(got) > AutoMaxLen {
			t.Errorf("%s: summary has %d runes", tc.name, utf8.RuneCountInString(got))
		}
	}
}

func TestBuildPrompt(t *testing.T) {
	var turns []Turn
	turns = append(turns, Turn{"user", "ORIGINAL GOAL: review the CSI change"})
	for i := 0; i < 20; i++ {
		turns = append(turns, Turn{"assistant", "progress note " + strings.Repeat("x", 10)})
	}
	turns = append(turns, Turn{"assistant", "LATEST: " + strings.Repeat("long ", 2000)})
	prompt := BuildPrompt(Input{
		Title:  "[PR #6247] CSI review",
		GitHub: "pull request volcano-sh/volcano#6247, state OPEN",
		Notes:  "check preemption",
		Turns:  turns,
	})
	for _, want := range []string{
		PromptMarker, "Objective:", "Status:", "<context>", "</context>",
		"Card title: [PR #6247] CSI review",
		"Linked GitHub item: pull request volcano-sh/volcano#6247, state OPEN",
		"Maintainer notes: check preemption",
		"[user, earlier] ORIGINAL GOAL: review the CSI change",
		"[assistant] LATEST: long",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if n := strings.Count(prompt, "\n[assistant] "); n != maxTurns {
		t.Errorf("prompt carries %d recent turns, want %d", n, maxTurns)
	}
	if n := utf8.RuneCountInString(prompt); n > maxPromptSize+16 {
		t.Errorf("prompt has %d runes, limit %d", n, maxPromptSize)
	}
	if !strings.HasPrefix(prompt, PromptMarker) {
		t.Error("prompt must start with the marker used to recognize summarizer runs")
	}
}

func TestCleanOutput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"well formed", "Objective: Review PR 6247.\nStatus: Regression reproduced; waiting for author.", "Objective: Review PR 6247.\nStatus: Regression reproduced; waiting for author."},
		{"markdown and preamble", "Here is the summary:\n\n**Objective:** Review PR 6247.\n- **Status**: Waiting for author. \U0001F680\n\nLet me know!", "Objective: Review PR 6247.\nStatus: Waiting for author."},
		{"latest status label", "objective - Ship the feature\nLatest Status: CI is green", "Objective: Ship the feature\nStatus: CI is green"},
		{"only status", "Status: blocked on review", "Status: blocked on review"},
		{"free form falls back to two lines", "The session reviews a PR.\n\nIt found a regression.\nThird line dropped.", "The session reviews a PR.\nIt found a regression."},
		{"empty", "  \n\n", ""},
	}
	for _, tc := range cases {
		if got := CleanOutput(tc.in); got != tc.want {
			t.Errorf("%s: CleanOutput = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSummarizeFallsBackToCodex(t *testing.T) {
	const good = "Objective: Review the change.\nStatus: Waiting for CI."
	cases := []struct {
		name       string
		copilot    func() (string, error)
		codex      func() (string, error)
		wantEngine string
		wantErr    string
		wantCalls  []string
	}{
		{"copilot answers", func() (string, error) { return good, nil }, nil, "copilot", "", []string{"copilot"}},
		{"copilot fails", func() (string, error) { return "", errors.New("copilot failed: not logged in") },
			func() (string, error) { return good, nil }, "codex", "", []string{"copilot", "codex"}},
		{"copilot returns nothing", func() (string, error) { return " \n", nil },
			func() (string, error) { return good, nil }, "codex", "", []string{"copilot", "codex"}},
		{"both fail", func() (string, error) { return "", errors.New("copilot timed out") },
			func() (string, error) { return "", errors.New("codex failed: quota") }, "", "copilot timed out; codex failed: quota", []string{"copilot", "codex"}},
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
			if !strings.HasPrefix(prompt, PromptMarker) {
				t.Errorf("%s: %s did not receive the prompt as its last argument", tc.name, cmd.Name)
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
		text, engine, err := ai.Summarize(context.Background(), Input{Title: "t", Turns: []Turn{{"user", "hi"}}})
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.wantErr)
			}
		} else if err != nil || text != good || engine != tc.wantEngine {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, nil)", tc.name, text, engine, err, good, tc.wantEngine)
		}
		if strings.Join(calls, ",") != strings.Join(tc.wantCalls, ",") {
			t.Errorf("%s: engines called = %v, want %v", tc.name, calls, tc.wantCalls)
		}
	}
}

func TestRunCommandReadsCodexOutputFile(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/last.txt"
	text, err := runCommand(context.Background(), Command{
		Name: "sh", Args: []string{"-c", "echo progress noise; printf 'final answer' > " + out},
		Dir: dir, Env: toolEnv(), OutputFile: out,
	})
	if err != nil || text != "final answer" {
		t.Fatalf("runCommand = %q, %v; want the output file content", text, err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("output file was not cleaned up: %v", err)
	}

	_, err = runCommand(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo broken >&2; exit 3"}, Dir: dir, Env: toolEnv()})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("failure did not surface stderr: %v", err)
	}
	if _, err := runCommand(context.Background(), Command{Name: "definitely-not-installed-cli", Dir: dir, Env: toolEnv()}); err == nil {
		t.Error("a missing binary did not fail")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
