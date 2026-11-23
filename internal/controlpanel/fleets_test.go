package controlpanel

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/fleet"
)

func TestFleetsListPage_renders_empty_state_when_no_fleets(t *testing.T) {
	html := renderToString(t, FleetsListPage(FleetsListView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "agones", Enabled: true,
	}))
	assert.Contains(t, html, "Create your first fleet")
	assert.NotContains(t, html, "<table")
}

func TestFleetsListPage_renders_disabled_state_when_no_backend(t *testing.T) {
	html := renderToString(t, FleetsListPage(FleetsListView{
		TenantID: 1, ProjectID: 2, Enabled: false,
	}))
	assert.Contains(t, html, "No fleet backend configured")
}

func TestFleetsListPage_lists_fleets_with_backend_mismatch_marker(t *testing.T) {
	html := renderToString(t, FleetsListPage(FleetsListView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "agones", Enabled: true,
		Fleets: []FleetRowView{
			{ID: 1, Name: "primary", Backend: "agones", BackendMatches: true, Summary: "doomerang"},
			{ID: 2, Name: "stale", Backend: "plugin:ovh", BackendMatches: false, Summary: "flavor=b2-7"},
		},
	}))
	assert.Contains(t, html, "primary")
	assert.Contains(t, html, "doomerang")
	assert.Contains(t, html, "stale")
	assert.Contains(t, html, "not active")
}

func TestNewFleetPage_renders_agones_fields_by_default(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "agones", Backend: "",
	}))
	assert.Contains(t, html, `name="fleet_name"`)
	assert.Contains(t, html, `name="namespace"`)
	assert.Contains(t, html, `name="selector_key[]"`)
	assert.NotContains(t, html, `name="image"`)
}

func TestNewFleetPage_renders_agones_fields_when_selected(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "agones", Backend: "agones",
	}))
	assert.Contains(t, html, `name="fleet_name"`)
	assert.Contains(t, html, `name="namespace"`)
	assert.Contains(t, html, `name="selector_key[]"`)
	assert.NotContains(t, html, `name="image"`)
}

func TestNewFleetPage_renders_plugin_fields_when_selected(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "plugin:ovh", Backend: "plugin",
	}))
	assert.Contains(t, html, `name="config_key[]"`)
	assert.Contains(t, html, `name="config_value[]"`)
	assert.NotContains(t, html, `name="image"`)
}

func TestNewFleetPage_shows_field_errors(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, Backend: "agones",
		FieldErrors: map[string]string{"fleet_name": "Fleet name is required."},
	}))
	assert.Contains(t, html, "Fleet name is required.")
}

func TestEditFleetPage_warns_when_backend_does_not_match_configured(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5,
		Name: "stale", Backend: "agones", BackendConfigured: "plugin:ovh",
		Config: map[string]string{"fleet_name": "doomerang"},
	}))
	assert.Contains(t, html, "does not match configured backend")
	assert.Contains(t, html, "doomerang")
}

func TestEditFleetPage_renders_delete_form(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5, Name: "primary",
		Backend: "agones", BackendConfigured: "agones",
		Config: map[string]string{"fleet_name": "doomerang"},
	}))
	assert.Contains(t, html, "/delete")
	assert.Contains(t, html, "Delete fleet")
}

func TestParseFleetConfigForm_agones_requires_fleet_name(t *testing.T) {
	_, errs := parseFleetConfigForm("agones", url.Values{})
	assert.Contains(t, errs, "fleet_name")
}

func TestParseFleetConfigForm_agones_merges_selector_pairs(t *testing.T) {
	cfg, errs := parseFleetConfigForm("agones", url.Values{
		"fleet_name":       {"doomerang"},
		"namespace":        {"games"},
		"selector_key[]":   {"tier", "build"},
		"selector_value[]": {"public", "v1"},
	})
	assert.Empty(t, errs)
	assert.Equal(t, "doomerang", cfg["fleet_name"])
	assert.Equal(t, "games", cfg["namespace"])
	assert.Equal(t, "public", cfg["selector.tier"])
	assert.Equal(t, "v1", cfg["selector.build"])
}

func TestParseFleetConfigForm_agones_drops_empty_keys(t *testing.T) {
	cfg, _ := parseFleetConfigForm("agones", url.Values{
		"fleet_name":       {"doomerang"},
		"selector_key[]":   {"", "build", "  "},
		"selector_value[]": {"orphan", "v1", "trim"},
	})
	// Empty / whitespace keys must not produce "selector." prefix entries.
	assert.NotContains(t, cfg, "selector.")
	assert.NotContains(t, cfg, "selector.  ")
	assert.Equal(t, "v1", cfg["selector.build"])
}

func TestParseFleetConfigForm_plugin_passes_arbitrary_kv(t *testing.T) {
	cfg, errs := parseFleetConfigForm("plugin:ovh", url.Values{
		"config_key[]":   {"flavor", "region"},
		"config_value[]": {"b2-7", "GRA9"},
	})
	assert.Empty(t, errs)
	assert.Equal(t, "b2-7", cfg["flavor"])
	assert.Equal(t, "GRA9", cfg["region"])
}

func TestSummarizeFleetConfig_per_backend(t *testing.T) {
	assert.Equal(t,
		"doomerang",
		summarizeFleetConfig("agones", map[string]string{"fleet_name": "doomerang"}),
	)
	assert.Equal(t,
		"api_token flavor",
		summarizeFleetConfig("plugin:ovh", map[string]string{"flavor": "b2-7", "api_token": pluginSecretMarker}),
	)
}

const pluginSecretMarker = "tok-SEALMARKER-7f3a"

func TestFleetsListPage_should_not_show_a_plugin_config_value(t *testing.T) {
	cfg := map[string]string{"api_token": pluginSecretMarker}
	html := renderToString(t, FleetsListPage(FleetsListView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "plugin:x", Enabled: true,
		Fleets: []FleetRowView{{ID: 1, Name: "p", Backend: "plugin:x", BackendMatches: true, Summary: summarizeFleetConfig("plugin:x", cfg)}},
	}))
	assert.NotContains(t, html, pluginSecretMarker)
}

func TestEditFleetPage_should_not_show_a_plugin_config_value(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5, Name: "p", Backend: "plugin:x", BackendConfigured: "plugin:x",
		Config: map[string]string{"api_token": pluginSecretMarker},
	}))
	assert.NotContains(t, html, pluginSecretMarker)
}

func TestEditFleetPage_should_show_each_plugin_key_with_a_keep_hint(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5, Name: "p", Backend: "plugin:x", BackendConfigured: "plugin:x",
		Config: map[string]string{"api_token": pluginSecretMarker},
	}))
	assert.Contains(t, html, `name="config_key[]" value="api_token"`)
	assert.Contains(t, html, "An empty value keeps the saved value")
}

func TestNewFleetPage_should_not_show_a_typed_plugin_value_again(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "plugin:x", Backend: "plugin:x",
		Config: map[string]string{"api_token": pluginSecretMarker},
	}))
	assert.NotContains(t, html, pluginSecretMarker)
}

func TestFleetFormConfig_should_hide_plugin_values_only(t *testing.T) {
	assert.Equal(t, map[string]string{"api_token": ""}, fleetFormConfig("plugin:x", map[string]string{"api_token": pluginSecretMarker}))
	assert.Equal(t, map[string]string{"fleet_name": "gs"}, fleetFormConfig("agones", map[string]string{"fleet_name": "gs"}))
}

func TestKeepStoredPluginValues(t *testing.T) {
	stored := map[string]string{"api_token": "saved", "region": "GRA9"}
	tests := []struct {
		name   string
		keys   []string
		values []string
		want   map[string]string
	}{
		{"should_keep_the_stored_value_when_the_value_is_empty", []string{"api_token", "region"}, []string{"", ""}, map[string]string{"api_token": "saved", "region": "GRA9"}},
		{"should_replace_the_stored_value_with_a_new_value", []string{"api_token", "region"}, []string{"next", ""}, map[string]string{"api_token": "next", "region": "GRA9"}},
		{"should_remove_a_key_whose_key_field_is_empty", []string{"", "region"}, []string{"", ""}, map[string]string{"region": "GRA9"}},
		{"should_remove_a_key_with_no_row", []string{"region"}, []string{""}, map[string]string{"region": "GRA9"}},
		{"should_clear_the_config_when_no_key_is_left", []string{"", "", ""}, []string{"", "", ""}, map[string]string{}},
		{"should_add_a_new_key_with_an_empty_value", []string{"api_token", "region", "zone"}, []string{"", "", ""}, map[string]string{"api_token": "saved", "region": "GRA9", "zone": ""}},
		{"should_lose_the_value_of_a_renamed_key_with_an_empty_value", []string{"token", "region"}, []string{"", ""}, map[string]string{"token": "", "region": "GRA9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _ := parseFleetConfigForm("plugin:x", url.Values{"config_key[]": tt.keys, "config_value[]": tt.values})

			err := keepStoredPluginValues(context.Background(), cfg, &fleet.Fleet{Config: stored})

			assert.Equal(t, []any{nil, tt.want}, []any{err, cfg})
		})
	}
}

func TestFleetBackendKind_buckets_plugin_variants(t *testing.T) {
	assert.Equal(t, "agones", fleetBackendKind("agones"))
	assert.Equal(t, "plugin", fleetBackendKind("plugin"))
	assert.Equal(t, "plugin", fleetBackendKind("plugin:ovh"))
	assert.Equal(t, "agones", fleetBackendKind(""))
}

func TestFleetSelectorLabels_strips_prefix(t *testing.T) {
	got := fleetSelectorLabels(map[string]string{
		"fleet_name":     "doomerang",
		"selector.tier":  "public",
		"selector.build": "v1",
	})
	assert.Equal(t, map[string]string{"tier": "public", "build": "v1"}, got)
}

func TestEditToNewFleetView_projects_fields(t *testing.T) {
	got := editToNewFleetView(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 99,
		Name: "n", Backend: "agones", Config: map[string]string{"fleet_name": "doomerang"},
		FieldErrors: map[string]string{"name": "required"},
	})
	assert.Equal(t, int64(1), got.TenantID)
	assert.Equal(t, int64(2), got.ProjectID)
	assert.Equal(t, "agones", got.Backend)
	assert.Equal(t, "doomerang", got.Config["fleet_name"])
	assert.Contains(t, got.FieldErrors, "name")
}

func TestNewFleetAllocationPage_blocks_form_until_a_fleet_exists(t *testing.T) {
	html := renderToString(t, NewFleetAllocationPage(NewAllocationView{
		TenantID: 1, ProjectID: 2, Enabled: true,
		Fleets: nil,
	}))
	assert.Contains(t, html, "No fleet templates exist")
	assert.NotContains(t, html, `name="fleet"`)
}

func TestNewFleetAllocationPage_renders_fleet_dropdown_with_mismatch_marker(t *testing.T) {
	html := renderToString(t, NewFleetAllocationPage(NewAllocationView{
		TenantID: 1, ProjectID: 2, Enabled: true,
		Fleets: []FleetOption{
			{ID: 1, Name: "primary", Backend: "agones", BackendMatches: true},
			{ID: 2, Name: "stale", Backend: "plugin:ovh", BackendMatches: false, BackendConfigured: "agones"},
		},
	}))
	assert.Contains(t, html, `name="fleet"`)
	assert.Contains(t, html, "primary")
	assert.Contains(t, html, "stale")
	assert.Contains(t, html, "backend mismatch")
}

func TestFleetsListPage_lists_deleted_fleets_with_a_restore_form(t *testing.T) {
	html := renderToString(t, FleetsListPage(FleetsListView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "agones", Enabled: true, CSRFToken: "tok",
		DeletedFleets: []FleetRowView{{ID: 7, Name: "old-one", Backend: "agones"}},
	}))
	assert.Contains(t, html, "old-one")
	assert.Contains(t, html, `action="/v1/control-panel/tenants/1/projects/2/fleets/7/restore"`)
	assert.Contains(t, html, `name="_csrf" value="tok"`)
}

func TestNewFleetPage_plugin_option_stores_the_configured_plugin_name(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "plugin:ovh",
	}))
	assert.Contains(t, html, `<option value="plugin:ovh"`)
}

func TestNewFleetPage_plugin_option_is_selected_for_the_configured_plugin(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "plugin:ovh", Backend: "plugin:ovh",
	}))
	assert.Contains(t, html, `<option value="plugin:ovh" selected`)
}

func TestParseFleetConfigForm_should_refuse_a_key_in_two_rows(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		form    url.Values
		field   string
	}{
		{"plugin", "plugin:x", url.Values{"config_key[]": {"api_token", " api_token"}, "config_value[]": {"next", ""}}, "config_key"},
		{"agones_selector", "agones", url.Values{"fleet_name": {"gs"}, "selector_key[]": {"zone", "zone"}, "selector_value[]": {"eu", "us"}}, "selector_key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := parseFleetConfigForm(tt.backend, tt.form)

			assert.Contains(t, errs, tt.field)
		})
	}
}

func TestEditFleetPage_should_not_show_the_keep_hint_without_keys(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5, Name: "p", Backend: "plugin:x", BackendConfigured: "plugin:x",
		Config: map[string]string{},
	}))
	assert.NotContains(t, html, "An empty value keeps the saved value")
}

func TestEditFleetPage_should_ask_for_the_values_again_after_a_failed_save(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5, Name: "p", Backend: "plugin:x", BackendConfigured: "plugin:x",
		Config: map[string]string{"api_token": ""}, Retry: true,
	}))
	assert.Contains(t, html, "Enter each value again")
}

func TestEditFleetPage_namespace_note(t *testing.T) {
	cases := []struct {
		name      string
		isolation bool
	}{
		{"should_show_the_note_when_isolation_is_on", true},
		{"should_not_show_the_note_when_isolation_is_off", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			html := renderToString(t, EditFleetPage(EditFleetView{
				TenantID: 1, ProjectID: 2, FleetID: 3, Name: "f", Backend: "agones",
				Config: map[string]string{"fleet_name": "gs", "namespace": "legacy"}, AgonesProjectIsolation: tc.isolation,
			}))

			assert.Equal(t, tc.isolation, strings.Contains(html, "This server uses AGONES_NAMESPACE. Leave this field empty"))
		})
	}
}
