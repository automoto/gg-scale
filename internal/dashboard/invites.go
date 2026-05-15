package dashboard

import (
	"github.com/ggscale/ggscale/internal/verifycode"
)

// Re-exported for the rest of the dashboard package; the shared
// primitives live in internal/verifycode so the end-user flow can use
// the same code generation + hashing convention.

var (
	generateVerificationCode = verifycode.GenerateCode
	generateInviteCode       = verifycode.GenerateInviteCode
	newSalt                  = verifycode.NewSalt
	hashCode                 = verifycode.Hash
	canResendCode            = verifycode.CanResend
	codeExpired              = verifycode.Expired
	verifyAttemptsExhausted  = verifycode.AttemptsExhausted
)

const (
	verificationCodeTTL = verifycode.CodeTTL
	inviteTTL           = verifycode.InviteTTL
	resendCooldown      = verifycode.ResendCooldown
	maxVerifyAttempts   = verifycode.MaxAttempts
	saltBytes           = 16
)

const (
	roleInvitePlatformAdmin = "platform_admin"
	roleInviteTenantAdmin   = "tenant_admin"
	roleInviteTenantMember  = "tenant_member"
)

func validInviteRole(role string) bool {
	switch role {
	case roleInvitePlatformAdmin, roleInviteTenantAdmin, roleInviteTenantMember:
		return true
	}
	return false
}
