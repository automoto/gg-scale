package relay

import "time"

// CheckTLSCert runs one TURNS certificate check, as the poll loop would.
func CheckTLSCert(s *Server) {
	s.certs.check()
}

// SetIssuerClock replaces the clock an Issuer checks expiry against.
func SetIssuerClock(i *Issuer, now func() time.Time) { i.now = now }
