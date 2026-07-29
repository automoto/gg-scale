package gamesession

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/matchmaker"
)

type fakeCreator struct {
	params  CreateParams
	created *Created
	err     error
}

func (f *fakeCreator) Create(_ context.Context, p CreateParams) (*Created, error) {
	f.params = p
	if f.err != nil {
		return nil, f.err
	}
	return f.created, nil
}

func TestMatchAdapter_should_map_project_cap_to_capacity_error(t *testing.T) {
	a := &MatchAdapter{svc: &fakeCreator{err: ErrProjectCapped}}

	_, _, err := a.CreateMatchSession(context.Background(), 7, "1v1", []int64{1, 2})

	assert.ErrorIs(t, err, matchmaker.ErrCapacity)
}

func TestMatchAdapter_should_keep_other_errors_unmapped(t *testing.T) {
	a := &MatchAdapter{svc: &fakeCreator{err: errors.New("db down")}}

	_, _, err := a.CreateMatchSession(context.Background(), 7, "1v1", []int64{1, 2})

	require.Error(t, err)
	assert.NotErrorIs(t, err, matchmaker.ErrCapacity)
}

func TestMatchAdapter_should_create_with_pending_ttl(t *testing.T) {
	f := &fakeCreator{created: &Created{SessionID: "gs_x", JoinCode: "ABC123"}}
	a := &MatchAdapter{svc: f}

	_, _, err := a.CreateMatchSession(context.Background(), 7, "1v1", []int64{1, 2})

	require.NoError(t, err)
	assert.Equal(t, MatchPendingTTL, f.params.TTL,
		"an unjoined matchmade session must expire quickly, not hold a cap slot for hours")
}
