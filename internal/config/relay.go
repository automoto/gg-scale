package config

import (
	"fmt"
	"net"
	"time"

	env "github.com/caarlos0/env/v11"
)

// RelayConfig is the minimal configuration for the standalone TURN relay
// process (`ggscale-server relay`). A relay node runs only the pion TURN
// listener — no database, HTTP API, or matchmaker — so it deliberately does
// not load the full server Config and never requires DATABASE_URL et al. It
// reuses the shared _FILE-resolution and empty-var handling from
// buildEnvironment, so RELAY_SHARED_SECRET_FILE works here too.
type RelayConfig struct {
	PublicIP         string        `env:"RELAY_PUBLIC_IP,required"`
	BindAddr         string        `env:"RELAY_BIND_ADDR" envDefault:"0.0.0.0"`
	UDPPort          int           `env:"RELAY_UDP_PORT" envDefault:"3478"`
	Realm            string        `env:"RELAY_REALM" envDefault:"ggscale"`
	SharedSecret     string        `env:"RELAY_SHARED_SECRET"`
	SharedSecretNext string        `env:"RELAY_SHARED_SECRET_NEXT"`
	CredTTL          time.Duration `env:"RELAY_CRED_TTL" envDefault:"5m"`
	LogLevel         string        `env:"LOG_LEVEL" envDefault:"info"`

	// TCP/TLS transports (M3). A port of 0 leaves that transport off. TLS
	// requires both a cert and key file.
	TCPPort     int    `env:"RELAY_TCP_PORT" envDefault:"0"`
	TLSPort     int    `env:"RELAY_TLS_PORT" envDefault:"0"`
	TLSCertFile string `env:"RELAY_TLS_CERT_FILE"`
	TLSKeyFile  string `env:"RELAY_TLS_KEY_FILE"`

	// MinPort/MaxPort bound the UDP ports allocated for relayed media so the
	// firewall can open only that range. Both-or-neither; 0/0 = OS ephemeral.
	MinPort int `env:"RELAY_MIN_PORT" envDefault:"0"`
	MaxPort int `env:"RELAY_MAX_PORT" envDefault:"0"`

	// MaxAllocations caps concurrently-live relay allocations node-wide so a
	// single (possibly leaked) credential can't exhaust the port range. 0 =
	// unlimited. Default 1000 matches the documented firewall port-range width.
	MaxAllocations int `env:"RELAY_MAX_ALLOCATIONS" envDefault:"1000"`

	// HealthAddr, when set (e.g. ":9091"), serves /healthz and /metrics for the
	// monitoring host to scrape the relay node over the tailnet.
	HealthAddr string `env:"RELAY_HEALTH_ADDR"`
	// MetricsToken, when set, requires a "Bearer <token>" header on /metrics.
	// Empty keeps the endpoint open (tailnet-only deployment is the default).
	MetricsToken string `env:"RELAY_METRICS_TOKEN"`
}

// LoadRelay reads the relay-node environment and returns a validated
// RelayConfig. It shares buildEnvironment with Load so the _FILE convention and
// set-but-empty handling are identical.
func LoadRelay() (*RelayConfig, error) {
	envMap, err := buildEnvironment()
	if err != nil {
		return nil, err
	}
	rc := &RelayConfig{}
	if err := env.ParseWithOptions(rc, env.Options{Environment: envMap}); err != nil {
		return nil, renameParseErrors(err)
	}
	if err := rc.validate(); err != nil {
		return nil, err
	}
	return rc, nil
}

// Secrets returns the ordered accepted secrets: active signer first, then any
// rotation secret.
func (rc *RelayConfig) Secrets() []string {
	return nonEmpty(rc.SharedSecret, rc.SharedSecretNext)
}

func (rc *RelayConfig) validate() error {
	if len(rc.SharedSecret) < 32 {
		return fmt.Errorf("RELAY_SHARED_SECRET must be >= 32 bytes (got %d)", len(rc.SharedSecret))
	}
	if rc.SharedSecretNext != "" && len(rc.SharedSecretNext) < 32 {
		return fmt.Errorf("RELAY_SHARED_SECRET_NEXT must be >= 32 bytes when set (got %d)", len(rc.SharedSecretNext))
	}
	if err := validateRelayPublicIP(rc.PublicIP); err != nil {
		return err
	}
	if err := checkPort("RELAY_UDP_PORT", rc.UDPPort); err != nil {
		return err
	}
	if rc.CredTTL <= 0 {
		return fmt.Errorf("RELAY_CRED_TTL %q: must be a positive duration", rc.CredTTL)
	}
	if rc.TCPPort != 0 {
		if err := checkPort("RELAY_TCP_PORT", rc.TCPPort); err != nil {
			return err
		}
	}
	if err := rc.validateTLS(); err != nil {
		return err
	}
	return validatePortRange(rc.MinPort, rc.MaxPort)
}

// validateTLS enforces that TURNS is all-or-nothing: a TLS port needs both a
// cert and key, and cert/key files are meaningless without a port.
func (rc *RelayConfig) validateTLS() error {
	hasFiles := rc.TLSCertFile != "" || rc.TLSKeyFile != ""
	if rc.TLSPort == 0 {
		if hasFiles {
			return fmt.Errorf("RELAY_TLS_CERT_FILE/RELAY_TLS_KEY_FILE set without RELAY_TLS_PORT")
		}
		return nil
	}
	if err := checkPort("RELAY_TLS_PORT", rc.TLSPort); err != nil {
		return err
	}
	if rc.TLSCertFile == "" || rc.TLSKeyFile == "" {
		return fmt.Errorf("RELAY_TLS_PORT set: both RELAY_TLS_CERT_FILE and RELAY_TLS_KEY_FILE are required")
	}
	return nil
}

// validateRelayPublicIP requires a valid IPv4 address: the relay listeners are
// hardcoded udp4/tcp4, so an IPv6 public address would pass a bare ParseIP but
// then fail to bind or advertise an unreachable relayed address.
func validateRelayPublicIP(raw string) error {
	ip := net.ParseIP(raw)
	if ip == nil {
		return fmt.Errorf("RELAY_PUBLIC_IP %q: must be a valid IP address", raw)
	}
	if ip.To4() == nil {
		return fmt.Errorf("RELAY_PUBLIC_IP %q: must be IPv4 (IPv6 relay listeners are not supported)", raw)
	}
	return nil
}

func checkPort(name string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%s %d: must be 1..65535", name, port)
	}
	return nil
}

// validatePortRange enforces the relayed-media port range: both bounds set
// together, each a valid port, and min <= max. 0/0 means "unset" (OS ephemeral
// range) and is allowed.
func validatePortRange(minPort, maxPort int) error {
	if minPort == 0 && maxPort == 0 {
		return nil
	}
	if minPort == 0 || maxPort == 0 {
		return fmt.Errorf("RELAY_MIN_PORT and RELAY_MAX_PORT must be set together")
	}
	if err := checkPort("RELAY_MIN_PORT", minPort); err != nil {
		return err
	}
	if err := checkPort("RELAY_MAX_PORT", maxPort); err != nil {
		return err
	}
	if minPort > maxPort {
		return fmt.Errorf("RELAY_MIN_PORT (%d) must be <= RELAY_MAX_PORT (%d)", minPort, maxPort)
	}
	return nil
}
