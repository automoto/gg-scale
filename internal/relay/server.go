package relay

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"

	pionturn "github.com/pion/turn/v3"
)

// ServerConfig wires NewServer. PublicIP is the address peers connect to;
// BindPort is the local UDP port the listener binds (default 3478). TCPPort
// and TLSPort are optional TURN/TCP and TURNS/TLS transports (0 = off) so
// clients behind UDP-blocking firewalls can still reach the relay.
type ServerConfig struct {
	PublicIP    string
	BindAddr    string // host portion of the bind address; empty -> 0.0.0.0
	BindPort    int    // UDP port
	TCPPort     int    // TURN-over-TCP port; 0 disables
	TLSPort     int    // TURNS (TLS) port; 0 disables
	TLSCertFile string // required when TLSPort != 0
	TLSKeyFile  string // required when TLSPort != 0
	// RelayMinPort/RelayMaxPort bound the UDP ports allocated for relayed
	// media. Both 0 (the default) allocates from the OS ephemeral range;
	// setting a range lets the firewall open only those ports. The range width
	// caps the number of concurrent relay allocations.
	RelayMinPort int
	RelayMaxPort int
	// MaxAllocations caps concurrently-live relay allocations node-wide,
	// bounding port-range exhaustion from a single credential. 0 = unlimited.
	MaxAllocations int
	// Logger receives the pion server's internal events; nil uses slog.Default.
	Logger *slog.Logger
	Issuer *Issuer
}

// Server wraps pion/turn/v3. One Server per process. It owns every listener it
// opens (UDP packet conns + TCP/TLS listeners); Close releases them all.
type Server struct {
	turn         *pionturn.Server
	conns        []net.PacketConn
	listeners    []net.Listener
	alloc        *allocationLimiter
	authFailures atomic.Int64
}

// NewServer binds the configured listeners and starts the underlying pion
// server. The returned Server owns the listeners; callers must invoke Close to
// release the ports.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Issuer == nil {
		return nil, errors.New("relay: issuer is required")
	}
	if cfg.PublicIP == "" {
		return nil, errors.New("relay: public IP is required")
	}
	ip := net.ParseIP(cfg.PublicIP)
	if ip == nil {
		return nil, fmt.Errorf("relay: invalid public IP %q", cfg.PublicIP)
	}
	// The listeners below are hardcoded udp4/tcp4, so an IPv6 public address
	// would either fail to bind or hand clients an unreachable relayed address.
	if ip.To4() == nil {
		return nil, fmt.Errorf("relay: public IP %q must be IPv4; IPv6 listeners are not supported", cfg.PublicIP)
	}
	if (cfg.RelayMinPort == 0) != (cfg.RelayMaxPort == 0) {
		return nil, errors.New("relay: RelayMinPort and RelayMaxPort must be set together")
	}
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}
	bindPort := cfg.BindPort
	if bindPort == 0 {
		bindPort = 3478
	}

	s := &Server{}
	s.alloc = newAllocationLimiter(relayGenerator(ip, bindAddr, cfg.RelayMinPort, cfg.RelayMaxPort), cfg.MaxAllocations)

	conn, err := net.ListenPacket("udp4", net.JoinHostPort(bindAddr, strconv.Itoa(bindPort)))
	if err != nil {
		return nil, fmt.Errorf("relay: bind udp %s:%d: %w", bindAddr, bindPort, err)
	}
	s.conns = append(s.conns, conn)
	packetConns := []pionturn.PacketConnConfig{{PacketConn: conn, RelayAddressGenerator: s.alloc}}

	listenerConfigs, err := s.buildTCPListeners(cfg, bindAddr, s.alloc)
	if err != nil {
		_ = s.closeListeners()
		return nil, err
	}

	turnServer, err := pionturn.NewServer(pionturn.ServerConfig{
		Realm:             cfg.Issuer.realm,
		AuthHandler:       s.authHandler(cfg.Issuer),
		PacketConnConfigs: packetConns,
		ListenerConfigs:   listenerConfigs,
		LoggerFactory:     newSlogLoggerFactory(cfg.Logger),
	})
	if err != nil {
		_ = s.closeListeners()
		return nil, fmt.Errorf("relay: turn server: %w", err)
	}
	s.turn = turnServer
	return s, nil
}

// relayGenerator picks the pion relay-address generator: a bounded port range
// when both min and max are set (so the firewall can open only that range),
// otherwise the static generator that uses the OS ephemeral range.
func relayGenerator(ip net.IP, bindAddr string, minPort, maxPort int) pionturn.RelayAddressGenerator {
	const maxPortNum = 65535
	if minPort > 0 && minPort <= maxPortNum && maxPort > 0 && maxPort <= maxPortNum {
		return &pionturn.RelayAddressGeneratorPortRange{
			RelayAddress: ip,
			Address:      bindAddr,
			MinPort:      uint16(minPort),
			MaxPort:      uint16(maxPort),
		}
	}
	return &pionturn.RelayAddressGeneratorStatic{RelayAddress: ip, Address: bindAddr}
}

// buildTCPListeners opens the optional TURN/TCP and TURNS/TLS listeners,
// registering each with the shared relay-address generator. Opened listeners
// are tracked on s so a later failure can release them.
func (s *Server) buildTCPListeners(cfg ServerConfig, bindAddr string, relayGen pionturn.RelayAddressGenerator) ([]pionturn.ListenerConfig, error) {
	var configs []pionturn.ListenerConfig

	if cfg.TCPPort != 0 {
		ln, err := net.Listen("tcp4", net.JoinHostPort(bindAddr, strconv.Itoa(cfg.TCPPort)))
		if err != nil {
			return nil, fmt.Errorf("relay: bind tcp %s:%d: %w", bindAddr, cfg.TCPPort, err)
		}
		s.listeners = append(s.listeners, ln)
		configs = append(configs, pionturn.ListenerConfig{Listener: ln, RelayAddressGenerator: relayGen})
	}

	if cfg.TLSPort != 0 {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("relay: load tls keypair: %w", err)
		}
		ln, err := tls.Listen("tcp4", net.JoinHostPort(bindAddr, strconv.Itoa(cfg.TLSPort)), &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
		if err != nil {
			return nil, fmt.Errorf("relay: bind tls %s:%d: %w", bindAddr, cfg.TLSPort, err)
		}
		s.listeners = append(s.listeners, ln)
		configs = append(configs, pionturn.ListenerConfig{Listener: ln, RelayAddressGenerator: relayGen})
	}

	return configs, nil
}

// Close stops the TURN server and releases every listener.
func (s *Server) Close() error {
	var turnErr error
	if s.turn != nil {
		turnErr = s.turn.Close()
	}
	listenerErr := s.closeListeners()
	if turnErr != nil {
		return turnErr
	}
	return listenerErr
}

// closeListeners releases the packet conns and listeners, ignoring the
// already-closed errors pion produces when it has torn a listener down itself.
func (s *Server) closeListeners() error {
	var firstErr error
	for _, c := range s.conns {
		if err := c.Close(); err != nil && !isClosedNetError(err) && firstErr == nil {
			firstErr = err
		}
	}
	for _, l := range s.listeners {
		if err := l.Close(); err != nil && !isClosedNetError(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func isClosedNetError(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

// authHandler returns a pion AuthHandler that recomputes the HMAC password for
// the incoming username — selecting the accepted secret by the key id in the
// username — and feeds it through pion's GenerateAuthKey helper. The HMAC
// binding itself is enforced by pion comparing the derived key against the
// client's message-integrity attribute. Every rejection (wrong realm, malformed
// / expired / unknown-kid username) bumps authFailures so credential probing is
// observable on the node.
func (s *Server) authHandler(iss *Issuer) pionturn.AuthHandler {
	return func(username, realm string, _ net.Addr) ([]byte, bool) {
		if realm != iss.realm {
			s.authFailures.Add(1)
			return nil, false
		}
		pw, ok := iss.passwordForAuth(username)
		if !ok {
			s.authFailures.Add(1)
			return nil, false
		}
		return pionturn.GenerateAuthKey(username, realm, pw), true
	}
}

// ActiveAllocations reports the number of currently-live relay allocations.
func (s *Server) ActiveAllocations() int64 {
	if s.alloc == nil {
		return 0
	}
	return s.alloc.live.Load()
}

// RejectedAllocations reports the cumulative count of allocations refused
// because the node allocation cap was reached.
func (s *Server) RejectedAllocations() int64 {
	if s.alloc == nil {
		return 0
	}
	return s.alloc.rejected.Load()
}

// AuthFailures reports the cumulative count of rejected TURN auth attempts.
func (s *Server) AuthFailures() int64 { return s.authFailures.Load() }
