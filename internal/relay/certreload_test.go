package relay_test

import (
	"crypto/tls"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relay"
)

// startTLSRelay starts a relay with a TURNS listener on the given cert files
// and returns the listener address.
func startTLSRelay(t *testing.T, certFile, keyFile string) string {
	t.Helper()
	port := unusedTCPPort(t)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:    "127.0.0.1",
		BindAddr:    "127.0.0.1",
		BindPort:    unusedUDPPort(t),
		TLSPort:     port,
		TLSCertFile: certFile,
		TLSKeyFile:  keyFile,
		Issuer:      relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// servedSerial completes a TLS handshake with addr and returns the serial of
// the certificate the server presented.
func servedSerial(t *testing.T, addr string) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp4", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test inspects the served cert
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

// touch moves both files' modification time forward so a change is visible
// even on filesystems with coarse timestamps.
func touch(t *testing.T, files ...string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	for _, f := range files {
		require.NoError(t, os.Chtimes(f, later, later))
	}
}

func TestTURNS_should_serve_renewed_cert_without_restart(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	addr := startTLSRelay(t, certFile, keyFile)
	require.Equal(t, int64(1), servedSerial(t, addr))

	writeCertPair(t, certFile, keyFile, 2)
	touch(t, certFile, keyFile)

	assert.Equal(t, int64(2), servedSerial(t, addr))
}

func TestTURNS_should_keep_last_good_cert_when_new_files_are_invalid(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	addr := startTLSRelay(t, certFile, keyFile)
	require.Equal(t, int64(1), servedSerial(t, addr))

	require.NoError(t, os.WriteFile(certFile, []byte("not a certificate"), 0o600))
	touch(t, certFile)

	assert.Equal(t, int64(1), servedSerial(t, addr))
}

func TestTURNS_should_load_cert_once_a_half_written_pair_is_complete(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	addr := startTLSRelay(t, certFile, keyFile)
	_, newKey := writeSelfSignedCert(t)
	newCert := certFile + ".new"
	writeCertPair(t, newCert, newKey, 3)

	// Renewal writes the cert first and the key second; between the two the
	// pair does not match.
	require.NoError(t, os.Rename(newCert, certFile))
	touch(t, certFile)
	require.Equal(t, int64(1), servedSerial(t, addr))
	require.NoError(t, os.Rename(newKey, keyFile))
	later := time.Now().Add(2 * time.Hour)
	require.NoError(t, os.Chtimes(keyFile, later, later))

	assert.Equal(t, int64(3), servedSerial(t, addr))
}
