package webutil

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// MinEmailLength / MaxEmailLength bound the addresses we accept anywhere
// in the codebase. RFC 5321 allows 254 bytes; we reject anything shorter
// than 5 ("a@b.c" is the minimum plausible form).
const (
	MinEmailLength = 5
	MaxEmailLength = 254
)

// ErrInvalidEmail is returned by ValidateEmail when the input is not a
// usable RFC 5322 address.
var ErrInvalidEmail = errors.New("webutil: invalid email address")

// ValidateEmail wraps net/mail.ParseAddress with three policy layers
// stdlib doesn't enforce: the RFC 5321 length cap, a refusal of the
// display-name "Alice <a@b.c>" form (signup fields should hold an
// address, not a mailbox), and lowercase normalisation of the domain.
// Control-char / CR / LF / NUL injection is already rejected by
// ParseAddress, so no separate scan is needed.
func ValidateEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < MinEmailLength || len(s) > MaxEmailLength {
		return "", fmt.Errorf("%w: length out of range", ErrInvalidEmail)
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidEmail, err)
	}
	if addr.Name != "" || addr.Address != s {
		return "", fmt.Errorf("%w: display name not allowed", ErrInvalidEmail)
	}
	local, domain, _ := strings.Cut(addr.Address, "@")
	return local + "@" + strings.ToLower(domain), nil
}
