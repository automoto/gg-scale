package config

import (
	"fmt"
	"time"
)

// validateLifecycle checks the lifecycle settings both processes read. The
// token can drain a node out of its load-balancer pool, so it gets the same
// length floor as the other shared bearer tokens. The read token must differ
// from it, or holding the read token would also allow a drain.
func validateLifecycle(token, readToken string, drainTimeout time.Duration) error {
	if token != "" && len(token) < 32 {
		return fmt.Errorf("LIFECYCLE_TOKEN must be >= 32 bytes when set (got %d)", len(token))
	}
	if readToken != "" && len(readToken) < 32 {
		return fmt.Errorf("LIFECYCLE_READ_TOKEN must be >= 32 bytes when set (got %d)", len(readToken))
	}
	if readToken != "" && readToken == token {
		return fmt.Errorf("LIFECYCLE_READ_TOKEN must differ from LIFECYCLE_TOKEN")
	}
	if drainTimeout < 0 {
		return fmt.Errorf("DRAIN_TIMEOUT %q: must not be negative", drainTimeout)
	}
	return nil
}
