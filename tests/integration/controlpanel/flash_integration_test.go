//go:build integration

// e2e:bucket a

package controlpanel_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const flashMarker = "FLASHMARKER Your account is locked, call 555-0100"

// Every page that shows a flash message must ignore free text in the URL.
func TestFlash_should_not_show_free_text_from_the_url(t *testing.T) {
	srv, raw, userID, tenantID, projectID, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")
	resp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectID, "weekly", "desc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var playerID int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'flash-player') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	tenant := pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10)
	project := tenant + "/projects/" + strconv.FormatInt(projectID, 10)
	pages := []string{
		tenant + "/projects",
		tenant + "/api-keys",
		tenant + "/rate-limits",
		tenant + "/team",
		tenant + "/settings",
		project + "/settings",
		project + "/players",
		project + "/players/" + strconv.FormatInt(playerID, 10),
		project + "/leaderboards",
		project + "/leaderboards/" + strconv.FormatInt(boardID(t, raw, projectID, "weekly"), 10),
		pathControlPanel + "/admin/team",
		pathControlPanel + "/admin/users",
		pathControlPanel + "/admin/tenant-signups",
		pathControlPanel + "/admin/change-requests",
		pathControlPanel + "/admin/player-accounts",
	}
	query := "?" + url.Values{"flash": {flashMarker}, "created": {flashMarker}, "error": {flashMarker}}.Encode()

	for _, page := range pages {
		t.Run(page, func(t *testing.T) {
			resp, body := tfGet(t, admin, srv.URL+page+query)

			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.NotContains(t, body, "FLASHMARKER")
		})
	}
}

// The fleet and management token pages need their server switches.
func TestFlash_fleet_and_token_pages_should_not_show_free_text_from_the_url(t *testing.T) {
	p := newFleetTokenPanel(t)
	c, _, _ := p.member(t, "owner@example.com", "owner")
	project := p.srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(p.tenantID, 10) +
		"/projects/" + strconv.FormatInt(p.project, 10)
	query := "?" + url.Values{"flash": {flashMarker}}.Encode()

	for _, page := range []string{"/management-tokens", "/fleets", "/allocations"} {
		t.Run(page, func(t *testing.T) {
			resp, body := tfGet(t, c, project+page+query)

			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.NotContains(t, body, "FLASHMARKER")
		})
	}
}

func TestFlash_should_show_the_text_of_a_known_code(t *testing.T) {
	srv, raw, userID, tenantID, projectID, _ := newLeaderboardServer(t)
	admin, _ := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	_, body := tfGet(t, admin, srv.URL+leaderboardsPath(tenantID, projectID)+"?flash=leaderboard_deleted")

	assert.Contains(t, body, "Leaderboard deleted.")
}

func TestFlash_project_create_should_redirect_with_a_code_and_show_its_text(t *testing.T) {
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")
	projects := srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) + "/projects"

	resp, _ := tfPostForm(t, admin, projects, url.Values{"_csrf": {csrf}, "name": {"flash-game"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	location := resp.Header.Get("Location")
	_, body := tfGet(t, admin, srv.URL+location)

	assert.Equal(t, pathControlPanel+"/tenants/"+strconv.FormatInt(tenantID, 10)+"/projects?flash=project_created", location)
	assert.Contains(t, body, "Game Project created.")
}
