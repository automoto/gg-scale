package billing

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

// keySecretName is the server_secrets row holding the auto-generated handoff
// HMAC key.
const keySecretName = "billing_handoff_key" //nolint:gosec // row name in server_secrets, not credential material

// LoadHandoffKey resolves the handoff HMAC key at boot. BILLING_HANDOFF_KEY
// (32-byte hex) wins when set and must then also be configured on the billing
// service; otherwise the stored key is used — generated on first boot, race-
// safe across instances (twofactor.Load pattern). Only in-flight upgrade
// links are invalidated by a key change.
func LoadHandoffKey(ctx context.Context, pool *db.Pool, envHexKey string) ([]byte, error) {
	if envHexKey != "" {
		key, err := hex.DecodeString(envHexKey)
		if err != nil {
			return nil, fmt.Errorf("billing handoff key: %w", err)
		}
		return key, nil
	}
	key, err := getStoredKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if key != nil {
		return key, nil
	}

	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("billing handoff key: %w", err)
	}
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).InsertServerSecret(ctx, sqlcgen.InsertServerSecretParams{
			Name:  keySecretName,
			Value: fresh,
		})
		return qerr
	}); err != nil {
		return nil, fmt.Errorf("billing handoff key: persist: %w", err)
	}
	// Read back rather than trusting our insert: a racing instance may have
	// won the ON CONFLICT, and everyone must converge on the same key.
	key, err = getStoredKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, errors.New("billing handoff key: missing after insert")
	}
	slog.Info("billing: generated a handoff key into server_secrets; the billing service needs the same key to verify upgrade tokens",
		"secret_name", keySecretName)
	return key, nil
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
		return nil, fmt.Errorf("billing handoff key: load: %w", err)
	}
	return value, nil
}
