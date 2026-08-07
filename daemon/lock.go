package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sync/semaphore"
)

var daemonLocks sync.Map

// daemonLockRetryDelay is the poll interval; the caller context bounds total wait.
const daemonLockRetryDelay = 50 * time.Millisecond

type daemonLock struct {
	local *semaphore.Weighted
	file  *flock.Flock
}

// AcquireStartLock serializes discovery, replacement, and daemon launch for a
// runtime store. The caller must invoke the returned release function.
func (s RuntimeStore) AcquireStartLock(ctx context.Context) (func(), error) {
	path, err := s.LockPath()
	if err != nil {
		return nil, err
	}
	return acquireDaemonLock(ctx, path, "acquire daemon start lock")
}

// AcquireOwnerLock grants exclusive writable ownership for the lifetime of a
// daemon. The caller must retain the lock until server teardown is complete.
func (s RuntimeStore) AcquireOwnerLock(ctx context.Context) (func(), error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return nil, err
	}
	if err := s.prepareDir(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, prefix+".owner.lock")
	return acquireDaemonLock(ctx, path, "acquire daemon owner lock")
}

func acquireDaemonLock(ctx context.Context, lockPath, action string) (func(), error) {
	if lockPath == "" {
		return nil, fmt.Errorf("%s: empty daemon lock path", action)
	}
	if !filepath.IsAbs(lockPath) {
		return nil, fmt.Errorf("%s: daemon lock path %q must be absolute", action, lockPath)
	}
	if err := ensurePrivateRuntimeDir(filepath.Dir(lockPath)); err != nil {
		return nil, fmt.Errorf("prepare daemon lock dir: %w", err)
	}
	value, _ := daemonLocks.LoadOrStore(lockPath, &daemonLock{
		local: semaphore.NewWeighted(1),
		file:  flock.New(lockPath),
	})
	lock := value.(*daemonLock)
	if err := lock.local.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	locked, err := lock.file.TryLockContext(ctx, daemonLockRetryDelay)
	if err != nil {
		lock.local.Release(1)
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if !locked {
		lock.local.Release(1)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		return nil, errors.New(action + ": lock not acquired")
	}
	return func() {
		_ = lock.file.Unlock()
		lock.local.Release(1)
	}, nil
}
