package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

var (
	errInvalidSignup        = errors.New("dashboard: tenant name, project name, and password are required")
	errBootstrapUnavailable = errors.New("dashboard: bootstrap unavailable")
)

type signupInput struct {
	ActorUserID int64
	TenantName  string
	ProjectName string
	KeyLabel    string
}

type signupResult struct {
	TenantID  int64
	ProjectID int64
	APIKeyID  int64
	APIKey    string
}

func (h *Handler) createTenant(ctx context.Context, in signupInput) (signupResult, error) {
	in.TenantName = strings.TrimSpace(in.TenantName)
	in.ProjectName = strings.TrimSpace(in.ProjectName)
	in.KeyLabel = strings.TrimSpace(in.KeyLabel)
	if in.ActorUserID <= 0 || in.TenantName == "" || in.ProjectName == "" {
		return signupResult{}, errInvalidSignup
	}
	if h.pool == nil {
		return signupResult{}, errors.New("dashboard: database pool is required")
	}

	apiKey, err := randomAPIKey()
	if err != nil {
		return signupResult{}, err
	}
	sum := sha256.Sum256([]byte(apiKey))

	var row sqlcgen.DashboardCreateTenantRow
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).DashboardCreateTenant(ctx, sqlcgen.DashboardCreateTenantParams{
			ActorUserID: in.ActorUserID,
			TenantName:  in.TenantName,
			ProjectName: in.ProjectName,
			KeyHash:     sum[:],
			KeyLabel:    in.KeyLabel,
		})
		if err != nil {
			return fmt.Errorf("dashboard create tenant: %w", err)
		}
		return nil
	})
	if err != nil {
		return signupResult{}, err
	}

	return signupResult{
		TenantID:  row.TenantID,
		ProjectID: row.ProjectID,
		APIKeyID:  row.ApiKeyID,
		APIKey:    apiKey,
	}, nil
}

func randomAPIKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("dashboard api key rand: %w", err)
	}
	return "ggs_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
