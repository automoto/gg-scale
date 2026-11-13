package relay_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relay"
)

func TestIssueCredentialsProducesParseableUsername(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)

	creds, err := issuer.Issue(1, 42)

	require.NoError(t, err)
	parts := strings.Split(creds.Username, ":")
	require.Len(t, parts, 4)
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Minute), time.Unix(expires, 0), 5*time.Second)
	assert.Equal(t, "1", parts[1])
	assert.Equal(t, "42", parts[2])
	assert.NotEmpty(t, parts[3], "username carries a key id")
}

func TestRotationAcceptsBothSecrets(t *testing.T) {
	oldSecret := strings.Repeat("o", 32)
	newSecret := strings.Repeat("n", 32)

	// A credential minted while the old secret was active...
	oldIssuer := relay.NewIssuer(oldSecret, "ggscale", time.Minute)
	oldCreds, err := oldIssuer.Issue(1, 42)
	require.NoError(t, err)

	// ...must still verify after the new secret is promoted to active, as long
	// as the old secret stays in the accepted set (the rotation overlap).
	rotated := relay.NewIssuerWithSecrets([]string{newSecret, oldSecret}, "ggscale", time.Minute)
	tenantID, playerID, err := rotated.Verify(oldCreds.Username, oldCreds.Password)
	require.NoError(t, err)
	assert.Equal(t, int64(1), tenantID)
	assert.Equal(t, int64(42), playerID)

	// New credentials verify too.
	newCreds, err := rotated.Issue(2, 7)
	require.NoError(t, err)
	_, _, err = rotated.Verify(newCreds.Username, newCreds.Password)
	require.NoError(t, err)

	// Once the old secret is dropped, its credentials no longer verify.
	newOnly := relay.NewIssuer(newSecret, "ggscale", time.Minute)
	_, _, err = newOnly.Verify(oldCreds.Username, oldCreds.Password)
	assert.Error(t, err)
}

func TestIssueCredentialsHonoursTTL(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", 30*time.Second)
	creds, err := issuer.Issue(1, 42)

	require.NoError(t, err)
	assert.Equal(t, int64(30), creds.TTLSeconds)
	assert.Equal(t, "ggscale", creds.Realm)
}

func TestVerifyRejectsExpiredAndMalformed(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", -time.Minute)
	expired, err := issuer.Issue(1, 42)
	require.NoError(t, err)

	fresh := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	_, _, err = fresh.Verify(expired.Username, expired.Password)
	assert.ErrorIs(t, err, relay.ErrCredentialsExpired, "expired username rejected")

	_, _, err = fresh.Verify("garbage", "any")
	assert.ErrorIs(t, err, relay.ErrCredentialsInvalid, "malformed username rejected")

	_, _, err = fresh.Verify("notanumber:1:2:kid", "any")
	assert.ErrorIs(t, err, relay.ErrCredentialsInvalid, "non-numeric expiry rejected")
}

func TestIssueIncludesConfiguredSTUNURLs(t *testing.T) {
	issuer := relay.NewIssuer(strings.Repeat("a", 32), "realm", time.Minute)
	issuer.SetSTUNURLs([]string{"stun:relay.example.com:3478"})

	creds, err := issuer.Issue(7, 42)
	require.NoError(t, err)
	assert.Equal(t, []string{"stun:relay.example.com:3478"}, creds.STUNURLs)
}

func TestVerifyAcceptsFreshCredentials(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)

	tenantID, playerID, err := issuer.Verify(creds.Username, creds.Password)

	require.NoError(t, err)
	assert.Equal(t, int64(1), tenantID)
	assert.Equal(t, int64(42), playerID)
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

func capOf(n int64) *int64 { return &n }

func TestIssueWithTenantCap_should_omit_cap_when_disabled(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)

	creds, err := issuer.IssueWithTenantCap(1, 42, capOf(200))

	require.NoError(t, err)
	assert.Len(t, strings.Split(creds.Username, ":"), 4, "old relays reject a fifth field")
}

func TestIssueWithTenantCap_should_append_cap_when_enabled(t *testing.T) {
	cases := map[int64]string{200: "200", 0: "0", -1: "-1"}
	for allocCap, want := range cases {
		issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
		issuer.SetTenantCapInCredentials(true)

		creds, err := issuer.IssueWithTenantCap(1, 42, capOf(allocCap))

		require.NoError(t, err)
		parts := strings.Split(creds.Username, ":")
		require.Len(t, parts, 5)
		assert.Equal(t, want, parts[4])
	}
}

func TestIssueWithTenantCap_should_omit_cap_when_tenant_has_none(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	issuer.SetTenantCapInCredentials(true)

	creds, err := issuer.IssueWithTenantCap(1, 42, nil)

	require.NoError(t, err)
	assert.Len(t, strings.Split(creds.Username, ":"), 4)
}

func TestVerify_should_accept_credential_with_tenant_cap(t *testing.T) {
	for _, allocCap := range []int64{200, 0, -1} {
		issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
		issuer.SetTenantCapInCredentials(true)
		creds, err := issuer.IssueWithTenantCap(1, 42, capOf(allocCap))
		require.NoError(t, err)

		tenantID, playerID, err := issuer.Verify(creds.Username, creds.Password)

		require.NoError(t, err, "cap %d", allocCap)
		assert.Equal(t, [2]int64{1, 42}, [2]int64{tenantID, playerID})
	}
}

func TestVerify_should_reject_raised_tenant_cap(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	issuer.SetTenantCapInCredentials(true)
	creds, err := issuer.IssueWithTenantCap(1, 42, capOf(200))
	require.NoError(t, err)

	raised := strings.TrimSuffix(creds.Username, ":200") + ":999999"
	_, _, err = issuer.Verify(raised, creds.Password)

	assert.ErrorIs(t, err, relay.ErrCredentialsInvalid)
}

func TestVerify_should_reject_malformed_tenant_cap(t *testing.T) {
	issuer := relay.NewIssuer("shared-secret", "ggscale", time.Minute)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)

	for _, suffix := range []string{":", ":abc", ":-2", ":1:2"} {
		_, _, err = issuer.Verify(creds.Username+suffix, creds.Password)
		assert.ErrorIs(t, err, relay.ErrCredentialsInvalid, "suffix %q", suffix)
	}
}
