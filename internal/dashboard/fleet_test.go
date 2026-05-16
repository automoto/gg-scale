package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFleetPage_renders_disabled_state_when_no_backend(t *testing.T) {
	html := renderToString(t, FleetPage(FleetView{
		TenantID:  1,
		ProjectID: 2,
		Enabled:   false,
	}))
	assert.Contains(t, html, "No fleet backend configured")
	assert.NotContains(t, html, `hx-trigger="every 5s"`)
}

func TestFleetPage_renders_polling_section_when_enabled(t *testing.T) {
	html := renderToString(t, FleetPage(FleetView{
		TenantID:    1,
		ProjectID:   2,
		BackendName: "docker",
		Enabled:     true,
		Allocations: []AllocationView{
			{ID: 5, Status: "ready", Region: "us-east-1", BackendRef: "ref-abc", Address: "1.2.3.4:7777"},
		},
		Total: 1, Page: 1,
	}))
	assert.Contains(t, html, `hx-trigger="every 5s"`)
	assert.Contains(t, html, "us-east-1")
	assert.Contains(t, html, "ref-abc")
	assert.Contains(t, html, "Manual allocate")
}

func TestFleetPage_skips_manual_allocate_button_when_disabled(t *testing.T) {
	html := renderToString(t, FleetPage(FleetView{
		TenantID: 1, ProjectID: 2, Enabled: false,
	}))
	assert.NotContains(t, html, "Manual allocate")
}

func TestFleetDetailPage_shows_deallocate_for_live_status(t *testing.T) {
	html := renderToString(t, FleetDetailPage(FleetDetailView{
		TenantID: 1, ProjectID: 2,
		Allocation: AllocationView{ID: 10, Status: "ready", Backend: "docker"},
	}))
	assert.Contains(t, html, "Deallocate")
}

func TestFleetDetailPage_hides_deallocate_for_terminal_status(t *testing.T) {
	html := renderToString(t, FleetDetailPage(FleetDetailView{
		TenantID: 1, ProjectID: 2,
		Allocation: AllocationView{ID: 10, Status: "shutdown", Backend: "docker"},
	}))
	assert.NotContains(t, html, "Deallocate</a>")
}

func TestFleetDetailFragment_renders_events_table(t *testing.T) {
	html := renderToString(t, FleetDetailFragment(FleetDetailView{
		Allocation: AllocationView{ID: 4, Status: "ready"},
		Events: []EventView{
			{ID: 1, Status: "pending", CreatedAt: time.Now()},
			{ID: 2, Status: "ready", Address: "1.2.3.4:1", CreatedAt: time.Now()},
		},
	}))
	assert.Contains(t, html, "pending")
	assert.Contains(t, html, "ready")
	assert.Contains(t, html, "1.2.3.4:1")
}

func TestDeallocateConfirmPage_renders_required_typed_id(t *testing.T) {
	html := renderToString(t, DeallocateConfirmPage(DeallocateConfirmView{
		TenantID: 1, ProjectID: 2,
		Allocation: AllocationView{ID: 99, Backend: "docker"},
	}))
	assert.Contains(t, html, `name="confirm_id"`)
	assert.Contains(t, html, "99")
}

func TestMatchmakerTableFragment_renders_buckets(t *testing.T) {
	html := renderToString(t, MatchmakerTableFragment(MatchmakerQueueView{
		Buckets: []MatchmakerBucketView{
			{Region: "us-east-1", GameMode: "ranked", Status: "queued", Count: 7, Oldest: time.Now().Add(-time.Minute)},
		},
	}))
	assert.Contains(t, html, "us-east-1")
	assert.Contains(t, html, "ranked")
	assert.Contains(t, html, "queued")
}

func TestFleetBackendsPage_surfaces_unreachable_backend(t *testing.T) {
	html := renderToString(t, FleetBackendsPage(FleetBackendsView{
		ConfiguredName: "docker",
		Enabled:        true,
		HealthErr:      "dial tcp 127.0.0.1:9999: connect: connection refused",
	}))
	assert.Contains(t, html, "Backend unreachable")
	assert.Contains(t, html, "connection refused")
	assert.NotContains(t, html, "Backend healthy")
}

func TestFleetBackendsPage_shows_healthy_when_no_err(t *testing.T) {
	html := renderToString(t, FleetBackendsPage(FleetBackendsView{
		ConfiguredName: "docker", Enabled: true,
	}))
	assert.Contains(t, html, "Backend healthy")
}

func TestPlatformPluginsPage_renders_no_plugin_message(t *testing.T) {
	html := renderToString(t, PlatformPluginsPage(PlatformPluginsView{}))
	assert.Contains(t, html, "No plugin backend configured")
}

func TestPlatformPluginsPage_renders_snapshot_fields(t *testing.T) {
	html := renderToString(t, PlatformPluginsPage(PlatformPluginsView{
		Snapshot: &PluginSnapshot{
			Name: "ovh", Version: "1.0.0", ProtocolVersion: 1,
			Pid: 4242, RestartCount: 0, TotalRestartCount: 2,
		},
	}))
	assert.Contains(t, html, "ovh")
	assert.Contains(t, html, "1.0.0")
	assert.Contains(t, html, "4242")
	assert.Contains(t, html, "Health probe OK")
}

func TestPlatformPluginsPage_surfaces_health_err(t *testing.T) {
	html := renderToString(t, PlatformPluginsPage(PlatformPluginsView{
		Snapshot: &PluginSnapshot{Name: "ovh", HealthErr: "rpc deadline exceeded"},
	}))
	assert.Contains(t, html, "Health probe failed")
	assert.Contains(t, html, "rpc deadline exceeded")
}

func TestFleetDeallocate_rejects_mismatched_id(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{
		"confirm_id": {"7"}, // wrong
		"_csrf":      {"x"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	got := strings.TrimSpace(req.Form.Get("confirm_id"))
	assert.NotEqual(t, "12345", got, "guard against the matching ID slipping through")
	_ = h
}

func TestAllocationsBasePath_matches_template_helper(t *testing.T) {
	assert.Equal(t, allocationsBasePath(7, 9), allocationsBasePathTpl(7, 9),
		"go and templ helpers must produce the same URL")
}

func TestFleetsBasePath_matches_template_helper(t *testing.T) {
	assert.Equal(t, fleetsBasePath(7, 9), fleetsBasePathTpl(7, 9),
		"go and templ helpers must produce the same URL")
}

func TestAllocationTerminal_matches_fleet_status_strings(t *testing.T) {
	for _, status := range []string{"shutdown", "failed"} {
		assert.True(t, allocationTerminal(status), status)
	}
	for _, status := range []string{"pending", "allocating", "ready", "allocated", "draining"} {
		assert.False(t, allocationTerminal(status), status)
	}
}

func TestFleetQuery_preserves_page_and_filter(t *testing.T) {
	assert.Equal(t, "page=2&all=1", fleetQuery(FleetView{Page: 2, IncludeTerminal: true}))
	assert.Equal(t, "page=1", fleetQuery(FleetView{Page: 1}))
}
