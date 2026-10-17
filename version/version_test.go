package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestDefaultsAreUnambiguousSentinels(t *testing.T) {
	if Tag != "dev" || GitCommit != "HEAD" || BuildDate != "unknown" {
		t.Fatalf("unset build identity = %q, %q, %q; want the dev/HEAD/unknown sentinels", Tag, GitCommit, BuildDate)
	}
}

func TestStringFormat(t *testing.T) {
	if got, want := String(), "dev (HEAD, built unknown)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestInfoCarriesEveryField(t *testing.T) {
	info := Info()
	for _, k := range []string{"tag", "git_commit", "build_date", "go_version"} {
		if info[k] == "" {
			t.Errorf("Info()[%q] is empty", k)
		}
	}
	if info["go_version"] != runtime.Version() || !strings.HasPrefix(info["go_version"], "go") {
		t.Errorf("go_version = %q, want runtime.Version() = %q", info["go_version"], runtime.Version())
	}
}
