package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CompatibleFunc returns true when a discovered daemon can serve this client.
type CompatibleFunc func(RuntimeRecord, PingInfo) bool

// StartFunc starts a daemon in the background.
type StartFunc func(context.Context) error

// Manager coordinates discovery and optional auto-start.
type Manager struct {
	Store      RuntimeStore
	Discover   DiscoverOptions
	Compatible CompatibleFunc
	Start      StartFunc
}

// Find returns a live compatible daemon, when one is already running.
func (m Manager) Find(ctx context.Context) (RuntimeRecord, PingInfo, bool, error) {
	rec, info, ok, err := Discover(ctx, m.Store, m.Discover)
	if err != nil || !ok {
		return RuntimeRecord{}, PingInfo{}, false, err
	}
	if m.Compatible != nil && !m.Compatible(rec, info) {
		return RuntimeRecord{}, PingInfo{}, false, nil
	}
	return rec, info, true, nil
}

// Ensure returns a live compatible daemon, starting one when necessary.
func (m Manager) Ensure(ctx context.Context, timeout time.Duration) (RuntimeRecord, PingInfo, error) {
	if rec, info, ok, err := m.Find(ctx); err != nil || ok {
		return rec, info, err
	}
	if m.Start == nil {
		return RuntimeRecord{}, PingInfo{}, errors.New("daemon not running")
	}
	if err := m.Start(ctx); err != nil {
		return RuntimeRecord{}, PingInfo{}, err
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		rec, info, ok, err := m.Find(ctx)
		if err != nil {
			lastErr = err
		} else if ok {
			return rec, info, nil
		}
		select {
		case <-ctx.Done():
			return RuntimeRecord{}, PingInfo{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return RuntimeRecord{}, PingInfo{}, fmt.Errorf("daemon failed to start within %s: %w", timeout, lastErr)
	}
	return RuntimeRecord{}, PingInfo{}, fmt.Errorf("daemon failed to start within %s", timeout)
}
