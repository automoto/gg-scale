package controlpanel

// Shared route and message literals. Extracting them keeps duplicated strings
// in sync across handlers and satisfies the duplicate-literal lint (go:S1192).
const (
	// Route fragments.
	pathControlPanel      = "/v1/control-panel"
	pathControlPanelLogin = "/v1/control-panel/login"
	pathTenantsPrefix     = "/v1/control-panel/tenants/"
	pathAdminUsersFlash   = "/v1/control-panel/admin/users?flash="
	pathAdminAccounts     = "/v1/control-panel/admin/player-accounts"
	segAPIKeys            = "/api-keys"
	segTeamInvite         = "/team/invite"
	queryFlash            = "?flash="

	// Operator-facing error messages reused across handlers.
	msgFleetDetailFailed      = "fleet detail failed"
	msgFleetLookupFailed      = "fleet lookup failed"
	msgNoFleetBackend         = "no fleet backend configured"
	msgProjectListFailed      = "Game Project list failed"
	msgControlPanelPoolNeeded = "control panel: database pool is required"
	msgSetupUnavailable       = "control panel setup is no longer available"
	msgLeaderboardLimit       = "This Game Project has the maximum number of leaderboards. Delete one first, or ask a platform admin to raise the limit."
)
