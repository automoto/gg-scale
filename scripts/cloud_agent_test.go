package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cursorPort struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

type cursorBuild struct {
	Dockerfile string `json:"dockerfile"`
	Context    string `json:"context"`
}

func TestCursorEnvironmentUsesRepositoryDockerfile(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, ".cursor", "environment.json"))
	require.NoError(t, err)

	var environment struct {
		Build cursorBuild `json:"build"`
	}
	require.NoError(t, json.Unmarshal(data, &environment))

	assert.Equal(t, cursorBuild{Dockerfile: "Dockerfile", Context: ".."}, environment.Build)
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

func TestCloudAgentDockerfileProvidesSystemToolchain(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, ".cursor", "Dockerfile"))
	require.NoError(t, err)
	dockerfile := string(data)

	tests := []struct {
		name string
		want string
	}{
		{name: "Ubuntu base", want: "FROM ubuntu:24.04"},
		{name: "Go version", want: "ARG GO_VERSION=1.26.5"},
		{name: "Docker version", want: "ARG DOCKER_ENGINE_VERSION=29.8.0"},
		{name: "lint version", want: "ARG GOLANGCI_LINT_VERSION=v2.11.4"},
		{name: "Compose plugin", want: "docker-compose-plugin"},
		{name: "nested storage driver", want: `"storage-driver": "fuse-overlayfs"`},
		{name: "legacy firewall", want: "iptables-legacy"},
		{name: "Docker access", want: "usermod -aG docker ubuntu"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, dockerfile, tt.want)
		})
	}
}

func TestCloudAgentDockerfileExposesGoOnDefaultPath(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, ".cursor", "Dockerfile"))
	require.NoError(t, err)

	found := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.Contains(line, "/usr/local/bin/go") {
			continue
		}
		if strings.Contains(line, "golangci-lint") {
			continue
		}
		found = true
		break
	}

	assert.True(t, found, "Dockerfile must put go on /usr/local/bin; Cloud Agent install does not inherit image ENV PATH")
}

func TestCloudAgentDockerfileDoesNotCopyRepository(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, ".cursor", "Dockerfile"))
	require.NoError(t, err)

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "COPY ") {
			continue
		}
		assert.Contains(t, line, "--from=", line)
	}
}

func TestCloudAgentInstallIsIdempotent(t *testing.T) {
	repoDir := t.TempDir()
	cursorDir := filepath.Join(repoDir, ".cursor")
	require.NoError(t, os.Mkdir(cursorDir, 0o755))
	copyFile(t, repoPath(t, ".cursor", "install.sh"), filepath.Join(cursorDir, "install.sh"), 0o755)
	copyFile(t, repoPath(t, ".env.example"), filepath.Join(repoDir, ".env.example"), 0o644)

	binDir := t.TempDir()
	goLog := filepath.Join(repoDir, "go.log")
	writeExecutable(t, filepath.Join(binDir, "go"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CLOUD_AGENT_GO_LOG\"\n")

	for range 2 {
		cmd := exec.Command("/bin/bash", filepath.Join(cursorDir, "install.sh"))
		cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "CLOUD_AGENT_GO_LOG="+goLog)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
	}

	environment, err := os.ReadFile(filepath.Join(repoDir, ".env"))
	require.NoError(t, err)
	environmentExample, err := os.ReadFile(filepath.Join(repoDir, ".env.example"))
	require.NoError(t, err)
	assert.Equal(t, environmentExample, environment)

	goCalls, err := os.ReadFile(goLog)
	require.NoError(t, err)
	assert.Equal(t, "mod download\nbuild ./...\nmod download\nbuild ./...\n", string(goCalls))
}

func TestCloudAgentStartAvoidsWorldWritablePaths(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, ".cursor", "start.sh"))
	require.NoError(t, err)
	script := string(data)

	for _, forbidden := range []string{"chmod 666", "chmod 0777"} {
		t.Run(forbidden, func(t *testing.T) {
			assert.NotContains(t, script, forbidden)
		})
	}
}

func repoPath(t *testing.T, elements ...string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(append([]string{filepath.Dir(filename), ".."}, elements...)...)
}

func copyFile(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	contents, err := os.ReadFile(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(destination, contents, mode))
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o755))
}
