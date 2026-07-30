package main

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommitFromBuildInfo(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{"no_vcs_info", nil, "unknown"},
		{
			"clean_revision_shortened",
			[]debug.BuildSetting{
				{Key: "vcs.revision", Value: "65cce64684f29f20d487fee7ce9187b7f6e373f2"},
				{Key: "vcs.modified", Value: "false"},
			},
			"65cce64684f2",
		},
		{
			"dirty_tree_marked",
			[]debug.BuildSetting{
				{Key: "vcs.revision", Value: "65cce64684f29f20d487fee7ce9187b7f6e373f2"},
				{Key: "vcs.modified", Value: "true"},
			},
			"65cce64684f2-dirty",
		},
		{
			"short_revision_kept",
			[]debug.BuildSetting{{Key: "vcs.revision", Value: "65cce64"}},
			"65cce64",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, commitFromBuildInfo(tt.settings))
		})
	}
}

func TestResolveCommit(t *testing.T) {
	vcs := []debug.BuildSetting{{Key: "vcs.revision", Value: "aaaabbbbccccddddeeeeffff0000111122223333"}}
	tests := []struct {
		name    string
		stamped string
		vcs     []debug.BuildSetting
		envRev  string
		want    string
	}{
		{"ldflags_wins_over_all", "abc1234", vcs, "9f29cee9f29cee9f29cee9f29cee9f29cee9f29c", "abc1234"},
		{"vcs_wins_over_env", "unknown", vcs, "9f29cee9f29cee9f29cee9f29cee9f29cee9f29c", "aaaabbbbcccc"},
		{"env_rev_when_nothing_stamped", "unknown", nil, "9f29cee9f29cee9f29cee9f29cee9f29cee9f29c", "9f29cee9f29c"},
		{"short_env_rev_kept", "unknown", nil, "9f29cee", "9f29cee"},
		{"unknown_when_no_source", "unknown", nil, "", "unknown"},
		{"empty_stamp_treated_as_unset", "", nil, "9f29cee", "9f29cee"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveCommit(tt.stamped, tt.vcs, tt.envRev))
		})
	}
}
