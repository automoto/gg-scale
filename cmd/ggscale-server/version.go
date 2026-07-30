package main

import "runtime/debug"

// commit is overridden at build time via -ldflags (see Dockerfile and
// Makefile). Binaries built without it fall back to the VCS revision the Go
// toolchain embeds.
var commit = "unknown"

const commitShortLen = 12

// buildCommit resolves the commit to report at startup: the -ldflags value
// when stamped, else the embedded VCS revision, else "unknown".
func buildCommit() string {
	if commit != "unknown" && commit != "" {
		return commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return commitFromBuildInfo(info.Settings)
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
	if len(rev) > commitShortLen {
		rev = rev[:commitShortLen]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
