package posthog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
)

func TestDailyCaptureValidatesBeforeClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "days.json")
	r, client, _ := newInstallAgeTestReporter(t, postHogTestStart,
		WithAllowedEvent("screen_viewed", AllowProperty("screen", AllowStringValues("queue"))),
		WithDailyEvent("screen_viewed", "screen", NewDailyClaims(path)))
	h := NewCaptureHandler(r)
	body := `{"event":"screen_viewed","properties":{"screen":"queue"}}`
	for _, tc := range []struct {
		ctype, body string
		code        int
	}{
		{"text/plain", body, 415},
		{"", body, 415},
		{"application/json", body + " {}", 400},
		{"application/json", `{"event":"screen_viewed","properties":{"screen":"unknown"}}`, 400},
		{"application/json", `{"event":"screen_viewed"}`, 400},
	} {
		assert.Equal(t, tc.code, postCaptureAs(t, h, http.MethodPost, tc.ctype, tc.body).Code)
		assert.Empty(t, client.messages)
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	assert.Equal(t, "queued", decodeStatus(t, postCapture(t, h, http.MethodPost, body)))
	assert.Equal(t, "skipped", decodeStatus(t, postCapture(t, h, http.MethodPost, body)))
	require.Len(t, client.messages, 1)
	for _, disable := range []func(*Reporter){func(r *Reporter) { require.NoError(t, r.Close()) }, func(*Reporter) { DisableProcess() }} {
		path := filepath.Join(t.TempDir(), "days.json")
		reporter, _, _ := newInstallAgeTestReporter(t, postHogTestStart,
			WithAllowedEvent("screen_viewed", RequireProperty("screen", AllowStringValues("queue"))),
			WithDailyEvent("screen_viewed", "screen", NewDailyClaims(path)))
		disable(reporter)
		assert.Equal(t, "disabled", decodeStatus(t, postCapture(t, NewCaptureHandler(reporter), http.MethodPost, body)))
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
		enablePostHogTelemetryForTest()
	}
}

func TestDailyClaimsRecoveryAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "days.json")
	claims := NewDailyClaims(path)
	clock := &fakePostHogClock{now: postHogTestStart}
	sends := 0
	send := func(time.Time) (Status, error) { sends++; return StatusQueued, nil }
	report := func(c *DailyClaims, id, event, key string) (Status, error) {
		return c.report(t.Context(), id, event, key, clock.Now, send)
	}
	_, err := claims.report(t.Context(), "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) { return "", errors.New("queue rejected") })
	require.Error(t, err)
	status, err := report(claims, "install", "screen", "queue")
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, status)
	status, err = report(NewDailyClaims(path), "install", "screen", "queue")
	require.NoError(t, err)
	assert.Equal(t, StatusSkipped, status)
	for _, key := range [][3]string{{"other-install", "screen", "queue"}, {"install", "other-event", "queue"}, {"install", "screen", "detail"}} {
		status, err = report(claims, key[0], key[1], key[2])
		require.NoError(t, err)
		assert.Equal(t, StatusQueued, status)
	}
	clock.Advance(-24 * time.Hour)
	status, err = report(claims, "install", "screen", "queue")
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, status, "future date must allow corrected clock")
	assert.Equal(t, 5, sends)
	clock.Advance(24 * time.Hour)
	status, err = report(NewDailyClaims(path), "install", "screen", "queue")
	require.NoError(t, err)
	assert.Equal(t, StatusSkipped, status, "returning to an accepted date must not count again")
	clock.Advance(-24 * time.Hour)
	status, err = report(NewDailyClaims(path), "install", "screen", "queue")
	require.NoError(t, err)
	assert.Equal(t, StatusSkipped, status)
	assert.Equal(t, 5, sends)
}

func TestDailyClaimsSerializeRejectedCapture(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(strconv.FormatBool(shared), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "days.json")
			claims := NewDailyClaims(path)
			nextClaims := func() *DailyClaims {
				if shared {
					return claims
				}
				return NewDailyClaims(path)
			}
			started, release := make(chan struct{}), make(chan struct{})
			first := make(chan error, 1)
			go func() {
				_, err := nextClaims().report(t.Context(), "id", "screen", "queue", time.Now, func(time.Time) (Status, error) { close(started); <-release; return "", errors.New("rejected") })
				first <- err
			}()
			<-started
			lock := flock.New(path + ".lock")
			locked, err := lock.TryLock()
			require.NoError(t, err)
			require.False(t, locked, "claim lock must remain held through capture")
			second := make(chan Status, 1)
			failures := make(chan error, 1)
			go func() {
				status, err := nextClaims().report(t.Context(), "id", "screen", "queue", time.Now, func(time.Time) (Status, error) { return StatusQueued, nil })
				second <- status
				failures <- err
			}()
			close(release)
			require.Error(t, <-first)
			require.NoError(t, <-failures)
			assert.Equal(t, StatusQueued, <-second)
		})
	}
}

type dailyLockWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *dailyLockWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestDailyClaimsClockAfterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "days.json")
	claims := NewDailyClaims(path)
	var timestamp atomic.Int64
	timestamp.Store(postHogTestStart.Unix())
	clock := func() time.Time { return time.Unix(timestamp.Load(), 0) }
	send := func(time.Time) (Status, error) { return StatusQueued, nil }
	_, err := claims.report(t.Context(), "id", "screen", "queue", clock, send)
	require.NoError(t, err)
	lock := flock.New(path + ".lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
	ctx := t.Context()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		t.Cleanup(cancel)
	}
	waitContext := &dailyLockWaitContext{Context: ctx, waiting: make(chan struct{})}
	result := make(chan Status, 1)
	failures := make(chan error, 1)
	go func() {
		status, err := claims.report(waitContext, "id", "screen", "queue", clock, send)
		result <- status
		failures <- err
	}()
	select {
	case <-waitContext.waiting:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	timestamp.Add(int64((24 * time.Hour) / time.Second))
	require.NoError(t, lock.Unlock())
	require.NoError(t, <-failures)
	assert.Equal(t, StatusQueued, <-result)
}

func TestDailyClaimsInvalidStateAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "days.json")
	clock := &fakePostHogClock{now: postHogTestStart}
	for _, body := range []string{`{`, `null`, `{}`, `{"version":"damaged","days":{}}`, `{"version":0}`, `{"version":1.0}`, `{"version":1,"days":{"key":["2026-10-08","tomorrow"]}}`, `{"version":1,"days":null}`} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		sends := 0
		status, err := NewDailyClaims(path).report(t.Context(), "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
		require.NoError(t, err, body)
		assert.Equal(t, StatusQueued, status, body)
		assert.Equal(t, 1, sends, body)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var state dailyState
		require.NoError(t, json.Unmarshal(data, &state))
		assert.Equal(t, json.Number("1"), state.Version)
		assert.Len(t, state.Days, 1)
		status, err = NewDailyClaims(path).report(t.Context(), "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
		require.NoError(t, err)
		assert.Equal(t, StatusSkipped, status)
		assert.Equal(t, 1, sends)
	}
	for _, body := range []string{`{"version":2}`, `{"version":2,"days":{"key":["tomorrow"]}}`, `{"version":9223372036854775808}`, `{"version":2} trailing bytes`} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		sends := 0
		_, err := NewDailyClaims(path).report(t.Context(), "id", "screen", "queue", time.Now, func(time.Time) (Status, error) {
			sends++
			return StatusQueued, nil
		})
		require.ErrorContains(t, err, "unsupported daily telemetry version")
		assert.Zero(t, sends, body)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, body, string(data))
	}
	lock := flock.New(path + ".lock")
	require.NoError(t, lock.Lock())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewDailyClaims(path).report(ctx, "id", "screen", "queue", time.Now, func(time.Time) (Status, error) { return StatusQueued, nil })
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, lock.Unlock())
}

func TestDailyClaimsCleanupPreservesNewerAcceptedDay(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(strconv.FormatBool(newer), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "days.json")
			claims := NewDailyClaims(path)
			clock := &fakePostHogClock{now: postHogTestStart}
			writes := 0
			claims.write = func(p string, b []byte, o ...atomicfile.Option) error {
				writes++
				if writes == 2 {
					return errors.New("rollback failed")
				}
				return atomicfile.WriteFile(p, b, o...)
			}
			_, err := claims.report(t.Context(), "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { return "", errors.New("rejected") })
			require.Error(t, err)
			if newer {
				clock.Advance(24 * time.Hour)
			}
			send := func(time.Time) (Status, error) { return StatusQueued, nil }
			if !newer {
				status, err := claims.report(t.Context(), "id", "screen", "queue", clock.Now, send)
				require.NoError(t, err)
				assert.Equal(t, StatusQueued, status)
				assert.Nil(t, claims.pending)
				return
			}
			status, err := NewDailyClaims(path).report(t.Context(), "id", "screen", "queue", clock.Now, send)
			require.NoError(t, err)
			assert.Equal(t, StatusQueued, status)
			status, err = claims.report(t.Context(), "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { assert.Fail(t, "newer day was erased"); return StatusQueued, nil })
			require.NoError(t, err)
			assert.Equal(t, StatusSkipped, status)
			assert.Nil(t, claims.pending)
		})
	}
}

func TestDailyReportKeepsReservationTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "days.json")
	claims := NewDailyClaims(path)
	start := time.Date(2026, 10, 8, 23, 59, 59, 0, time.UTC)
	r, client, clock := newInstallAgeTestReporter(t, start,
		WithAllowedEvent("screen_viewed", RequireProperty("screen", AllowStringValues("queue"))),
		WithDailyEvent("screen_viewed", "screen", claims))
	clock.now = start
	claims.write = func(p string, data []byte, options ...atomicfile.Option) error {
		clock.Advance(2 * time.Second)
		return atomicfile.WriteFile(p, data, options...)
	}
	status, err := r.Report(t.Context(), "screen_viewed", map[string]any{"screen": "queue"})
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, status)
	require.Len(t, client.messages, 1)
	assert.Equal(t, start, client.messages[0].Timestamp)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state dailyState
	require.NoError(t, json.Unmarshal(data, &state))
	for _, days := range state.Days {
		assert.Equal(t, []string{start.Format(time.DateOnly)}, days)
	}
}

func TestReporterConfigurationValidation(t *testing.T) {
	claims := NewDailyClaims(filepath.Join(t.TempDir(), "days.json"))
	valid := RequireProperty("screen", AllowStringValues("queue"))
	nilFilter := RequireProperty("screen", nil)
	blank := RequireProperty(" \t", AllowStringValues("queue"))
	allowed := WithAllowedEvent("screen_viewed", valid)
	for _, disabled := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			options []Option
			invalid bool
		}{
			{"unknown event", []Option{allowed, WithDailyEvent("unknown", "screen", claims)}, true},
			{"missing property", []Option{allowed, WithDailyEvent("screen_viewed", "other", claims)}, true},
			{"nil claims", []Option{allowed, WithDailyEvent("screen_viewed", "screen", nil)}, true},
			{"blank path", []Option{allowed, WithDailyEvent("screen_viewed", "screen", NewDailyClaims(" "))}, true},
			{"allowed first", []Option{allowed, WithDailyEvent("screen_viewed", "screen", claims)}, false},
			{"daily first", []Option{WithDailyEvent("screen_viewed", "screen", claims), allowed}, false},
			{"reporter source", []Option{WithAllowedEvent("screen_viewed", AllowProperty("source", AllowStringValues("frontend", "tui"))), WithDailyEvent("screen_viewed", "source", claims)}, true},
			{"reporter age", []Option{WithAllowedEvent("screen_viewed", AllowProperty(postHogInstallAgeProperty, AllowNumber)), WithDailyEvent("screen_viewed", postHogInstallAgeProperty, claims)}, true},
			{"nil filter", []Option{WithAllowedEvent("screen_viewed", nilFilter)}, true},
			{"blank name", []Option{WithAllowedEvent("screen_viewed", blank)}, true},
			{"valid then nil", []Option{allowed, WithAllowedEvent("screen_viewed", nilFilter)}, true},
			{"nil then valid", []Option{WithAllowedEvent("screen_viewed", nilFilter), allowed}, true},
			{"valid then blank", []Option{allowed, WithAllowedEvent("screen_viewed", blank)}, true},
			{"blank then valid", []Option{WithAllowedEvent("screen_viewed", blank), allowed}, true},
			{"invalid optional", []Option{WithAllowedEvent("screen_viewed", AllowProperty("screen", nil), AllowProperty(" ", AllowStringValues("queue")))}, false},
		} {
			t.Run(tc.name+strconv.FormatBool(disabled), func(t *testing.T) {
				t.Setenv(GenericEnabledEnv, "1")
				t.Setenv("KATA_TELEMETRY_ENABLED", "1")
				if disabled {
					t.Setenv("KATA_TELEMETRY_ENABLED", "0")
				}
				calls := 0
				r, err := newPostHogReporter(Options{APIKey: "caller-owned-key", Application: "kata", EnvPrefix: "KATA", DistinctID: "id"}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
					calls++
					return &recordingPostHogClient{}, nil
				}, tc.options...)
				if tc.invalid {
					require.Error(t, err)
					assert.Zero(t, calls)
					return
				}
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, r.Close()) })
				assert.Equal(t, !disabled, r.Enabled())
			})
		}
	}
}

func TestDailyClaimsPublicationRecovery(t *testing.T) {
	for _, publishedErr := range []error{atomicfile.ErrPublished, atomicfile.ErrNotDurable} {
		releaseErr := errors.New("release failed before publication")
		for _, tc := range []struct {
			name        string
			errors      []error
			wantErrors  []error
			omitError   error
			retryError  error
			retry       bool
			freshStatus Status
		}{
			{name: "published reservation", errors: []error{publishedErr}, wantErrors: []error{publishedErr}, freshStatus: StatusQueued},
			{name: "unpublished reservation", errors: []error{releaseErr}, wantErrors: []error{releaseErr}, freshStatus: StatusQueued},
			{name: "published rollback", errors: []error{nil, publishedErr}, wantErrors: []error{publishedErr}, freshStatus: StatusQueued},
			{name: "published deferred cleanup", errors: []error{nil, releaseErr, publishedErr}, wantErrors: []error{releaseErr}, retryError: publishedErr, freshStatus: StatusQueued},
			{name: "unpublished reservation release", errors: []error{publishedErr, releaseErr}, wantErrors: []error{publishedErr, releaseErr}, retry: true, freshStatus: StatusSkipped},
			{name: "published reservation release", errors: []error{publishedErr, errors.Join(releaseErr, publishedErr)}, wantErrors: []error{publishedErr}, omitError: releaseErr, freshStatus: StatusQueued},
		} {
			t.Run(tc.name+publishedErr.Error(), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "days.json")
				claims := NewDailyClaims(path)
				clock := &fakePostHogClock{now: postHogTestStart}
				writes, accepted := 0, 0
				claims.write = func(p string, data []byte, options ...atomicfile.Option) error {
					writes++
					var failure error
					if writes <= len(tc.errors) {
						failure = tc.errors[writes-1]
					}
					if failure != nil && !errors.Is(failure, atomicfile.ErrPublished) {
						return failure
					}
					if err := atomicfile.WriteFile(p, data, options...); err != nil {
						return err
					}
					return failure
				}
				send := func(time.Time) (Status, error) { accepted++; return StatusQueued, nil }
				report := func(c *DailyClaims, callback func(time.Time) (Status, error)) (Status, error) {
					return c.report(t.Context(), "id", "screen", "queue", clock.Now, callback)
				}
				_, err := report(claims, func(time.Time) (Status, error) {
					if tc.errors[0] != nil {
						assert.Fail(t, "capture after failed reservation")
					}
					return "", errors.New("rejected")
				})
				require.Error(t, err)
				for _, failure := range tc.wantErrors {
					require.ErrorIs(t, err, failure)
				}
				if tc.omitError != nil {
					assert.NotErrorIs(t, err, tc.omitError)
				}
				assert.Zero(t, accepted)
				if len(tc.errors) > 1 {
					assert.Equal(t, 2, writes)
				}
				if tc.retryError != nil {
					require.NotNil(t, claims.pending)
					_, err = report(claims, send)
					require.ErrorIs(t, err, tc.retryError)
					assert.Zero(t, accepted)
				}
				if tc.retry {
					require.NotNil(t, claims.pending)
					status, err := report(claims, send)
					require.NoError(t, err)
					assert.Equal(t, StatusQueued, status)
				}
				assert.Nil(t, claims.pending)
				status, err := report(NewDailyClaims(path), send)
				require.NoError(t, err)
				assert.Equal(t, tc.freshStatus, status)
				status, err = report(claims, send)
				require.NoError(t, err)
				assert.Equal(t, StatusSkipped, status)
				assert.Equal(t, 1, accepted)
			})
		}
	}
}

type failingDailyJSON struct{ err error }

func (value failingDailyJSON) MarshalJSON() ([]byte, error) { return nil, value.err }

func TestDailyReportSerializationFailureThenSameDayRetry(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	var mu sync.Mutex
	var events []postHogCaptureRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload struct {
			Batch []postHogCaptureRequest `json:"batch"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			assert.NoError(t, err)
			http.Error(w, "invalid batch", http.StatusBadRequest)
			return
		}
		mu.Lock()
		events = append(events, payload.Batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "days.json")
	reporter, err := NewReporter(Options{APIKey: "caller-owned-key", Application: "kata", EnvPrefix: "KATA", DistinctID: "id", Endpoint: server.URL},
		WithAllowedEvent("screen_viewed", RequireProperty("screen", AllowStringValues("queue")), AllowProperty("value", allowAnyTelemetryValue)),
		WithDailyEvent("screen_viewed", "screen", NewDailyClaims(path)),
		postHogOptionFunc(func(config *postHogReporterConfig) { config.now = func() time.Time { return postHogTestStart } }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	failure := errors.New("cannot encode filtered value")
	status, err := reporter.Report(t.Context(), "screen_viewed", map[string]any{"screen": "queue", "value": failingDailyJSON{failure}})
	require.ErrorIs(t, err, failure)
	assert.Empty(t, status)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	status, err = reporter.Report(t.Context(), "screen_viewed", map[string]any{"screen": "queue", "value": "corrected"})
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, status)
	status, err = reporter.Report(t.Context(), "screen_viewed", map[string]any{"screen": "queue", "value": "corrected"})
	require.NoError(t, err)
	assert.Equal(t, StatusSkipped, status)
	require.NoError(t, reporter.Close())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 1)
	assert.Equal(t, "screen_viewed", events[0].Event)
	assert.Equal(t, "corrected", events[0].Properties["value"])
}
