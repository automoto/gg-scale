package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/projectadmin"
)

func fixedOrigins(origins ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return origins, nil }
}

func TestOriginSet_should_accept_origin_in_a_project_list(t *testing.T) {
	s := newOriginSet([]string{"https://app.ggscale.com"}, fixedOrigins("https://html-classic.itch.zone"))

	assert.True(t, s.allowed(context.Background(), "https://html-classic.itch.zone"))
}

func TestOriginSet_should_refuse_unknown_origin(t *testing.T) {
	s := newOriginSet([]string{"https://app.ggscale.com"}, fixedOrigins("https://html-classic.itch.zone"))

	assert.False(t, s.allowed(context.Background(), "https://evil.example"))
}

func TestOriginSet_should_accept_env_origin(t *testing.T) {
	s := newOriginSet([]string{"https://app.ggscale.com"}, fixedOrigins())

	assert.True(t, s.allowed(context.Background(), "https://app.ggscale.com"))
}

func TestOriginSet_should_accept_all_with_env_wildcard(t *testing.T) {
	s := newOriginSet([]string{"*"}, fixedOrigins())

	assert.True(t, s.allowed(context.Background(), "https://anything.example"))
}

func TestOriginSet_should_reload_after_ttl(t *testing.T) {
	list := []string{}
	s := newOriginSet(nil, func(context.Context) ([]string, error) { return list, nil })
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	s.allowed(context.Background(), "https://new.example")

	list = []string{"https://new.example"}
	now = now.Add(projectadmin.OriginCacheTTL + time.Second)

	assert.True(t, s.allowed(context.Background(), "https://new.example"))
}

func TestOriginSet_should_keep_last_list_when_reload_fails(t *testing.T) {
	fail := false
	s := newOriginSet(nil, func(context.Context) ([]string, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return []string{"https://kept.example"}, nil
	})
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	s.allowed(context.Background(), "https://kept.example")

	fail = true
	now = now.Add(projectadmin.OriginCacheTTL + time.Second)

	assert.True(t, s.allowed(context.Background(), "https://kept.example"))
}

func TestOriginSet_should_not_query_again_within_ttl_after_a_failure(t *testing.T) {
	calls := 0
	s := newOriginSet(nil, func(context.Context) ([]string, error) {
		calls++
		return nil, errors.New("db down")
	})

	s.allowed(context.Background(), "https://a.example")
	s.allowed(context.Background(), "https://a.example")

	assert.Equal(t, 1, calls)
}
