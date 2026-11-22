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

// ValidateSteamAppID checks that the App ID is a decimal number.
func ValidateSteamAppID(s string) error {
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return ErrInvalidSteamAppID
	}
	return nil
}

// ValidateSteamWebAPIKey checks the 32-hex-character publisher key format and
// returns the key as the exact bytes Valve expects in the key= parameter.
func ValidateSteamWebAPIKey(s string) ([]byte, error) {
	if len(s) != steamWebAPIKeyHexLen {
		return nil, ErrInvalidSteamKey
	}
	if _, err := hex.DecodeString(s); err != nil {
		return nil, ErrInvalidSteamKey
	}
	return []byte(s), nil
}

// SetSteamAuth saves the Steam App ID and, when rawKey is not empty, replaces
// the stored Web API key. The key is sealed with cipher when cipher is set.
// An empty rawKey keeps the stored key, and is refused when none is stored.
// The audit row never holds the key. It returns whether the key changed, and
// pgx.ErrNoRows when the project does not exist.
func SetSteamAuth(ctx context.Context, pool *db.Pool, cipher *secretseal.Cipher, tenantID, projectID int64, appID, rawKey string, actor Actor) (bool, error) {
	if err := ValidateSteamAppID(appID); err != nil {
		return false, err
	}
	var key []byte
	if rawKey != "" {
		var err error
		if key, err = ValidateSteamWebAPIKey(rawKey); err != nil {
			return false, err
		}
		if cipher != nil {
			if key, err = cipher.Encrypt(key); err != nil {
				return false, err
			}
		}
	}
	ctx = db.WithTenant(ctx, tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
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
		return actor.audit(ctx, tx, tenantID, projectID, steamAuthAuditAction, strconv.FormatInt(projectID, 10), map[string]any{
			"app_id":      appID,
			"key_changed": key != nil,
		})
	})
	return key != nil, err
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
		return actor.audit(ctx, tx, tenantID, projectID, steamAuthAuditAction, strconv.FormatInt(projectID, 10), map[string]any{
			"cleared": true,
		})
	})
}
