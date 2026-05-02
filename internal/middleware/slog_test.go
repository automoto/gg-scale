package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/middleware"
)

func newJSONLogger(buf *bytes.Buffer) *slog.Logger {
	h := middleware.NewContextHandler(slog.NewJSONHandler(buf, nil))
	return slog.New(h)
}

func parseLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	return m
}

func TestContextHandler_adds_tenant_id_from_context(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(&buf)
	ctx := db.WithTenant(context.Background(), 42)

	log.InfoContext(ctx, "hello")

	m := parseLogLine(t, &buf)
	assert.Equal(t, float64(42), m["tenant_id"])
}

func TestContextHandler_adds_project_id_when_present(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(&buf)
	ctx := db.WithTenant(context.Background(), 1)
	ctx = db.WithProject(ctx, 99)

	log.InfoContext(ctx, "hello")

	m := parseLogLine(t, &buf)
	assert.Equal(t, float64(99), m["project_id"])
}

func TestContextHandler_omits_project_id_when_absent(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(&buf)
	ctx := db.WithTenant(context.Background(), 1)

	log.InfoContext(ctx, "hello")

	m := parseLogLine(t, &buf)
	_, hasProject := m["project_id"]
	assert.False(t, hasProject)
}

func TestContextHandler_adds_request_id_from_context(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(&buf)
	ctx := middleware.WithRequestID(context.Background(), "req-abc")

	log.InfoContext(ctx, "hello")

	m := parseLogLine(t, &buf)
	assert.Equal(t, "req-abc", m["request_id"])
}

func TestContextHandler_omits_tenant_id_when_absent(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(&buf)

	log.InfoContext(context.Background(), "hello")

	m := parseLogLine(t, &buf)
	_, hasTenant := m["tenant_id"]
	assert.False(t, hasTenant)
}

func TestContextHandler_WithAttrs_preserves_context_enrichment(t *testing.T) {
	var buf bytes.Buffer
	base := middleware.NewContextHandler(slog.NewJSONHandler(&buf, nil))
	log := slog.New(base.WithAttrs([]slog.Attr{slog.String("service", "test")}))
	ctx := db.WithTenant(context.Background(), 7)

	log.InfoContext(ctx, "hello")

	m := parseLogLine(t, &buf)
	assert.Equal(t, float64(7), m["tenant_id"])
	assert.Equal(t, "test", m["service"])
}

func TestContextHandler_WithGroup_context_attrs_appear_in_group(t *testing.T) {
	var buf bytes.Buffer
	base := middleware.NewContextHandler(slog.NewJSONHandler(&buf, nil))
	log := slog.New(base.WithGroup("req"))
	ctx := db.WithTenant(context.Background(), 5)

	log.InfoContext(ctx, "hello")

	m := parseLogLine(t, &buf)
	// WithGroup scopes all attrs into "req"; context attrs follow the same rule.
	group, ok := m["req"].(map[string]any)
	require.True(t, ok, "expected a 'req' group in log output")
	assert.Equal(t, float64(5), group["tenant_id"])
}
