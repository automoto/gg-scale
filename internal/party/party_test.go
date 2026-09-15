package party

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestInviteCodeHasEightyRandomBits(t *testing.T) {
	code, err := newCode()
	if !assert.NoError(t, err) {
		return
	}
	assert.Regexp(t, `^[0123456789ABCDEFGHJKMNPQRSTVWXYZ]{16}$`, code)
}

func TestInviteCodeAcceptsCaseAndDisplayHyphens(t *testing.T) {
	assert.Equal(t, codeHash("0123456789ABCDEF"), codeHash("0123-4567-89ab-cdef"))
}

func TestRosterChangeClearsEveryMemberReady(t *testing.T) {
	p := Party{Version: 1, RosterVersion: 1, Members: []Member{{PlayerID: 1, ReadyVersion: 1}, {PlayerID: 2, ReadyVersion: 1}}}
	p.rosterChanged()
	assert.Equal(t, Party{Version: 2, RosterVersion: 2, Members: []Member{{PlayerID: 1}, {PlayerID: 2}}}, p)
}

func TestQueueRequiresLeaderReadyAndCapacity(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		caller int64
		ready  int64
		max    int
		want   error
	}{
		{"leader only", 2, 1, 2, ErrNotLeader}, {"leader must be ready", 1, 0, 2, ErrNotReady},
		{"capacity", 1, 1, 1, ErrModeCapacity}, {"ready roster", 1, 1, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Party{LeaderID: 1, State: "idle", RosterVersion: 1, Settings: Settings{MaxCount: tc.max}, Members: []Member{{PlayerID: 1, ReadyVersion: tc.ready, DisconnectDeadline: now.Add(time.Second)}, {PlayerID: 2, ReadyVersion: 1, DisconnectDeadline: now.Add(time.Second)}}}
			assert.ErrorIs(t, p.canQueue(tc.caller, now), tc.want)
		})
	}
}

func TestPromotionUsesConnectedOldestThenPlayerID(t *testing.T) {
	now := time.Now()
	p := Party{LeaderID: 9, Members: []Member{{PlayerID: 1, JoinedAt: now.Add(-time.Hour), DisconnectDeadline: now}, {PlayerID: 3, JoinedAt: now, DisconnectDeadline: now.Add(time.Second)}, {PlayerID: 2, JoinedAt: now, DisconnectDeadline: now.Add(time.Second)}}}
	p.promote(now)
	assert.Equal(t, int64(2), p.LeaderID)
}

func TestInviteCodeNormalizationDoesNotDiscardOtherCharacters(t *testing.T) {
	assert.NotEqual(t, codeHash(strings.Repeat("A", 16)), codeHash(strings.Repeat("A", 16)+"!"))
}

func TestSettingsRejectInvalidQueueCriteria(t *testing.T) {
	for _, settings := range []Settings{
		{Mode: "unknown", MinCount: 1, MaxCount: 2, CountMultiple: 1},
		{Mode: "match_only", MinCount: 3, MaxCount: 2, CountMultiple: 1},
		{Mode: "match_only", MinCount: 1, MaxCount: 2, CountMultiple: 0},
		{Mode: "fleet_allocation", MinCount: 1, MaxCount: 2, CountMultiple: 1},
		{Mode: "match_only", MinCount: 1, MaxCount: 2, CountMultiple: 1, Query: "("},
	} {
		assert.Error(t, settings.Validate())
	}
}

func TestSettingsAcceptValidQueueCriteria(t *testing.T) {
	assert.NoError(t, (Settings{Mode: "match_only", MinCount: 2, MaxCount: 4, CountMultiple: 2, Query: "*"}).Validate())
}
