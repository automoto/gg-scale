package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/observability"
)

const (
	headerRequestID = "X-Request-Id"
)

type requestIDKey struct{}

// WithRequestID returns ctx tagged with id. Used by NewRequestID and by
// callers that want to attach the id to slog records.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the id installed by NewRequestID.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}

// NewRequestID accepts an inbound X-Request-Id header (so a calling load
// balancer or test harness can choose the value) or generates a fresh
// random one. The id is installed on the request context and echoed back
// in the response header so a caller can correlate logs.
func NewRequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(headerRequestID)
			if id == "" {
				buf := make([]byte, 8)
				_, _ = rand.Read(buf)
				id = hex.EncodeToString(buf)
			}
			w.Header().Set(headerRequestID, id)
			r = r.WithContext(WithRequestID(r.Context(), id))
			next.ServeHTTP(w, r)
		})
	}
}

// NewObservability records request latency, error rate, and per-version
// counts on reg. Mount inside the /v1 group after the version middleware.
func NewObservability(reg prometheus.Registerer) func(http.Handler) http.Handler {
	dur := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ggscale_http_request_duration_seconds",
			Help:    "HTTP request latency by route, method, and status.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"route", "method", "status"},
	)
	errs := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_http_errors_total",
			Help: "HTTP responses with status >= 400, by route and status class.",
		},
		[]string{"route", "status_class"},
	)
	reg.MustRegister(dur, errs)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// chi's wrapper preserves optional interfaces (Hijacker,
			// Flusher, Pusher, ReaderFrom); a custom wrapper would drop them.
			rec := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(rec, r)
			elapsed := time.Since(start).Seconds()

			// Use the chi route pattern, not raw URL, to bound label cardinality.
			route := observability.RouteLabel(r)
			code := rec.Status()
			if code == 0 {
				code = http.StatusOK
			}
			status := strconv.Itoa(code)
			dur.WithLabelValues(route, r.Method, status).Observe(elapsed)
			if code >= 400 {
				errs.WithLabelValues(route, statusClass(code)).Inc()
			}
		})
	}
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}
