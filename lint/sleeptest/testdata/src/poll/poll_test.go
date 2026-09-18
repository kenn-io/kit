package poll

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPollsOutsideBubble(t *testing.T) {
	ready := func() bool { return true }
	require.Eventually(t, ready, time.Second, time.Millisecond)                           // want "require.Eventually in a test outside a synctest bubble"
	assert.Eventually(t, ready, time.Second, time.Millisecond)                            // want "assert.Eventually in a test outside a synctest bubble"
	require.EventuallyWithT(t, func(*require.CollectT) {}, time.Second, time.Millisecond) // want "require.EventuallyWithT in a test outside a synctest bubble"
	require.Never(t, ready, time.Second, time.Millisecond)                                // want "require.Never in a test outside a synctest bubble"
	req := require.New(t)
	req.Eventually(ready, time.Second, time.Millisecond) // want "require.Eventually in a test outside a synctest bubble"
	assert.Equal(t, 1, 1)
}

func TestPollsInsideBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require.Eventually(t, func() bool { return true }, time.Second, time.Millisecond)
	})
}
