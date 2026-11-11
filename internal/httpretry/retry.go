// Package httpretry provides shared HTTP retry classification.
package httpretry

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retryable reports whether the status can succeed on a later request.
func Retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// RetryAfter parses the server's requested delay. A positive
// limit caps the delay; zero leaves the parsed duration uncapped.
func RetryAfter(header string, limit time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	seconds, err := strconv.Atoi(header)
	if err == nil {
		if seconds <= 0 {
			return 0
		}
		if limit > 0 && int64(seconds) > int64(limit/time.Second) {
			return limit
		}
		return time.Duration(seconds) * time.Second
	}
	if errors.Is(err, strconv.ErrRange) {
		if header[0] != '-' {
			return limit
		}
		return 0
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	delay := max(0, time.Until(when))
	if limit > 0 {
		delay = min(delay, limit)
	}
	return delay
}
