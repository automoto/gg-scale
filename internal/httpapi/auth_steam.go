package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auth"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/steamauth"
	"github.com/ggscale/ggscale/internal/webutil"
)

type steamAuthRequest struct {
	Ticket string `json:"ticket" minLength:"20" maxLength:"8192" pattern:"^[0-9a-fA-F]+$" example:"14000000048bcd42aabbccdd"`
}

type steamAuthInput struct {
	Body steamAuthRequest
}

var errSteamNotConfigured = errors.New("auth: steam sign-in not configured")

func registerAuthSteam(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "authSteam",
		Method:      "POST",
		Path:        "/v1/auth/steam",
		Summary:     "Exchange a Steam session ticket for a session",
		Description: "Verifies a Steamworks session ticket with Valve and signs the " +
			"player in, creating the player on first sign-in. Obtain the ticket " +
			"with ISteamUser::GetAuthTicketForWebApi(\"" + steamauth.WebAPIIdentity + "\") " +
			"and hex-encode it. Requires the project's Steam credentials to be " +
			"configured in the control panel. 401 means Valve rejected the " +
			"ticket (fetch a fresh one); 502 means Valve was unreachable (retry " +
			"with backoff).",
		Tags:     []string{"Authentication"},
		Security: apiKeySecurity,
	}, authSteam(d))
}

func authSteam(d Deps) func(context.Context, *steamAuthInput) (*sessionOutput, error) {
	return func(ctx context.Context, in *steamAuthInput) (*sessionOutput, error) {
		projectID, tenantID, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		refreshTok, err := webutil.RandomHex("", 32)
		if err != nil {
			return nil, serverError(ctx, "steam auth: rand", err)
		}
		now := apiNow(d)

		// Config read in its own short transaction: the Valve round-trip
		// below must never run while a DB transaction is open.
		var appID string
		var webAPIKey []byte
		err = d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			cfg, qerr := sqlcgen.New(tx).GetProjectSteamAuthConfig(ctx, projectID)
			if qerr != nil {
				return qerr
			}
			if cfg.SteamAppID == "" || len(cfg.SteamWebAPIKey) == 0 {
				return errSteamNotConfigured
			}
			appID = cfg.SteamAppID
			webAPIKey = d.CredentialCipher.DecryptOrPlain(cfg.SteamWebAPIKey)
			return nil
		})
		switch {
		case errors.Is(err, errSteamNotConfigured), errors.Is(err, pgx.ErrNoRows):
			return nil, huma.Error400BadRequest("steam sign-in not configured for this project")
		case err != nil:
			return nil, serverError(ctx, "steam auth: config", err)
		}

		res, err := d.SteamAuth.Verify(ctx, appID, string(webAPIKey), in.Body.Ticket)
		switch {
		case errors.Is(err, steamauth.ErrInvalidTicket):
			return nil, huma.Error401Unauthorized("invalid steam session ticket")
		case err != nil:
			slog.ErrorContext(ctx, "steam auth: verify", "error", err)
			return nil, huma.Error502BadGateway("steam verification is temporarily unavailable")
		}
		if res.VACBanned || res.PublisherBanned {
			return nil, huma.Error403Forbidden("steam account banned")
		}
		// Identity keys on the playing account, never ownersteamid: under
		// Family Sharing the borrower is a distinct player from the owner.
		if !validSteamID(res.SteamID) {
			slog.ErrorContext(ctx, "steam auth: unexpected steamid", "steamid", res.SteamID)
			return nil, huma.Error502BadGateway("steam verification is temporarily unavailable")
		}
		externalID := "steam:" + res.SteamID

		var playerID, sessionEpoch int64
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			playerID, sessionEpoch, err = signInExternalPlayer(ctx, tx, projectID, externalID, "auth.steam", refreshTok, now)
			return err
		})
		switch {
		case errors.Is(err, errPlayerBanned):
			return nil, huma.Error403Forbidden("account banned")
		case isPlayerQuotaError(err):
			d.Metrics.QuotaRejection(quota.AxisPlayers)
			return nil, huma.Error403Forbidden("player creation is disabled: the tenant has reached its registered-player limit")
		case err != nil:
			return nil, serverError(ctx, "steam auth: tx", err)
		}

		return mintSession(ctx, d, "steam", auth.Claims{
			PlayerID: playerID, TenantID: tenantID, ProjectID: projectID,
			SessionEpoch: sessionEpoch,
			ExpiresAt:    now.Add(accessTokenTTL),
		}, refreshTok)
	}
}

// validSteamID sanity-checks Valve's steamid before it becomes an external
// identity: SteamID64s are 17-digit decimal numbers.
func validSteamID(s string) bool {
	if len(s) < 15 || len(s) > 20 {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}
