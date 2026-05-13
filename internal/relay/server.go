package relay

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	pionturn "github.com/pion/turn/v3"
)

// ServerConfig wires NewServer. PublicIP is the address peers connect to;
// BindPort is the local UDP port the listener binds (default 3478).
type ServerConfig struct {
	PublicIP string
	BindAddr string // host portion of the bind address; empty -> 0.0.0.0
	BindPort int    // UDP port
	Issuer   *Issuer
}

// Server wraps pion/turn/v3. One Server per process.
type Server struct {
	turn *pionturn.Server
	conn net.PacketConn
}

// NewServer binds a UDP listener and starts the underlying pion server.
// The returned Server owns the listener; callers must invoke Close to
// release the port.
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
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}
	bindPort := cfg.BindPort
	if bindPort == 0 {
		bindPort = 3478
	}

	conn, err := net.ListenPacket("udp4", bindAddr+":"+strconv.Itoa(bindPort))
	if err != nil {
		return nil, fmt.Errorf("relay: bind udp %s:%d: %w", bindAddr, bindPort, err)
	}

	turnServer, err := pionturn.NewServer(pionturn.ServerConfig{
		Realm:       cfg.Issuer.realm,
		AuthHandler: authHandlerFor(cfg.Issuer),
		PacketConnConfigs: []pionturn.PacketConnConfig{
			{
				PacketConn: conn,
				RelayAddressGenerator: &pionturn.RelayAddressGeneratorStatic{
					RelayAddress: ip,
					Address:      bindAddr,
				},
			},
		},
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: turn server: %w", err)
	}
	return &Server{turn: turnServer, conn: conn}, nil
}

// Close stops the TURN server and releases the listener.
func (s *Server) Close() error {
	if err := s.turn.Close(); err != nil {
		return err
	}
	return nil
}

// authHandlerFor returns a pion AuthHandler that recomputes the HMAC
// password for the incoming username and feeds it through pion's
// GenerateAuthKey helper.
func authHandlerFor(iss *Issuer) pionturn.AuthHandler {
	return func(username, realm string, _ net.Addr) ([]byte, bool) {
		if realm != iss.realm {
			return nil, false
		}
		if _, _, err := iss.Verify(username, iss.passwordFor(username)); err != nil {
			return nil, false
		}
		return pionturn.GenerateAuthKey(username, realm, iss.passwordFor(username)), true
	}
}
