//go:build integration

package managementapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectAPI_create_leaderboard_at_limit_should_answer_409(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit") VALUES ($1, 'leaderboards', 2)`, f.tenantA)
	tok := f.scopedToken("leaderboards:write")
	for i := range 2 {
		got := f.do(tok, http.MethodPost, "/v1/project/leaderboards", fmt.Sprintf(`{"name":"b-%d"}`, i))
		require.Equal(t, http.StatusOK, got.status, got.raw)
	}

	// Act
	got := f.do(tok, http.MethodPost, "/v1/project/leaderboards", `{"name":"one-more"}`)

	// Assert
	assert.Equal(t, []any{http.StatusConflict, true},
		[]any{got.status, strings.Contains(got.detail(), "maximum number of leaderboards")}, got.raw)
}

func TestProjectAPI_list_leaderboards_should_report_truncated_over_50(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO leaderboards (tenant_id, project_id, name)
		SELECT $1, $2, 'b-' || g FROM generate_series(1, 60) g`, f.tenantA, f.projectA)

	// Act
	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/leaderboards", "")

	// Assert
	assert.Equal(t, []any{http.StatusOK, true, 50},
		[]any{got.status, got.body["truncated"], len(got.body["leaderboards"].([]any))}, got.raw)
}
