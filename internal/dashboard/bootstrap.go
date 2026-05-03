package dashboard

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// Bootstrap tracks the first-run dashboard setup token.
type Bootstrap struct {
	mu      sync.Mutex
	pending bool
	hash    [32]byte
}

// NewBootstrap returns a pending bootstrap guard for token.
func NewBootstrap(token string) *Bootstrap {
	return &Bootstrap{pending: true, hash: sha256.Sum256([]byte(token))}
}

// DisabledBootstrap returns a bootstrap guard with setup disabled.
func DisabledBootstrap() *Bootstrap {
	return &Bootstrap{}
}

// Pending reports whether first-run setup is still available.
func (b *Bootstrap) Pending() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

func (b *Bootstrap) tokenMatches(token string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.pending {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], b.hash[:]) == 1
}

func (b *Bootstrap) complete() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = false
}

// LoadBootstrap builds a first-run bootstrap guard from the dashboard users
// table. When no users exist it logs a one-time token and optionally writes it
// to tokenFile with owner-only permissions.
func LoadBootstrap(ctx context.Context, pool *db.Pool, tokenFile string, logger *slog.Logger) (*Bootstrap, error) {
	if pool == nil {
		return DisabledBootstrap(), nil
	}
	var count int64
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		count, err = sqlcgen.New(tx).CountDashboardUsers(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("dashboard bootstrap count users: %w", err)
	}
	if count > 0 {
		return DisabledBootstrap(), nil
	}

	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	if tokenFile != "" {
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write dashboard bootstrap token file: %w", err)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("dashboard bootstrap token generated", "path", "/v1/dashboard/setup", "token", token, "token_file", tokenFile)
	return NewBootstrap(token), nil
}
