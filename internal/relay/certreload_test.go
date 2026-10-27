package relay_test

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relay"
)

// startTLSRelay starts a relay with a TURNS listener on the given cert files
// and returns it with the listener address.
func startTLSRelay(t *testing.T, certFile, keyFile string, logs *bytes.Buffer) (*relay.Server, string) {
	t.Helper()
	port := unusedTCPPort(t)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:    "127.0.0.1",
		BindAddr:    "127.0.0.1",
		BindPort:    unusedUDPPort(t),
		TLSPort:     port,
		TLSCertFile: certFile,
		TLSKeyFile:  keyFile,
		Logger:      slog.New(slog.NewTextHandler(logs, nil)),
		Issuer:      relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return srv, net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
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

func TestTURNS_should_serve_renewed_cert_without_restart(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	srv, addr := startTLSRelay(t, certFile, keyFile, &bytes.Buffer{})
	writeCertPair(t, certFile, keyFile, 2)

	relay.CheckTLSCert(srv)

	assert.Equal(t, int64(2), servedSerial(t, addr))
}

func TestTURNS_should_serve_renewed_cert_when_modification_times_are_unchanged(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	srv, addr := startTLSRelay(t, certFile, keyFile, &bytes.Buffer{})
	certInfo, err := os.Stat(certFile)
	require.NoError(t, err)
	keyInfo, err := os.Stat(keyFile)
	require.NoError(t, err)
	writeCertPair(t, certFile, keyFile, 2)
	require.NoError(t, os.Chtimes(certFile, certInfo.ModTime(), certInfo.ModTime()))
	require.NoError(t, os.Chtimes(keyFile, keyInfo.ModTime(), keyInfo.ModTime()))

	relay.CheckTLSCert(srv)

	assert.Equal(t, int64(2), servedSerial(t, addr))
}

func TestTURNS_should_serve_renewed_cert_after_symlink_swap(t *testing.T) {
	dir := t.TempDir()
	oldCert, oldKey := writeSelfSignedCert(t)
	certLink := filepath.Join(dir, "cert.pem")
	keyLink := filepath.Join(dir, "key.pem")
	require.NoError(t, os.Symlink(oldCert, certLink))
	require.NoError(t, os.Symlink(oldKey, keyLink))
	srv, addr := startTLSRelay(t, certLink, keyLink, &bytes.Buffer{})
	newCert, newKey := writeSelfSignedCert(t)
	writeCertPair(t, newCert, newKey, 2)
	for link, target := range map[string]string{certLink: newCert, keyLink: newKey} {
		require.NoError(t, os.Symlink(target, link+".tmp"))
		require.NoError(t, os.Rename(link+".tmp", link))
	}

	relay.CheckTLSCert(srv)

	assert.Equal(t, int64(2), servedSerial(t, addr))
}

func TestTURNS_should_keep_last_good_cert_when_new_files_are_invalid(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	srv, addr := startTLSRelay(t, certFile, keyFile, &bytes.Buffer{})
	require.NoError(t, os.WriteFile(certFile, []byte("not a certificate"), 0o600))

	relay.CheckTLSCert(srv)

	assert.Equal(t, int64(1), servedSerial(t, addr))
}

func TestTURNS_should_load_a_half_written_pair_once_it_is_complete(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	srv, addr := startTLSRelay(t, certFile, keyFile, &bytes.Buffer{})
	newCert, newKey := writeSelfSignedCert(t)
	writeCertPair(t, newCert, newKey, 3)
	require.NoError(t, os.Rename(newCert, certFile))
	relay.CheckTLSCert(srv)
	require.Equal(t, int64(1), servedSerial(t, addr))
	require.NoError(t, os.Rename(newKey, keyFile))

	relay.CheckTLSCert(srv)

	assert.Equal(t, int64(3), servedSerial(t, addr))
}

func TestTURNS_should_keep_serving_when_a_file_is_deleted_and_reload_when_restored(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	srv, addr := startTLSRelay(t, certFile, keyFile, &bytes.Buffer{})
	require.NoError(t, os.Remove(keyFile))
	relay.CheckTLSCert(srv)
	require.Equal(t, int64(1), servedSerial(t, addr))
	writeCertPair(t, certFile, keyFile, 2)

	relay.CheckTLSCert(srv)

	assert.Equal(t, int64(2), servedSerial(t, addr))
}

func TestTURNS_should_log_a_repeated_failure_once(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	logs := &bytes.Buffer{}
	srv, _ := startTLSRelay(t, certFile, keyFile, logs)
	require.NoError(t, os.Remove(keyFile))

	relay.CheckTLSCert(srv)
	relay.CheckTLSCert(srv)

	assert.Equal(t, 1, strings.Count(logs.String(), "reload tls keypair failed"))
}
