package matchmaker

import (
	"testing"
	"testing/quick"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGroupsDoNotTrimPartyToMeetCountMultiple(t *testing.T) {
	now := time.Now()
	tickets := []*Ticket{
		{ID: 1, PlayerID: 1, EntryID: 1, PartyID: 10, MinCount: 2, MaxCount: 4, CountMultiple: 2, CreatedAt: now},
		{ID: 2, PlayerID: 2, EntryID: 1, PartyID: 10, MinCount: 2, MaxCount: 4, CountMultiple: 2, CreatedAt: now},
		{ID: 3, PlayerID: 3, EntryID: 1, PartyID: 10, MinCount: 2, MaxCount: 4, CountMultiple: 2, CreatedAt: now},
	}
	groups := formGroups(tickets, now, groupConfig{})
	assert.Empty(t, groups)
}

func TestGroupsIncludeCompletePartyAndSolo(t *testing.T) {
	now := time.Now()
	tickets := []*Ticket{
		{ID: 1, PlayerID: 1, EntryID: 1, PartyID: 10, MinCount: 3, MaxCount: 3, CountMultiple: 1, CreatedAt: now},
		{ID: 2, PlayerID: 2, EntryID: 1, PartyID: 10, MinCount: 3, MaxCount: 3, CountMultiple: 1, CreatedAt: now},
		{ID: 3, PlayerID: 3, EntryID: 2, MinCount: 3, MaxCount: 3, CountMultiple: 1, CreatedAt: now},
	}
	groups := formGroups(tickets, now, groupConfig{})
	assert.Equal(t, [][]*Ticket{tickets}, groups)
}

func TestGroupsAlwaysKeepEntriesWhole(t *testing.T) {
	property := func(sizes []uint8) bool {
		now := time.Now()
		var tickets []*Ticket
		counts := map[int64]int{}
		for i, size := range sizes {
			entry := int64(i + 1)
			for range int(size%8 + 1) {
				id := int64(len(tickets) + 1)
				tickets = append(tickets, &Ticket{ID: id, PlayerID: id, EntryID: entry, MinCount: 2, MaxCount: 8, CountMultiple: 2, CreatedAt: now})
				counts[entry]++
			}
		}
		seen := map[int64]bool{}
		for _, group := range formGroups(tickets, now, groupConfig{}) {
			got := map[int64]int{}
			for _, ticket := range group {
				got[ticket.EntryID]++
			}
			for entry, count := range got {
				if seen[entry] || count != counts[entry] {
					return false
				}
				seen[entry] = true
			}
		}
		return true
	}
	assert.NoError(t, quick.Check(property, &quick.Config{MaxCount: 200}))
}

func TestMemoryClaimKeepsPartyAtBatchBoundary(t *testing.T) {
	q := NewMemQueue()
	tickets, err := q.EnqueueEntry(t.Context(), 10, []EnqueueRequest{{TenantID: 1, ProjectID: 1, PlayerID: 1, MinCount: 2, MaxCount: 2}, {TenantID: 1, ProjectID: 1, PlayerID: 2, MinCount: 2, MaxCount: 2}})
	if !assert.NoError(t, err) {
		return
	}
	claim, err := q.ClaimBucket(t.Context(), bucketKey(tickets[0]), 1, time.Minute)
	if !assert.NoError(t, err) || !assert.NotNil(t, claim) {
		return
	}
	assert.Len(t, claim.Tickets, 2)
	_, err = q.CommitTickets(t.Context(), claim, []int64{tickets[0].ID}, "partial", "", "")
	assert.ErrorIs(t, err, ErrShortCommit)
}

func TestMemoryEntryRejectsDifferentQueueSettings(t *testing.T) {
	q := NewMemQueue()
	_, err := q.EnqueueEntry(t.Context(), 10, []EnqueueRequest{{TenantID: 1, ProjectID: 1, PlayerID: 1, Mode: ModeMatchOnly}, {TenantID: 1, ProjectID: 1, PlayerID: 2, Mode: ModeGameSession}})
	assert.ErrorIs(t, err, ErrShortCommit)
}
