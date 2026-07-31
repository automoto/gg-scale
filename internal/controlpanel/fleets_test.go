package controlpanel

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
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
		"flavor=b2-7",
		summarizeFleetConfig("plugin:ovh", map[string]string{"flavor": "b2-7"}),
	)
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
