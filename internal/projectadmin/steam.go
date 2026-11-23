package projectadmin

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/secretseal"
)

const steamWebAPIKeyHexLen = 32

// The text of these errors is shown to the person or the agent, as a
// sentence. It never holds the value that was refused.
var (
	ErrInvalidSteamAppID = errors.New("Enter the numeric Steam App ID.")               //nolint:staticcheck // shown as a sentence
	ErrInvalidSteamKey   = errors.New("Enter the 32-character publisher Web API key.") //nolint:staticcheck // shown as a sentence
	// ErrSteamKeyMissing means no key was given and none is stored.
	ErrSteamKeyMissing = errors.New("Enter your publisher Web API key to enable Steam sign-in.") //nolint:staticcheck // shown as a sentence
)

const steamAuthAuditAction = "control_panel.steam_auth.update"

// validateSteamAppID checks that the App ID is a decimal number.
func validateSteamAppID(s string) error {
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return ErrInvalidSteamAppID
	}
	return nil
}

// validateSteamKey checks the 32-hex-character publisher key format. The
// key is stored as the exact bytes Valve expects in the key= parameter.
func validateSteamKey(s string) error {
	if len(s) != steamWebAPIKeyHexLen {
		return ErrInvalidSteamKey
	}
	if _, err := hex.DecodeString(s); err != nil {
		return ErrInvalidSteamKey
	}
	return nil
}

// SetSteamAuth saves the Steam App ID and, when rawKey is not empty, replaces
// the stored Web API key. The key is sealed with cipher when cipher is set.
// An empty rawKey keeps the stored key, and is refused when none is stored.
// The audit row never holds the key. It returns pgx.ErrNoRows when the
// project does not exist.
func SetSteamAuth(ctx context.Context, pool *db.Pool, cipher *secretseal.Cipher, tenantID, projectID int64, appID, rawKey string, actor Actor) error {
	if err := validateSteamAppID(appID); err != nil {
		return err
	}
	var key []byte
	if rawKey != "" {
		if err := validateSteamKey(rawKey); err != nil {
			return err
		}
		key = []byte(rawKey)
		if cipher != nil {
			var err error
			if key, err = cipher.Encrypt(key); err != nil {
				return err
			}
		}
	}
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if key == nil {
			cfg, err := q.GetProjectSteamAuthConfig(ctx, projectID)
			if err != nil {
				return err
			}
			if len(cfg.SteamWebAPIKey) == 0 {
				return ErrSteamKeyMissing
			}
		}
		rows, err := q.UpdateProjectSteamAuthConfig(ctx, sqlcgen.UpdateProjectSteamAuthConfigParams{
			SteamAppID: appID, SteamWebAPIKey: key, ProjectID: projectID,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return pgx.ErrNoRows
		}
		return actor.Audit(ctx, tx, tenantID, projectID, steamAuthAuditAction, strconv.FormatInt(projectID, 10), map[string]any{
			"app_id":      appID,
			"key_changed": key != nil,
		})
	})
}

// ClearSteamAuth removes the Steam App ID and the stored Web API key. The key
// is lost. It returns pgx.ErrNoRows when the project does not exist.
func ClearSteamAuth(ctx context.Context, pool *db.Pool, tenantID, projectID int64, actor Actor) error {
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ClearProjectSteamAuthConfig(ctx, projectID)
		if err != nil {
			return err
		}
		if rows == 0 {
			return pgx.ErrNoRows
		}
		return actor.Audit(ctx, tx, tenantID, projectID, steamAuthAuditAction, strconv.FormatInt(projectID, 10), map[string]any{
			"cleared": true,
		})
	})
}
