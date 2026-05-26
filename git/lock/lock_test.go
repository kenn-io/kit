package gitlock

import (
	"context"
	"testing"
)

func TestUnlockRejectsDoubleRelease(t *testing.T) {
	locker, err := New("").Acquire(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := locker.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := locker.Unlock(); err == nil {
		t.Fatal("second Unlock succeeded, want error")
	}
}
