package relay

import (
	"errors"
	"net/url"
)

// ValidateTURNURI accepts the turn:/turns: URI forms clients dial. TURN URIs
// are opaque (turn:host:port?transport=udp), so the host lands in Opaque
// rather than Host after url.Parse.
func ValidateTURNURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "turn" && u.Scheme != "turns" {
		return errors.New("must use the turn: or turns: scheme")
	}
	host := u.Opaque
	if host == "" {
		host = u.Host
	}
	if host == "" {
		return errors.New("missing host")
	}
	return nil
}
