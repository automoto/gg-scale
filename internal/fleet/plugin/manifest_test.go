package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ggscale-fleet-ovh")
	require.NoError(t, os.WriteFile(bin+".manifest.toml", []byte(content), 0o600))
	return bin
}

func TestReadManifestReturnsNilWhenAbsent(t *testing.T) {
	dir := t.TempDir()

	got, err := readManifest(filepath.Join(dir, "ggscale-fleet-ghost"))

	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestReadManifestParsesAllFields(t *testing.T) {
	bin := writeManifest(t, `
# OVH fleet plugin
name = "ovh"
version = "1.0.0"
protocol_version = 1
`)

	got, err := readManifest(bin)

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ovh", got.Name)
	assert.Equal(t, "1.0.0", got.Version)
	assert.Equal(t, 1, got.ProtocolVersion)
}

func TestReadManifestSupportsPartialContent(t *testing.T) {
	bin := writeManifest(t, `name = "ovh"`)

	got, err := readManifest(bin)

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ovh", got.Name)
	assert.Empty(t, got.Version)
	assert.Zero(t, got.ProtocolVersion)
}

func TestReadManifestIgnoresUnknownFields(t *testing.T) {
	bin := writeManifest(t, `
name = "ovh"
forward_compat_field = "future"
`)

	got, err := readManifest(bin)

	require.NoError(t, err)
	assert.Equal(t, "ovh", got.Name)
}

func TestReadManifestStripsSingleAndDoubleQuotes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"double_quotes", `name = "ovh"`, "ovh"},
		{"single_quotes", `name = 'ovh'`, "ovh"},
		{"no_quotes", `name = ovh`, "ovh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeManifest(t, tc.content)

			got, err := readManifest(bin)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Name)
		})
	}
}

func TestReadManifestSkipsLinesWithoutEquals(t *testing.T) {
	// TOML sections like [meta] and stray plain text must be ignored — the
	// parser only recognises three flat keys; everything else is
	// forward-compat noise.
	bin := writeManifest(t, "this is not a kv line\nname = \"ovh\"")

	got, err := readManifest(bin)

	require.NoError(t, err)
	assert.Equal(t, "ovh", got.Name)
}

func TestReadManifestSkipsTomlSections(t *testing.T) {
	bin := writeManifest(t, "[meta]\nname = \"ovh\"")

	got, err := readManifest(bin)

	require.NoError(t, err)
	assert.Equal(t, "ovh", got.Name)
}

func TestReadManifestStripsInlineComments(t *testing.T) {
	bin := writeManifest(t, `name = "ovh" # author note`)

	got, err := readManifest(bin)

	require.NoError(t, err)
	assert.Equal(t, "ovh", got.Name)
}

func TestReadManifestStripsCommentsOnBareValues(t *testing.T) {
	bin := writeManifest(t, "name = ovh # x")

	got, err := readManifest(bin)

	require.NoError(t, err)
	assert.Equal(t, "ovh", got.Name)
}

func TestReadManifestPreservesHashInsideQuotes(t *testing.T) {
	bin := writeManifest(t, `name = "ab#cd"`)

	got, err := readManifest(bin)

	require.NoError(t, err)
	assert.Equal(t, "ab#cd", got.Name)
}

func TestReadManifestRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ggscale-fleet-ovh")
	huge := make([]byte, 5*1024)
	for i := range huge {
		huge[i] = '#'
	}
	require.NoError(t, os.WriteFile(bin+".manifest.toml", huge, 0o600))

	_, err := readManifest(bin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestReadManifestRejectsNonIntegerProtocolVersion(t *testing.T) {
	bin := writeManifest(t, `protocol_version = "one"`)

	_, err := readManifest(bin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "protocol_version")
}

func TestReadManifestRejectsMismatchedProtocolVersion(t *testing.T) {
	// Handshake.ProtocolVersion is 1; a manifest declaring 99 must fail
	// loudly rather than waiting for the gRPC handshake to time out.
	bin := writeManifest(t, `protocol_version = 99`)

	_, err := readManifest(bin)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "protocol_version")
	assert.Contains(t, err.Error(), "99")
}
