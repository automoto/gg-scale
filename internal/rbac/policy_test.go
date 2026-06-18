package rbac

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultPolicyProjectObjectsHaveHelpers(t *testing.T) {
	const projectID = int64(99)
	helperObjects := map[string]struct{}{
		toPolicyProjectObject(ProjectPlayersObject(projectID)):              {},
		toPolicyProjectObject(ProjectConfigObject(projectID)):               {},
		toPolicyProjectObject(ProjectFleetObject(projectID)):                {},
		toPolicyProjectObject(ProjectAllocationObject(projectID)):           {},
		toPolicyProjectObject(ProjectMatchmakerObject(projectID)):           {},
		toPolicyProjectObject(ProjectRelayObject(projectID)):                {},
		toPolicyProjectObject(ProjectDedicatedMatchmakingObject(projectID)): {},
	}

	for _, line := range strings.Split(defaultPolicyCSV, "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 4 || strings.TrimSpace(parts[0]) != "p" {
			continue
		}
		obj := strings.TrimSpace(parts[3])
		if !strings.HasPrefix(obj, "project:*:") {
			continue
		}
		assert.Contains(t, helperObjects, obj)
	}
}

func toPolicyProjectObject(obj string) string {
	return strings.Replace(obj, ":99:", ":*:", 1)
}
