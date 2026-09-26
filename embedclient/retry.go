package embedclient

import (
	"context"
	"errors"
	"time"
)

// Retry configures retries of failures that may succeed later. The zero value
// does not retry. Only a retryable APIError (408, 429, or 5xx) and a
// TransportError are retried; an input or credential rejection never is. Each
// retry waits for the backoff, or for the provider's Retry-After when that is
// longer, and stops when the call's context ends.
type Retry struct {
	// MaxAttempts is the total number of tries, including the first. Values
	// of one or less do not retry.
	MaxAttempts int
	// InitialBackoff is the wait before the first retry. Each later retry
	// doubles it. Zero uses 500ms.
	InitialBackoff time.Duration
	// MaxBackoff caps the doubled backoff. Zero uses 30s.
	MaxBackoff time.Duration
	// MaxRetryAfter caps how long a provider's Retry-After can make a retry
	// wait. Zero uses 60s.
	MaxRetryAfter time.Duration
}

func (r Retry) withDefaults() Retry {
	if r.InitialBackoff <= 0 {
		r.InitialBackoff = 500 * time.Millisecond
	}
	if r.MaxBackoff <= 0 {
		r.MaxBackoff = 30 * time.Second
	}
	if r.MaxRetryAfter <= 0 {
		r.MaxRetryAfter = 60 * time.Second
	}
	return r
}

// do runs call until it succeeds, fails with an error that a retry cannot
// fix, runs out of attempts, or ctx ends.
func (r Retry) do(ctx context.Context, call func() ([][]float32, error)) ([][]float32, error) {
	r = r.withDefaults()
	backoff := r.InitialBackoff
	for attempt := 1; ; attempt++ {
		vectors, err := call()
		if err == nil || attempt >= r.MaxAttempts || ctx.Err() != nil {
			return vectors, err
		}
		wait, ok := r.delay(err, backoff)
		if !ok {
			return nil, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		backoff = min(backoff*2, r.MaxBackoff)
	}
}

// delay reports whether err is worth retrying and how long to wait first.
func (r Retry) delay(err error, backoff time.Duration) (time.Duration, bool) {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		if !apiErr.Retryable() {
			return 0, false
		}
		return max(backoff, min(apiErr.RetryAfter, r.MaxRetryAfter)), true
	}
	if _, ok := errors.AsType[*TransportError](err); ok {
		return backoff, true
	}
	return 0, false
}
