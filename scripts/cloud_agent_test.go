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
	for _, name := range []string{
		filepath.Join(".cursor", "start.sh"),
		filepath.Join("scripts", "bootstrap-token.sh"),
	} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(repoPath(t, name))
			require.NoError(t, err)
			script := string(data)

			for _, forbidden := range []string{"chmod 666", "chmod 0777"} {
				assert.NotContains(t, script, forbidden)
			}
		})
	}
}

func TestBootstrapTokenDocsUsePlainCat(t *testing.T) {
	for _, name := range []string{"README.md", "AGENTS.md", "docker-compose.yml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(repoPath(t, name))
			require.NoError(t, err)
			text := string(data)

			assert.NotContains(t, text, "sudo cat")
			assert.Contains(t, text, "cat ./data/bootstrap.token")
		})
	}
}

func TestBootstrapTokenScriptChownsExistingFile(t *testing.T) {
	repoDir := setupBootstrapTokenRepo(t)
	tokenFile := filepath.Join(repoDir, "data", "bootstrap.token")
	require.NoError(t, os.Mkdir(filepath.Join(repoDir, "data"), 0o755))
	require.NoError(t, os.WriteFile(tokenFile, []byte("secret\n"), 0o640))

	output := runBootstrapTokenScript(t, repoDir, nil)

	assert.Contains(t, output, "bootstrap.token")
	info, err := os.Stat(tokenFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	sudoCalls, err := os.ReadFile(filepath.Join(repoDir, "sudo.log"))
	require.NoError(t, err)
	assert.Contains(t, string(sudoCalls), "chown")
	assert.Contains(t, string(sudoCalls), "chmod 0600")
}

func TestBootstrapTokenScriptReportsMissingFile(t *testing.T) {
	repoDir := setupBootstrapTokenRepo(t)

	output, err := runBootstrapTokenScriptResult(t, repoDir, map[string]string{
		"BOOTSTRAP_TOKEN_WAIT": "0",
	})

	require.Error(t, err)
	assert.Contains(t, output, "make up")
}

func TestBootstrapTokenScriptIfPresentSkipsMissingFile(t *testing.T) {
	repoDir := setupBootstrapTokenRepo(t)

	output, err := runBootstrapTokenScriptResult(t, repoDir, map[string]string{
		"BOOTSTRAP_TOKEN_WAIT": "0",
	}, "--if-present")

	require.NoError(t, err, output)
	assert.Contains(t, output, "make up")
}

func TestCloudAgentStartClaimsExistingBootstrapToken(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, ".cursor", "start.sh"))
	require.NoError(t, err)
	script := string(data)

	assert.Contains(t, script, `install -d -m 0755 -o "$SERVER_UID" -g "$SERVER_GID" data`)
	assert.Contains(t, script, "scripts/bootstrap-token.sh --if-present")
}

func TestBootstrapTokenScriptPrepareRestoresServerOwner(t *testing.T) {
	repoDir := setupBootstrapTokenRepo(t)
	tokenFile := filepath.Join(repoDir, "data", "bootstrap.token")
	require.NoError(t, os.Mkdir(filepath.Join(repoDir, "data"), 0o755))
	require.NoError(t, os.WriteFile(tokenFile, []byte("secret\n"), 0o600))

	output, err := runBootstrapTokenScriptResult(t, repoDir, nil, "--prepare")

	require.NoError(t, err, output)
	sudoCalls, err := os.ReadFile(filepath.Join(repoDir, "sudo.log"))
	require.NoError(t, err)
	assert.Contains(t, string(sudoCalls), "chown 65532:65532")
	assert.Contains(t, string(sudoCalls), "chmod 0600")
}

func TestBootstrapTokenScriptPrepareIsNoopWhenMissing(t *testing.T) {
	repoDir := setupBootstrapTokenRepo(t)

	output, err := runBootstrapTokenScriptResult(t, repoDir, nil, "--prepare")

	require.NoError(t, err, output)
	_, statErr := os.Stat(filepath.Join(repoDir, "sudo.log"))
	assert.Error(t, statErr)
}

func TestMakefileExposesBootstrapTokenTarget(t *testing.T) {
	data, err := os.ReadFile(repoPath(t, "Makefile"))
	require.NoError(t, err)
	text := string(data)

	assert.Contains(t, text, "bootstrap-token")
	assert.Contains(t, text, "scripts/bootstrap-token.sh")
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

func setupBootstrapTokenRepo(t *testing.T) string {
	t.Helper()
	repoDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoDir, "scripts"), 0o755))
	copyFile(t, repoPath(t, "scripts", "bootstrap-token.sh"), filepath.Join(repoDir, "scripts", "bootstrap-token.sh"), 0o755)
	writeExecutable(t, filepath.Join(repoDir, "sudo"), `#!/bin/sh
printf '%s\n' "$*" >> "$SUDO_LOG"
if [ "$1" = "chown" ]; then
  case "$2" in
    65532:65532) exit 0 ;;
  esac
fi
exec "$@"
`)
	return repoDir
}

func runBootstrapTokenScript(t *testing.T, repoDir string, extraEnv map[string]string, args ...string) string {
	t.Helper()
	output, err := runBootstrapTokenScriptResult(t, repoDir, extraEnv, args...)
	require.NoError(t, err, output)
	return output
}

func runBootstrapTokenScriptResult(t *testing.T, repoDir string, extraEnv map[string]string, args ...string) (string, error) {
	t.Helper()
	cmdArgs := append([]string{filepath.Join(repoDir, "scripts", "bootstrap-token.sh")}, args...)
	cmd := exec.Command("/bin/bash", cmdArgs...)
	cmd.Dir = repoDir
	env := append(os.Environ(),
		"PATH="+repoDir+":"+os.Getenv("PATH"),
		"SUDO_LOG="+filepath.Join(repoDir, "sudo.log"),
	)
	for key, value := range extraEnv {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	return string(output), err
}
