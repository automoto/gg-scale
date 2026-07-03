package dashboard

// Shared route and message literals. Extracting them keeps duplicated strings
// in sync across handlers and satisfies the duplicate-literal lint (go:S1192).
const (
	// Route fragments.
	pathDashboard       = "/v1/dashboard"
	pathDashboardLogin  = "/v1/dashboard/login"
	pathTenantsPrefix   = "/v1/dashboard/tenants/"
	pathAdminUsersFlash = "/v1/dashboard/admin/users?flash="
	pathAdminAccounts   = "/v1/dashboard/admin/player-accounts"
	segAPIKeys          = "/api-keys"
	segTeamInvite       = "/team/invite"
	queryFlash          = "?flash="

	// Operator-facing error messages reused across handlers.
	msgFleetDetailFailed   = "fleet detail failed"
	msgFleetLookupFailed   = "fleet lookup failed"
	msgNoFleetBackend      = "no fleet backend configured"
	msgProjectListFailed   = "project list failed"
	msgDashboardPoolNeeded = "dashboard: database pool is required"
	msgSetupUnavailable    = "dashboard setup is no longer available"
)
