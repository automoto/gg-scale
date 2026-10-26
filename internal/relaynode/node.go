// Package relaynode is the API server's registry of standalone relay nodes.
// A platform admin registers and retires nodes; a periodic probe reads each
// node's GET /lifecycle; relay credentials list only the nodes that are
// eligible, preferring the client's region.
package relaynode

import (
	"errors"
	"fmt"
	"net/url"
	"time"
	"unicode"

	"github.com/automoto/gg-scale/internal/relay"
)

// ProbeInterval is how often the probe job reads every known node.
const ProbeInterval = 10 * time.Second

// FreshnessWindow is how old a node's last good probe may be before the node
// stops being advertised: three probe intervals.
const FreshnessWindow = 3 * ProbeInterval

// Registration states.
const (
	StateKnown   = "known"
	StateRetired = "retired"
)

const (
	maxRegionLen       = 64
	maxTURNURLs        = 16
	maxLifecycleURLLen = 2048
)

// ErrInvalid marks a Spec that fails validation.
var ErrInvalid = errors.New("relaynode: invalid node")

// ErrNotFound is returned for an unknown node id.
var ErrNotFound = errors.New("relaynode: node not found")

// ErrDuplicate is returned when a known node already uses the lifecycle URL.
var ErrDuplicate = errors.New("relaynode: a known node already uses this lifecycle_url")

// Spec is what an admin supplies to register a node.
type Spec struct {
	Region       string
	TURNURLs     []string
	LifecycleURL string
}

// Validate reports a wrapped ErrInvalid for the first bad field.
func (s Spec) Validate() error {
	if err := validateRegion(s.Region); err != nil {
		return fmt.Errorf("%w: region: %v", ErrInvalid, err)
	}
	if len(s.TURNURLs) == 0 || len(s.TURNURLs) > maxTURNURLs {
		return fmt.Errorf("%w: turn_urls: must list 1 to %d URIs", ErrInvalid, maxTURNURLs)
	}
	for _, raw := range s.TURNURLs {
		if err := relay.ValidateTURNURI(raw); err != nil {
			return fmt.Errorf("%w: turn_urls %q: %v", ErrInvalid, raw, err)
		}
	}
	if err := validateLifecycleURL(s.LifecycleURL); err != nil {
		return fmt.Errorf("%w: lifecycle_url: %v", ErrInvalid, err)
	}
	return nil
}

// validateRegion allows lowercase letters, digits and dashes, so a client's
// ?region= matches exactly what the admin registered.
func validateRegion(region string) error {
	if region == "" || len(region) > maxRegionLen {
		return fmt.Errorf("must be 1 to %d characters", maxRegionLen)
	}
	for _, r := range region {
		if !isRegionRune(r) {
			return errors.New("must use lowercase letters, digits and dashes")
		}
	}
	return nil
}

func isRegionRune(r rune) bool {
	return r <= unicode.MaxASCII && (r == '-' || unicode.IsDigit(r) || unicode.IsLower(r))
}

func validateLifecycleURL(raw string) error {
	if raw == "" || len(raw) > maxLifecycleURLLen {
		return fmt.Errorf("must be 1 to %d characters", maxLifecycleURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must use the http or https scheme")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	if u.User != nil {
		return errors.New("must not carry credentials; the probe sends LIFECYCLE_TOKEN or RELAY_PROBE_TOKEN")
	}
	return checkLiteralHost(u.Hostname())
}

// Node is one registered relay node as the admin API reports it.
type Node struct {
	ID           int64    `json:"id" example:"7"`
	Region       string   `json:"region" example:"us-east"`
	TURNURLs     []string `json:"turn_urls" example:"turn:relay-1.example.com:3478?transport=udp"`
	LifecycleURL string   `json:"lifecycle_url" example:"http://10.0.0.5:9091/lifecycle"`
	State        string   `json:"state" enum:"known,retired" example:"known"`
	// Eligible reports whether credentials currently advertise this node.
	Eligible    bool       `json:"eligible" example:"true"`
	LastProbeAt *time.Time `json:"last_probe_at" example:"2026-10-26T12:00:00Z"`
	LastProbeOK bool       `json:"last_probe_ok" example:"true"`
	// LastProbeError is why the last probe failed, such as "timeout" or
	// "status 503". nil when it succeeded or has not run.
	LastProbeError *string `json:"last_probe_error" example:"timeout"`
	// LastLifecycleState and LastActiveAllocations are the state and
	// active_work the last probe read. nil when it read no valid /lifecycle
	// document.
	LastLifecycleState    *string   `json:"last_lifecycle_state" example:"active"`
	LastActiveAllocations *int64    `json:"last_active_allocations" example:"12"`
	CreatedAt             time.Time `json:"created_at" example:"2026-10-26T11:00:00Z"`
	UpdatedAt             time.Time `json:"updated_at" example:"2026-10-26T12:00:00Z"`
}
