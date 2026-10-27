package relay

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves the TURNS certificate and reads the key pair again when
// either file's modification time changes, so a renewed certificate is used
// without restarting the relay and dropping its allocations. A pair that fails
// to load keeps the last good certificate and is retried on the next change.
type certReloader struct {
	certFile, keyFile string
	logger            *slog.Logger

	mu              sync.Mutex
	cert            *tls.Certificate
	certMod, keyMod time.Time
}

func newCertReloader(certFile, keyFile string, logger *slog.Logger) (*certReloader, error) {
	certMod, keyMod, err := modTimes(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &certReloader{
		certFile: certFile,
		keyFile:  keyFile,
		logger:   logger,
		cert:     &cert,
		certMod:  certMod,
		keyMod:   keyMod,
	}, nil
}

// GetCertificate implements tls.Config.GetCertificate. It stats both files on
// every handshake; TURNS handshakes are rare enough for that to be cheap.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	certMod, keyMod, err := modTimes(r.certFile, r.keyFile)
	if err != nil || (certMod.Equal(r.certMod) && keyMod.Equal(r.keyMod)) {
		return r.cert, nil
	}
	r.certMod, r.keyMod = certMod, keyMod

	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		r.logger.Error("relay: reload tls keypair failed; serving the previous certificate", "err", err)
		return r.cert, nil
	}
	r.cert = &cert
	r.logger.Info("relay: reloaded tls keypair", "cert_file", r.certFile)
	return r.cert, nil
}

func modTimes(certFile, keyFile string) (time.Time, time.Time, error) {
	certInfo, err := os.Stat(certFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("stat tls cert: %w", err)
	}
	keyInfo, err := os.Stat(keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("stat tls key: %w", err)
	}
	return certInfo.ModTime(), keyInfo.ModTime(), nil
}
