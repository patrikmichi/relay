package main

import "testing"

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
