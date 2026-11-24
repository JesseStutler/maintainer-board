package github

import (
	"errors"
	"testing"
)

func TestParseRef(t *testing.T) {
	const def = "volcano-sh/volcano"
	valid := []struct {
		in   string
		want Ref
	}{
		{"#6247", Ref{def, 6247, ""}},
		{"6247", Ref{def, 6247, ""}},
		{"  #6247  ", Ref{def, 6247, ""}},
		{"volcano-sh/volcano#6247", Ref{def, 6247, ""}},
		{"kubernetes-sigs/kueue#123", Ref{"kubernetes-sigs/kueue", 123, ""}},
		{"my-org-1/repo_name.go#7", Ref{"my-org-1/repo_name.go", 7, ""}},
		{"https://github.com/volcano-sh/volcano/pull/6247", Ref{def, 6247, KindPR}},
		{"https://github.com/volcano-sh/volcano/pull/6247/files#diff-abc", Ref{def, 6247, KindPR}},
		{"https://github.com/volcano-sh/volcano/pull/6247?notification_referrer_id=1", Ref{def, 6247, KindPR}},
		{"https://github.com/volcano-sh/volcano/issues/6100", Ref{def, 6100, KindIssue}},
		{"https://github.com/volcano-sh/volcano/issues/6100#issuecomment-1", Ref{def, 6100, KindIssue}},
		{"http://www.github.com/a/b/issues/5", Ref{"a/b", 5, KindIssue}},
		{"github.com/a/b/pull/9", Ref{"a/b", 9, KindPR}},
	}
	for _, tc := range valid {
		got, err := ParseRef(tc.in, def)
		if err != nil {
			t.Errorf("ParseRef(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"", "   ", "#", "#0", "0", "abc", "#12a", "volcano#12", "a/b/c#12", "-bad/repo#1", "a/b#", "my.org/repo#7",
		"https://github.com/volcano-sh/volcano", "https://github.com/volcano-sh/volcano/commit/abc",
		"https://gitlab.com/a/b/pull/1", "a/..#3", "--repo=x/y#1",
	}
	for _, in := range invalid {
		if got, err := ParseRef(in, def); !errors.Is(err, ErrBadRef) {
			t.Errorf("ParseRef(%q) = %+v, %v; want ErrBadRef", in, got, err)
		}
	}
}

func TestFindRef(t *testing.T) {
	const def = "volcano-sh/volcano"
	cases := []struct {
		text string
		want Ref
		ok   bool
	}{
		{"[PR #6247][Review] CSI NodeVolumeLimits", Ref{def, 6247, ""}, true},
		{"#602 logo size is wrong", Ref{def, 602, ""}, true},
		{"review\u4e00\u4e0b#6255\uff0cReplicas", Ref{def, 6255, ""}, true}, // CJK text around the reference
		{"fix kubeflow/trainer#42 integration", Ref{"kubeflow/trainer", 42, ""}, true},
		{"see https://github.com/volcano-sh/website/pull/602 for the logo", Ref{"volcano-sh/website", 602, KindPR}, true},
		{"first #11 then #22", Ref{def, 11, ""}, true},
		{"Kubeflow Trainer integration", Ref{}, false},
		{"color &#8203; entity and C# code", Ref{}, false},
		{"path/to/file#L20", Ref{}, false},
	}
	for _, tc := range cases {
		got, ok := FindRef(tc.text, def)
		if ok != tc.ok || got != tc.want {
			t.Errorf("FindRef(%q) = %+v, %v; want %+v, %v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}
