package fleet_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/projectadmin"
)

func TestValidateConfig(t *testing.T) {
	long := strings.Repeat("x", fleet.MaxConfigKeyOrVal+1)
	manyKeys, manySelectors := map[string]string{}, map[string]string{"fleet_name": "gs"}
	for i := 0; i <= fleet.MaxConfigKeys; i++ {
		manyKeys[strconv.Itoa(i)] = "v"
		manySelectors["selector."+strconv.Itoa(i)] = "v"
	}
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
		{"should_refuse_too_many_keys", "plugin:demo", manyKeys, true},
		{"should_refuse_too_many_selector_keys", "agones", manySelectors, true},
		{"should_refuse_a_nul_in_a_value", "plugin:demo", map[string]string{"k": "a\x00b"}, true},
		{"should_refuse_a_nul_in_a_key", "plugin:demo", map[string]string{"k\x00": "v"}, true},
		{"should_refuse_invalid_utf8_in_a_value", "plugin:demo", map[string]string{"k": "\xff"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fleet.ValidateConfig(tc.backend, tc.cfg)

			var invalid fleet.InvalidError
			assert.Equal(t, tc.wantErr, errors.As(err, &invalid))
		})
	}
}

func TestValidateConfig_should_accept_what_the_dashboard_form_allows(t *testing.T) {
	cfg := map[string]string{"fleet_name": "gs", "namespace": "ns"}
	for i := 0; i < fleet.MaxConfigKeys; i++ {
		cfg["selector."+strings.Repeat("k", fleet.MaxConfigKeyOrVal-len(strconv.Itoa(i)))+strconv.Itoa(i)] = "v"
	}

	assert.NoError(t, fleet.ValidateConfig("agones", cfg))
}

func TestAllocateServer_should_refuse_bad_input_before_any_backend_call(t *testing.T) {
	backend := &fakeBackend{name: "fake"}
	mgr := fleet.NewManager(newFakeStore(), newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock, MaxCapacity: 10})
	f := &fleet.Fleet{ID: 1, ProjectID: 2, Name: "default", Backend: backend.name}
	cases := []struct {
		name string
		in   fleet.ServerAllocate
	}{
		{"should_refuse_a_long_region", fleet.ServerAllocate{Fleet: f, Region: strings.Repeat("x", fleet.MaxConfigKeyOrVal+1)}},
		{"should_refuse_a_long_game_mode", fleet.ServerAllocate{Fleet: f, Region: "eu", GameMode: strings.Repeat("x", fleet.MaxConfigKeyOrVal+1)}},
		{"should_refuse_a_nul_in_region", fleet.ServerAllocate{Fleet: f, Region: "e\x00u"}},
		{"should_refuse_invalid_utf8_in_game_mode", fleet.ServerAllocate{Fleet: f, Region: "eu", GameMode: "\xff"}},
		{"should_refuse_capacity_above_the_limit", fleet.ServerAllocate{Fleet: f, Region: "eu", Capacity: 11}},
		{"should_refuse_a_negative_capacity", fleet.ServerAllocate{Fleet: f, Region: "eu", Capacity: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fleet.AllocateServer(t.Context(), mgr, 1, tc.in, projectadmin.Actor{UserID: 1})

			var invalid fleet.InvalidError
			assert.True(t, errors.As(err, &invalid), "err=%v", err)
		})
	}
}

func TestValidateBackend(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		wantErr bool
	}{
		{"should_accept_agones", "agones", false},
		{"should_accept_bare_plugin", "plugin", false},
		{"should_accept_named_plugin", "plugin:example", false},
		{"should_accept_name_with_dash_underscore_digit", "plugin:my_plug-1", false},
		{"should_refuse_empty_plugin_name", "plugin:", true},
		{"should_refuse_slash_in_name", "plugin:a/b", true},
		{"should_refuse_dot_dot_in_name", "plugin:..", true},
		{"should_refuse_space_in_name", "plugin:a b", true},
		{"should_refuse_control_char_in_name", "plugin:a\x01b", true},
		{"should_refuse_upper_case_in_name", "plugin:Example", true},
		{"should_refuse_name_over_64_chars", "plugin:" + strings.Repeat("a", 65), true},
		{"should_refuse_plugin_prefix_without_colon", "pluginx", true},
		{"should_refuse_unknown_backend", "memory", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fleet.ValidateBackendForTest(tc.backend)

			var invalid fleet.InvalidError
			assert.Equal(t, tc.wantErr, errors.As(err, &invalid))
		})
	}
}
