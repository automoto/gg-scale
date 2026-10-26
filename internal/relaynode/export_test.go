package relaynode

import "time"

// SetClock replaces the Selector's clock in tests.
func (s *Selector) SetClock(now func() time.Time) { s.now = now }
