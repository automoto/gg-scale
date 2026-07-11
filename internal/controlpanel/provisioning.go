package controlpanel

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
	"github.com/ggscale/ggscale/internal/tenant"
)

var (
	errInvalidSignup        = errors.New("control panel: tenant name, project name, and password are required")
	errBootstrapUnavailable = errors.New("control panel: bootstrap unavailable")
	errDuplicateTenantName  = errors.New("control panel: a tenant with that name already exists")
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
		return signupResult{}, errors.New(msgControlPanelPoolNeeded)
	}

	// The bootstrap tenant-creation key is a secret key (server-side use).
	apiKey, err := randomAPIKey(tenant.KeyTypeSecret)
	if err != nil {
		return signupResult{}, err
	}
	sum := sha256.Sum256([]byte(apiKey))

	var row sqlcgen.ControlPanelCreateTenantRow
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).ControlPanelCreateTenant(ctx, sqlcgen.ControlPanelCreateTenantParams{
			ActorUserID: in.ActorUserID,
			TenantName:  in.TenantName,
			ProjectName: in.ProjectName,
			KeyHash:     sum[:],
			KeyLabel:    in.KeyLabel,
		})
		if err != nil {
			return fmt.Errorf("control panel create tenant: %w", err)
		}
		if h.rbac != nil {
			if err := h.rbac.SetControlPanelMembershipRoleTx(ctx, tx, in.ActorUserID, row.TenantID, roleOwner); err != nil {
				return fmt.Errorf("rbac tenant owner: %w", err)
			}
			if err := h.rbac.AddAPIKeyRoleTx(ctx, tx, row.ApiKeyID, row.TenantID, tenant.KeyTypeSecret); err != nil {
				return fmt.Errorf("rbac bootstrap api key: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return signupResult{}, errDuplicateTenantName
		}
		return signupResult{}, err
	}
	h.reloadRBACPolicy(ctx)

	return signupResult{
		TenantID:  row.TenantID,
		ProjectID: row.ProjectID,
		APIKeyID:  row.ApiKeyID,
		APIKey:    apiKey,
	}, nil
}

// randomAPIKey mints a fresh plaintext API key with a Stripe-style
// type-indicating prefix: ggp_ for publishable, ggs_ for secret. The
// prefix is part of the stored value (the caller hashes the whole string),
// so it eases log-grep and accidental-leak detection without affecting
// server policy.
func randomAPIKey(keyType tenant.KeyType) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("control panel api key rand: %w", err)
	}
	prefix := "ggs_"
	if keyType == tenant.KeyTypePublishable {
		prefix = "ggp_"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
