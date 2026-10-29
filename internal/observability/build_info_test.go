package observability_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/observability"
)

func TestRegisterBuildInfo_should_expose_the_release_and_commit(t *testing.T) {
	reg := prometheus.NewRegistry()

	observability.RegisterBuildInfo(reg, "v0.9.76", "abc123")

	expected := `
# HELP ggscale_build_info Always 1. The labels name the release and commit this process runs.
# TYPE ggscale_build_info gauge
ggscale_build_info{commit="abc123",version="v0.9.76"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_build_info"))
}
