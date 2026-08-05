package controlpanel

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseLeaderboardForm_defaults_for_minimal_form(t *testing.T) {
	fields, errs := parseLeaderboardForm(url.Values{"name": {"weekly"}}, false)
	assert.Empty(t, errs)
	assert.Equal(t, "weekly", fields.Name)
	assert.Equal(t, "desc", fields.SortOrder)
	assert.Equal(t, "best", fields.ScoreOperator)
	assert.Equal(t, "none", fields.ResetSchedule)
	assert.False(t, fields.ClientSubmissions)
	assert.Nil(t, fields.ScoreMin)
	assert.Nil(t, fields.ScoreMax)
	assert.Nil(t, fields.AttemptCap)
	assert.Nil(t, fields.Metadata)
}

func TestParseLeaderboardForm_full_valid_form(t *testing.T) {
	fields, errs := parseLeaderboardForm(url.Values{
		"name":               {"arcade"},
		"sort_order":         {"asc"},
		"score_operator":     {"incr"},
		"client_submissions": {"on"},
		"score_min":          {"0"},
		"score_max":          {"100000"},
		"reset_schedule":     {"weekly"},
		"attempt_cap":        {"3"},
		"metadata":           {`{"icon": "gold"}`},
	}, false)
	assert.Empty(t, errs)
	assert.Equal(t, "incr", fields.ScoreOperator)
	assert.True(t, fields.ClientSubmissions)
	assert.Equal(t, int64(0), *fields.ScoreMin)
	assert.Equal(t, int64(100000), *fields.ScoreMax)
	assert.Equal(t, "weekly", fields.ResetSchedule)
	assert.Equal(t, int32(3), *fields.AttemptCap)
	assert.JSONEq(t, `{"icon":"gold"}`, string(fields.Metadata))
}

func TestParseLeaderboardForm_field_errors(t *testing.T) {
	cases := []struct {
		name  string
		form  url.Values
		field string
	}{
		{"missing_name", url.Values{}, "name"},
		{"bad_operator", url.Values{"name": {"b"}, "score_operator": {"max"}}, "score_operator"},
		{"bad_schedule", url.Values{"name": {"b"}, "reset_schedule": {"hourly"}}, "reset_schedule"},
		{"min_not_a_number", url.Values{"name": {"b"}, "score_min": {"low"}}, "score_min"},
		{"max_not_a_number", url.Values{"name": {"b"}, "score_max": {"high"}}, "score_max"},
		{"min_above_max", url.Values{"name": {"b"}, "score_min": {"10"}, "score_max": {"5"}}, "score_min"},
		{"cap_zero", url.Values{"name": {"b"}, "attempt_cap": {"0"}}, "attempt_cap"},
		{"cap_negative", url.Values{"name": {"b"}, "attempt_cap": {"-2"}}, "attempt_cap"},
		{"cap_not_a_number", url.Values{"name": {"b"}, "attempt_cap": {"lots"}}, "attempt_cap"},
		{"metadata_not_object", url.Values{"name": {"b"}, "metadata": {`[1,2]`}}, "metadata"},
		{"metadata_invalid_json", url.Values{"name": {"b"}, "metadata": {`{"a":`}}, "metadata"},
		{"metadata_oversized", url.Values{"name": {"b"}, "metadata": {`{"pad":"` + strings.Repeat("x", 17<<10) + `"}`}}, "metadata"},
		// PostgreSQL cannot store NUL in text, and the duplicate-name
		// translator does not match that error, so an unscreened name renders
		// a 500 instead of a field error.
		{"name_with_nul", url.Values{"name": {"we\x00ekly"}}, "name"},
		{"name_with_control_char", url.Values{"name": {"we\x01ekly"}}, "name"},
		{"name_invalid_utf8", url.Values{"name": {"weekly\xff"}}, "name"},
		{"name_overlong", url.Values{"name": {strings.Repeat("x", leaderboardNameMax+1)}}, "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errs := parseLeaderboardForm(c.form, false)
			assert.Contains(t, errs, c.field)
		})
	}
}

func TestParseLeaderboardForm_edit_ignores_submitted_operator(t *testing.T) {
	fields, errs := parseLeaderboardForm(url.Values{
		"name": {"b"}, "score_operator": {"incr"},
	}, true)
	assert.Empty(t, errs)
	assert.Empty(t, fields.ScoreOperator, "the operator is fixed at creation; edits never carry one")
}

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

func TestNewLeaderboardPage_renders_feature_fields(t *testing.T) {
	html := renderToString(t, NewLeaderboardPage(LeaderboardFormView{
		TenantID: 1, ProjectID: 2, SortOrder: "desc", ScoreOperator: "best", ResetSchedule: "none",
	}))
	assert.Contains(t, html, `name="score_operator"`)
	for _, op := range []string{"best", "set", "incr"} {
		assert.Contains(t, html, `value="`+op+`"`)
	}
	assert.Contains(t, html, `name="client_submissions"`)
	assert.Contains(t, html, `name="score_min"`)
	assert.Contains(t, html, `name="score_max"`)
	assert.Contains(t, html, `name="reset_schedule"`)
	for _, s := range []string{"none", "daily", "weekly", "monthly"} {
		assert.Contains(t, html, `value="`+s+`"`)
	}
	assert.Contains(t, html, `name="attempt_cap"`)
	assert.Contains(t, html, `name="metadata"`)
}

func TestEditLeaderboardPage_operator_is_read_only_and_fields_prefill(t *testing.T) {
	html := renderToString(t, EditLeaderboardPage(LeaderboardFormView{
		TenantID: 1, ProjectID: 2, LeaderboardID: 7, Name: "arcade", SortOrder: "asc",
		ScoreOperator: "incr", ClientSubmissions: true,
		ScoreMin: "0", ScoreMax: "100000", ResetSchedule: "weekly", AttemptCap: "3",
		Metadata: `{"icon":"gold"}`, CurrentPeriod: 2,
	}))
	assert.NotContains(t, html, `<select name="score_operator"`,
		"the operator must not be editable after creation")
	assert.Contains(t, html, "<strong>incr</strong>")
	assert.Contains(t, html, `name="client_submissions" checked`)
	assert.Contains(t, html, `name="score_min" value="0"`)
	assert.Contains(t, html, `name="score_max" value="100000"`)
	assert.Contains(t, html, `<option value="weekly" selected>`)
	assert.Contains(t, html, `name="attempt_cap" value="3"`)
	// The textarea HTML-escapes the JSON quotes.
	assert.Contains(t, html, `{&#34;icon&#34;:&#34;gold&#34;}`)
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
