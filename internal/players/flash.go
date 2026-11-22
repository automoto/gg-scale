package players

import "net/http"

// flashCode selects a fixed page message. A redirect carries only the code
// (?flash=<code>), so a link cannot put its own text on a page. Messages do
// not hold names, addresses or other data from the request.
type flashCode string

// flashNotice is the text of a flash code. err shows it as an error.
type flashNotice struct {
	text string
	err  bool
}

const (
	flashPasswordIncorrect flashCode = "password_incorrect"
	flashTwoFactorBadCode  flashCode = "two_factor_bad_code"
	flashTwoFactorLocked   flashCode = "two_factor_locked"
	flashTwoFactorBroken   flashCode = "two_factor_broken"
	flashSSORetry          flashCode = "sso_retry"
	flashSSOUnreachable    flashCode = "sso_unreachable"
	flashSSOCancelled      flashCode = "sso_cancelled"
	flashSSOLinked         flashCode = "sso_linked"
	flashSSOAlreadyLinked  flashCode = "sso_already_linked"
	flashSSOCannotLink     flashCode = "sso_cannot_link"
	flashSSOProviderTaken  flashCode = "sso_provider_taken"
	flashSSOUnlinked       flashCode = "sso_unlinked"
	flashSSONotLinked      flashCode = "sso_not_linked"
	flashSSOLastMethod     flashCode = "sso_last_method"
	flashUnlinkBlocked     flashCode = "unlink_blocked"
	flashProjectUnlinked   flashCode = "project_unlinked"
	flashDeleteScheduled   flashCode = "delete_scheduled"
	flashDeleteCancelled   flashCode = "delete_cancelled"
	flashDeleteByTeam      flashCode = "delete_by_team"
	flashDeleteNone        flashCode = "delete_none"
	flashAddrAdded         flashCode = "addr_added"
	flashAddrUpdated       flashCode = "addr_updated"
	flashAddrRemoved       flashCode = "addr_removed"
	flashFriendTargetEmpty flashCode = "friend_target_empty"
	flashFriendRequestSent flashCode = "friend_request_sent"
	flashFriendAmbiguous   flashCode = "friend_ambiguous"
	flashFriendSelf        flashCode = "friend_self"
	flashFriendUnknown     flashCode = "friend_unknown"
	flashFriendNotAllowed  flashCode = "friend_not_allowed"
	flashFriendAdded       flashCode = "friend_added"
	flashFriendDeclined    flashCode = "friend_declined"
	flashFriendRequestGone flashCode = "friend_request_gone"
	flashFriendRemoved     flashCode = "friend_removed"
	flashFriendBlocked     flashCode = "friend_blocked"
	flashFriendUnblocked   flashCode = "friend_unblocked"
)

var flashNotices = map[flashCode]flashNotice{
	flashPasswordIncorrect: {text: "Current password is incorrect.", err: true},
	flashTwoFactorBadCode:  {text: msgTwoFactorBadCode, err: true},
	flashTwoFactorLocked:   {text: msgTwoFactorLocked, err: true},
	flashTwoFactorBroken:   {text: msgTwoFactorBroken, err: true},
	flashSSORetry:          {text: msgSSORetry, err: true},
	flashSSOUnreachable:    {text: "Could not reach the sign-in provider. Try again.", err: true},
	flashSSOCancelled:      {text: "The sign-in with the provider was cancelled.", err: true},
	flashSSOLinked:         {text: "Provider account linked."},
	flashSSOAlreadyLinked:  {text: "This provider account is already linked."},
	flashSSOCannotLink:     {text: "This provider account cannot be linked.", err: true},
	flashSSOProviderTaken:  {text: "Your account already has an account of this provider linked. Unlink it first.", err: true},
	flashSSOUnlinked:       {text: "Provider account unlinked."},
	flashSSONotLinked:      {text: "That provider is not linked.", err: true},
	flashSSOLastMethod: {
		text: "That provider is your only sign-in method. Set a password or link a different provider first.",
		err:  true,
	},
	flashUnlinkBlocked: {
		text: "A data deletion is scheduled for this game. Cancel the deletion before unlinking.",
		err:  true,
	},
	flashProjectUnlinked:   {text: "Unlinked from the game. Your game data is kept; accepting a new invite restores it."},
	flashDeleteScheduled:   {text: "Deletion of your game data is scheduled. You can cancel until the purge runs."},
	flashDeleteCancelled:   {text: "Deletion cancelled. Your game data is kept."},
	flashDeleteByTeam:      {text: "The game's team requested this deletion. Contact them to cancel it."},
	flashDeleteNone:        {text: "No pending deletion for this game."},
	flashAddrAdded:         {text: "Connection address added."},
	flashAddrUpdated:       {text: "Connection address updated."},
	flashAddrRemoved:       {text: "Connection address removed."},
	flashFriendTargetEmpty: {text: "Enter an email or display name.", err: true},
	flashFriendRequestSent: {text: friendRequestSentFlash},
	flashFriendAmbiguous:   {text: "Multiple accounts share that display name — use an email.", err: true},
	flashFriendSelf:        {text: "You can't friend yourself.", err: true},
	flashFriendUnknown:     {text: "Unknown account.", err: true},
	flashFriendNotAllowed:  {text: "That action isn't allowed.", err: true},
	flashFriendAdded:       {text: "Friend added."},
	flashFriendDeclined:    {text: "Request declined."},
	flashFriendRequestGone: {text: "That request is no longer available."},
	flashFriendRemoved:     {text: "Removed."},
	flashFriendBlocked:     {text: "Blocked."},
	flashFriendUnblocked:   {text: "Unblocked."},
}

// flashFromRequest returns the message and the error message of the flash
// code in the request URL. An unknown code or free text gives neither.
func flashFromRequest(r *http.Request) (msg, errMsg string) {
	notice, found := flashNotices[flashCode(r.URL.Query().Get("flash"))]
	switch {
	case !found:
		return "", ""
	case notice.err:
		return "", notice.text
	default:
		return notice.text, ""
	}
}

// redirectFlash redirects to path with the flash code. An empty code adds
// no query.
func redirectFlash(w http.ResponseWriter, r *http.Request, path string, code flashCode) {
	if code != "" {
		path += "?flash=" + string(code)
	}
	http.Redirect(w, r, path, http.StatusSeeOther) //nolint:gosec // path is a fixed account path and code is a fixed constant
}
