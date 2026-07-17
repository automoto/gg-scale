//go:build integration

package controlpanel_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/rbac"
)

func leaderboardsPath(tenantID, projectID int64) string {
	return pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/leaderboards"
}

func createPlatformAdminUser(t *testing.T, raw *pgxpool.Pool, email string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(tfTestPassword), bcrypt.MinCost)
	require.NoError(t, err)
	var id int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`INSERT INTO control_panel_users (email, password_hash, is_platform_admin, email_verified_at)
		 VALUES ($1, $2, true, now()) RETURNING id`, email, hash).Scan(&id))
	_, err = raw.Exec(context.Background(),
		`INSERT INTO casbin_rule (ptype, v0, v1, v2)
		 VALUES ('g', 'control_panel:user:' || $1::bigint, 'role:platform_admin', '*')
		 ON CONFLICT DO NOTHING`, id)
	require.NoError(t, err)
	return id
}

func createLeaderboardProject(t *testing.T, raw *pgxpool.Pool, tenantID int64, name string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, $2) RETURNING id`, tenantID, name).Scan(&id))
	return id
}

func boardID(t *testing.T, raw *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`SELECT id FROM leaderboards WHERE project_id = $1 AND name = $2 AND deleted_at IS NULL`,
		projectID, name).Scan(&id))
	return id
}

// newLeaderboardServer brings up a migrated Postgres seeded with one
// platform-admin control panel user and two projects under the same tenant,
// then mounts the real control panel router in front of it. Platform admins
// get tenant-scope access from their "*"-domain casbin grouping row, so no
// membership rows need seeding.
func newLeaderboardServer(t *testing.T) (srv *httptest.Server, raw *pgxpool.Pool, userID, tenantID, projectA, projectB int64) {
	t.Helper()
	pool, raw := startTwoFactorDB(t)
	ctx := context.Background()

	userID = createPlatformAdminUser(t, raw, "lb-admin@example.com")
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('lb-test') RETURNING id`).Scan(&tenantID))
	projectA = createLeaderboardProject(t, raw, tenantID, "p1")
	projectB = createLeaderboardProject(t, raw, tenantID, "p2")

	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)

	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{
		Pool: pool, Config: controlpanel.Config{Mount: true}, Mailer: noopMailer, RBAC: authorizer,
		VerifySigningKey: []byte(testEmailVerifySigningKey),
	}))
	srv = httptest.NewServer(root)
	t.Cleanup(srv.Close)
	return srv, raw, userID, tenantID, projectA, projectB
}

func loginAsAdmin(t *testing.T, srv *httptest.Server, raw *pgxpool.Pool, userID int64, email string) (*http.Client, string) {
	t.Helper()
	c := newBrowser(t)
	resp, _ := tfPostForm(t, c, srv.URL+tfLoginPath, loginForm(email))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
	require.NotNil(t, jarCookie(t, c, srv.URL, sessionCookieName))
	return c, latestCSRF(t, raw, userID)
}

func tfGet(t *testing.T, c *http.Client, rawURL string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(rawURL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(body)
}

func createBoardViaHTTP(t *testing.T, c *http.Client, csrf, base string, tenantID, projectID int64, name, sortOrder string) (*http.Response, string) {
	t.Helper()
	return tfPostForm(t, c, base+leaderboardsPath(tenantID, projectID), url.Values{
		"_csrf": {csrf}, "name": {name}, "sort_order": {sortOrder},
	})
}

// sortBadgeForRow extracts the rendered sort-order badge text ("High score
// first" / "Low score first") for the list-page row matching name.
func sortBadgeForRow(t *testing.T, body, name string) string {
	t.Helper()
	re := regexp.MustCompile(`<td>` + regexp.QuoteMeta(name) + `</td>\s*<td>\s*<span class="badge[^"]*">([^<]+)</span>`)
	m := re.FindStringSubmatch(body)
	require.Len(t, m, 2, "row for %q not found in list page:\n%s", name, body)
	return m[1]
}

func TestLeaderboards_create_list_and_sort_order_roundtrip(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "weekly", "desc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, _ = createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "fastest-lap", "asc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	listResp, body := tfGet(t, admin, srv.URL+leaderboardsPath(tenantID, projectA))
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	assert.Equal(t, "High score first", sortBadgeForRow(t, body, "weekly"))
	assert.Equal(t, "Low score first", sortBadgeForRow(t, body, "fastest-lap"))
}

func TestLeaderboards_get_is_project_scoped(t *testing.T) {
	srv, raw, userID, tenantID, projectA, projectB := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "board", "desc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	id := boardID(t, raw, projectA, "board")

	sameProjectResp, _ := tfGet(t, admin, srv.URL+leaderboardsPath(tenantID, projectA)+"/"+strconv.FormatInt(id, 10))
	assert.Equal(t, http.StatusOK, sameProjectResp.StatusCode)

	// The same id under the sibling project must not resolve.
	crossProjectResp, _ := tfGet(t, admin, srv.URL+leaderboardsPath(tenantID, projectB)+"/"+strconv.FormatInt(id, 10))
	assert.Equal(t, http.StatusNotFound, crossProjectResp.StatusCode)
}

func TestLeaderboards_update_changes_name_and_sort_order(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "board", "desc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	id := boardID(t, raw, projectA, "board")

	updateResp, _ := tfPostForm(t, admin, srv.URL+leaderboardsPath(tenantID, projectA)+"/"+strconv.FormatInt(id, 10),
		url.Values{"_csrf": {csrf}, "name": {"renamed"}, "sort_order": {"asc"}})
	require.Equal(t, http.StatusSeeOther, updateResp.StatusCode)

	editResp, editBody := tfGet(t, admin, srv.URL+leaderboardsPath(tenantID, projectA)+"/"+strconv.FormatInt(id, 10))
	require.Equal(t, http.StatusOK, editResp.StatusCode)
	assert.Contains(t, editBody, `<input name="name" value="renamed"`)
	assert.Contains(t, editBody, `<option value="asc" selected>`)

	_, listBody := tfGet(t, admin, srv.URL+leaderboardsPath(tenantID, projectA))
	assert.Equal(t, "Low score first", sortBadgeForRow(t, listBody, "renamed"))
}

func TestLeaderboards_update_and_delete_of_soft_deleted_id_returns_not_found(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "board", "desc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	id := boardID(t, raw, projectA, "board")
	boardPath := leaderboardsPath(tenantID, projectA) + "/" + strconv.FormatInt(id, 10)

	deleteResp, _ := tfPostForm(t, admin, srv.URL+boardPath+"/delete", url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusSeeOther, deleteResp.StatusCode)

	updateResp, _ := tfPostForm(t, admin, srv.URL+boardPath,
		url.Values{"_csrf": {csrf}, "name": {"renamed"}, "sort_order": {"desc"}})
	assert.Equal(t, http.StatusNotFound, updateResp.StatusCode, "updating a soft-deleted leaderboard must 404")

	redeleteResp, _ := tfPostForm(t, admin, srv.URL+boardPath+"/delete", url.Values{"_csrf": {csrf}})
	assert.Equal(t, http.StatusNotFound, redeleteResp.StatusCode, "deleting an already-deleted leaderboard must 404")
}

func TestLeaderboards_soft_delete_hides_and_frees_name(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "board", "desc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	id := boardID(t, raw, projectA, "board")

	deleteResp, _ := tfPostForm(t, admin, srv.URL+leaderboardsPath(tenantID, projectA)+"/"+strconv.FormatInt(id, 10)+"/delete",
		url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusSeeOther, deleteResp.StatusCode)

	_, listBody := tfGet(t, admin, srv.URL+leaderboardsPath(tenantID, projectA))
	assert.NotContains(t, listBody, "<td>board</td>")

	// The partial unique index only covers live rows, so the name is reusable.
	recreateResp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "board", "desc")
	assert.Equal(t, http.StatusSeeOther, recreateResp.StatusCode)
}

func TestLeaderboards_duplicate_name_on_create_and_rename(t *testing.T) {
	srv, raw, userID, tenantID, projectA, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "board", "desc")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	dupResp, dupBody := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "board", "desc")
	assert.Equal(t, http.StatusConflict, dupResp.StatusCode, "duplicate create should 409")
	assert.Contains(t, dupBody, "already exists")

	otherResp, _ := createBoardViaHTTP(t, admin, csrf, srv.URL, tenantID, projectA, "other", "desc")
	require.Equal(t, http.StatusSeeOther, otherResp.StatusCode)
	otherID := boardID(t, raw, projectA, "other")

	renameResp, renameBody := tfPostForm(t, admin, srv.URL+leaderboardsPath(tenantID, projectA)+"/"+strconv.FormatInt(otherID, 10),
		url.Values{"_csrf": {csrf}, "name": {"board"}, "sort_order": {"desc"}})
	assert.Equal(t, http.StatusConflict, renameResp.StatusCode, "rename collision should 409")
	assert.Contains(t, renameBody, "already exists")
}
