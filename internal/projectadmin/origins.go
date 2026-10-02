package projectadmin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// OriginCacheTTL is how long a saved origin list takes to reach each app
// host: each host caches the origins of all projects for this long.
const OriginCacheTTL = 30 * time.Second

// ErrInvalidOrigins wraps every origin validation failure. The wrapped
// message is safe to show the person or the agent.
var ErrInvalidOrigins = errors.New("invalid allowed origins")

func invalidOrigin(raw, why string) error {
	return fmt.Errorf("%w: %q %s", ErrInvalidOrigins, raw, why)
}

// NormalizeOrigin checks one browser origin and returns it in the form a
// browser sends: lower-case scheme://host[:port], with no default port. It
// refuses a path, query, fragment, user info, and wildcards.
func NormalizeOrigin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "*") {
		return "", invalidOrigin(raw, "must not contain a wildcard")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", invalidOrigin(raw, "must be scheme://host or scheme://host:port")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", invalidOrigin(raw, "must use http or https")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", invalidOrigin(raw, "must have no path, query, fragment, or user info")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", invalidOrigin(raw, "must have a host")
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", invalidOrigin(raw, "has an invalid port")
		}
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return scheme + "://" + host, nil
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

// NormalizeOrigins checks a list, drops blank lines and duplicates, and
// refuses more than limit origins.
func NormalizeOrigins(raw []string, limit int) ([]string, error) {
	out := []string{}
	for _, r := range raw {
		if strings.TrimSpace(r) == "" {
			continue
		}
		o, err := NormalizeOrigin(r)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	if len(out) > limit {
		return nil, fmt.Errorf("%w: a Game Project can have at most %d origins", ErrInvalidOrigins, limit)
	}
	return out, nil
}

// SetAllowedOrigins replaces the project's origin list and returns the old
// list, so a wrong value can be put back. There is no revision history for
// origins. It returns pgx.ErrNoRows when the project does not exist.
func SetAllowedOrigins(ctx context.Context, pool *db.Pool, tenantID, projectID int64, raw []string, limit int, actor Actor) ([]string, error) {
	origins, err := NormalizeOrigins(raw, limit)
	if err != nil {
		return nil, err
	}
	var old []string
	ctx = db.WithTenant(ctx, tenantID)
	err = pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if old, err = q.GetAllowedOriginsForUpdate(ctx, projectID); err != nil {
			return err
		}
		if err := q.SetAllowedOrigins(ctx, sqlcgen.SetAllowedOriginsParams{Origins: origins, ProjectID: projectID}); err != nil {
			return err
		}
		return actor.audit(ctx, tx, tenantID, projectID, "project.allowed_origins.update", strconv.FormatInt(projectID, 10), map[string]any{
			"old_origins": old,
			"new_origins": origins,
		})
	})
	return old, err
}

// AllowedOrigins returns the project's origin list.
func AllowedOrigins(ctx context.Context, pool *db.Pool, tenantID, projectID int64) ([]string, error) {
	var out []string
	ctx = db.WithTenant(ctx, tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = sqlcgen.New(tx).GetAllowedOrigins(ctx, projectID)
		return err
	})
	return out, err
}

// AllProjectOrigins returns the distinct origins of all live projects, for
// the process-wide CORS and WebSocket origin check. It runs with no tenant
// set through a SECURITY DEFINER function that returns origins only.
func AllProjectOrigins(ctx context.Context, pool *db.Pool) ([]string, error) {
	var out []string
	err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = sqlcgen.New(tx).ListAllProjectAllowedOrigins(ctx)
		return err
	})
	return out, err
}
