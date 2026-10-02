package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/automoto/gg-scale/internal/projectadmin"
)

// originSet decides which browser origins may call the API and open the
// realtime WebSocket. An origin is allowed when CORS_ALLOWED_ORIGINS lists it
// or any live project lists it. A preflight carries no API key, so the check
// cannot be per project; the API key is the access control.
type originSet struct {
	env      []string
	wildcard bool
	load     func(context.Context) ([]string, error)
	now      func() time.Time

	mu       sync.Mutex
	projects map[string]bool
	loadedAt time.Time
}

func newOriginSet(env []string, load func(context.Context) ([]string, error)) *originSet {
	return &originSet{
		env:      env,
		wildcard: slices.Contains(env, "*"),
		load:     load,
		now:      time.Now,
	}
}

// allowOrigin has the signature of cors.Options.AllowOriginFunc and
// realtime.Options.AllowOrigin. A nil set allows nothing extra.
func (s *originSet) allowOrigin(r *http.Request, origin string) bool {
	return s != nil && s.allowed(r.Context(), origin)
}

func (s *originSet) allowed(ctx context.Context, origin string) bool {
	if s.wildcard || slices.Contains(s.env, origin) {
		return true
	}
	return s.projectOrigins(ctx)[origin]
}

// projectOrigins returns the cached project origins and reloads them when
// they are older than projectadmin.OriginCacheTTL. The lock is held during the reload, so
// one host makes at most one query for each TTL, also when the query fails.
// A failed reload keeps the last list.
func (s *originSet) projectOrigins(ctx context.Context) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loadedAt.IsZero() && s.now().Sub(s.loadedAt) < projectadmin.OriginCacheTTL {
		return s.projects
	}
	s.loadedAt = s.now()
	list, err := s.load(ctx)
	if err != nil {
		slog.WarnContext(ctx, "cors: load project origins", "err", err)
		return s.projects
	}
	s.projects = make(map[string]bool, len(list))
	for _, o := range list {
		s.projects[o] = true
	}
	return s.projects
}
