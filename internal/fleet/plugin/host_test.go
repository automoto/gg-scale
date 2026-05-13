package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

func TestPluginCloseIsNilSafe(t *testing.T) {
	var p *Plugin

	assert.NoError(t, p.Close())
}
