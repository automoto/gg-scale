package rbac

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		toPolicyProjectObject(ProjectLeaderboardObject(projectID)):          {},
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

// TestPlatformAdminPolicy_superset_of_tenant_owner guards against the lockout
// bug class that motivated the old code-level bypass: a control-panel route
// permission that tenant_owner holds but platform_admin lacks would silently
// lock platform admins out of tenant administration. Platform admins must be
// able to do at least everything a tenant owner can, in every tenant.
func TestPlatformAdminPolicy_superset_of_tenant_owner(t *testing.T) {
	rules := parsePolicyRules(t)
	admin := rules[RolePlatformAdmin]
	require.NotEmpty(t, admin)
	require.NotEmpty(t, rules[RoleTenantOwner])

	for _, rule := range rules[RoleTenantOwner] {
		assert.True(t, policyGrants(admin, rule.obj, rule.act),
			"platform_admin policy is missing tenant_owner grant %q %q", rule.obj, rule.act)
	}
}

type policyRule struct {
	obj, act string
}

// parsePolicyRules groups defaultPolicyCSV p-rules by subject role.
func parsePolicyRules(t *testing.T) map[string][]policyRule {
	t.Helper()
	rules := make(map[string][]policyRule)
	for _, line := range strings.Split(defaultPolicyCSV, "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 5 || strings.TrimSpace(parts[0]) != "p" {
			continue
		}
		sub := strings.TrimSpace(parts[1])
		rules[sub] = append(rules[sub], policyRule{
			obj: strings.TrimSpace(parts[3]),
			act: strings.TrimSpace(parts[4]),
		})
	}
	return rules
}

func policyGrants(rules []policyRule, obj, act string) bool {
	for _, r := range rules {
		if r.obj == obj && (r.act == "*" || r.act == act) {
			return true
		}
	}
	return false
}
