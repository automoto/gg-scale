//go:build integration

// e2e:bucket a

package controlpanel_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPageHeader_should_show_names_on_project_settings(t *testing.T) {
	// Arrange
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, _ := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	// Act
	_, body := tfGet(t, admin, srv.URL+projectSettingsPath(tenantID, projectA))

	// Assert
	tenantLink := "/tenants/" + strconv.FormatInt(tenantID, 10) + `/projects">lb-test</a>`
	projectLink := "/projects/" + strconv.FormatInt(projectA, 10) + `/settings">p1</a>`
	assert.Contains(t, body, tenantLink)
	assert.Contains(t, body, projectLink)
	assert.NotContains(t, body, "Game Project #")
	assert.NotContains(t, body, `class="eyebrow"`)
}

func TestPageHeader_should_show_the_tenant_name_on_tenant_settings(t *testing.T) {
	// Arrange
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	admin, _ := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	// Act
	_, body := tfGet(t, admin, srv.URL+pathControlPanel+"/tenants/"+strconv.FormatInt(tenantID, 10)+"/settings")

	// Assert
	assert.Contains(t, body, "/tenants/"+strconv.FormatInt(tenantID, 10)+`/projects">lb-test</a>`)
	assert.NotContains(t, body, "Account Tenant #")
}

func TestPageHeader_should_not_show_a_project_name_from_another_tenant(t *testing.T) {
	// Arrange
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	admin, _ := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")
	var otherTenant int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`INSERT INTO tenants (name) VALUES ('other-tenant') RETURNING id`).Scan(&otherTenant))
	otherProject := createLeaderboardProject(t, raw, otherTenant, "other-secret")

	// Act
	resp, body := tfGet(t, admin, srv.URL+pathControlPanel+"/tenants/"+strconv.FormatInt(tenantID, 10)+"/projects/"+strconv.FormatInt(otherProject, 10)+"/players")

	// Assert
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, body, "other-secret")
}

func TestPageHeader_should_escape_the_names(t *testing.T) {
	// Arrange
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, _ := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")
	_, err := raw.Exec(context.Background(), `UPDATE projects SET name = '<b>x</b>' WHERE id = $1`, projectA)
	require.NoError(t, err)
	_, err = raw.Exec(context.Background(), `UPDATE tenants SET name = '<i>t</i>' WHERE id = $1`, tenantID)
	require.NoError(t, err)

	// Act
	_, body := tfGet(t, admin, srv.URL+projectSettingsPath(tenantID, projectA))

	// Assert
	assert.Contains(t, body, "/tenants/"+strconv.FormatInt(tenantID, 10)+`/projects">&lt;i&gt;t&lt;/i&gt;</a>`)
	assert.Contains(t, body, "/projects/"+strconv.FormatInt(projectA, 10)+`/settings">&lt;b&gt;x&lt;/b&gt;</a>`)
	assert.NotContains(t, body, "<b>x</b>")
	assert.NotContains(t, body, "<i>t</i>")
}
