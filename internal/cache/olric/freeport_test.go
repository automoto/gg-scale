package olric_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// freePort asks the kernel for an unused TCP port and returns it. Race window
// between Close and the caller binding is acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}
