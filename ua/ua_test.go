package ua_test

import (
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/ua"
)

func TestBuild(t *testing.T) {
	got := ua.Build("go_api_extractor", "1.2.1")
	want := "go_api_extractor/1.2.1 (+https://github.com/baditaflorin/go_api_extractor)"
	if got != want {
		t.Errorf("Build() = %q, want %q", got, want)
	}
}

func TestBuildContainsVersion(t *testing.T) {
	got := ua.Build("go_cors_scanner", "2.0.0")
	if !strings.Contains(got, "2.0.0") {
		t.Errorf("Build() missing version: %q", got)
	}
	if !strings.Contains(got, "go_cors_scanner") {
		t.Errorf("Build() missing serviceID: %q", got)
	}
}
