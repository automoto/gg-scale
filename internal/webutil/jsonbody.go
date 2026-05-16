package webutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrBodyTooLarge wraps *http.MaxBytesError when a request body exceeds
// the configured cap. Handlers can distinguish this from other decode
// failures (413 vs 400).
var ErrBodyTooLarge = errors.New("webutil: request body too large")

// DecodeJSON reads the request body capped at maxBytes and decodes JSON
// into a fresh T. Unknown fields fail the decode (catches client typos
// early). On a MaxBytesError the wrapped sentinel is ErrBodyTooLarge.
func DecodeJSON[T any](w http.ResponseWriter, r *http.Request, maxBytes int64) (T, error) {
	var out T
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return out, fmt.Errorf("%w: %d bytes max", ErrBodyTooLarge, maxBytes)
		}
		return out, fmt.Errorf("webutil: decode json: %w", err)
	}
	// Reject trailing junk; a second Decode after the first must hit EOF
	// or the body had extra content beyond a single JSON value.
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return out, fmt.Errorf("webutil: decode json: unexpected trailing data")
	}
	return out, nil
}
