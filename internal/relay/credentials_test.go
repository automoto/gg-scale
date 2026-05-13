package relay_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/relay"
)

func TestIssueCredentialsProducesParseableUsername(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)

	creds, err := issuer.Issue(1, 42)

	require.NoError(t, err)
	parts := strings.Split(creds.Username, ":")
	require.Len(t, parts, 3)
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Minute), time.Unix(expires, 0), 5*time.Second)
	assert.Equal(t, "1", parts[1])
	assert.Equal(t, "42", parts[2])
}

func TestIssueCredentialsHonoursTTL(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", 30*time.Second)
	creds, err := issuer.Issue(1, 42)

	require.NoError(t, err)
	assert.Equal(t, int64(30), creds.TTLSeconds)
	assert.Equal(t, "ggscale", creds.Realm)
}

func TestVerifyAcceptsFreshCredentials(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)

	tenantID, endUserID, err := issuer.Verify(creds.Username, creds.Password)

	require.NoError(t, err)
	assert.Equal(t, int64(1), tenantID)
	assert.Equal(t, int64(42), endUserID)
}

func TestVerifyRejectsExpiredCredentials(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", -time.Second)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)

	_, _, err = issuer.Verify(creds.Username, creds.Password)

	assert.ErrorIs(t, err, relay.ErrCredentialsExpired)
}

func TestVerifyRejectsTamperedPassword(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)

	_, _, err = issuer.Verify(creds.Username, creds.Password+"x")

	assert.ErrorIs(t, err, relay.ErrCredentialsInvalid)
}

func TestVerifyRejectsTamperedUsername(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)

	// Swap to a different tenant id and re-issue the username; the password
	// still binds to the original username so verification must fail.
	parts := strings.Split(creds.Username, ":")
	parts[1] = "999"
	tampered := strings.Join(parts, ":")

	_, _, err = issuer.Verify(tampered, creds.Password)

	assert.ErrorIs(t, err, relay.ErrCredentialsInvalid)
}

func TestVerifyRejectsMalformedUsername(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)

	_, _, err := issuer.Verify("not-a-real-username", "any-password")

	assert.ErrorIs(t, err, relay.ErrCredentialsInvalid)
}
