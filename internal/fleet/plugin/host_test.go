package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchConfigBinaryPathDefaultsToEtcGgscalePlugins(t *testing.T) {
	cfg := LaunchConfig{Name: "ovh"}

	got := cfg.BinaryPath()

	assert.Equal(t, "/etc/ggscale/plugins/ggscale-fleet-ovh", got)
}

func TestLaunchConfigBinaryPathHonoursDir(t *testing.T) {
	cfg := LaunchConfig{Dir: "/opt/plugins", Name: "ovh"}

	got := cfg.BinaryPath()

	assert.Equal(t, "/opt/plugins/ggscale-fleet-ovh", got)
}

func TestValidPluginName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"simple", "ovh", true},
		{"with_underscore", "my_plugin", true},
		{"with_hyphen", "my-plugin", true},
		{"digit_in_middle", "ovh1", true},
		{"starts_with_digit", "1ovh", true},
		{"empty", "", false},
		{"uppercase_rejected", "OVH", false},
		{"path_traversal_dotdot", "../etc/passwd", false},
		{"path_separator", "etc/passwd", false},
		{"leading_hyphen", "-x", false},
		{"leading_underscore", "_x", false},
		{"spaces", "ovh plugin", false},
		{"too_long", "a234567890123456789012345678901234567890123456789012345678901234x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := validPluginName.MatchString(tc.input)

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLaunchRejectsInvalidPluginName(t *testing.T) {
	_, err := Launch(LaunchConfig{Dir: t.TempDir(), Name: "../escape"})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid plugin name")
}

func TestVerifyPluginPathRejectsWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o777))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	bin := filepath.Join(dir, "ggscale-fleet-ovh")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	err := verifyPluginPath(bin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "group/world writable")
}

func TestVerifyPluginPathRejectsSymlinkedBinary(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755))
	link := filepath.Join(dir, "ggscale-fleet-ovh")
	require.NoError(t, os.Symlink(target, link))

	err := verifyPluginPath(link)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
}

func TestPluginCloseIsNilSafe(t *testing.T) {
	var p *Plugin

	assert.NoError(t, p.Close())
}
