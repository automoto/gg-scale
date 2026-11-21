package fleet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/fleet"
)

func TestValidateConfig(t *testing.T) {
	long := strings.Repeat("x", fleet.MaxConfigKeyOrVal+1)
	cases := []struct {
		name    string
		backend string
		cfg     map[string]string
		wantErr bool
	}{
		{"should_accept_agones_with_fleet_name_and_selector", "agones", map[string]string{"fleet_name": "gs", "selector.zone": "eu"}, false},
		{"should_refuse_agones_without_fleet_name", "agones", map[string]string{"namespace": "n"}, true},
		{"should_refuse_unknown_agones_key", "agones", map[string]string{"fleet_name": "gs", "other": "v"}, true},
		{"should_accept_free_form_plugin_config", "plugin:demo", map[string]string{"api_key": "v"}, false},
		{"should_refuse_a_long_value", "plugin:demo", map[string]string{"k": long}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fleet.ValidateConfig(tc.backend, tc.cfg)

			var invalid fleet.InvalidError
			assert.Equal(t, tc.wantErr, errors.As(err, &invalid))
		})
	}
}

func TestValidateConfig_should_refuse_too_many_keys(t *testing.T) {
	cfg := map[string]string{}
	for i := 0; i <= fleet.MaxConfigKeys; i++ {
		cfg[strings.Repeat("k", i+1)] = "v"
	}

	assert.Error(t, fleet.ValidateConfig("plugin:demo", cfg))
}
