package dashboard

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmitBootstrapToken_writes_token_to_file(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "bootstrap.token")
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	err := emitBootstrapToken("testtoken123", tokenFile, logger, &bytes.Buffer{})
	require.NoError(t, err)

	content, err := os.ReadFile(tokenFile)
	require.NoError(t, err)
	assert.Equal(t, "testtoken123\n", string(content))
}

func TestEmitBootstrapToken_file_has_owner_only_permissions(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "bootstrap.token")
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	require.NoError(t, emitBootstrapToken("testtoken123", tokenFile, logger, &bytes.Buffer{}))

	info, err := os.Stat(tokenFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestEmitBootstrapToken_token_absent_from_log_when_file_set(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "bootstrap.token")
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	require.NoError(t, emitBootstrapToken("testtoken123", tokenFile, logger, &bytes.Buffer{}))

	assert.NotContains(t, logBuf.String(), "testtoken123")
	assert.Contains(t, logBuf.String(), tokenFile)
}

func TestEmitBootstrapToken_no_file_writes_token_to_writer(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	var out bytes.Buffer

	require.NoError(t, emitBootstrapToken("testtoken123", "", logger, &out))

	assert.Contains(t, out.String(), "testtoken123")
}

func TestEmitBootstrapToken_no_file_token_absent_from_log(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	require.NoError(t, emitBootstrapToken("testtoken123", "", logger, &bytes.Buffer{}))

	assert.NotContains(t, logBuf.String(), "testtoken123")
	assert.Contains(t, logBuf.String(), "DASHBOARD_BOOTSTRAP_TOKEN_FILE")
}

func TestNewBootstrap_StoresTokenFilePath(t *testing.T) {
	b := NewBootstrap("tok", "/tmp/x")
	assert.Equal(t, "/tmp/x", b.TokenFilePath())
}

func TestDisabledBootstrap_TokenFilePathEmpty(t *testing.T) {
	assert.Equal(t, "", DisabledBootstrap().TokenFilePath())
}

func TestBootstrap_TokenFilePathNilSafe(t *testing.T) {
	var b *Bootstrap
	assert.Equal(t, "", b.TokenFilePath())
}
