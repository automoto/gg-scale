package controlpanel

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFleetsListPage_renders_empty_state_when_no_fleets(t *testing.T) {
	html := renderToString(t, FleetsListPage(FleetsListView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "docker", Enabled: true,
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
		TenantID: 1, ProjectID: 2, BackendConfigured: "docker", Enabled: true,
		Fleets: []FleetRowView{
			{ID: 1, Name: "primary", Backend: "docker", BackendMatches: true, Summary: "traefik/whoami :80"},
			{ID: 2, Name: "stale", Backend: "agones", BackendMatches: false, Summary: "doomerang"},
		},
	}))
	assert.Contains(t, html, "primary")
	assert.Contains(t, html, "traefik/whoami :80")
	assert.Contains(t, html, "stale")
	assert.Contains(t, html, "not active")
}

func TestNewFleetPage_renders_docker_fields_by_default(t *testing.T) {
	html := renderToString(t, NewFleetPage(NewFleetView{
		TenantID: 1, ProjectID: 2, BackendConfigured: "docker", Backend: "docker",
	}))
	assert.Contains(t, html, `name="image"`)
	assert.Contains(t, html, `name="port"`)
	assert.Contains(t, html, `name="probe_type"`)
	assert.NotContains(t, html, `name="fleet_name"`)
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
		TenantID: 1, ProjectID: 2, Backend: "docker",
		FieldErrors: map[string]string{"image": "Image is required."},
	}))
	assert.Contains(t, html, "Image is required.")
}

func TestEditFleetPage_warns_when_backend_does_not_match_configured(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5,
		Name: "stale", Backend: "agones", BackendConfigured: "docker",
		Config: map[string]string{"fleet_name": "doomerang"},
	}))
	assert.Contains(t, html, "does not match configured backend")
	assert.Contains(t, html, "doomerang")
}

func TestEditFleetPage_renders_delete_form(t *testing.T) {
	html := renderToString(t, EditFleetPage(EditFleetView{
		TenantID: 1, ProjectID: 2, FleetID: 5, Name: "primary",
		Backend: "docker", BackendConfigured: "docker",
		Config: map[string]string{"image": "x:1", "port": "80"},
	}))
	assert.Contains(t, html, "/delete")
	assert.Contains(t, html, "Delete fleet")
}

func TestParseFleetConfigForm_docker_requires_image_and_port(t *testing.T) {
	cfg, errs := parseFleetConfigForm("docker", url.Values{})
	assert.Equal(t, "", cfg["image"])
	assert.Contains(t, errs, "image")
	assert.Contains(t, errs, "port")
}

func TestParseFleetConfigForm_docker_rejects_invalid_port(t *testing.T) {
	cases := []string{"0", "-1", "abc", "99999"}
	for _, p := range cases {
		_, errs := parseFleetConfigForm("docker", url.Values{
			"image": {"x:1"},
			"port":  {p},
		})
		assert.Contains(t, errs, "port", p)
	}
}

func TestParseFleetConfigForm_docker_passes_with_valid_inputs(t *testing.T) {
	cfg, errs := parseFleetConfigForm("docker", url.Values{
		"image":      {"traefik/whoami:latest"},
		"port":       {"80"},
		"probe_type": {"http"},
		"probe_path": {"/healthz"},
		"pull_image": {"on"},
	})
	assert.Empty(t, errs)
	assert.Equal(t, "traefik/whoami:latest", cfg["image"])
	assert.Equal(t, "80", cfg["port"])
	assert.Equal(t, "http", cfg["probe_type"])
	assert.Equal(t, "/healthz", cfg["probe_path"])
	assert.Equal(t, "true", cfg["pull_image"])
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
		"traefik/whoami:latest :80",
		summarizeFleetConfig("docker", map[string]string{"image": "traefik/whoami:latest", "port": "80"}),
	)
	assert.Equal(t,
		"doomerang",
		summarizeFleetConfig("agones", map[string]string{"fleet_name": "doomerang"}),
	)
}

func TestFleetBackendKind_buckets_plugin_variants(t *testing.T) {
	assert.Equal(t, "docker", fleetBackendKind("docker"))
	assert.Equal(t, "agones", fleetBackendKind("agones"))
	assert.Equal(t, "plugin", fleetBackendKind("plugin"))
	assert.Equal(t, "plugin", fleetBackendKind("plugin:ovh"))
	assert.Equal(t, "docker", fleetBackendKind(""))
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
		Name: "n", Backend: "docker", Config: map[string]string{"image": "x"},
		FieldErrors: map[string]string{"name": "required"},
	})
	assert.Equal(t, int64(1), got.TenantID)
	assert.Equal(t, int64(2), got.ProjectID)
	assert.Equal(t, "docker", got.Backend)
	assert.Equal(t, "x", got.Config["image"])
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
			{ID: 1, Name: "primary", Backend: "docker", BackendMatches: true},
			{ID: 2, Name: "stale", Backend: "agones", BackendMatches: false, BackendConfigured: "docker"},
		},
	}))
	assert.Contains(t, html, `name="fleet"`)
	assert.Contains(t, html, "primary")
	assert.Contains(t, html, "stale")
	assert.Contains(t, html, "backend mismatch")
}
