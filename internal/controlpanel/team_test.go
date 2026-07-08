package controlpanel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInviteTeammateInput_role_validation(t *testing.T) {
	tenantID := int64(7)
	tests := []struct {
		name     string
		in       inviteTeammateInput
		validErr error
	}{
		{
			name: "tenant_admin_with_tenant_ok",
			in:   inviteTeammateInput{Email: "a@b.com", Role: "tenant_admin", TenantID: &tenantID},
		},
		{
			name: "tenant_member_with_tenant_ok",
			in:   inviteTeammateInput{Email: "a@b.com", Role: "tenant_member", TenantID: &tenantID},
		},
		{
			name:     "platform_admin_with_tenant_rejected",
			in:       inviteTeammateInput{Email: "a@b.com", Role: "platform_admin", TenantID: &tenantID},
			validErr: errInvalidInviteRole,
		},
		{
			name: "platform_admin_without_tenant_ok",
			in:   inviteTeammateInput{Email: "a@b.com", Role: "platform_admin", TenantID: nil},
		},
		{
			name:     "tenant_admin_without_tenant_rejected",
			in:       inviteTeammateInput{Email: "a@b.com", Role: "tenant_admin", TenantID: nil},
			validErr: errInvalidInviteRole,
		},
		{
			name:     "unknown_role_rejected",
			in:       inviteTeammateInput{Email: "a@b.com", Role: "wizard", TenantID: &tenantID},
			validErr: errInvalidInviteRole,
		},
		{
			name:     "blank_email_rejected",
			in:       inviteTeammateInput{Email: "", Role: "tenant_admin", TenantID: &tenantID},
			validErr: errInvalidInviteEmail,
		},
		{
			name:     "garbage_email_rejected",
			in:       inviteTeammateInput{Email: "not-an-email", Role: "tenant_admin", TenantID: &tenantID},
			validErr: errInvalidInviteEmail,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInviteInput(tc.in)
			if tc.validErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.validErr)
		})
	}
}

func TestTenantTeamPath(t *testing.T) {
	assert.Equal(t, "/v1/control-panel/tenants/42/team", tenantTeamPath(42))
}

func TestInviteAcceptURL_format(t *testing.T) {
	h := &Handler{cfg: Config{BaseURL: "https://app.example.com/"}}
	got := h.inviteAcceptURL("abc-XYZ_123")
	assert.Equal(t, "https://app.example.com/v1/control-panel/invite/accept?code=abc-XYZ_123", got)
}

func TestInviteAcceptURL_empty_base(t *testing.T) {
	h := &Handler{cfg: Config{}}
	got := h.inviteAcceptURL("zzz")
	assert.Equal(t, "/v1/control-panel/invite/accept?code=zzz", got)
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"plain_error", assertError("oops"), false},
		{"with_23505", assertError("ERROR: duplicate key value violates unique constraint (SQLSTATE 23505)"), true},
		{"without_23505", assertError("ERROR: relation not found (SQLSTATE 42P01)"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isUniqueViolation(tc.err))
		})
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }
