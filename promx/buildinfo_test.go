package promx

import (
	"runtime/debug"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestGoCommonVersionOf(t *testing.T) {
	t.Parallel()
	const mod = "github.com/baditaflorin/go-common"
	for name, tc := range map[string]struct {
		bi   debug.BuildInfo
		want string
	}{
		"a release in the dependency list": {debug.BuildInfo{Main: debug.Module{Path: "example.com/svc"}, Deps: []*debug.Module{
			{Path: "github.com/other/thing", Version: "v1.0.0"}, {Path: mod, Version: "v0.109.0"}}}, "v0.109.0"},
		"a pseudo-version": {debug.BuildInfo{Main: debug.Module{Path: "example.com/svc"}, Deps: []*debug.Module{
			{Path: mod, Version: "v0.109.1-0.20261021120000-abcdef123456"}}}, "v0.109.1-0.20261021120000-abcdef123456"},
		"replaced by a module with a version": {debug.BuildInfo{Main: debug.Module{Path: "example.com/svc"}, Deps: []*debug.Module{
			{Path: mod, Version: "v0.109.0", Replace: &debug.Module{Path: "example.com/fork", Version: "v0.109.0-fork.1"}}}}, "v0.109.0-fork.1"},
		"replaced by a directory": {debug.BuildInfo{Main: debug.Module{Path: "example.com/svc"}, Deps: []*debug.Module{
			{Path: mod, Version: "v0.109.0", Replace: &debug.Module{Path: "../go-common"}}}}, "devel"},
		"go-common is the main module":          {debug.BuildInfo{Main: debug.Module{Path: mod, Version: "(devel)"}}, "devel"},
		"go-common is the main module at a tag": {debug.BuildInfo{Main: debug.Module{Path: mod, Version: "v0.109.0"}}, "v0.109.0"},
		"not linked at all":                     {debug.BuildInfo{Main: debug.Module{Path: "example.com/svc"}, Deps: []*debug.Module{{Path: "github.com/other/thing", Version: "v1.0.0"}}}, "_unknown"},
		"a similar module path":                 {debug.BuildInfo{Main: debug.Module{Path: "example.com/svc"}, Deps: []*debug.Module{{Path: mod + "-extras", Version: "v0.1.0"}}}, "_unknown"},
	} {
		bi := tc.bi
		if got := goCommonVersionOf(&bi); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// build_info carries the go-common version next to the service's own, on the same series.
func TestBuildInfoHasTheGoCommonVersion(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	registerBuildInfo(reg, "svc", "1.2.3")
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "build_info" || len(families[0].GetMetric()) != 1 {
		t.Fatalf("want one build_info series, got %v", families)
	}
	labels := map[string]string{}
	for _, l := range families[0].GetMetric()[0].GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	for _, name := range []string{"service", "version", "git_sha", "built_at", "go_version", "go_common_version"} {
		if labels[name] == "" {
			t.Errorf("build_info has no %q label: %v", name, labels)
		}
	}
	if labels["service"] != "svc" || labels["version"] != "1.2.3" {
		t.Errorf("the service labels changed: %v", labels)
	}
	if want := goCommonVersion(); labels["go_common_version"] != want {
		t.Errorf("go_common_version = %q, want %q", labels["go_common_version"], want)
	}
	if len(labels) != 6 {
		t.Errorf("build_info has %d labels, want the six named above: %v", len(labels), labels)
	}
}
