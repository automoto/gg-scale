package entitlement

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// tokenSecretName is the server_secrets row holding the auto-generated
// entitlement API bearer token.
const tokenSecretName = "entitlement_api_token" //nolint:gosec // row name in server_secrets, not credential material

// LoadToken resolves the entitlement API bearer token at boot. With
// ENTITLEMENT_API_TOKEN set, that value wins and the database is never
// touched. With no env token, the stored token is used — generated and
// persisted on first boot so the API works with zero configuration.
// Generation is race-safe across instances (INSERT ... ON CONFLICT DO
// NOTHING, then read back the winner). Same pattern as twofactor.Load.
func LoadToken(ctx context.Context, pool *db.Pool, envToken string) (string, error) {
	if envToken != "" {
		return envToken, nil
	}
	stored, err := getStoredToken(ctx, pool)
	if err != nil {
		return "", err
	}
	if stored != "" {
		slog.Info("entitlement api: using the database-stored token; set ENTITLEMENT_API_TOKEN to keep it out of DB backups")
		return stored, nil
	}

	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return "", fmt.Errorf("entitlement token: %w", err)
	}
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).InsertServerSecret(ctx, sqlcgen.InsertServerSecretParams{
			Name:  tokenSecretName,
			Value: []byte(hex.EncodeToString(fresh)),
		})
		return qerr
	}); err != nil {
		return "", fmt.Errorf("entitlement token: persist: %w", err)
	}
	// Read back rather than trusting our insert: a racing instance may have
	// won the ON CONFLICT, and everyone must converge on the same token.
	stored, err = getStoredToken(ctx, pool)
	if err != nil {
		return "", err
	}
	if stored == "" {
		return "", errors.New("entitlement token: missing after insert")
	}
	slog.Info("entitlement api: generated a bearer token into server_secrets; the billing service presents it as Authorization: Bearer <token>",
		"secret_name", tokenSecretName)
	return stored, nil
}

func getStoredToken(ctx context.Context, pool *db.Pool) (string, error) {
	var value []byte
	err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		value, qerr = sqlcgen.New(tx).GetServerSecret(ctx, tokenSecretName)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("entitlement token: load: %w", err)
	}
	return string(value), nil
}
