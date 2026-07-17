package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AssertAppRole verifies that a runtime pool is operating as ggscale_app.
// Production additionally rejects a login identity that could recover tenant
// table ownership after RESET ROLE. Development may deliberately use one owner
// credential for migrations and runtime.
func AssertAppRole(ctx context.Context, pool *pgxpool.Pool, requireUnprivilegedSession bool) error {
	var currentUser string
	var sessionUser string
	var ownsTenantTables bool
	var sessionCanAssumeTenantOwner bool
	if err := pool.QueryRow(ctx, `
SELECT current_user,
       session_user,
       EXISTS (
           SELECT 1
           FROM pg_class c
           JOIN pg_namespace n ON n.oid = c.relnamespace
           JOIN pg_roles r ON r.oid = c.relowner
           WHERE n.nspname = 'public'
             AND c.relkind IN ('r', 'p')
             AND c.relname IN ('tenants', 'projects', 'api_keys', 'project_players', 'sessions')
             AND r.rolname = current_user
       ),
       EXISTS (
           SELECT 1
           FROM pg_class c
           JOIN pg_namespace n ON n.oid = c.relnamespace
           JOIN pg_roles r ON r.oid = c.relowner
           WHERE n.nspname = 'public'
             AND c.relkind IN ('r', 'p')
             AND c.relname IN ('tenants', 'projects', 'api_keys', 'project_players', 'sessions')
             AND (
                 r.rolname = session_user
                 OR pg_has_role(session_user, r.rolname, 'MEMBER')
             )
       )`).Scan(&currentUser, &sessionUser, &ownsTenantTables, &sessionCanAssumeTenantOwner); err != nil {
		return fmt.Errorf("db role assertion: %w", err)
	}
	if currentUser != "ggscale_app" {
		return fmt.Errorf("db role assertion: current_user is %q, want ggscale_app", currentUser)
	}
	if ownsTenantTables {
		return fmt.Errorf("db role assertion: ggscale_app must not own tenant tables")
	}
	if requireUnprivilegedSession && sessionCanAssumeTenantOwner {
		return fmt.Errorf("db role assertion: session_user %q can assume a tenant-table owner role", sessionUser)
	}
	return nil
}
