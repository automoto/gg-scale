package main

import (
	"os"
	"runtime/debug"
)

// commit is overridden at build time via -ldflags (see Dockerfile and
// Makefile).
var commit = "unknown"

const commitShortLen = 12

// buildCommit resolves the commit to report at startup, in order of trust:
// the -ldflags stamp, the VCS revision the Go toolchain embeds, then the
// GIT_REV environment variable set in the app container (Docker
// builds copy no .git, so the embedded revision is absent there).
func buildCommit() string {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return resolveCommit(commit, settings, os.Getenv("GIT_REV"))
}

func resolveCommit(stamped string, settings []debug.BuildSetting, envRev string) string {
	if stamped != "unknown" && stamped != "" {
		return stamped
	}
	if rev := commitFromBuildInfo(settings); rev != "unknown" {
		return rev
	}
	if envRev != "" {
		return shortRev(envRev)
	}
	return "unknown"
}

// commitFromBuildInfo extracts a short revision from the build settings,
// marking builds from a modified working tree with a -dirty suffix.
func commitFromBuildInfo(settings []debug.BuildSetting) string {
	rev, dirty := "", false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if dirty {
		return shortRev(rev) + "-dirty"
	}
	return shortRev(rev)
}

func shortRev(rev string) string {
	if len(rev) > commitShortLen {
		return rev[:commitShortLen]
	}
	return rev
}
