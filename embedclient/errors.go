package embedclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is a non-2xx embedding response. The provider body is discarded.
type APIError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("embed endpoint returned %d", e.StatusCode)
}

// Definitive reports that the same request will fail until the caller changes
// the input or the credentials.
func (e *APIError) Definitive() bool {
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	if delay := time.Until(when); delay > 0 {
		return delay
	}
	return 0
}

func classifyTransport(err error) error {
	if errors.Is(err, ErrOrigin) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("embed request failed: %w", context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("embed request failed: %w", context.DeadlineExceeded)
		}
		return ErrOrigin
	}
	return errors.New("embed request failed")
}
