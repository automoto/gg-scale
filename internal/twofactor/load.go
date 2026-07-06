package twofactor

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

// keySecretName is the server_secrets row holding the auto-generated
// two-factor encryption key.
const keySecretName = "two_factor_enc_key" //nolint:gosec // row name in server_secrets, not credential material

// Load resolves the two-factor key material at boot. With TWO_FACTOR_ENC_KEY
// set, that key is primary; a previously auto-generated database key is kept
// as a decrypt-only fallback so older enrollments survive the switch. With no
// env key, the database key is used — generated and persisted on first boot,
// so 2FA works with zero configuration. Generation is race-safe across
// instances (INSERT ... ON CONFLICT DO NOTHING, then read back the winner).
func Load(ctx context.Context, pool *db.Pool, envHexKey string) (*Cipher, error) {
	var envKey []byte
	if envHexKey != "" {
		k, err := decodeHexKey(envHexKey)
		if err != nil {
			return nil, err
		}
		envKey = k
	}
	dbKey, err := getStoredKey(ctx, pool)
	if err != nil {
		return nil, err
	}

	switch {
	case envKey != nil && dbKey == nil:
		// Operator opted for env-only key storage; never create a DB key.
		return NewCipherKeyring(envKey)
	case envKey != nil:
		slog.Info("two-factor: TWO_FACTOR_ENC_KEY active; database key kept for decrypting older enrollments")
		return NewCipherKeyring(envKey, dbKey)
	case dbKey != nil:
		slog.Info("two-factor: using the database-stored key; set TWO_FACTOR_ENC_KEY to keep it out of DB backups")
		return NewCipherKeyring(dbKey)
	}

	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("two-factor key: %w", err)
	}
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).InsertServerSecret(ctx, sqlcgen.InsertServerSecretParams{
			Name:  keySecretName,
			Value: fresh,
		})
		return qerr
	}); err != nil {
		return nil, fmt.Errorf("two-factor key: persist: %w", err)
	}
	// Read back rather than trusting our insert: a racing instance may have
	// won the ON CONFLICT, and everyone must converge on the same key.
	dbKey, err = getStoredKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if dbKey == nil {
		return nil, errors.New("two-factor key: missing after insert")
	}
	slog.Info("two-factor: generated a key into the database; set TWO_FACTOR_ENC_KEY to keep it out of DB backups")
	return NewCipherKeyring(dbKey)
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
		return nil, fmt.Errorf("two-factor key: load: %w", err)
	}
	return value, nil
}
