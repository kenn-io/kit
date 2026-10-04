package posthog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type heartbeatClient struct {
	mu     sync.Mutex
	events []string
	err    error
}

func (c *heartbeatClient) Capture(event string, _ map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	return c.err
}

func (c *heartbeatClient) Close() error  { return nil }
func (c *heartbeatClient) Enabled() bool { return true }

func (c *heartbeatClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func TestRunHeartbeatSendsAtStartAndOnEachTick(t *testing.T) {
	client := &heartbeatClient{}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHeartbeat(ctx, client, ticks, nil)
	}()

	require.Eventually(t, func() bool { return client.count() == 1 }, time.Second, time.Millisecond)
	ticks <- time.Now()
	ticks <- time.Now()
	require.Eventually(t, func() bool { return client.count() == 3 }, time.Second, time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, []string{EventDaemonActive, EventDaemonActive, EventDaemonActive}, client.events)
}

func TestRunHeartbeatLogsCaptureErrorsAndKeepsRunning(t *testing.T) {
	client := &heartbeatClient{err: errors.New("queue full")}
	var logs bytes.Buffer
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHeartbeat(ctx, client, ticks, slog.New(slog.NewTextHandler(&logs, nil)))
	}()

	require.Eventually(t, func() bool { return client.count() == 2 }, time.Second, time.Millisecond)
	cancel()
	<-done

	assert.Contains(t, logs.String(), "queue full")
}

func TestRunHeartbeatSendsNothingOnceCanceled(t *testing.T) {
	client := &heartbeatClient{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	RunHeartbeat(ctx, client, nil)

	assert.Zero(t, client.count())
}
