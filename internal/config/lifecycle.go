package config

import (
	"fmt"
	"time"
)

// validateLifecycle checks the lifecycle settings both processes read. The
// token can drain a node out of its load-balancer pool, so it gets the same
// length floor as the other shared bearer tokens.
func validateLifecycle(token string, drainTimeout time.Duration) error {
	if token != "" && len(token) < 32 {
		return fmt.Errorf("LIFECYCLE_TOKEN must be >= 32 bytes when set (got %d)", len(token))
	}
	if drainTimeout < 0 {
		return fmt.Errorf("DRAIN_TIMEOUT %q: must not be negative", drainTimeout)
	}
	return nil
}
