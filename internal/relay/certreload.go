package relay

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// certPollInterval is how often the relay checks its TURNS key pair for a
// renewal. Certificates renew weeks before they expire, so a minute is ample.
const certPollInterval = time.Minute

// certReloader serves the TURNS certificate and replaces it when the cert or
// key file content changes, so a renewed certificate is used without
// restarting the relay and dropping its allocations. Change detection compares
// file content with the last pair that loaded, so atomic symlink swaps and
// copies that keep timestamps are seen, and a pair that fails to load is tried
// again on every check until it loads.
type certReloader struct {
	certFile, keyFile string
	logger            *slog.Logger
	cert              atomic.Pointer[tls.Certificate]

	// Owned by check, which runs on one goroutine at a time.
	sum     [sha256.Size]byte
	lastErr string

	stop chan struct{}
	done chan struct{}
}

func newCertReloader(certFile, keyFile string, logger *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, logger: logger}
	cert, sum, err := r.load()
	if err != nil {
		return nil, err
	}
	r.cert.Store(cert)
	r.sum = sum
	return r, nil
}

// GetCertificate implements tls.Config.GetCertificate. It never touches the
// filesystem, so a slow mount cannot stall handshakes.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cert.Load(), nil
}

// start runs check every interval until close.
func (r *certReloader) start(interval time.Duration) {
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.check()
			}
		}
	}()
}

func (r *certReloader) close() {
	if r.stop == nil {
		return
	}
	close(r.stop)
	<-r.done
}

// check loads the pair when its content differs from the last good load. A
// failure keeps the current certificate and is logged once per distinct error.
func (r *certReloader) check() {
	cert, sum, err := r.load()
	if err != nil {
		if msg := err.Error(); msg != r.lastErr {
			r.lastErr = msg
			r.logger.Error("relay: reload tls keypair failed; serving the previous certificate", "err", err)
		}
		return
	}
	if r.lastErr != "" {
		r.lastErr = ""
		r.logger.Info("relay: tls keypair readable again")
	}
	if sum == r.sum {
		return
	}
	r.cert.Store(cert)
	r.sum = sum
	r.logger.Info("relay: reloaded tls keypair", "cert_file", r.certFile)
}

func (r *certReloader) load() (*tls.Certificate, [sha256.Size]byte, error) {
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	sum := sha256.Sum256(bytes.Join([][]byte{certPEM, keyPEM}, []byte{0}))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, sum, err
	}
	return &cert, sum, nil
}
