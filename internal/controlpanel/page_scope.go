package controlpanel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

const pageScopeContextKey contextKey = "control-panel-page-scope"

// pageScope holds the names shown in the page breadcrumb. Empty fields mean
// the name is not known and the breadcrumb leaves it out.
type pageScope struct {
	TenantID    int64
	TenantName  string
	ProjectID   int64
	ProjectName string
}

func withPageScope(ctx context.Context, s pageScope) context.Context {
	return withPageScopeLoader(ctx, func() pageScope { return s })
}

// withPageScopeLoader stores a loader that runs the first time a page reads
// the scope. Redirects and fragments never read it, so they skip the lookup.
func withPageScopeLoader(ctx context.Context, load func() pageScope) context.Context {
	return context.WithValue(ctx, pageScopeContextKey, sync.OnceValue(load))
}

func pageScopeFromContext(ctx context.Context) pageScope {
	load, ok := ctx.Value(pageScopeContextKey).(func() pageScope)
	if !ok {
		return pageScope{}
	}
	return load()
}

// projectIDFromRoutePath returns the project ID when the path starts with
// /projects/{id}. Any other path gives 0.
func projectIDFromRoutePath(routePath string) int64 {
	rest, ok := strings.CutPrefix(routePath, "/projects/")
	if !ok {
		return 0
	}
	segment, _, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(segment, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// pageScopeIDs reads the IDs inside the tenant route group. Middleware runs
// before chi matches {projectID}, so the project ID comes from the route path.
func pageScopeIDs(r *http.Request) (tenantID, projectID int64) {
	tenantID, _ = strconv.ParseInt(chi.URLParam(r, "tenantID"), 10, 64)
	if tenantID <= 0 {
		return 0, 0
	}
	if rc := chi.RouteContext(r.Context()); rc != nil {
		projectID = projectIDFromRoutePath(rc.RoutePath)
	}
	return tenantID, projectID
}

// loadPageScope gives the request a loader for the names that feed the page
// breadcrumb. The lookup runs only when a page draws the breadcrumb.
func (h *Handler) loadPageScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID, projectID := pageScopeIDs(r)
		if h.pool == nil || tenantID == 0 {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		load := func() pageScope { return h.lookupPageScope(ctx, tenantID, projectID) }
		next.ServeHTTP(w, r.WithContext(withPageScopeLoader(ctx, load)))
	})
}

// lookupPageScope reads the names. A failed lookup is logged and gives no
// names, so the page still renders.
func (h *Handler) lookupPageScope(ctx context.Context, tenantID, projectID int64) pageScope {
	var names sqlcgen.GetPageScopeNamesRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		names, err = sqlcgen.New(tx).GetPageScopeNames(ctx, sqlcgen.GetPageScopeNamesParams{
			ProjectID: projectID,
			TenantID:  tenantID,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pageScope{}
	}
	if err != nil {
		slog.WarnContext(ctx, "page scope load failed", "err", err)
		return pageScope{}
	}
	scope := pageScope{TenantID: tenantID, TenantName: names.TenantName}
	if names.ProjectName != nil {
		scope.ProjectID = projectID
		scope.ProjectName = *names.ProjectName
	}
	return scope
}
