package plugin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Manifest is the optional metadata sidecar a plugin author drops alongside
// the binary as "<binary>.manifest.toml". Only flat top-level keys are
// supported — that's all the host needs and avoids a full TOML dependency.
type Manifest struct {
	Name            string
	Version         string
	ProtocolVersion int
}

// manifestMaxBytes caps the sidecar file size. A real manifest is well
// under 1 KiB; anything larger is either a misconfigured drop or an attempt
// to OOM the host.
const manifestMaxBytes = 4096

// readManifest loads "<binaryPath>.manifest.toml" if present. Returns
// (nil, nil) when the file is absent. A declared protocol_version that does
// not match Handshake.ProtocolVersion is rejected here so the operator sees
// a clear error instead of a gRPC handshake timeout.
//
// The parser is intentionally permissive: lines without an `=` (TOML
// section headers, stray text) are skipped, and inline `#` comments are
// stripped from values. Unknown keys are ignored for forward compatibility.
func readManifest(binaryPath string) (*Manifest, error) {
	path := binaryPath + ".manifest.toml"
	f, err := os.Open(path) // #nosec G304 -- binaryPath is operator-controlled config
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("manifest: open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	data, err := io.ReadAll(io.LimitReader(f, manifestMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	if len(data) > manifestMaxBytes {
		return nil, fmt.Errorf("manifest: %s exceeds %d bytes", path, manifestMaxBytes)
	}

	m := &Manifest{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue // section headers / stray text — forward-compat skip
		}
		k = strings.TrimSpace(k)
		v = stripQuotes(strings.TrimSpace(stripComment(v)))
		switch k {
		case "name":
			m.Name = v
		case "version":
			m.Version = v
		case "protocol_version":
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 0 {
				return nil, fmt.Errorf("manifest: protocol_version must be a non-negative integer, got %q", v)
			}
			if uint(n) != Handshake.ProtocolVersion { //nolint:gosec // n is bounds-checked above
				return nil, fmt.Errorf("manifest: protocol_version %d does not match host %d", n, Handshake.ProtocolVersion)
			}
			m.ProtocolVersion = n
		}
	}
	return m, nil
}

// stripComment truncates s at the first unquoted '#'. Quote awareness keeps
// values like `"ab#cd"` intact.
func stripComment(s string) string {
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQuote = c
		case '#':
			return s[:i]
		}
	}
	return s
}

func stripQuotes(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
