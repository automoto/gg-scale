// Package deps holds blank imports to lock direct module dependencies during
// Phase 0 scaffolding. Delete this file once each dependency is imported by a
// real package elsewhere in the tree.
package deps

import (
	_ "agones.dev/agones/pkg/sdk"
	_ "github.com/coder/websocket"
	_ "github.com/go-chi/chi/v5"
	_ "github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/pion/turn/v3"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/stretchr/testify/assert"
	_ "github.com/stripe/stripe-go/v79"
	_ "k8s.io/client-go/kubernetes"
)
