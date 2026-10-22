package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionOutputFromBuildVars(t *testing.T) {
	origV, origC := version, commit
	t.Cleanup(func() { version, commit = origV, origC })

	version = "v1.4.0"
	commit = "abc123def"
	got := versionOutput()
	want := "version: v1.4.0\ncommit: abc123def\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestVersionOutputFallback(t *testing.T) {
	origV, origC := version, commit
	t.Cleanup(func() { version, commit = origV, origC })

	version, commit = "", ""
	got := versionOutput()
	want := "version: dev\ncommit: " + commitFromBuildInfo() + "\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	version = "v9.9.9"
	got = versionOutput()
	want = "version: v9.9.9\ncommit: " + commitFromBuildInfo() + "\n"
	if got != want {
		t.Fatalf("version set, commit unset: got %q want %q", got, want)
	}
}

func TestCommitFromBuildInfo(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	want := "unknown"
	if ok {
		want = commitFromSettings(info.Settings)
	}
	if got := commitFromBuildInfo(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCommitFromSettings(t *testing.T) {
	cases := []struct {
		name string
		in   []debug.BuildSetting
		want string
	}{
		{name: "none", want: "unknown"},
		{
			name: "revision",
			in:   []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}},
			want: "abc",
		},
		{
			name: "clean",
			in: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "abc",
		},
		{
			name: "dirty",
			in: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "abc-dirty",
		},
		{
			name: "modified without revision",
			in:   []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
			want: "unknown",
		},
		{
			name: "other settings ignored",
			in: []debug.BuildSetting{
				{Key: "vcs", Value: "git"},
				{Key: "vcs.time", Value: "2026-10-22T00:00:00Z"},
				{Key: "vcs.revision", Value: "def456"},
			},
			want: "def456",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitFromSettings(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
