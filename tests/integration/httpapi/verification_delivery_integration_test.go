//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/players"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/verifycode"
)

var errSMTPRejected = errors.New("smtp rejected MAIL FROM")

type switchableMailer struct {
	mu   sync.Mutex
	err  error
	sent []mailer.Message
}

func (m *switchableMailer) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *switchableMailer) reject() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = errSMTPRejected
}

func (m *switchableMailer) accept() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = nil
}

func (m *switchableMailer) messages() []mailer.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mailer.Message(nil), m.sent...)
}

func newVerificationDeliveryServer(t *testing.T, c *cluster, bootstrap *controlpanel.Bootstrap, testMailer mailer.Mailer) (*httptest.Server, *prometheus.Registry) {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	router := httpapi.NewRouter(httpapi.Deps{
		Version:               "v1",
		Commit:                "test",
		Pool:                  pool,
		Lookup:                tenant.NewSQLLookup(c.appPool),
		Limiter:               ratelimit.NewCacheLimiter(c.cache),
		Signer:                signer,
		Cache:                 c.cache,
		Mailer:                mailer.Metered(testMailer, metrics),
		MailFrom:              "no-reply@example.test",
		EmailVerifySigningKey: []byte(testEmailVerifySigningKey),
		Registry:              reg,
		Metrics:               metrics,
		RBAC:                  authorizer,
		ControlPanel: controlpanel.Config{
			Mount:    true,
			MailFrom: "no-reply@example.test",
		},
		ControlPanelBootstrap: bootstrap,
		Players:               players.Config{Mount: true},
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, reg
}

func TestControlPanelSetup_SMTPRejectionReturnsRetryable503(t *testing.T) {
	c := startCluster(t)
	testMailer := &switchableMailer{err: errSMTPRejected}
	srv, _ := newVerificationDeliveryServer(t, c, controlpanel.NewBootstrap("setup-token", "/tmp/bootstrap-token"), testMailer)
	form := url.Values{
		"bootstrap_token": {"setup-token"},
		"email":           {"owner@example.test"},
		"password":        {"correct-horse-battery-staple"},
	}

	resp, err := noRedirectClient().Post(srv.URL+"/v1/control-panel/setup", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotContains(t, string(body), "owner@example.test")
	assert.NotContains(t, string(body), errSMTPRejected.Error())
	assert.Empty(t, resp.Header.Get("Location"))

	var codeHash []byte
	var lastSent *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT email_verification_code_hash, email_verification_last_sent_at
		FROM control_panel_users WHERE email = $1`, "owner@example.test").Scan(&codeHash, &lastSent))
	assert.Empty(t, codeHash)
	assert.Nil(t, lastSent)
}

func TestControlPanelLogin_SMTPRejectionRestoresPreviousCodeAndAllowsImmediateRetry(t *testing.T) {
	c := startCluster(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse-battery-staple"), 12)
	require.NoError(t, err)
	oldSalt := []byte("old-salt")
	oldHash := verifycode.Hash(oldSalt, "123456")
	oldSent := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Microsecond)
	_, err = c.bootstrapPool.Exec(context.Background(), `
		INSERT INTO control_panel_users (
			email, password_hash, email_verification_code_hash,
			email_verification_salt, email_verification_expires_at,
			email_verification_attempts, email_verification_last_sent_at
		) VALUES ($1, $2, $3, $4, $5, 2, $6)`,
		"owner@example.test", hash, oldHash, oldSalt, time.Now().Add(10*time.Minute), oldSent)
	require.NoError(t, err)

	testMailer := &switchableMailer{err: errSMTPRejected}
	srv, _ := newVerificationDeliveryServer(t, c, controlpanel.DisabledBootstrap(), testMailer)

	for range 2 {
		resp := controlPanelLogin(t, srv.URL, "owner@example.test", "correct-horse-battery-staple")
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, readErr)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
		assert.NotContains(t, string(body), errSMTPRejected.Error())
	}

	var gotHash []byte
	var gotAttempts int
	var gotSent time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT email_verification_code_hash, email_verification_attempts,
		       email_verification_last_sent_at
		FROM control_panel_users WHERE email = $1`, "owner@example.test").Scan(&gotHash, &gotAttempts, &gotSent))
	assert.Equal(t, oldHash, gotHash)
	assert.Equal(t, 2, gotAttempts)
	assert.WithinDuration(t, oldSent, gotSent, time.Millisecond)
}

func TestControlPanelResend_SMTPRejectionRestoresPreviousCode(t *testing.T) {
	c := startCluster(t)
	userID := seedControlPanelUser(t, c, "owner@example.test", "correct-horse-battery-staple", false)
	_, err := c.bootstrapPool.Exec(context.Background(), `UPDATE control_panel_users SET email_verified_at = NULL WHERE id = $1`, userID)
	require.NoError(t, err)

	testMailer := &switchableMailer{}
	srv, _ := newVerificationDeliveryServer(t, c, controlpanel.DisabledBootstrap(), testMailer)
	loginResp := controlPanelLogin(t, srv.URL, "owner@example.test", "correct-horse-battery-staple")
	require.Equal(t, http.StatusSeeOther, loginResp.StatusCode)
	verifyCookie := loginResp.Cookies()[0]
	loginResp.Body.Close()

	var oldHash []byte
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		UPDATE control_panel_users SET email_verification_last_sent_at = now() - interval '2 minutes'
		WHERE id = $1 RETURNING email_verification_code_hash`, userID).Scan(&oldHash))
	testMailer.reject()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control-panel/verify/resend", strings.NewReader(""))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(verifyCookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Location"))
	var gotHash []byte
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT email_verification_code_hash FROM control_panel_users WHERE id = $1`, userID).Scan(&gotHash))
	assert.Equal(t, oldHash, gotHash)
}

func TestPlayerSignup_SMTPRejectionIsUniformAndClearsDeliveryReservation(t *testing.T) {
	c := startCluster(t)
	testMailer := &switchableMailer{err: errSMTPRejected}
	srv, reg := newVerificationDeliveryServer(t, c, controlpanel.DisabledBootstrap(), testMailer)

	freshStatus, freshLocation := accountSignup(t, srv.URL, "player@example.test", "hunter2hunter2")
	duplicateStatus, duplicateLocation := accountSignup(t, srv.URL, "player@example.test", "hunter2hunter2")

	assert.Equal(t, http.StatusServiceUnavailable, freshStatus)
	assert.Equal(t, freshStatus, duplicateStatus)
	assert.Equal(t, freshLocation, duplicateLocation)
	assert.Empty(t, freshLocation)

	var codeHash []byte
	var lastSent *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT email_verification_code_hash, email_verification_last_sent_at
		FROM player_accounts WHERE email = $1`, "player@example.test").Scan(&codeHash, &lastSent))
	assert.Empty(t, codeHash)
	assert.Nil(t, lastSent)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`# HELP ggscale_mail_sends_total Transactional mail sends by result.
# TYPE ggscale_mail_sends_total counter
ggscale_mail_sends_total{result="error"} 2
`), "ggscale_mail_sends_total"))

	testMailer.accept()
	retryStatus, retryLocation := accountSignup(t, srv.URL, "player@example.test", "hunter2hunter2")
	require.Equal(t, http.StatusSeeOther, retryStatus)
	assert.Equal(t, "/v1/players/account/verify", retryLocation)
	require.Len(t, testMailer.messages(), 1)
	assert.Equal(t, "Your ggscale verification code", testMailer.messages()[0].Subject)

	duplicateStatus, duplicateLocation = accountSignup(t, srv.URL, "player@example.test", "hunter2hunter2")
	assert.Equal(t, retryStatus, duplicateStatus)
	assert.Equal(t, retryLocation, duplicateLocation)
	require.Len(t, testMailer.messages(), 2)
	assert.Equal(t, "ggscale account", testMailer.messages()[1].Subject)
}

func TestPlayerResend_SMTPRejectionRestoresPreviousCodeAndAllowsRetry(t *testing.T) {
	c := startCluster(t)
	testMailer := &switchableMailer{}
	srv, _ := newVerificationDeliveryServer(t, c, controlpanel.DisabledBootstrap(), testMailer)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signupURL := srv.URL + "/v1/players/account/signup"
	getResp, err := client.Get(signupURL)
	require.NoError(t, err)
	body, err := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	require.NoError(t, err)
	csrf := extractCSRFFromForm(t, string(body))
	form := url.Values{"_csrf": {csrf}, "email": {"player@example.test"}, "password": {"hunter2hunter2"}}
	postResp, err := client.Post(signupURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	postResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, postResp.StatusCode)

	var oldHash []byte
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		UPDATE player_accounts SET email_verification_last_sent_at = now() - interval '2 minutes'
		WHERE email = $1 RETURNING email_verification_code_hash`, "player@example.test").Scan(&oldHash))
	testMailer.reject()

	for range 2 {
		resend := url.Values{"_csrf": {csrf}}
		resp, postErr := client.Post(srv.URL+"/v1/players/account/verify/resend", "application/x-www-form-urlencoded", strings.NewReader(resend.Encode()))
		require.NoError(t, postErr)
		resp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
	}

	var gotHash []byte
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT email_verification_code_hash FROM player_accounts WHERE email = $1`, "player@example.test").Scan(&gotHash))
	assert.Equal(t, oldHash, gotHash)
}

func TestVerificationCompensation_DoesNotClobberNewerConcurrentCode(t *testing.T) {
	c := startCluster(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse-battery-staple"), 12)
	require.NoError(t, err)
	oldHash := verifycode.Hash([]byte("old-salt"), "123456")
	failedHash := verifycode.Hash([]byte("failed-salt"), "234567")
	newerHash := verifycode.Hash([]byte("newer-salt"), "345678")
	var userID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		INSERT INTO control_panel_users (
			email, password_hash, email_verification_code_hash,
			email_verification_salt, email_verification_expires_at,
			email_verification_attempts, email_verification_last_sent_at
		) VALUES ($1, $2, $3, 'failed-salt', now() + interval '15 minutes', 0, now())
		RETURNING id`, "race@example.test", hash, failedHash).Scan(&userID))

	newerInstalled := make(chan struct{})
	restoreDone := make(chan error, 1)
	go func() {
		<-newerInstalled
		pool := db.NewPool(c.appPool)
		restoreDone <- pool.BootstrapQ(context.Background(), func(tx pgx.Tx) error {
			return sqlcgen.New(tx).RestoreControlPanelUserVerificationCode(context.Background(), sqlcgen.RestoreControlPanelUserVerificationCodeParams{
				PreviousCodeHash:   oldHash,
				PreviousCodeSalt:   []byte("old-salt"),
				PreviousExpiresAt:  pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute), Valid: true},
				PreviousAttempts:   2,
				PreviousLastSentAt: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
				ID:                 userID,
				ExpectedCodeHash:   failedHash,
			})
		})
	}()

	_, err = c.bootstrapPool.Exec(context.Background(), `
		UPDATE control_panel_users
		SET email_verification_code_hash = $1,
		    email_verification_salt = 'newer-salt',
		    email_verification_expires_at = now() + interval '15 minutes'
		WHERE id = $2`, newerHash, userID)
	require.NoError(t, err)
	close(newerInstalled)
	require.NoError(t, <-restoreDone)

	var gotHash []byte
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT email_verification_code_hash FROM control_panel_users WHERE id = $1`, userID).Scan(&gotHash))
	assert.Equal(t, newerHash, gotHash)
}
