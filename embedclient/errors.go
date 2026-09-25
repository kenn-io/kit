package embedclient

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidVector reports that the provider returned a vector that failed
// validation: wrong width, a null or non-finite component, or a zero norm.
// A *VectorError carries the input it belongs to.
var ErrInvalidVector = errors.New("embed vector is invalid")

// ErrResponseTooLarge reports a response body above Transport.MaxResponseBytes.
var ErrResponseTooLarge = errors.New("embed response exceeds the configured cap")

// VectorError is an invalid vector for one input. Index is the position in
// the inputs passed to Embed, EmbedTexts, or the EncodeFunc call. It matches
// ErrInvalidVector with errors.Is.
type VectorError struct {
	Index int
	Err   error
}

func (e *VectorError) Error() string {
	return fmt.Sprintf("embed vector %d: %v", e.Index, e.Err)
}

func (e *VectorError) Unwrap() []error { return []error{ErrInvalidVector, e.Err} }

// TransportError is a request that did not produce an HTTP response, such as
// a DNS, connection, or TLS failure. Its message omits the cause, because
// transport errors can name internal hosts. Unwrap returns the cause, so
// errors.As and errors.Is still reach it. Cancellation and deadline errors
// are returned wrapped in TransportError too.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "embed request failed" }

func (e *TransportError) Unwrap() error { return e.Err }

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

// Retryable reports a response that may succeed later: 408, 429, or a 5xx.
// Check RetryAfter for the delay the provider asked for.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
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
	if errors.Is(err, ErrOrigin) {
		return ErrOrigin
	}
	return &TransportError{Err: err}
}

// remapVectorIndex rewrites the index of a VectorError in err with index.
// Each request numbers its own inputs; callers see their own positions.
func remapVectorIndex(err error, index func(int) int) error {
	var vectorErr *VectorError
	if errors.As(err, &vectorErr) {
		vectorErr.Index = index(vectorErr.Index)
	}
	return err
}
