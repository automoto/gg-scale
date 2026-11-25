package controlpanel

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/automoto/gg-scale/internal/gamesession"
	"github.com/automoto/gg-scale/internal/projectadmin"
)

// flashCode selects a fixed page message. A redirect carries only the code
// (?flash=<code>), so a link cannot put its own text on a page. Messages do
// not hold names, addresses or other data from the request.
type flashCode string

const (
	flashProjectCreated flashCode = "project_created"

	flashRateBurstInvalid        flashCode = "rate_burst_invalid"
	flashAPILimitUpdated         flashCode = "api_limit_updated"
	flashConnectionLimitsInvalid flashCode = "connection_limits_invalid"
	flashConnectionLimitUpdated  flashCode = "connection_limit_updated"
	flashRecipientBurstInvalid   flashCode = "recipient_burst_invalid"
	flashRecipientBurstUpdated   flashCode = "recipient_burst_updated"
	flashQuotaLimitInvalid       flashCode = "quota_limit_invalid"
	flashSessionsAboveCap        flashCode = "sessions_above_cap"
	flashLeaderboardsUnlimited   flashCode = "leaderboards_unlimited"
	flashQuotaOverrideUpdated    flashCode = "quota_override_updated"
	flashInviteQuotasInvalid     flashCode = "invite_quotas_invalid"
	flashInviteQuotasUpdated     flashCode = "invite_quotas_updated"
	flashLimitValuesInvalid      flashCode = "limit_values_invalid"
	flashLimitIncomplete         flashCode = "limit_incomplete"
	flashConnectionCeiling       flashCode = "connection_ceiling"
	flashConnectionBurstRatio    flashCode = "connection_burst_ratio"
	flashConnectionAbsoluteMax   flashCode = "connection_absolute_max"
	flashConnectionEnvOverride   flashCode = "connection_env_override"
	flashStorageMBInvalid        flashCode = "storage_mb_invalid"
	flashStorageAboveTenant      flashCode = "storage_above_tenant"
	flashInviteAboveCap          flashCode = "invite_above_cap"
	flashTenantStorageUpdated    flashCode = "tenant_storage_updated"
	flashProjectStorageUpdated   flashCode = "project_storage_updated"
	flashCustomTokenSaved        flashCode = "custom_token_saved"
	flashCustomTokenDisabled     flashCode = "custom_token_disabled"
	flashRequestClassInvalid     flashCode = "request_class_invalid"
	flashRequestFeatureInvalid   flashCode = "request_feature_invalid"
	flashRequestQuotaInvalid     flashCode = "request_quota_invalid"
	flashRequestLimitInvalid     flashCode = "request_limit_invalid"
	flashRequestTypeUnknown      flashCode = "request_type_unknown"
	flashRequestClassNotAbove    flashCode = "request_class_not_above"
	flashRequestFeatureEnabled   flashCode = "request_feature_enabled"
	flashRequestPending          flashCode = "request_pending"
	flashRequestSubmitted        flashCode = "request_submitted"
	flashTenantAlreadyDisabled   flashCode = "tenant_already_disabled"
	flashTenantDisabled          flashCode = "tenant_disabled"
	flashTenantNotReenabled      flashCode = "tenant_not_reenabled"
	flashTenantReenabled         flashCode = "tenant_reenabled"
	flashTierInvalid             flashCode = "tier_invalid"
	flashTierUnchanged           flashCode = "tier_unchanged"
	flashTierChanged             flashCode = "tier_changed"
	flashFeatureInvalid          flashCode = "feature_invalid"
	flashFeatureSwitchOff        flashCode = "feature_switch_off"
	flashFeatureAlreadyEnabled   flashCode = "feature_already_enabled"
	flashFeatureAlreadyDisabled  flashCode = "feature_already_disabled"
	flashFeatureEnabled          flashCode = "feature_enabled"
	flashFeatureDisabled         flashCode = "feature_disabled"
	flashRequestHandled          flashCode = "request_handled"
	flashRequestClassNotApproved flashCode = "request_class_not_approved"
	flashRequestCapNotApproved   flashCode = "request_cap_not_approved"
	flashRequestApproved         flashCode = "request_approved"
	flashRequestDenied           flashCode = "request_denied"
	flashSignupDisabled          flashCode = "signup_disabled"
	flashSignupEnabled           flashCode = "signup_enabled"
	flashTenantNameInvalid       flashCode = "tenant_name_invalid"
	flashTenantNameTaken         flashCode = "tenant_name_taken"
	flashSignupApproved          flashCode = "signup_approved"
	flashInviteSent              flashCode = "invite_sent"
	flashInviteRevoked           flashCode = "invite_revoked"
	flashRolesUpdated            flashCode = "roles_updated"
	flashRemoveSelf              flashCode = "remove_self"
	flashLastOwner               flashCode = "last_owner"
	flashMemberRemoved           flashCode = "member_removed"
	flashLeaderboardCreated      flashCode = "leaderboard_created"
	flashLeaderboardUpdated      flashCode = "leaderboard_updated"
	flashLeaderboardDeleted      flashCode = "leaderboard_deleted"
	flashLeaderboardRestored     flashCode = "leaderboard_restored"
	flashLeaderboardRolledBack   flashCode = "leaderboard_rolled_back"
	flashRemoteConfigRolledBack  flashCode = "remote_config_rolled_back"
	flashRemoteConfigSaved       flashCode = "remote_config_saved"
	flashSteamSaved              flashCode = "steam_saved"
	flashSteamDisabled           flashCode = "steam_disabled"
	flashOriginsSaved            flashCode = "origins_saved"
	flashUserDisableSelf         flashCode = "user_disable_self"
	flashUserLastAdmin           flashCode = "user_last_admin"
	flashUserReenabled           flashCode = "user_reenabled"
	flashUserDisabled            flashCode = "user_disabled"
	flashAccountReenabled        flashCode = "account_reenabled"
	flashAccountDisabled         flashCode = "account_disabled"
	flashPlayerDisabled          flashCode = "player_disabled"
	flashPlayerReenabled         flashCode = "player_reenabled"
	flashPlayerDeleteScheduled   flashCode = "player_delete_scheduled"
	flashPlayerDeleteCancelled   flashCode = "player_delete_cancelled"
	flashPlayerBanned            flashCode = "player_banned"
	flashPlayerUnbanned          flashCode = "player_unbanned"
	flashFleetCreated            flashCode = "fleet_created"
	flashFleetUpdated            flashCode = "fleet_updated"
	flashFleetDeleted            flashCode = "fleet_deleted"
	flashFleetNameTaken          flashCode = "fleet_name_taken"
	flashFleetRestored           flashCode = "fleet_restored"
	flashAllocationCreated       flashCode = "allocation_created"
	flashAllocationDeallocated   flashCode = "allocation_deallocated"
	flashManagementTokenRevoked  flashCode = "management_token_revoked"
	flashVerifyCodeSent          flashCode = "verify_code_sent"
)

var flashMessages = map[flashCode]string{ //nolint:gosec // page message texts, not credentials
	flashProjectCreated: "Game Project created.",

	flashRateBurstInvalid:        "Rate and burst must be non-negative numbers.",
	flashAPILimitUpdated:         "API limit updated.",
	flashConnectionLimitsInvalid: "Connection limits must be non-negative whole numbers.",
	flashConnectionLimitUpdated:  "Connection limit updated.",
	flashRecipientBurstInvalid:   "Recipient burst and cooldown must be non-negative numbers.",
	flashRecipientBurstUpdated:   "Recipient burst updated.",
	flashQuotaLimitInvalid:       "Enter a limit of 0 or more (-1 for unlimited), or leave blank to restore the tier default.",
	flashSessionsAboveCap: fmt.Sprintf(
		"Open game sessions per Game Project can't exceed %d (the platform-wide cap) and can't be unlimited.",
		gamesession.SessionsHardCap),
	flashLeaderboardsUnlimited:  "Leaderboards per Game Project can't be unlimited. Enter a number.",
	flashQuotaOverrideUpdated:   "Quota override updated.",
	flashInviteQuotasInvalid:    "Invite quotas must be non-negative numbers.",
	flashInviteQuotasUpdated:    "Invite quotas updated.",
	flashLimitValuesInvalid:     "Values must be finite non-negative numbers.",
	flashLimitIncomplete:        "Enter both values, or clear both to restore the default.",
	flashConnectionCeiling:      "Temporary connection maximum must be at least the sustained limit.",
	flashConnectionBurstRatio:   "Temporary connection maximum cannot exceed twice the sustained limit.",
	flashConnectionAbsoluteMax:  "Connection limits cannot exceed 500,000 without a capacity-reviewed release.",
	flashConnectionEnvOverride:  "REALTIME_MAX_PER_TENANT is active; unset it before editing tenant connection limits.",
	flashStorageMBInvalid:       projectadmin.ErrInvalidStorageMB.Error(),
	flashStorageAboveTenant:     projectadmin.ErrStorageAboveTenant.Error(),
	flashInviteAboveCap:         projectadmin.ErrInviteAboveCap.Error(),
	flashTenantStorageUpdated:   "Account Tenant storage limit updated.",
	flashProjectStorageUpdated:  "Game Project storage limit updated.",
	flashCustomTokenSaved:       "Custom token signing key saved.",
	flashCustomTokenDisabled:    "Custom token sign-in disabled.",
	flashRequestClassInvalid:    "Choose a valid target class to upgrade to.",
	flashRequestFeatureInvalid:  "Choose a feature that's available to request.",
	flashRequestQuotaInvalid:    "Choose a valid quota to request a change for.",
	flashRequestLimitInvalid:    "Enter a limit of 0 or more (or -1 for unlimited).",
	flashRequestTypeUnknown:     "Unknown request type.",
	flashRequestClassNotAbove:   "Choose a target class above your current class.",
	flashRequestFeatureEnabled:  "That feature is already enabled for this Account Tenant.",
	flashRequestPending:         "You already have a pending request of this type.",
	flashRequestSubmitted:       "Request submitted. A platform admin will review it.",
	flashTenantAlreadyDisabled:  "Account Tenant is already disabled.",
	flashTenantDisabled:         "Account Tenant disabled. API keys and player traffic are blocked.",
	flashTenantNotReenabled:     "Account Tenant was not re-enabled. A platform disable can only be undone by a platform admin.",
	flashTenantReenabled:        "Account Tenant re-enabled.",
	flashTierInvalid:            "Choose a valid Account Tenant tier.",
	flashTierUnchanged:          "Account Tenant is already on that tier.",
	flashTierChanged:            "Account Tenant tier changed.",
	flashFeatureInvalid:         "Choose a valid feature to grant.",
	flashFeatureSwitchOff:       "That feature's server switch is off; enable it in server settings first.",
	flashFeatureAlreadyEnabled:  "Feature is already enabled.",
	flashFeatureAlreadyDisabled: "Feature is already disabled.",
	flashFeatureEnabled:         "Feature enabled.",
	flashFeatureDisabled:        "Feature disabled.",
	flashRequestHandled:         "That request was already handled.",
	flashRequestClassNotApproved: "The Account Tenant is already at or above the requested class; " +
		"the request was not approved.",
	flashRequestCapNotApproved: "The requested limit exceeds the platform-wide session cap; " +
		"the request was not approved. Deny it with a reason instead.",
	flashRequestApproved: "Request approved and applied.",
	flashRequestDenied:   "Request denied.",
	flashSignupDisabled:  "Public Account Tenant sign-up disabled.",
	flashSignupEnabled:   "Public Account Tenant sign-up enabled.",
	flashTenantNameInvalid: fmt.Sprintf("Enter a valid Account Tenant name (%d–%d characters).",
		tenantNameMin, tenantNameMax),
	flashTenantNameTaken:        "That Account Tenant name is taken; edit it and try again.",
	flashSignupApproved:         "Request approved. Invite emailed.",
	flashInviteSent:             "Invite sent.",
	flashInviteRevoked:          "Invite revoked.",
	flashRolesUpdated:           "Roles updated.",
	flashRemoveSelf:             "You can't remove yourself; ask another admin.",
	flashLastOwner:              "You can't remove the last owner.",
	flashMemberRemoved:          "Member removed.",
	flashLeaderboardCreated:     "Leaderboard created.",
	flashLeaderboardUpdated:     "Leaderboard updated.",
	flashLeaderboardDeleted:     "Leaderboard deleted.",
	flashLeaderboardRestored:    "Leaderboard restored.",
	flashLeaderboardRolledBack:  "Settings rolled back. Scores did not change.",
	flashRemoteConfigRolledBack: "Remote config rolled back.",
	flashRemoteConfigSaved:      "Remote config saved.",
	flashSteamSaved:             "Steam sign-in settings saved.",
	flashSteamDisabled:          "Steam sign-in disabled.",
	flashOriginsSaved:           "Allowed origins saved. Each server uses the new list after a short delay.",
	flashUserDisableSelf:        "You can't disable yourself; ask another platform admin.",
	flashUserLastAdmin:          "You can't disable the last enabled platform admin.",
	flashUserReenabled:          "User re-enabled.",
	flashUserDisabled:           "User disabled. Sessions and pending invitations revoked.",
	flashAccountReenabled:       "Account re-enabled.",
	flashAccountDisabled:        "Account disabled platform-wide. Sessions revoked.",
	flashPlayerDisabled:         "Player disabled.",
	flashPlayerReenabled:        "Player re-enabled.",
	flashPlayerDeleteScheduled:  "Deletion scheduled. Cancel before the purge time to keep the data.",
	flashPlayerDeleteCancelled:  "Deletion cancelled. The player's data is kept.",
	flashPlayerBanned:           "Player banned across the Account Tenant.",
	flashPlayerUnbanned:         "Player unbanned.",
	flashFleetCreated:           "Fleet created.",
	flashFleetUpdated:           "Fleet updated.",
	flashFleetDeleted:           "Fleet deleted.",
	flashFleetNameTaken:         "Cannot restore: a fleet with that name exists. Rename or delete it first.",
	flashFleetRestored:          "Fleet restored.",
	flashAllocationCreated:      "Allocation created.",
	flashAllocationDeallocated:  "Allocation deallocated.",
	flashManagementTokenRevoked: "Management token revoked.",
	flashVerifyCodeSent:         "A new code was sent.",
}

// limitFlashes holds the code of each refused limit value. Their texts are
// the LimitError texts.
var limitFlashes = map[projectadmin.LimitError]flashCode{
	projectadmin.ErrInvalidStorageMB:   flashStorageMBInvalid,
	projectadmin.ErrStorageAboveTenant: flashStorageAboveTenant,
	projectadmin.ErrInvalidInviteLimit: flashLimitValuesInvalid,
	projectadmin.ErrInviteAboveCap:     flashInviteAboveCap,
}

// flashMessage returns the message of the flash code in the request URL. An
// unknown code or free text gives an empty message.
func flashMessage(r *http.Request) string {
	return flashMessages[flashCode(r.URL.Query().Get("flash"))]
}

// withFlash returns path with the flash code as its query.
func withFlash(path string, code flashCode) string {
	return path + "?flash=" + string(code)
}

// limitFlash returns the code of a refused limit value.
func limitFlash(err error) flashCode {
	var limitErr projectadmin.LimitError
	if errors.As(err, &limitErr) {
		if code, ok := limitFlashes[limitErr]; ok {
			return code
		}
	}
	return flashLimitValuesInvalid
}

// redirectFlash redirects to base with a flash code.
func redirectFlash(w http.ResponseWriter, r *http.Request, base string, code flashCode) {
	http.Redirect(w, r, withFlash(base, code), http.StatusSeeOther) //nolint:gosec // base is built from parsed integer IDs and code is a fixed constant
}
