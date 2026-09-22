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

// InputRejected reports that this input was refused. The same input will
// fail again. The provider body is not included.
func (e *APIError) InputRejected() bool {
	return e.StatusCode == http.StatusBadRequest
}

// CredentialsRejected reports that the key or the permission was refused.
// That is not a reason to skip one document. The provider body is not included.
func (e *APIError) CredentialsRejected() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// Definitive reports that this input was rejected. A credential failure is
// not definitive: the caller should stop, not skip the document.
func (e *APIError) Definitive() bool {
	return e.InputRejected()
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
