package webutil

import (
	"errors"
	"unicode"
	"unicode/utf8"
)

// MaxHeaderLineBytes is RFC 5322's line-length limit. Header values longer
// than this are rejected to avoid producing malformed messages.
const MaxHeaderLineBytes = 998

// ErrHeaderInjection is returned by SanitizeHeader when the input contains
// CR, LF, NUL, or other control characters that could split or hijack a
// protocol header.
var ErrHeaderInjection = errors.New("webutil: header contains control characters")

// ErrHeaderTooLong is returned when the input exceeds MaxHeaderLineBytes.
var ErrHeaderTooLong = errors.New("webutil: header exceeds maximum length")

// SanitizeHeader validates a value destined for a protocol header (SMTP
// From/To/Subject, HTTP X-* etc.) and returns it unchanged on success. It
// rejects \r, \n, NUL, and other C0/C1 control characters that an attacker
// could use to inject additional headers; it also rejects lines longer
// than RFC 5322 allows.
//
// The function never mutates input — callers receive either the same
// string or an error. Strip-vs-reject is intentionally reject-only: silent
// stripping changes the user's intended value (e.g. drops the newline in
// a multi-line display name), which is the kind of behaviour that hides
// bugs upstream.
func SanitizeHeader(s string) (string, error) {
	if len(s) > MaxHeaderLineBytes {
		return "", ErrHeaderTooLong
	}
	if !utf8.ValidString(s) {
		return "", ErrHeaderInjection
	}
	for _, r := range s {
		// Tab is the one control character RFC 5322 permits inside a
		// header value (it's used for folded continuation lines).
		if r == '\t' {
			continue
		}
		// Reject every other control character — C0 (NUL through US),
		// DEL, and the C1 range. CR and LF are the injection vectors of
		// interest but the broader check is what closes the boundary.
		if unicode.IsControl(r) {
			return "", ErrHeaderInjection
		}
	}
	return s, nil
}
