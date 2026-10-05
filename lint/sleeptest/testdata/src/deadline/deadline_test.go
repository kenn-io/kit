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
	_, cancel := context.WithTimeout(ctx, 5*time.Millisecond) // want "context.WithTimeout with budget 5ms in a test outside a synctest bubble races the wall clock"
	defer cancel()
	_, cancel = context.WithTimeoutCause(ctx, 10*time.Millisecond, errors.New("slow")) // want "context.WithTimeoutCause with budget 10ms"
	defer cancel()
	_, cancel = context.WithDeadline(ctx, time.Now().Add(50*time.Millisecond)) // want "context.WithDeadline with budget 50ms"
	defer cancel()
	_, cancel = context.WithDeadlineCause(ctx, time.Now().Add(shortWait), errors.New("slow")) // want "context.WithDeadlineCause with budget 50ms"
	defer cancel()
	<-time.After(75 * time.Millisecond)           // want "time.After with budget 75ms"
	timer := time.NewTimer(20 * time.Millisecond) // want "time.NewTimer with budget 20ms"
	defer timer.Stop()
	time.AfterFunc(shortWait, func() {}).Stop()                                    // want "time.AfterFunc with budget 50ms"
	require.Eventually(t, ready, 300*time.Millisecond, time.Millisecond)           // want "require.Eventually with budget 300ms"
	assert.Never(t, ready, shortWait, time.Millisecond)                            // want "assert.Never with budget 50ms"
	require.Eventuallyf(t, ready, 500*time.Millisecond, time.Millisecond, "ready") // want "require.Eventuallyf with budget 500ms"
	req := require.New(t)
	req.Eventually(ready, 100*time.Millisecond, time.Millisecond)                            // want "require.Eventually with budget 100ms"
	(*require.Assertions).Eventually(req, ready, 300*time.Millisecond, time.Second)          // want "require.Eventually with budget 300ms"
	(require.Eventually)(t, ready, 200*time.Millisecond, time.Second)                        // want "require.Eventually with budget 200ms"
	require.EventuallyWithT(t, func(*assert.CollectT) {}, 400*time.Millisecond, time.Second) // want "require.EventuallyWithT with budget 400ms"
	_, cancel = context.WithDeadline(ctx, (time.Now()).Add(60*time.Millisecond))             // want "context.WithDeadline with budget 60ms"
	defer cancel()
	_, cancel = context.WithDeadline(ctx, (time.Now().Add(70 * time.Millisecond))) // want "context.WithDeadline with budget 70ms"
	defer cancel()
	_, cancel = context.WithDeadline(ctx, (time.Now().Add)(90*time.Millisecond)) // want "context.WithDeadline with budget 90ms"
	defer cancel()
}

func TestZeroAndNegativePollingBudgets(t *testing.T) {
	require.Eventually(t, ready, 0, time.Millisecond)           // want "require.Eventually with budget 0s"
	assert.Never(t, ready, -time.Millisecond, time.Millisecond) // want "assert.Never with budget -1ms"
}

func TestParenthesizedBubble(t *testing.T) {
	synctest.Test(t, (func(t *testing.T) {
		<-time.After(75 * time.Millisecond)
	}))
}

var parenBubble = (func(t *testing.T) {
	<-time.After(75 * time.Millisecond)
})

func TestParenthesizedNamedBubble(t *testing.T) {
	synctest.Test(t, parenBubble)
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
