package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// keySecretName is the server_secrets row holding the auto-generated JWT
// signing key.
const keySecretName = "jwt_signing_key" //nolint:gosec // row name in server_secrets, not credential material

// Load resolves the JWT signing key at boot. With JWT_SIGNING_KEY set, that
// key is used and the database is never touched. With no env key, the
// database key is used — generated and persisted on first boot, so player
// sessions survive restarts with zero configuration. Generation is race-safe
// across instances (INSERT ... ON CONFLICT DO NOTHING, then read back the
// winner). Unlike the two-factor key there is no keyring: switching to an
// env key invalidates in-flight access tokens, and clients recover through
// the refresh flow within the token TTL.
func Load(ctx context.Context, pool *db.Pool, envHex string) (*Signer, error) {
	if envHex != "" {
		return NewSignerFromHex(envHex)
	}

	dbKey, err := getStoredKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if dbKey != nil {
		slog.Info("auth: using the database-stored JWT signing key; set JWT_SIGNING_KEY to keep it out of DB backups")
		return NewSigner(dbKey)
	}

	fresh := make([]byte, minKeyLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("auth: signing key: %w", err)
	}
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).InsertServerSecret(ctx, sqlcgen.InsertServerSecretParams{
			Name:  keySecretName,
			Value: fresh,
		})
		return qerr
	}); err != nil {
		return nil, fmt.Errorf("auth: signing key: persist: %w", err)
	}
	// Read back rather than trusting our insert: a racing instance may have
	// won the ON CONFLICT, and everyone must converge on the same key.
	dbKey, err = getStoredKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if dbKey == nil {
		return nil, errors.New("auth: signing key: missing after insert")
	}
	slog.Info("auth: generated a JWT signing key into the database; set JWT_SIGNING_KEY to keep it out of DB backups")
	return NewSigner(dbKey)
}

func getStoredKey(ctx context.Context, pool *db.Pool) ([]byte, error) {
	var value []byte
	err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		value, qerr = sqlcgen.New(tx).GetServerSecret(ctx, keySecretName)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: signing key: load: %w", err)
	}
	return value, nil
}
