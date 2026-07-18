//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/jobs"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/quota"
)

type storageHTTPResult struct {
	status int
	body   string
	err    error
}

func storageRequestStatus(method, target, apiKey, accessToken string, body []byte, ifMatch string) storageHTTPResult {
	return storageRequestStatusWithContentType(method, target, apiKey, accessToken, body, ifMatch, "application/json")
}

func storageRequestStatusWithContentType(method, target, apiKey, accessToken string, body []byte, ifMatch, contentType string) storageHTTPResult {
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return storageHTTPResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Session-Token", accessToken)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return storageHTTPResult{err: err}
	}
	raw, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return storageHTTPResult{status: resp.StatusCode, err: readErr}
	}
	return storageHTTPResult{status: resp.StatusCode, body: string(raw), err: closeErr}
}

func TestStoragePut_rejects_nonJSON_content_type(t *testing.T) {
	// Arrange
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-content-type")
	srv := newQuotaServerWithAllowAllLimiter(t, c)
	access := anonymousLogin(t, srv.URL, "storage-content-type")
	target := srv.URL + "/v1/storage/objects/text-plain"

	// Act
	result := storageRequestStatusWithContentType(http.MethodPut, target,
		"storage-content-type", access, []byte(`{"valid":true}`), "", "text/plain")

	// Assert
	require.NoError(t, result.err)
	assert.Equal(t, http.StatusUnsupportedMediaType, result.status, result.body)
	var count int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT count(*) FROM storage_objects
		WHERE tenant_id = $1 AND key = 'text-plain' AND deleted_at IS NULL`, tenantID).Scan(&count))
	assert.Zero(t, count)
}

func assertTenantStorageUsageMatches(t *testing.T, c *cluster, tenantID int64) int64 {
	t.Helper()
	var metered, actual int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT
			COALESCE((SELECT total_bytes FROM tenant_storage_usage WHERE tenant_id = $1), 0),
			COALESCE((SELECT sum(octet_length(value::text)) FROM storage_objects
			          WHERE tenant_id = $1 AND deleted_at IS NULL), 0)`, tenantID).Scan(&metered, &actual))
	assert.Equal(t, actual, metered)
	assert.GreaterOrEqual(t, metered, int64(0))
	return metered
}

func TestBranchFollowup_storage_counter_lifecycle_uses_canonical_bytes(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-lifecycle")
	srv := newQuotaServerWithAllowAllLimiter(t, c)
	access := anonymousLogin(t, srv.URL, "storage-lifecycle")
	target := srv.URL + "/v1/storage/objects/save"

	values := [][]byte{
		[]byte(`{"hp":100}`),
		[]byte(`{"blob":"0123456789abcdef0123456789abcdef"}`),
		[]byte(`null`),
		[]byte(`{"nested":{"emoji":"\ud83d\ude80","empty":[]},"text":"caf\u00e9"}`),
	}
	var prior int64
	for _, value := range values {
		result := storageRequestStatus(http.MethodPut, target, "storage-lifecycle", access, value, "")
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.status, result.body)
		current := assertTenantStorageUsageMatches(t, c, tenantID)
		assert.NotEqual(t, prior, current)
		prior = current
	}

	deleted := storageRequestStatus(http.MethodDelete, target, "storage-lifecycle", access, nil, "")
	require.NoError(t, deleted.err)
	require.Equal(t, http.StatusNoContent, deleted.status, deleted.body)
	assert.Zero(t, assertTenantStorageUsageMatches(t, c, tenantID))

	deleted = storageRequestStatus(http.MethodDelete, target, "storage-lifecycle", access, nil, "")
	require.NoError(t, deleted.err)
	require.Equal(t, http.StatusNoContent, deleted.status, deleted.body)
	assert.Zero(t, assertTenantStorageUsageMatches(t, c, tenantID))
}

func TestBranchFollowup_storage_failed_writes_roll_back_object_and_counter(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-rollback")
	srv := newQuotaServerWithAllowAllLimiter(t, c)
	access := anonymousLogin(t, srv.URL, "storage-rollback")
	target := srv.URL + "/v1/storage/objects/save"

	created := storageRequestStatus(http.MethodPut, target, "storage-rollback", access, []byte(`{"v":1}`), "")
	require.NoError(t, created.err)
	require.Equal(t, http.StatusOK, created.status, created.body)
	baseline := assertTenantStorageUsageMatches(t, c, tenantID)

	invalid := storageRequestStatus(http.MethodPut, target, "storage-rollback", access, []byte(`{"v":`), "")
	require.NoError(t, invalid.err)
	assert.Equal(t, http.StatusBadRequest, invalid.status, invalid.body)
	assert.Equal(t, baseline, assertTenantStorageUsageMatches(t, c, tenantID))

	oversize := append([]byte(`{"blob":"`), bytes.Repeat([]byte{'x'}, (1<<20)+1)...)
	oversize = append(oversize, []byte(`"}`)...)
	tooLarge := storageRequestStatus(http.MethodPut, srv.URL+"/v1/storage/objects/too-large",
		"storage-rollback", access, oversize, "")
	require.NoError(t, tooLarge.err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, tooLarge.status, tooLarge.body)
	assert.Equal(t, baseline, assertTenantStorageUsageMatches(t, c, tenantID))

	stale := storageRequestStatus(http.MethodPut, target, "storage-rollback", access, []byte(`{"v":2}`), "0")
	require.NoError(t, stale.err)
	assert.Equal(t, http.StatusPreconditionFailed, stale.status, stale.body)
	assert.Equal(t, baseline, assertTenantStorageUsageMatches(t, c, tenantID))

	_, err := c.bootstrapPool.Exec(context.Background(), `
		CREATE FUNCTION branch_followup_fail_storage_write() RETURNS trigger AS $$
		BEGIN
			IF NEW.key = 'db-failure' THEN
				RAISE EXCEPTION 'branch follow-up injected storage failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER branch_followup_fail_storage_write
		BEFORE INSERT OR UPDATE ON storage_objects
		FOR EACH ROW EXECUTE FUNCTION branch_followup_fail_storage_write()`)
	require.NoError(t, err)
	dbFailure := storageRequestStatus(http.MethodPut, srv.URL+"/v1/storage/objects/db-failure",
		"storage-rollback", access, []byte(`{"v":3}`), "")
	require.NoError(t, dbFailure.err)
	assert.Equal(t, http.StatusInternalServerError, dbFailure.status, dbFailure.body)
	assert.Equal(t, baseline, assertTenantStorageUsageMatches(t, c, tenantID))

	conditional := storageRequestStatus(http.MethodPut, target, "storage-rollback", access, []byte(`{"v":4}`), "1")
	require.NoError(t, conditional.err)
	assert.Equal(t, http.StatusOK, conditional.status, conditional.body)
	assertTenantStorageUsageMatches(t, c, tenantID)

	var version int64
	var value string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT version, value::text FROM storage_objects
		WHERE tenant_id = $1 AND key = 'save' AND deleted_at IS NULL`, tenantID).Scan(&version, &value))
	assert.Equal(t, int64(2), version)
	assert.JSONEq(t, `{"v":4}`, value)
}

func TestBranchFollowup_storage_boundary_recovery_isolation_and_opt_out(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-boundary")
	otherTenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-other")
	unenforcedTenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-unenforced")
	srv := newQuotaServerWithAllowAllLimiter(t, c)
	access := anonymousLogin(t, srv.URL, "storage-boundary")
	otherAccess := anonymousLogin(t, srv.URL, "storage-other")
	unenforcedAccess := anonymousLogin(t, srv.URL, "storage-unenforced")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = (id <> $2) WHERE id IN ($1, $2, $3)`,
		tenantID, unenforcedTenantID, otherTenantID)
	require.NoError(t, err)

	payload := []byte(`{"blob":"boundary"}`)
	var valueBytes int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT octet_length($1::jsonb::text)::bigint`, payload).Scan(&valueBytes))
	limit := quota.LimitsForClass(0).StorageBytes
	setUsage := func(id, total int64) {
		t.Helper()
		_, err := c.bootstrapPool.Exec(context.Background(), `
			INSERT INTO tenant_storage_usage (tenant_id, total_bytes)
			VALUES ($1, $2)
			ON CONFLICT (tenant_id) DO UPDATE SET total_bytes = EXCLUDED.total_bytes`, id, total)
		require.NoError(t, err)
	}
	usage := func(id int64) int64 {
		t.Helper()
		var total int64
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
			`SELECT total_bytes FROM tenant_storage_usage WHERE tenant_id = $1`, id).Scan(&total))
		return total
	}

	setUsage(tenantID, limit-valueBytes)
	exactTarget := srv.URL + "/v1/storage/objects/exact"
	exact := storageRequestStatus(http.MethodPut, exactTarget, "storage-boundary", access, payload, "")
	require.NoError(t, exact.err)
	require.Equal(t, http.StatusOK, exact.status, exact.body)
	assert.Equal(t, limit, usage(tenantID))

	over := storageRequestStatus(http.MethodPut, srv.URL+"/v1/storage/objects/over",
		"storage-boundary", access, []byte(`0`), "")
	require.NoError(t, over.err)
	assert.Equal(t, http.StatusForbidden, over.status, over.body)
	assert.Equal(t, limit, usage(tenantID))

	get := storageRequestStatus(http.MethodGet, exactTarget, "storage-boundary", access, nil, "")
	require.NoError(t, get.err)
	assert.Equal(t, http.StatusOK, get.status, get.body)
	list := storageRequestStatus(http.MethodGet, srv.URL+"/v1/storage/objects",
		"storage-boundary", access, nil, "")
	require.NoError(t, list.err)
	assert.Equal(t, http.StatusOK, list.status, list.body)

	setUsage(tenantID, limit+100)
	growth := storageRequestStatus(http.MethodPut, exactTarget, "storage-boundary", access,
		[]byte(`{"blob":"boundary-growth"}`), "")
	require.NoError(t, growth.err)
	assert.Equal(t, http.StatusForbidden, growth.status, growth.body)
	shrunk := storageRequestStatus(http.MethodPut, exactTarget, "storage-boundary", access, []byte(`0`), "")
	require.NoError(t, shrunk.err)
	assert.Equal(t, http.StatusOK, shrunk.status, shrunk.body)
	assert.Less(t, usage(tenantID), limit+100)
	deleted := storageRequestStatus(http.MethodDelete, exactTarget, "storage-boundary", access, nil, "")
	require.NoError(t, deleted.err)
	assert.Equal(t, http.StatusNoContent, deleted.status, deleted.body)

	other := storageRequestStatus(http.MethodPut, srv.URL+"/v1/storage/objects/other",
		"storage-other", otherAccess, payload, "")
	require.NoError(t, other.err)
	assert.Equal(t, http.StatusOK, other.status, other.body)
	assert.Equal(t, valueBytes, usage(otherTenantID))

	setUsage(unenforcedTenantID, limit)
	optOut := storageRequestStatus(http.MethodPut, srv.URL+"/v1/storage/objects/opt-out",
		"storage-unenforced", unenforcedAccess, payload, "")
	require.NoError(t, optOut.err)
	assert.Equal(t, http.StatusOK, optOut.status, optOut.body)
	assert.Equal(t, limit+valueBytes, usage(unenforcedTenantID))
}

func TestBranchFollowup_storage_quota_serializes_different_keys_tenant_wide(t *testing.T) {
	c := startCluster(t)
	tenantID, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-race-a")
	var projectB int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'storage-race-b') RETURNING id`, tenantID).Scan(&projectB))
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectB, "storage-race-b", "publishable")

	srvA := newQuotaServerWithAllowAllLimiter(t, c)
	srvB := newQuotaServerWithAllowAllLimiter(t, c)
	accessA := anonymousLogin(t, srvA.URL, "storage-race-a")
	accessB := anonymousLogin(t, srvB.URL, "storage-race-b")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true WHERE id = $1`, tenantID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(), `
		CREATE FUNCTION branch_followup_pause_storage_insert() RETURNS trigger AS $$
		BEGIN
			IF NEW.key LIKE 'race-%' THEN
				PERFORM pg_sleep(0.05);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER branch_followup_pause_storage_insert
		BEFORE INSERT ON storage_objects
		FOR EACH ROW EXECUTE FUNCTION branch_followup_pause_storage_insert()`)
	require.NoError(t, err)

	payload := []byte(`{"blob":"0123456789abcdef"}`)
	var valueBytes int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT octet_length($1::jsonb::text)::bigint`, payload).Scan(&valueBytes))
	limit := quota.LimitsForClass(0).StorageBytes

	for iteration := 0; iteration < 50; iteration++ {
		_, err = c.bootstrapPool.Exec(context.Background(), `
			INSERT INTO tenant_storage_usage (tenant_id, total_bytes)
			VALUES ($1, $2)
			ON CONFLICT (tenant_id) DO UPDATE SET total_bytes = EXCLUDED.total_bytes`,
			tenantID, limit-valueBytes)
		require.NoError(t, err)

		paths := []string{
			fmt.Sprintf("%s/v1/storage/objects/race-a-%02d", srvA.URL, iteration),
			fmt.Sprintf("%s/v1/storage/objects/race-b-%02d", srvB.URL, iteration),
		}
		apiKeys := []string{"storage-race-a", "storage-race-b"}
		accessTokens := []string{accessA, accessB}
		results := make([]storageHTTPResult, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i] = storageRequestStatus(http.MethodPut, paths[i], apiKeys[i], accessTokens[i], payload, "")
			}(i)
		}
		close(start)
		wg.Wait()

		statuses := []int{results[0].status, results[1].status}
		require.NoError(t, results[0].err)
		require.NoError(t, results[1].err)
		assert.ElementsMatch(t, []int{http.StatusOK, http.StatusForbidden}, statuses,
			"iteration %d: first=%q second=%q", iteration, results[0].body, results[1].body)

		var total int64
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
			`SELECT total_bytes FROM tenant_storage_usage WHERE tenant_id = $1`, tenantID).Scan(&total))
		assert.Equal(t, limit, total, "iteration %d", iteration)

		winner := 0
		if results[1].status == http.StatusOK {
			winner = 1
		}
		deleted := storageRequestStatus(http.MethodDelete, paths[winner], apiKeys[winner], accessTokens[winner], nil, "")
		require.NoError(t, deleted.err)
		require.Equal(t, http.StatusNoContent, deleted.status, deleted.body)
	}

	var liveObjects int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM storage_objects WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&liveObjects))
	assert.Zero(t, liveObjects)
	assert.NotEqual(t, projectA, projectB)
}

func tenantStorageWarningState(t *testing.T, c *cluster, tenantID int64) int16 {
	t.Helper()
	var threshold int16
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT last_notified_threshold FROM tenant_storage_usage WHERE tenant_id = $1`, tenantID).Scan(&threshold))
	return threshold
}

func setTenantStorageWarningUsage(t *testing.T, c *cluster, tenantID, total int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(), `
		INSERT INTO tenant_storage_usage (tenant_id, total_bytes, last_notified_threshold)
		VALUES ($1, $2, 0)
		ON CONFLICT (tenant_id) DO UPDATE SET total_bytes = EXCLUDED.total_bytes`, tenantID, total)
	require.NoError(t, err)
}

func TestBranchFollowup_storage_warning_transitions_recipients_and_deduplication(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-warning")
	otherTenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-warning-other")
	ownerID := seedControlPanelUser(t, c, "warning-owner@example.test", "correct-horse-battery-staple", false)
	adminID := seedControlPanelUser(t, c, "warning-admin@example.test", "correct-horse-battery-staple", false)
	memberID := seedControlPanelUser(t, c, "warning-member@example.test", "correct-horse-battery-staple", false)
	unverifiedID := seedControlPanelUser(t, c, "warning-unverified@example.test", "correct-horse-battery-staple", false)
	removedID := seedControlPanelUser(t, c, "warning-removed@example.test", "correct-horse-battery-staple", false)
	otherOwnerID := seedControlPanelUser(t, c, "warning-other@example.test", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	seedControlPanelMembership(t, c, memberID, tenantID, "member")
	seedControlPanelMembership(t, c, unverifiedID, tenantID, "admin")
	seedControlPanelMembership(t, c, removedID, tenantID, "admin")
	seedControlPanelMembership(t, c, otherOwnerID, otherTenantID, "owner")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE control_panel_users SET email_verified_at = NULL WHERE id = $1`, unverifiedID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`DELETE FROM control_panel_memberships WHERE tenant_id = $1 AND control_panel_user_id = $2`, tenantID, removedID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = (id = $1) WHERE id IN ($1, $2)`, tenantID, otherTenantID)
	require.NoError(t, err)

	limit := quota.LimitsForClass(0).StorageBytes
	setTenantStorageWarningUsage(t, c, tenantID, limit*79/100)
	setTenantStorageWarningUsage(t, c, otherTenantID, limit)
	recorder := &mailer.Recorder{}
	worker := jobs.NewStorageWarnWorker(db.NewPool(c.appPool), recorder, "no-reply@example.test")
	run := func() {
		t.Helper()
		require.NoError(t, worker.Work(context.Background(), nil))
	}

	run()
	assert.Empty(t, recorder.Sent)
	assert.Zero(t, tenantStorageWarningState(t, c, tenantID))
	assert.Zero(t, tenantStorageWarningState(t, c, otherTenantID))

	setTenantStorageWarningUsage(t, c, tenantID, limit*80/100)
	run()
	require.Len(t, recorder.Sent, 1)
	assert.ElementsMatch(t, []string{"warning-owner@example.test", "warning-admin@example.test"}, recorder.Sent[0].To)
	assert.Contains(t, recorder.Sent[0].Subject, "80%")
	assert.Equal(t, int16(80), tenantStorageWarningState(t, c, tenantID))
	run()
	assert.Len(t, recorder.Sent, 1)

	setTenantStorageWarningUsage(t, c, tenantID, limit)
	run()
	require.Len(t, recorder.Sent, 2)
	assert.Contains(t, recorder.Sent[1].Subject, "limit reached")
	assert.Equal(t, int16(100), tenantStorageWarningState(t, c, tenantID))

	setTenantStorageWarningUsage(t, c, tenantID, limit*90/100)
	run()
	assert.Len(t, recorder.Sent, 2)
	assert.Equal(t, int16(80), tenantStorageWarningState(t, c, tenantID))
	setTenantStorageWarningUsage(t, c, tenantID, limit*79/100)
	run()
	assert.Len(t, recorder.Sent, 2)
	assert.Zero(t, tenantStorageWarningState(t, c, tenantID))
	setTenantStorageWarningUsage(t, c, tenantID, limit*80/100)
	run()
	assert.Len(t, recorder.Sent, 3)
	assert.Equal(t, int16(80), tenantStorageWarningState(t, c, tenantID))
}

type failOnceStorageMailer struct {
	calls int
	sent  []mailer.Message
}

func (m *failOnceStorageMailer) Send(_ context.Context, message mailer.Message) error {
	m.calls++
	if m.calls == 1 {
		return errors.New("injected mail failure")
	}
	m.sent = append(m.sent, message)
	return nil
}

func TestBranchFollowup_storage_warning_failed_delivery_is_retried(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "storage-warning-retry")
	ownerID := seedControlPanelUser(t, c, "warning-retry@example.test", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET enforce_quotas = true WHERE id = $1`, tenantID)
	require.NoError(t, err)
	limit := quota.LimitsForClass(0).StorageBytes
	setTenantStorageWarningUsage(t, c, tenantID, limit*80/100)

	failing := &failOnceStorageMailer{}
	worker := jobs.NewStorageWarnWorker(db.NewPool(c.appPool), failing, "no-reply@example.test")
	require.NoError(t, worker.Work(context.Background(), nil))
	assert.Equal(t, 1, failing.calls)
	assert.Empty(t, failing.sent)
	assert.Zero(t, tenantStorageWarningState(t, c, tenantID))

	require.NoError(t, worker.Work(context.Background(), nil))
	assert.Equal(t, 2, failing.calls)
	require.Len(t, failing.sent, 1)
	assert.Equal(t, []string{"warning-retry@example.test"}, failing.sent[0].To)
	assert.Equal(t, int16(80), tenantStorageWarningState(t, c, tenantID))
}
