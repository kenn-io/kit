package embedclient

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v7"
)

// Retry configures retries of failures that may succeed later. The zero value
// does not retry. Only a retryable APIError (408, 429, or 5xx) and a
// TransportError are retried; an input or credential rejection never is. Each
// retry waits for the backoff, or for the provider's Retry-After when it sends
// one, and stops when the call's context ends.
type Retry struct {
	// MaxAttempts is the total number of tries, including the first. Values
	// of one or less do not retry.
	MaxAttempts int
	// InitialBackoff is the wait before the first retry. Later retries grow
	// it exponentially with jitter. Zero uses 500ms.
	InitialBackoff time.Duration
	// MaxBackoff caps the growing backoff before jitter, so one wait can
	// reach 1.5 times this value. Zero uses 30s.
	MaxBackoff time.Duration
	// MaxRetryAfter caps how long a provider's Retry-After can make a retry
	// wait. Zero uses 60s.
	MaxRetryAfter time.Duration
}

// retryCall runs call until it succeeds, fails with an error that a retry cannot
// fix, runs out of attempts, or ctx ends.
func retryCall[T any](ctx context.Context, r Retry, splitTimeout bool, call func() (T, error)) (T, error) {
	var zero T
	if r.MaxAttempts <= 1 {
		return call()
	}
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = cmp.Or(r.InitialBackoff, 500*time.Millisecond)
	policy.MaxInterval = cmp.Or(r.MaxBackoff, 30*time.Second)
	maxRetryAfter := cmp.Or(r.MaxRetryAfter, 60*time.Second)

	value, err := backoff.Retry(ctx, func() (T, error) {
		value, err := call()
		if err == nil {
			return value, nil
		}
		if apiErr, ok := errors.AsType[*APIError](err); ok {
			if !apiErr.Retryable() {
				return zero, backoff.Permanent(err)
			}
			if apiErr.RetryAfter > 0 {
				return zero, backoff.RetryAfter(min(apiErr.RetryAfter, maxRetryAfter), err)
			}
			return zero, err
		}
		if transport, ok := errors.AsType[*TransportError](err); ok {
			if splitTimeout && transport.Timeout() {
				return zero, backoff.Permanent(err)
			}
			return zero, err
		}
		return zero, backoff.Permanent(err)
	},
		backoff.WithBackOff(policy),
		backoff.WithMaxTries(uint(r.MaxAttempts)),
		backoff.WithMaxElapsedTime(0),
	)
	// Report the provider's error as the call would without retries, joined
	// with the context error when the context ended the retries.
	if retryErr := backoff.AsRetryError(err); retryErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, errors.Join(retryErr.LastErr, ctxErr)
		}
		return zero, retryErr.LastErr
	}
	return value, err
}
