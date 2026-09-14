package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cursorPort struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

func TestCursorEnvironmentPortsUseSchemaObjects(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, ".cursor", "environment.json"))
	require.NoError(t, err)

	var environment struct {
		Ports []cursorPort `json:"ports"`
	}
	require.NoError(t, json.Unmarshal(data, &environment))

	assert.Equal(t, []cursorPort{
		{Name: "ggscale", Port: 8080},
		{Name: "mailpit", Port: 8025},
		{Name: "prometheus", Port: 9090},
	}, environment.Ports)
}

func TestDockerToolchainReadyRequiresEveryCapability(t *testing.T) {
	script := repoPath(t, ".cursor", "install.sh")

	tests := []struct {
		name          string
		missing       string
		composeWorks  bool
		dockerVersion string
		want          bool
	}{
		{name: "complete toolchain", composeWorks: true, dockerVersion: "29.8.0", want: true},
		{name: "missing client", missing: "docker", composeWorks: true, dockerVersion: "29.8.0"},
		{name: "missing daemon", missing: "dockerd", composeWorks: true, dockerVersion: "29.8.0"},
		{name: "missing storage driver", missing: "fuse-overlayfs", composeWorks: true, dockerVersion: "29.8.0"},
		{name: "missing legacy iptables", missing: "iptables-legacy", composeWorks: true, dockerVersion: "29.8.0"},
		{name: "missing compose plugin", dockerVersion: "29.8.0"},
		{name: "different Docker version", composeWorks: true, dockerVersion: "29.7.2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			for _, tool := range []string{"docker", "dockerd", "fuse-overlayfs", "iptables-legacy"} {
				if tool == tt.missing {
					continue
				}
				writeFakeTool(t, binDir, tool, tool != "docker" || tt.composeWorks, tt.dockerVersion)
			}

			cmd := exec.Command("/bin/bash", "-c", `source "$1"; docker_toolchain_ready`, "cloud-agent-test", script)
			cmd.Env = []string{"PATH=" + binDir}
			output, err := cmd.CombinedOutput()

			assert.Equal(t, tt.want, err == nil, string(output))
		})
	}
}

func repoPath(t *testing.T, elements ...string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(append([]string{filepath.Dir(filename), ".."}, elements...)...)
}

func writeFakeTool(t *testing.T, dir, name string, succeeds bool, dockerVersion string) {
	t.Helper()
	exitCode := "1"
	if succeeds {
		exitCode = "0"
	}
	contents := []byte("#!/bin/sh\nexit " + exitCode + "\n")
	if name == "docker" {
		contents = []byte("#!/bin/sh\n" +
			"if [ \"$1\" = \"--version\" ]; then\n" +
			"  echo \"Docker version " + dockerVersion + ", build test\"\n" +
			"  exit 0\n" +
			"fi\n" +
			"exit " + exitCode + "\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), contents, 0o755))
}
