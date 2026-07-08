package controlpanel

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeSortOrder(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "desc", true},
		{"asc", "asc", true},
		{"desc", "desc", true},
		{"DESC", "desc", true},
		{"  Asc  ", "asc", true},
		{"garbage", "", false},
		{"ascending", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeSortOrder(c.in)
		assert.Equal(t, c.ok, ok, "in=%q", c.in)
		assert.Equal(t, c.want, got, "in=%q", c.in)
	}
}

func TestLeaderboardsListPage_renders_empty_state(t *testing.T) {
	html := renderToString(t, LeaderboardsListPage(LeaderboardsListView{TenantID: 1, ProjectID: 2}))
	assert.Contains(t, html, "Create your first leaderboard")
	assert.NotContains(t, html, "<table")
}

func TestLeaderboardsListPage_lists_rows_with_sort_labels(t *testing.T) {
	html := renderToString(t, LeaderboardsListPage(LeaderboardsListView{
		TenantID: 1, ProjectID: 2,
		Leaderboards: []LeaderboardRowView{
			{ID: 10, Name: "weekly-high", SortOrder: "desc", CreatedAt: time.Date(2026, 6, 22, 14, 30, 0, 0, time.UTC)},
			{ID: 11, Name: "fastest-lap", SortOrder: "asc"},
		},
	}))
	assert.Contains(t, html, "weekly-high")
	assert.Contains(t, html, "fastest-lap")
	assert.Contains(t, html, "High score first")
	assert.Contains(t, html, "Low score first")
	// Edit link points at the row id.
	assert.Contains(t, html, "/projects/2/leaderboards/10")
	// Timestamp uses the human-readable format.
	assert.Contains(t, html, "14:30 2026/06/22")
}

func TestNewLeaderboardPage_renders_form_with_sort_options(t *testing.T) {
	html := renderToString(t, NewLeaderboardPage(LeaderboardFormView{TenantID: 1, ProjectID: 2, SortOrder: "desc"}))
	assert.Contains(t, html, `name="name"`)
	assert.Contains(t, html, `name="sort_order"`)
	assert.Contains(t, html, `value="asc"`)
	assert.Contains(t, html, `value="desc"`)
}

func TestNewLeaderboardPage_shows_field_errors(t *testing.T) {
	html := renderToString(t, NewLeaderboardPage(LeaderboardFormView{
		TenantID: 1, ProjectID: 2,
		FieldErrors: map[string]string{"name": "Name is required."},
	}))
	assert.Contains(t, html, "Name is required.")
}

func TestEditLeaderboardPage_prefills_and_has_delete_form(t *testing.T) {
	html := renderToString(t, EditLeaderboardPage(LeaderboardFormView{
		TenantID: 1, ProjectID: 2, LeaderboardID: 7, Name: "weekly-high", SortOrder: "asc",
	}))
	assert.Contains(t, html, `value="weekly-high"`)
	// asc option is preselected.
	assert.Contains(t, html, `value="asc" selected`)
	// Delete form posts to the delete route.
	assert.Contains(t, html, "/projects/2/leaderboards/7/delete")
}

func TestProjectsPage_shows_leaderboards_link_even_without_fleet(t *testing.T) {
	html := renderToString(t, ProjectsPage(ProjectsView{
		TenantID:     1,
		FleetEnabled: false,
		Projects:     []ProjectOption{{ID: 5, Name: "arcade"}},
	}))
	assert.Contains(t, html, "/projects/5/leaderboards")
	// Proves the link is not gated behind the fleet feature.
	assert.NotContains(t, html, "/projects/5/fleets")
}
