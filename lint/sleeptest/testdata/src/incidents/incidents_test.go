package incidents

import (
	"testing"
	"time"
)

func TestRemovedSleepForms(t *testing.T) {
	expiresAt := time.Now()
	time.Sleep(max(time.Until(expiresAt), 0) + 50*time.Millisecond) //nolint:kennlint // consent expiry uses the real clock // want "time.Sleep in a test outside a synctest bubble"
	const ordinaryReadTimeout = 20 * time.Millisecond
	time.Sleep(2 * ordinaryReadTimeout) //nolint:kennlint // outlasts the real socket read deadline // want "time.Sleep in a test outside a synctest bubble"
}
