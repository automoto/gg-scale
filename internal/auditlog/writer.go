// Package auditlog writes append-only audit_log rows. Callers pass an
// active pgx.Tx (typically obtained inside db.Pool.Q) so the audit row
// commits or rolls back atomically with the action it records.
//
// audit_log has GRANT INSERT, SELECT but no UPDATE/DELETE for the app
// role (migration 0007), so a compromised handler cannot rewrite history.
package auditlog

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// Write inserts a tenant-scoped row. actorUserID may be 0 — the column is
// nullable. payload is JSON-marshalled; pass nil to record an empty object.
func Write(ctx context.Context, tx pgx.Tx, actorUserID int64, action, target string, payload any) error {
	body := []byte("{}")
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("auditlog: marshal payload: %w", err)
		}
	}

	q := sqlcgen.New(tx)
	var actor *int64
	if actorUserID != 0 {
		actor = &actorUserID
	}
	var tgt *string
	if target != "" {
		t := target
		tgt = &t
	}
	if err := q.WriteAudit(ctx, sqlcgen.WriteAuditParams{
		ActorUserID: actor,
		Action:      action,
		Target:      tgt,
		Payload:     body,
	}); err != nil {
		return fmt.Errorf("auditlog: write: %w", err)
	}
	return nil
}

// WritePlatform inserts a platform-scoped row into platform_audit_log (no
// tenant FK). Use for dashboard login/logout and other platform events.
func WritePlatform(ctx context.Context, tx pgx.Tx, actorUserID int64, action, target string, payload any) error {
	body := []byte("{}")
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("auditlog: marshal payload: %w", err)
		}
	}

	q := sqlcgen.New(tx)
	var actor *int64
	if actorUserID != 0 {
		actor = &actorUserID
	}
	var tgt *string
	if target != "" {
		t := target
		tgt = &t
	}
	if err := q.WritePlatformAudit(ctx, sqlcgen.WritePlatformAuditParams{
		ActorUserID: actor,
		Action:      action,
		Target:      tgt,
		Payload:     body,
	}); err != nil {
		return fmt.Errorf("auditlog: write platform: %w", err)
	}
	return nil
}
