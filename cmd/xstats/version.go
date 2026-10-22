package main

import (
	"fmt"
	"runtime/debug"
)

// version and commit are set at build time:
//
//	go build -ldflags "-X main.version=... -X main.commit=..."
//
// When a value is empty, version is "dev" and commit comes from
// runtime/debug.ReadBuildInfo (vcs.revision, with -dirty when
// vcs.modified is true). If that revision is missing, commit is "unknown".
var (
	version = ""
	commit  = ""
)

func versionStamp() (string, string) {
	ver := version
	if ver == "" {
		ver = "dev"
	}
	rev := commit
	if rev == "" {
		rev = commitFromBuildInfo()
	}
	return ver, rev
}

func versionOutput() string {
	ver, rev := versionStamp()
	return fmt.Sprintf("version: %s\ncommit: %s\n", ver, rev)
}

func commitFromBuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return commitFromSettings(info.Settings)
}

func commitFromSettings(settings []debug.BuildSetting) string {
	var rev, modified string
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "unknown"
	}
	if modified == "true" {
		rev += "-dirty"
	}
	return rev
}
