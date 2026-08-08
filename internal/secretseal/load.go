package secretseal

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// keySecretName is the server_secrets row holding the auto-generated
// credential encryption key.
const keySecretName = "credential_enc_key" //nolint:gosec // row name in server_secrets, not credential material

// Load resolves the credential-sealing key at boot, mirroring twofactor.Load.
// With CREDENTIAL_ENC_KEY set, that key is primary; a previously
// auto-generated database key is kept as a decrypt-only fallback. With no env
// key, the database key is used — generated and persisted on first boot, so
// sealing works with zero configuration. Generation is race-safe across
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
		slog.Info("secretseal: CREDENTIAL_ENC_KEY active; database key kept for decrypting older values")
		return NewCipherKeyring(envKey, dbKey)
	case dbKey != nil:
		slog.Info("secretseal: using the database-stored key; set CREDENTIAL_ENC_KEY to keep it out of DB backups")
		return NewCipherKeyring(dbKey)
	}

	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("secretseal key: %w", err)
	}
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).InsertServerSecret(ctx, sqlcgen.InsertServerSecretParams{
			Name:  keySecretName,
			Value: fresh,
		})
		return qerr
	}); err != nil {
		return nil, fmt.Errorf("secretseal key: persist: %w", err)
	}
	// Read back rather than trusting our insert: a racing instance may have
	// won the ON CONFLICT, and everyone must converge on the same key.
	dbKey, err = getStoredKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if dbKey == nil {
		return nil, errors.New("secretseal key: missing after insert")
	}
	slog.Info("secretseal: generated a key into the database; set CREDENTIAL_ENC_KEY to keep it out of DB backups")
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
		return nil, fmt.Errorf("secretseal key: load: %w", err)
	}
	return value, nil
}
