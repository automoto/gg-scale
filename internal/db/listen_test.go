package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeListenConnection struct {
	wait func(context.Context) (*pgconn.Notification, error)
	ping func(context.Context) error
}

func (c *fakeListenConnection) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	return c.wait(ctx)
}

func (c *fakeListenConnection) Ping(ctx context.Context) error {
	return c.ping(ctx)
}

func TestListenForNotifications_should_resume_after_successful_liveness_ping(t *testing.T) {
	waitCalls := 0
	pingCalls := 0
	errStop := errors.New("stop")
	conn := &fakeListenConnection{}
	conn.wait = func(ctx context.Context) (*pgconn.Notification, error) {
		waitCalls++
		if waitCalls == 1 {
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if waitCalls == 2 {
			return &pgconn.Notification{Payload: "after ping"}, nil
		}
		return nil, errStop
	}
	conn.ping = func(ctx context.Context) error {
		pingCalls++
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline)
		return nil
	}
	var payloads []string

	err := listenForNotifications(context.Background(), conn, func(payload string) {
		payloads = append(payloads, payload)
	}, 10*time.Millisecond, 10*time.Millisecond)

	require.ErrorIs(t, err, errStop)
	assert.Equal(t, 1, pingCalls)
	assert.Equal(t, []string{"after ping"}, payloads)
}

func TestListenForNotifications_should_return_error_when_liveness_ping_fails(t *testing.T) {
	errPing := errors.New("blackholed connection")
	conn := &fakeListenConnection{
		wait: func(ctx context.Context) (*pgconn.Notification, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		ping: func(context.Context) error { return errPing },
	}

	err := listenForNotifications(context.Background(), conn, func(string) {
		t.Fatal("unexpected notification")
	}, 10*time.Millisecond, 10*time.Millisecond)

	require.ErrorIs(t, err, errPing)
	assert.ErrorContains(t, err, "listen liveness")
}

func TestListenForNotifications_should_bound_liveness_ping(t *testing.T) {
	conn := &fakeListenConnection{
		wait: func(ctx context.Context) (*pgconn.Notification, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		ping: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}

	err := listenForNotifications(context.Background(), conn, func(string) {}, 10*time.Millisecond, 10*time.Millisecond)

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestListenForNotifications_should_return_nil_on_cancellation(t *testing.T) {
	conn := &fakeListenConnection{
		wait: func(context.Context) (*pgconn.Notification, error) {
			return nil, context.Canceled
		},
		ping: func(context.Context) error {
			t.Fatal("unexpected ping")
			return nil
		},
	}

	err := listenForNotifications(context.Background(), conn, func(string) {}, time.Second, time.Second)

	assert.NoError(t, err)
}
