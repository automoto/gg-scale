package verifycode

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

const (
	// SigningKeySize is the required email-verification cookie HMAC key size.
	SigningKeySize = 32

	signingKeySecretName = "email_verify_signing_key_v1" //nolint:gosec // row name, not credential material
)

// LoadSigningKey resolves the shared player/control-panel verification-cookie
// key. An external key wins and leaves no database copy. Otherwise the first
// process generates a versioned server_secrets row and concurrent processes
// read back the same winning value.
func LoadSigningKey(ctx context.Context, pool *db.Pool, envHex string) ([]byte, error) {
	if envHex != "" {
		return decodeSigningKey(envHex)
	}

	stored, err := getStoredSigningKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if len(stored) != SigningKeySize {
			return nil, fmt.Errorf("email verify signing key: stored key has %d bytes, want %d", len(stored), SigningKeySize)
		}
		slog.Info("email verification: using the database-stored signing key; set EMAIL_VERIFY_SIGNING_KEY to keep it out of DB backups")
		return stored, nil
	}

	fresh := make([]byte, SigningKeySize)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("email verify signing key: generate: %w", err)
	}
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).InsertServerSecret(ctx, sqlcgen.InsertServerSecretParams{
			Name:  signingKeySecretName,
			Value: fresh,
		})
		return qerr
	}); err != nil {
		return nil, fmt.Errorf("email verify signing key: persist: %w", err)
	}

	stored, err = getStoredSigningKey(ctx, pool)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, errors.New("email verify signing key: missing after insert")
	}
	if len(stored) != SigningKeySize {
		return nil, fmt.Errorf("email verify signing key: stored key has %d bytes, want %d", len(stored), SigningKeySize)
	}
	slog.Info("email verification: generated a signing key into the database; set EMAIL_VERIFY_SIGNING_KEY to keep it out of DB backups")
	return stored, nil
}

func decodeSigningKey(raw string) ([]byte, error) {
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("email verify signing key: decode: %w", err)
	}
	if len(key) != SigningKeySize {
		return nil, fmt.Errorf("email verify signing key: got %d bytes, want %d", len(key), SigningKeySize)
	}
	return key, nil
}

func getStoredSigningKey(ctx context.Context, pool *db.Pool) ([]byte, error) {
	var value []byte
	err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var queryErr error
		value, queryErr = sqlcgen.New(tx).GetServerSecret(ctx, signingKeySecretName)
		return queryErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("email verify signing key: load: %w", err)
	}
	return value, nil
}
