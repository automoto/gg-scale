package relay

// CheckTLSCert runs one TURNS certificate check, as the poll loop would.
func CheckTLSCert(s *Server) {
	s.certs.check()
}
