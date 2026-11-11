// Package httpretry provides shared HTTP retry classification.
package httpretry

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retryable reports whether the status can succeed on a later request.
func Retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 599
}

// RetryAfter parses the server's requested delay relative to now.
func RetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(header, 10, 64)
	if err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > math.MaxInt64/int64(time.Second) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	if errors.Is(err, strconv.ErrRange) && header[0] != '-' {
		return time.Duration(math.MaxInt64)
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	return max(0, when.Sub(now))
}
