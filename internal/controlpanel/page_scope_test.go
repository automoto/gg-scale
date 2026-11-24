package controlpanel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
)

func TestProjectIDFromRoutePath(t *testing.T) {
	tests := []struct {
		path string
		want int64
	}{
		{"/projects/4/settings", 4},
		{"/projects/4", 4},
		{"/projects/new", 0},
		{"/projects", 0},
		{"/projects/0/settings", 0},
		{"/projects/-3/settings", 0},
		{"/rate-limits/projects/4/invites", 0},
		{"/settings", 0},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, projectIDFromRoutePath(tt.path))
		})
	}
}

func TestPageScopeIDs_should_read_ids_through_the_tenant_route(t *testing.T) {
	tests := []struct {
		url  string
		want [2]int64
	}{
		{"/tenants/7/projects/4/settings", [2]int64{7, 4}},
		{"/tenants/7/settings", [2]int64{7, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			var got [2]int64
			noop := func(http.ResponseWriter, *http.Request) {}
			r := chi.NewRouter()
			r.Route("/tenants/{tenantID}", func(r chi.Router) {
				r.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						got[0], got[1] = pageScopeIDs(req)
						next.ServeHTTP(w, req)
					})
				})
				r.Get("/projects/{projectID}/settings", noop)
				r.Get("/settings", noop)
			})

			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.url, nil))

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPageScopeFromContext_should_return_zero_without_scope(t *testing.T) {
	assert.Equal(t, pageScope{}, pageScopeFromContext(context.Background()))
}
