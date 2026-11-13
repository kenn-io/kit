package embedclient_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func retryClient(t *testing.T, retry embedclient.Retry, handler http.HandlerFunc) *embedclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		Retry:      retry,
	})
	require.NoError(t, err)
	return client
}

func TestRetryRecoversFromTransientFailures(t *testing.T) {
	var calls atomic.Int32
	client := retryClient(t, embedclient.Retry{MaxAttempts: 6, InitialBackoff: time.Millisecond, MaxRetryAfter: time.Millisecond},
		func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) < 6 {
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
		})
	vectors, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.Equal(t, int32(6), calls.Load(), "a capped Retry-After still retries")
}

func TestRetryStopsOnAnInputRejection(t *testing.T) {
	var calls atomic.Int32
	client := retryClient(t, embedclient.Retry{MaxAttempts: 5, InitialBackoff: time.Millisecond},
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"input too long","code":"context_length_exceeded"}}`)
		})
	_, err := client.Embed(t.Context(), oneText())
	var apiErr *embedclient.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.True(t, apiErr.InputRejected())
	assert.Equal(t, int32(1), calls.Load(), "a rejected input is never retried")
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	client := retryClient(t, embedclient.Retry{MaxAttempts: 3, InitialBackoff: time.Millisecond},
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		})
	_, err := client.Embed(t.Context(), oneText())
	var apiErr *embedclient.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
	assert.Equal(t, int32(3), calls.Load())
}

func TestRetryWaitEndsWithTheContext(t *testing.T) {
	client := retryClient(t, embedclient.Retry{MaxAttempts: 5, InitialBackoff: time.Hour},
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the hour-long backoff ends only when the context does
	defer cancel()
	_, err := client.Embed(ctx, oneText())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var apiErr *embedclient.APIError
	require.ErrorAs(t, err, &apiErr, "the last provider error is kept")
}

func TestZeroRetryDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	client := retryClient(t, embedclient.Retry{}, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := client.Embed(t.Context(), oneText())
	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load())
}

func TestRetryRateLimitsOutlastMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	client := retryClient(t, embedclient.Retry{MaxAttempts: 2, InitialBackoff: time.Millisecond, RetryRateLimits: true},
		func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) < 6 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
		})
	_, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	assert.Equal(t, int32(6), calls.Load(), "429s do not spend MaxAttempts")
}

func TestRetryRateLimitsKeepsTheBudgetForOtherFailures(t *testing.T) {
	var calls atomic.Int32
	client := retryClient(t, embedclient.Retry{MaxAttempts: 3, InitialBackoff: time.Millisecond, RetryRateLimits: true},
		func(w http.ResponseWriter, _ *http.Request) {
			// 429, 503, 429, 503, 429, 503: three 503s spend the budget.
			if calls.Add(1)%2 == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		})
	_, err := client.Embed(t.Context(), oneText())
	var apiErr *embedclient.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	assert.Equal(t, int32(6), calls.Load())
}

func TestRetryRateLimitsAloneRetriesOnlyRateLimits(t *testing.T) {
	var calls atomic.Int32
	client := retryClient(t, embedclient.Retry{InitialBackoff: time.Millisecond, RetryRateLimits: true},
		func(w http.ResponseWriter, _ *http.Request) {
			switch calls.Add(1) {
			case 1, 2:
				w.WriteHeader(http.StatusTooManyRequests)
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
	_, err := client.Embed(t.Context(), oneText())
	require.Error(t, err)
	assert.Equal(t, int32(3), calls.Load(), "a 503 gets no retry without MaxAttempts")
}

func TestRetryRateLimitsRetriesEachSplitRequestAlone(t *testing.T) {
	var inputs atomic.Int32
	var rateLimited, unavailable atomic.Bool
	client := timeoutClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, jsonv2.UnmarshalRead(r.Body, &body)) {
			return
		}
		inputs.Add(int32(len(body.Input)))
		if len(body.Input) > 2 {
			time.Sleep(120 * time.Millisecond) //nolint:kennlint // sleeptest: the handler exceeds the client timeout
			return
		}
		if len(body.Input) == 2 && body.Input[0] == "item2" && rateLimited.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if len(body.Input) == 2 && body.Input[0] == "item0" && unavailable.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		data := make([]map[string]any, len(body.Input))
		for i := range body.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 1}}
		}
		writeJSON(t, w, map[string]any{"data": data})
	}, embedclient.Retry{MaxAttempts: 2, InitialBackoff: time.Millisecond, RetryRateLimits: true})
	_, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"item0", "item1", "item2", "item3"})
	require.NoError(t, err)
	assert.Equal(t, int32(4+2+2+2+2), inputs.Load(),
		"a 429 or 503 inside a split resends that half alone and the parent timeout spends no budget")
}
