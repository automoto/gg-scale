package relay_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/relay"
)

func TestServerCloseIgnoresListenerAlreadyClosedByTurn(t *testing.T) {
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "127.0.0.1",
		BindAddr: "127.0.0.1",
		BindPort: unusedUDPPort(t),
		Issuer:   relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.NoError(t, err)

	require.NoError(t, srv.Close())
}

func TestServerBindsTCPListener(t *testing.T) {
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "127.0.0.1",
		BindAddr: "127.0.0.1",
		BindPort: unusedUDPPort(t),
		TCPPort:  unusedTCPPort(t),
		Issuer:   relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.NoError(t, err)

	require.NoError(t, srv.Close())
}

func TestServerBindsTLSListener(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:    "127.0.0.1",
		BindAddr:    "127.0.0.1",
		BindPort:    unusedUDPPort(t),
		TLSPort:     unusedTCPPort(t),
		TLSCertFile: certFile,
		TLSKeyFile:  keyFile,
		Issuer:      relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.NoError(t, err)

	require.NoError(t, srv.Close())
}

func TestServerTLSRejectsMissingCert(t *testing.T) {
	_, err := relay.NewServer(relay.ServerConfig{
		PublicIP:    "127.0.0.1",
		BindAddr:    "127.0.0.1",
		BindPort:    unusedUDPPort(t),
		TLSPort:     unusedTCPPort(t),
		TLSCertFile: "/nonexistent/cert.pem",
		TLSKeyFile:  "/nonexistent/key.pem",
		Issuer:      relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.Error(t, err)
}

func TestServerRejectsIPv6PublicIP(t *testing.T) {
	_, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "2001:db8::1",
		BindAddr: "127.0.0.1",
		BindPort: unusedUDPPort(t),
		Issuer:   relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.ErrorContains(t, err, "IPv4")
}

func TestServerRejectsHalfSetPortRange(t *testing.T) {
	_, err := relay.NewServer(relay.ServerConfig{
		PublicIP:     "127.0.0.1",
		BindAddr:     "127.0.0.1",
		BindPort:     unusedUDPPort(t),
		RelayMinPort: 50100,
		Issuer:       relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.ErrorContains(t, err, "set together")
}

func unusedUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func unusedTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// writeSelfSignedCert generates an ephemeral cert/key pair to temp files for
// exercising the TURNS listener path.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile
}
