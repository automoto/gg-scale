package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPartyOpenAPIIncludesPlayerOperations(t *testing.T) {
	doc := OpenAPIDoc("test")
	for _, path := range []string{"/v1/parties", "/v1/parties/current", "/v1/parties/{id}", "/v1/parties/{id}/members/me", "/v1/parties/{id}/members/{player_id}", "/v1/parties/{id}/members/me/ready", "/v1/parties/{id}/heartbeat", "/v1/parties/{id}/invites", "/v1/party-invites", "/v1/party-invites/{id}/accept", "/v1/party-invites/{id}", "/v1/parties/{id}/invite-codes", "/v1/parties/{id}/invite-codes/{code_id}", "/v1/parties/join", "/v1/parties/{id}/queue", "/v1/parties/{id}/rematch"} {
		assert.Contains(t, doc.Paths, path)
	}
}

func TestPartyMutationSchemasRequireExpectedVersion(t *testing.T) {
	doc := OpenAPIDoc("test")
	for _, name := range []string{"PartyCodeBody", "PartyInviteBody", "PartyReadyInputBody", "PartyRematchInputBody", "PartySettingsBody"} {
		schema := doc.Components.Schemas.Map()[name]
		if assert.NotNil(t, schema) {
			assert.Contains(t, schema.Required, "expected_version", name)
		}
	}
}
