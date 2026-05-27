package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sync/semaphore"
)

var startLocks sync.Map

// startLockRetryDelay is the poll interval; the caller context bounds total wait.
const startLockRetryDelay = 50 * time.Millisecond

type startLock struct {
	local *semaphore.Weighted
	file  *flock.Flock
}

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
	opts := m.Discover
	accept := opts.Accept
	opts.Accept = func(rec RuntimeRecord, info PingInfo) bool {
		if accept != nil && !accept(rec, info) {
			return false
		}
		return m.Compatible == nil || m.Compatible(rec, info)
	}
	return Discover(ctx, m.Store, opts)
}

// Ensure returns a live compatible daemon, starting one when necessary.
func (m Manager) Ensure(ctx context.Context, timeout time.Duration) (RuntimeRecord, PingInfo, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if rec, info, ok, err := m.Find(ctx); err != nil || ok {
		return rec, info, err
	}
	if m.Start == nil {
		return RuntimeRecord{}, PingInfo{}, errors.New("daemon not running")
	}
	unlock, err := m.lockStart(ctx)
	if err != nil {
		return RuntimeRecord{}, PingInfo{}, err
	}
	defer unlock()

	if rec, info, ok, err := m.Find(ctx); err != nil || ok {
		return rec, info, err
	}
	if err := m.Start(ctx); err != nil {
		return RuntimeRecord{}, PingInfo{}, err
	}
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

func (m Manager) lockStart(ctx context.Context) (func(), error) {
	lockPath, err := m.Store.LockPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir daemon lock dir: %w", err)
	}
	value, _ := startLocks.LoadOrStore(lockPath, &startLock{
		local: semaphore.NewWeighted(1),
		file:  flock.New(lockPath),
	})
	lock := value.(*startLock)
	if err := lock.local.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("acquire daemon start lock: %w", err)
	}
	locked, err := lock.file.TryLockContext(ctx, startLockRetryDelay)
	if err != nil {
		lock.local.Release(1)
		return nil, fmt.Errorf("acquire daemon start lock: %w", err)
	}
	if !locked {
		lock.local.Release(1)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("acquire daemon start lock: %w", err)
		}
		return nil, errors.New("daemon start lock not acquired")
	}
	return func() {
		_ = lock.file.Unlock()
		lock.local.Release(1)
	}, nil
}
