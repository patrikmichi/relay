package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersionString(t *testing.T) {
	got := buildVersionString("v9.9.9", "abc1234", "2026-07-06T00:00:00Z")
	want := "v9.9.9 (abc1234, 2026-07-06T00:00:00Z)"
	if got != want {
		t.Errorf("buildVersionString: got %q, want %q", got, want)
	}
}

func TestBuildVersionString_Defaults(t *testing.T) {
	got := buildVersionString("dev", "none", "unknown")
	want := "dev (none, unknown)"
	if got != want {
		t.Errorf("buildVersionString defaults: got %q, want %q", got, want)
	}
}

func TestModuleVersion(t *testing.T) {
	withMain := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/patrikmichi/relay", Version: v}}
	}
	cases := []struct {
		name     string
		injected string
		info     *debug.BuildInfo
		want     string
	}{
		{"injected wins", "0.2.0", withMain("v0.2.0"), "0.2.0"},
		{"go install tag", "dev", withMain("v0.2.0"), "v0.2.0"},
		{"local build", "dev", withMain("(devel)"), "dev"},
		{"empty module version", "dev", withMain(""), "dev"},
		{"no build info", "dev", nil, "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := moduleVersion(tc.injected, tc.info); got != tc.want {
				t.Errorf("moduleVersion(%q) = %q, want %q", tc.injected, got, tc.want)
			}
		})
	}
}
