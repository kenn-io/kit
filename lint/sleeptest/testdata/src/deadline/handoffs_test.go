package deadline

import (
	"deadline/helper"
	"deadlineother"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

type durationAlias = time.Duration
type config struct {
	Timeout durationAlias
	Count   int
}

func (config) Wait(time.Duration)           {}
func probe(*testing.T, time.Duration)       {}
func watcher(int, time.Duration, any)       {}
func waits(time.Duration, ...time.Duration) {}

const watcherInterval = 100 * time.Millisecond

func TestHandoffs(t *testing.T) {
	_ = http.Client{Timeout: 20 * time.Millisecond}              // want "Timeout with budget 20ms"
	c := config{20 * time.Millisecond, 20}                       // want "Timeout with budget 20ms"
	c.Timeout = 30 * time.Millisecond                            // want "Timeout with budget 30ms"
	probe(t, 300*time.Millisecond)                               // want "probe with budget 300ms"
	watcher(0, 100*time.Millisecond, nil)                        // want "watcher with budget 100ms"
	waits(watcherInterval)                                       // want "waits with budget 100ms"
	waits(time.Second, 10*time.Millisecond, 20*time.Millisecond) // want "waits with budget 10ms" "waits with budget 20ms"
	c.Wait(shortWait)                                            // want "Wait with budget 50ms"
	config.Wait(c, shortWait)                                    // want "Wait with budget 50ms"
	helper.Wait(shortWait)                                       // want "Wait with budget 50ms"
	_ = time.Now().Add(50 * time.Millisecond)                    // want "time.Now\\(\\).Add with budget 50ms"

	runtime := 20 * time.Millisecond
	_ = config{Timeout: runtime, Count: 20}
	_ = config{Timeout: 0}
	c.Count = 20
	c.Timeout = runtime
	waits(0)
	deadlineother.Wait(shortWait)
	_ = time.Now().Add(0)
	_ = time.Now().Add(runtime)
	synctest.Test(t, func(t *testing.T) {
		_ = config{Timeout: shortWait}
		c.Timeout = shortWait
		helper.Wait(shortWait)
		_ = time.Now().Add(shortWait)
	})
}
