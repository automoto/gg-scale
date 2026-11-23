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

func TestAgonesServerSetKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"namespace", true},
		{"selector.ggscale.dev/project", true},
		{"selector.ggscale.dev/other", true},
		{"selector.GGSCALE.DEV/project", true},
		{"selector. ggscale.dev/project", true},
		{" namespace ", true},
		{" selector.ggscale.dev/project", true},
		{"fleet_name", false},
		{"selector.zone", false},
		{"selector.agones.dev/fleet", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			assert.Equal(t, tc.want, fleet.AgonesServerSetKey(tc.key))
		})
	}
}

func TestCreateFleet_should_refuse_a_server_set_agones_key_when_isolation_is_on(t *testing.T) {
	for _, key := range []string{"namespace", "selector.ggscale.dev/project"} {
		t.Run(key, func(t *testing.T) {
			store := fleet.NewPostgresFleetStore(nil, nil)
			store.AgonesProjectIsolation = true

			_, err := store.CreateFleet(t.Context(), 1, fleet.FleetCreate{
				ProjectID: 2, Name: "a", Backend: "agones", Config: map[string]string{"fleet_name": "gs", key: "SECRETVALUE"},
			}, projectadmin.Actor{UserID: 1})

			assert.Equal(t, []any{true, false}, []any{errors.Is(err, fleet.ErrAgonesServerSetKey), strings.Contains(err.Error(), "SECRETVALUE")})
		})
	}
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

func TestAllocateServer_should_cap_the_live_allocations_of_the_project(t *testing.T) {
	cases := []struct {
		name      string
		seed      []fleet.Status
		otherLive int
		wantLimit bool
	}{
		{"should_allocate_under_the_cap", []fleet.Status{fleet.StatusReady}, 0, false},
		{"should_refuse_at_the_cap", []fleet.Status{fleet.StatusReady, fleet.StatusPending}, 0, true},
		{"should_not_count_terminal_allocations", []fleet.Status{fleet.StatusReady, fleet.StatusShutdown, fleet.StatusFailed}, 0, false},
		{"should_not_count_another_project", []fleet.Status{fleet.StatusReady}, 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			for _, st := range tc.seed {
				id, _ := store.InsertPending(t.Context(), fleet.AllocationRequest{ProjectID: 2}, "fake")
				store.allocations[id].Status = st
			}
			for range tc.otherLive {
				_, _ = store.InsertPending(t.Context(), fleet.AllocationRequest{ProjectID: 9}, "fake")
			}
			backend := &fakeBackend{name: "fake", allocateImpl: func(int) (*fleet.Allocation, error) {
				return &fleet.Allocation{BackendRef: "ref", Address: "10.0.0.1:7777"}, nil
			}}
			mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock, MaxLiveAllocations: 2})
			f := &fleet.Fleet{ID: 1, ProjectID: 2, Name: "default", Backend: backend.name}

			_, err := fleet.AllocateServer(t.Context(), mgr, 1, fleet.ServerAllocate{Fleet: f, Region: "eu"}, projectadmin.Actor{UserID: 1})

			wantCalls := 1
			if tc.wantLimit {
				wantCalls = 0
			}
			assert.Equal(t, []any{tc.wantLimit, wantCalls}, []any{errors.Is(err, fleet.ErrLiveAllocationLimit), backend.allocateCalls}, "err=%v", err)
		})
	}
}

func TestAllocateServer_should_name_the_setting_in_the_limit_error(t *testing.T) {
	assert.Contains(t, fleet.ErrLiveAllocationLimit.Error(), "FLEET_MAX_LIVE_ALLOCATIONS_PER_PROJECT")
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
