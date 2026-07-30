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

func TestBuildCommit_prefers_ldflags_value(t *testing.T) {
	orig := commit
	defer func() { commit = orig }()
	commit = "abc1234"
	assert.Equal(t, "abc1234", buildCommit())
}
