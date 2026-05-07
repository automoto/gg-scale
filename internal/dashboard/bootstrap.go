package dashboard

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// Bootstrap tracks the first-run dashboard setup token.
type Bootstrap struct {
	mu            sync.Mutex
	pending       bool
	hash          [32]byte
	tokenFilePath string
}

// NewBootstrap returns a pending bootstrap guard for token. tokenFilePath is
// the on-disk location where the token was written (empty if it was emitted
// to stderr); it is exposed so the setup UI can tell the operator where to
// read the token from.
func NewBootstrap(token, tokenFilePath string) *Bootstrap {
	return &Bootstrap{
		pending:       true,
		hash:          sha256.Sum256([]byte(token)),
		tokenFilePath: tokenFilePath,
	}
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

// TokenFilePath returns the on-disk path the bootstrap token was written to,
// or "" if it was emitted to stderr or the bootstrap is nil.
func (b *Bootstrap) TokenFilePath() string {
	if b == nil {
		return ""
	}
	return b.tokenFilePath
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

// emitBootstrapToken writes the token to tokenFile (owner-only) or to w, and
// logs the delivery location — never the token value itself.
func emitBootstrapToken(token, tokenFile string, logger *slog.Logger, w io.Writer) error {
	if tokenFile != "" {
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			return fmt.Errorf("write dashboard bootstrap token file: %w", err)
		}
		logger.Warn("dashboard bootstrap token written to file",
			"path", "/v1/dashboard/setup",
			"token_file", tokenFile)
		return nil
	}
	logger.Warn("dashboard bootstrap token written to stderr — set DASHBOARD_BOOTSTRAP_TOKEN_FILE to avoid this",
		"path", "/v1/dashboard/setup")
	_, _ = fmt.Fprintf(w, "\n  ggscale bootstrap token: %s\n\n", token)
	return nil
}

// LoadBootstrap builds a first-run bootstrap guard from the dashboard users
// table. When no users exist it emits a one-time token to tokenFile (if set)
// or stderr, then returns a pending Bootstrap.
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
	if logger == nil {
		logger = slog.Default()
	}
	if err := emitBootstrapToken(token, tokenFile, logger, os.Stderr); err != nil {
		return nil, err
	}
	return NewBootstrap(token, tokenFile), nil
}
