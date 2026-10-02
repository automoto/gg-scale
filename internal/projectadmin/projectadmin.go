// Package projectadmin holds the project settings writes that the dashboard
// and MCP tokens share. A function does no authorization: the caller checks
// permissions first. Each write does the change, its revision row, and its
// audit row in one tenant-scoped transaction.
package projectadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// Resource kinds in settings_revisions.
const (
	KindRemoteConfig = "remote_config"
	KindLeaderboard  = "leaderboard"
)

const (
	sourceDashboard = "dashboard"
	sourceMCP       = "mcp"
	sourceRollback  = "rollback"

	// keepRevisions is the number of revisions kept for each resource,
	// the base row included.
	keepRevisions = 3
)

var (
	// ErrRevisionConflict means the caller's expected revision is not the
	// newest revision of the resource.
	ErrRevisionConflict = errors.New("projectadmin: expected revision is not the newest revision")
	// ErrRevisionNotFound means the revision is not kept for the resource.
	ErrRevisionNotFound = errors.New("projectadmin: revision not found")
)

// Actor is who made a change. Set UserID for a dashboard user, or TokenID
// and TokenCreatorID for an MCP token.
type Actor struct {
	UserID         int64
	TokenID        int64
	TokenCreatorID int64
}

func (a Actor) source() string {
	if a.TokenID != 0 {
		return sourceMCP
	}
	return sourceDashboard
}

// audit writes the platform audit row inside tx, so it commits or rolls back
// with the change.
func (a Actor) audit(ctx context.Context, tx pgx.Tx, tenantID, projectID int64, action, target string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["tenant_id"] = tenantID
	if projectID != 0 {
		payload["project_id"] = projectID
	}
	if a.TokenID == 0 {
		return auditlog.WritePlatform(ctx, tx, a.UserID, action, target, payload)
	}
	payload["created_by_user_id"] = a.TokenCreatorID
	return auditlog.WritePlatformService(ctx, tx, "mcp_token:"+strconv.FormatInt(a.TokenID, 10), action, target, payload)
}

// Revision is one kept settings revision.
type Revision struct {
	Revision    int64
	Snapshot    json.RawMessage
	ActorUserID *int64
	TokenID     *int64
	Source      string
	CreatedAt   time.Time
}

// ListRevisions returns the kept revisions of one resource, newest first.
func ListRevisions(ctx context.Context, pool *db.Pool, tenantID, projectID int64, kind string, resourceID int64) ([]Revision, error) {
	var out []Revision
	err := pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListSettingsRevisions(ctx, sqlcgen.ListSettingsRevisionsParams{
			ProjectID: projectID, ResourceKind: kind, ResourceID: resourceID,
		})
		if err != nil {
			return err
		}
		out = make([]Revision, 0, len(rows))
		for _, r := range rows {
			out = append(out, Revision{
				Revision:    r.Revision,
				Snapshot:    r.Snapshot,
				ActorUserID: r.ActorUserID,
				TokenID:     r.McpTokenID,
				Source:      r.Source,
				CreatedAt:   r.CreatedAt.Time,
			})
		}
		return nil
	})
	return out, err
}

type revisionWrite struct {
	projectID  int64
	kind       string
	resourceID int64
	// before is the value before the change. It becomes the base row when
	// the resource has no revision yet. Nil for a create.
	before   []byte
	after    []byte
	expected *int64
	actor    Actor
	source   string
}

// checkRevision returns the newest revision number, or ErrRevisionConflict
// when expected is set and differs. Call it after the resource row is locked.
func checkRevision(ctx context.Context, q *sqlcgen.Queries, w revisionWrite) (int64, error) {
	latest, err := q.LatestSettingsRevision(ctx, sqlcgen.LatestSettingsRevisionParams{
		ProjectID: w.projectID, ResourceKind: w.kind, ResourceID: w.resourceID,
	})
	if err != nil {
		return 0, err
	}
	if w.expected != nil && *w.expected != latest {
		return 0, ErrRevisionConflict
	}
	return latest, nil
}

// recordRevision writes the base row if needed, the new revision, and drops
// rows older than the newest keepRevisions. It returns the new revision.
func recordRevision(ctx context.Context, q *sqlcgen.Queries, latest int64, w revisionWrite) (int64, error) {
	insert := func(rev int64, snapshot []byte, actor Actor, source string) error {
		p := sqlcgen.InsertSettingsRevisionParams{
			ProjectID: w.projectID, ResourceKind: w.kind, ResourceID: w.resourceID,
			Revision: rev, Snapshot: snapshot, Source: source,
		}
		if actor.UserID != 0 {
			p.ActorUserID = &actor.UserID
		}
		if actor.TokenID != 0 {
			p.McpTokenID = &actor.TokenID
		}
		return q.InsertSettingsRevision(ctx, p)
	}
	if latest == 0 && w.before != nil {
		// The base row holds a value that no tracked actor wrote.
		if err := insert(1, w.before, Actor{}, w.source); err != nil {
			return 0, err
		}
		latest = 1
	}
	next := latest + 1
	if err := insert(next, w.after, w.actor, w.source); err != nil {
		return 0, err
	}
	err := q.PruneSettingsRevisions(ctx, sqlcgen.PruneSettingsRevisionsParams{
		ProjectID: w.projectID, ResourceKind: w.kind, ResourceID: w.resourceID,
		BelowOrAt: next - keepRevisions,
	})
	return next, err
}

func loadRevision(ctx context.Context, q *sqlcgen.Queries, projectID int64, kind string, resourceID, revision int64) ([]byte, error) {
	snapshot, err := q.GetSettingsRevision(ctx, sqlcgen.GetSettingsRevisionParams{
		ProjectID: projectID, ResourceKind: kind, ResourceID: resourceID, Revision: revision,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionNotFound
	}
	return snapshot, err
}

// SetRemoteConfig replaces the project's remote config. config must already
// be a validated JSON object. expected, when set, must be the newest
// revision number. It returns pgx.ErrNoRows when the project does not exist.
func SetRemoteConfig(ctx context.Context, pool *db.Pool, tenantID, projectID int64, config []byte, expected *int64, actor Actor) (int64, error) {
	return writeRemoteConfig(ctx, pool, tenantID, projectID, expected, actor, actor.source(),
		"control_panel.remote_config.update",
		func(*sqlcgen.Queries) ([]byte, error) { return config, nil })
}

// RollbackRemoteConfig applies a kept revision as a new revision.
func RollbackRemoteConfig(ctx context.Context, pool *db.Pool, tenantID, projectID, revision int64, expected *int64, actor Actor) (int64, error) {
	return writeRemoteConfig(ctx, pool, tenantID, projectID, expected, actor, sourceRollback,
		"control_panel.remote_config.rollback",
		func(q *sqlcgen.Queries) ([]byte, error) {
			return loadRevision(ctx, q, projectID, KindRemoteConfig, projectID, revision)
		})
}

func writeRemoteConfig(ctx context.Context, pool *db.Pool, tenantID, projectID int64, expected *int64, actor Actor, source, action string, value func(*sqlcgen.Queries) ([]byte, error)) (int64, error) {
	var rev int64
	ctx = db.WithTenant(ctx, tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		before, err := q.GetRemoteConfigForUpdate(ctx, projectID)
		if err != nil {
			return err
		}
		w := revisionWrite{
			projectID: projectID, kind: KindRemoteConfig, resourceID: projectID,
			before: before, expected: expected, actor: actor, source: source,
		}
		latest, err := checkRevision(ctx, q, w)
		if err != nil {
			return err
		}
		if w.after, err = value(q); err != nil {
			return err
		}
		if _, err := q.UpdateRemoteConfig(ctx, sqlcgen.UpdateRemoteConfigParams{
			RemoteConfig: w.after, ProjectID: projectID,
		}); err != nil {
			return err
		}
		if rev, err = recordRevision(ctx, q, latest, w); err != nil {
			return err
		}
		hash := sha256.Sum256(w.after)
		return actor.audit(ctx, tx, tenantID, projectID, action, strconv.FormatInt(projectID, 10), map[string]any{
			"config_bytes": len(w.after),
			"config_hash":  hex.EncodeToString(hash[:]),
			"revision":     rev,
		})
	})
	return rev, err
}
