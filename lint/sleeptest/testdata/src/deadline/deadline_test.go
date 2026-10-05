package deadline

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shortWait = 50 * time.Millisecond

func ready() bool { return true }

func TestShortBudgetsOutsideBubble(t *testing.T) {
	ctx := context.Background()
	_, cancel := context.WithTimeout(ctx, 5*time.Millisecond) // want "context.WithTimeout with a 5ms budget in a test outside a synctest bubble races the wall clock"
	defer cancel()
	_, cancel = context.WithTimeoutCause(ctx, 10*time.Millisecond, errors.New("slow")) // want "context.WithTimeoutCause with a 10ms budget"
	defer cancel()
	_, cancel = context.WithDeadline(ctx, time.Now().Add(50*time.Millisecond)) // want "context.WithDeadline with a 50ms budget"
	defer cancel()
	_, cancel = context.WithDeadlineCause(ctx, time.Now().Add(shortWait), errors.New("slow")) // want "context.WithDeadlineCause with a 50ms budget"
	defer cancel()
	<-time.After(75 * time.Millisecond)           // want "time.After with a 75ms budget"
	timer := time.NewTimer(20 * time.Millisecond) // want "time.NewTimer with a 20ms budget"
	defer timer.Stop()
	time.AfterFunc(shortWait, func() {}).Stop()                                    // want "time.AfterFunc with a 50ms budget"
	require.Eventually(t, ready, 300*time.Millisecond, time.Millisecond)           // want "require.Eventually with a 300ms budget"
	assert.Never(t, ready, shortWait, time.Millisecond)                            // want "assert.Never with a 50ms budget"
	require.Eventuallyf(t, ready, 500*time.Millisecond, time.Millisecond, "ready") // want "require.Eventuallyf with a 500ms budget"
	req := require.New(t)
	req.Eventually(ready, 100*time.Millisecond, time.Millisecond) // want "require.Eventually with a 100ms budget"
}

func TestShortBudgetsInsideBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		_, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
		defer cancel()
		_, cancel = context.WithDeadline(ctx, time.Now().Add(50*time.Millisecond))
		defer cancel()
		<-time.After(75 * time.Millisecond)
		time.NewTimer(20 * time.Millisecond).Stop()
		time.AfterFunc(shortWait, func() {}).Stop()
		require.Eventually(t, ready, 300*time.Millisecond, time.Millisecond)
		assert.Never(t, ready, shortWait, time.Millisecond)
		require.New(t).Eventually(ready, 100*time.Millisecond, time.Millisecond)
	})
}

func bubbleBody(t *testing.T) {
	ctx := context.Background()
	_, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	<-time.After(75 * time.Millisecond)
	time.NewTimer(20 * time.Millisecond).Stop()
	require.Eventually(t, ready, 300*time.Millisecond, time.Millisecond)
}

func TestNamedCallbackBubble(t *testing.T) {
	synctest.Test(t, bubbleBody)
}

func TestBudgetsThatPass(t *testing.T) {
	ctx := context.Background()
	wait := 50 * time.Millisecond
	_, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, cancel = context.WithTimeout(ctx, 0)
	defer cancel()
	_, cancel = context.WithTimeout(ctx, -time.Millisecond)
	defer cancel()
	_, cancel = context.WithTimeout(ctx, wait)
	defer cancel()
	_, cancel = context.WithDeadline(ctx, time.Now())
	defer cancel()
	<-time.After(0)
	time.NewTimer(2 * time.Second).Stop()
	require.Eventually(t, ready, 5*time.Second, 10*time.Millisecond)
	assert.Never(t, ready, wait, time.Millisecond)
}
