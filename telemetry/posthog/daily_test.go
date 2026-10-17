package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

func TestDailyReportConcurrentCaptures(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		shared, rejected, cancelWaiting bool
	}{
		{"separate accepted", false, false, false},
		{"shared accepted", true, false, false},
		{"separate rejected", false, true, false},
		{"shared rejected", true, true, false},
		{"cancel while waiting", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "days.json")
			claims := NewDailyClaims(path)
			secondClaims := claims
			if !tc.shared {
				secondClaims = NewDailyClaims(path)
			}
			clock := func() time.Time { return postHogTestStart }
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var markers []string
			var markersMu sync.Mutex
			mark := func(marker string) {
				// Shared-instance markers expose broken serialization to the race detector.
				if !tc.shared {
					markersMu.Lock()
					defer markersMu.Unlock()
				}
				markers = append(markers, marker)
			}
			first := make(chan error, 1)
			rejection := errors.New("queue rejected")
			go func() {
				_, err := claims.report(t.Context(), nil, "id", "screen", "queue", clock, func(time.Time) (Status, error) {
					mark("first entered")
					pending := claims.pending
					assert.NotNil(t, pending)
					close(started)
					<-release
					assert.Same(t, pending, claims.pending, "waiting report must preserve the active reservation")
					mark("first returning")
					if tc.rejected {
						return "", rejection
					}
					return StatusQueued, nil
				})
				first <- err
			}()
			select {
			case <-started:
			case err := <-first:
				require.NoError(t, err)
				require.FailNow(t, "first report returned without entering capture")
			case <-time.After(5 * time.Second):
				require.FailNow(t, "first report did not enter capture")
			}
			lock := flock.New(path + ".lock")
			locked, err := lock.TryLock()
			require.NoError(t, err)
			if locked {
				require.NoError(t, lock.Unlock())
			}
			require.False(t, locked, "claim lock must remain held through capture")
			secondKey := "detail"
			if tc.rejected || tc.cancelWaiting {
				secondKey = "queue"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waitContext := &dailyWaitContext{Context: ctx, doneCalled: make(chan struct{})}
			type result struct {
				status Status
				err    error
			}
			second := make(chan result, 1)
			entered := make(chan struct{}, 1)
			go func() {
				status, err := secondClaims.report(waitContext, nil, "id", "screen", secondKey, clock, func(time.Time) (Status, error) {
					mark("second entered")
					entered <- struct{}{}
					return StatusQueued, nil
				})
				second <- result{status, err}
			}()
			select {
			case <-waitContext.doneCalled:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "second report did not start waiting")
			}
			select {
			case <-entered:
				assert.Fail(t, "second capture entered before the first returned")
			case got := <-second:
				assert.Fail(t, "second report returned before the first capture", "%+v", got)
				second <- got
			case <-time.After(time.Second):
			}
			if tc.cancelWaiting {
				cancel()
				select {
				case got := <-second:
					require.ErrorIs(t, got.err, context.Canceled)
					require.ErrorContains(t, got.err, "lock daily telemetry")
				case <-time.After(time.Second):
					require.FailNow(t, "canceled report remained blocked")
				}
				unblock()
				require.NoError(t, <-first)
				return
			}
			unblock()
			if tc.rejected {
				require.ErrorIs(t, <-first, rejection)
			} else {
				require.NoError(t, <-first)
			}
			got := <-second
			require.NoError(t, got.err)
			assert.Equal(t, StatusQueued, got.status)
			assert.Equal(t, []string{"first entered", "first returning", "second entered"}, markers)
			for _, key := range []string{"queue", secondKey} {
				status, err := claims.report(t.Context(), nil, "id", "screen", key, clock, func(time.Time) (Status, error) {
					assert.Fail(t, "accepted claim must skip capture")
					return StatusQueued, nil
				})
				require.NoError(t, err)
				assert.Equal(t, StatusSkipped, status)
			}
		})
	}
}

func TestDailyClaimsRecoveryAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "days.json")
	claims := NewDailyClaims(path)
	clock := &fakePostHogClock{now: postHogTestStart}
	sends := 0
	send := func(time.Time) (Status, error) { sends++; return StatusQueued, nil }
	report := func(c *DailyClaims, id, event, key string) (Status, error) {
		return c.report(t.Context(), nil, id, event, key, clock.Now, send)
	}
	_, err := claims.report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) { return "", errors.New("queue rejected") })
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

type dailyWaitContext struct {
	context.Context
	doneCalled    chan struct{}
	once          sync.Once
	skipDoneCalls int
}

func (c *dailyWaitContext) Done() <-chan struct{} {
	if c.skipDoneCalls > 0 {
		c.skipDoneCalls--
		return c.Context.Done()
	}
	c.once.Do(func() { close(c.doneCalled) })
	return c.Context.Done()
}

func TestDailyClaimsClockAfterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "days.json")
	claims := NewDailyClaims(path)
	var timestamp atomic.Int64
	timestamp.Store(postHogTestStart.Unix())
	clock := func() time.Time { return time.Unix(timestamp.Load(), 0) }
	send := func(time.Time) (Status, error) { return StatusQueued, nil }
	_, err := claims.report(t.Context(), nil, "id", "screen", "queue", clock, send)
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
	// semaphore.Acquire calls ctx.Done once on an uncontended acquire; skip it to signal the file lock wait.
	waitContext := &dailyWaitContext{Context: ctx, doneCalled: make(chan struct{}), skipDoneCalls: 1}
	result := make(chan Status, 1)
	failures := make(chan error, 1)
	go func() {
		status, err := claims.report(waitContext, nil, "id", "screen", "queue", clock, send)
		result <- status
		failures <- err
	}()
	select {
	case <-waitContext.doneCalled:
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
	for _, body := range []string{
		`{`, `null`, `{}`, `{"version":"damaged","days":{}}`, `{"version":0}`, `{"version":-1e99999999999}`,
		`{"version":"1","days":{"[\"id\",\"screen\",\"queue\"]":["` + clock.Now().UTC().Format(time.DateOnly) + `"]}}`,
		`{"version":1,"days":{"key":["2026-10-08","tomorrow"]}}`, `{"version":1,"days":null}`,
		`{"version":3,"version":1,"days":null}`,
		`{"version":2,"days":null}`, `{"version":2,"days":{"key":["2026-10-08"]}}`,
		`{"version":2,"days":{"key":[{"day":"tomorrow","pending":true}]}}`,
		`{"version":2,"days":{"key":[{"day":"2026-10-08","pending":"yes"}]}}`,
	} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		sends := 0
		status, err := NewDailyClaims(path).report(t.Context(), nil, "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
		require.NoError(t, err, body)
		assert.Equal(t, StatusQueued, status, body)
		assert.Equal(t, 1, sends, body)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var state dailyState
		require.NoError(t, json.Unmarshal(data, &state))
		assert.Equal(t, 2, state.Version)
		assert.Len(t, state.Days, 1)
		status, err = NewDailyClaims(path).report(t.Context(), nil, "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
		require.NoError(t, err)
		assert.Equal(t, StatusSkipped, status)
		assert.Equal(t, 1, sends)
	}
	for _, body := range []string{
		`{"version":3}`, `{"version":3,"days":`, `{"version":3,"days":{"key":["tomorrow"]}}`, `{"version":2.0}`, `{"version":3.0}`, `{"version":1.0}`, `{"version":1.0000000000000001}`, `{"version":1e99999999999}`, `{"version":3} trailing bytes`,
		`{"Version":3,"days":{}}`, `{"version":1,"version":3,"days":{}}`,
		`{"version":3,"version":null,"days":{}}`, `{"version":3,"Version":"damaged","days":{}}`,
		`{"version":3,"version":true,"days":{}}`,
	} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		sends := 0
		_, err := NewDailyClaims(path).report(t.Context(), nil, "id", "screen", "queue", time.Now, func(time.Time) (Status, error) {
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
	t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waitContext := &dailyWaitContext{Context: ctx, doneCalled: make(chan struct{}), skipDoneCalls: 1}
	result := make(chan error, 1)
	go func() {
		_, err := NewDailyClaims(path).report(waitContext, nil, "id", "screen", "queue", time.Now, func(time.Time) (Status, error) { return StatusQueued, nil })
		result <- err
	}()
	select {
	case <-waitContext.doneCalled:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "report did not wait for the file lock")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		require.FailNow(t, "canceled report remained blocked")
	}
}

func TestDailyClaimsCleanupPreservesNewerAcceptedDay(t *testing.T) {
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
	_, err := claims.report(t.Context(), nil, "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { return "", errors.New("rejected") })
	require.Error(t, err)
	clock.Advance(24 * time.Hour)
	send := func(time.Time) (Status, error) { return StatusQueued, nil }
	status, err := NewDailyClaims(path).report(t.Context(), nil, "id", "screen", "queue", clock.Now, send)
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, status)
	status, err = claims.report(t.Context(), nil, "id", "screen", "queue", clock.Now, func(time.Time) (Status, error) { assert.Fail(t, "newer day was erased"); return StatusQueued, nil })
	require.NoError(t, err)
	assert.Equal(t, StatusSkipped, status)
	assert.Nil(t, claims.pending)
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
		assert.Equal(t, []string{start.Format(time.DateOnly)}, acceptedDailyDates(t, days))
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
	publishedErr := atomicfile.ErrNotDurable
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
		t.Run(tc.name, func(t *testing.T) {
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
				return c.report(t.Context(), nil, "id", "screen", "queue", clock.Now, callback)
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
				require.NotErrorIs(t, err, tc.omitError)
			}
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

func seedDailyCapacity(t *testing.T) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "days.json")
	days := make([]string, 35)
	for i := range days {
		days[i] = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(time.DateOnly)
	}
	data, err := json.Marshal(dailyLegacyState{Version: 1, Days: map[string][]string{`["install","screen","queue"]`: days}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path, days
}

func TestDailyClaimsCompactsSkippedReports(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		t.Run(strconv.FormatBool(failWrite), func(t *testing.T) {
			enablePostHogTelemetryForTest()
			t.Cleanup(enablePostHogTelemetryForTest)
			t.Setenv(GenericEnabledEnv, "1")
			t.Setenv("TEST_TELEMETRY_ENABLED", "1")
			path := filepath.Join(t.TempDir(), "days.json")
			state := dailyLegacyState{Version: 1, Days: map[string][]string{"sparse": {"1970-01-01"}, "empty": {}}}
			for key := range 10 {
				claim, err := json.Marshal([]string{"install", "screen", strconv.Itoa(key)})
				require.NoError(t, err)
				for day := 365; day >= 0; day-- {
					state.Days[string(claim)] = append(state.Days[string(claim)], time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -day).Format(time.DateOnly))
				}
			}
			data, err := json.Marshal(state)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			claims := NewDailyClaims(path)
			if failWrite {
				claims.write = func(string, []byte, ...atomicfile.Option) error { return errors.New("storage unavailable") }
			}
			var logs bytes.Buffer
			client := &recordingPostHogClient{}
			reporter, err := newPostHogReporter(Options{
				APIKey: "test", Application: "test", EnvPrefix: "TEST", DistinctID: "install",
				Logger: slog.New(slog.NewTextHandler(&logs, nil)),
			}, func(string, phsdk.Config) (postHogEnqueueCloser, error) { return client, nil },
				WithAllowedEvent("screen", AllowProperty("screen", AllowStringValues("0"))),
				WithDailyEvent("screen", "screen", claims),
				postHogOptionFunc(func(c *postHogReporterConfig) {
					c.now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
				}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reporter.Close()) })
			status, err := reporter.Report(t.Context(), "screen", map[string]any{"screen": "0"})
			require.NoError(t, err)
			assert.Equal(t, StatusSkipped, status)
			assert.Empty(t, client.messages)
			if failWrite {
				assert.Contains(t, logs.String(), "level=WARN")
				assert.Contains(t, logs.String(), "storage unavailable")
				unchanged, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(data), string(unchanged))
				claims.write = nil
				status, err = reporter.Report(t.Context(), "screen", map[string]any{"screen": "0"})
				require.NoError(t, err)
				assert.Equal(t, StatusSkipped, status)
			}
			data, err = os.ReadFile(path)
			require.NoError(t, err)
			var saved dailyState
			require.NoError(t, json.Unmarshal(data, &saved))
			require.Len(t, saved.Days, 11)
			assert.NotContains(t, saved.Days, "empty")
			assert.Equal(t, []string{"1970-01-01"}, acceptedDailyDates(t, saved.Days["sparse"]))
			delete(saved.Days, "sparse")
			for _, days := range saved.Days {
				require.Len(t, days, 35)
				assert.Equal(t, "2026-09-13", days[0].Day)
				assert.Equal(t, "2026-10-17", days[34].Day)
			}
			claims.write = func(string, []byte, ...atomicfile.Option) error { return errors.New("unchanged skip must not save") }
			status, err = reporter.Report(t.Context(), "screen", map[string]any{"screen": "0"})
			require.NoError(t, err)
			assert.Equal(t, StatusSkipped, status)
			assert.NotContains(t, logs.String(), "unchanged skip must not save")
			status, err = NewDailyClaims(path).report(t.Context(), nil, "install", "screen", "0", func() time.Time { return time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC) }, func(time.Time) (Status, error) { assert.Fail(t, "accepted date was erased"); return StatusQueued, nil })
			require.NoError(t, err)
			assert.Equal(t, StatusSkipped, status)
			assert.Empty(t, client.messages)
		})
	}
}

func TestDailyClaimsWrongClockKeepsRecentAcceptedDate(t *testing.T) {
	path, days := seedDailyCapacity(t)
	clock := &fakePostHogClock{now: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)}
	sends := 0
	report := func(want Status) {
		status, err := NewDailyClaims(path).report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
		require.NoError(t, err)
		assert.Equal(t, want, status)
	}
	report(StatusQueued)
	clock.now = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	report(StatusSkipped)
	clock.now = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	report(StatusSkipped)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state dailyState
	require.NoError(t, json.Unmarshal(data, &state))
	assert.Equal(t, append(days[1:], "1970-01-01"), acceptedDailyDates(t, state.Days[`["install","screen","queue"]`]))
	assert.Equal(t, 1, sends)
}

func TestDailyClaimsEvictionRollback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		published   bool
		intervening int
	}{
		{name: "rejected enqueue"},
		{name: "published reservation", published: true},
		{name: "deferred rejection", intervening: 1},
		{name: "deferred cleanup after full replacement", intervening: 35},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, original := seedDailyCapacity(t)
			claims := NewDailyClaims(path)
			clock := &fakePostHogClock{now: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
			writes := 0
			claims.write = func(p string, b []byte, o ...atomicfile.Option) error {
				writes++
				if writes == 2 && tc.intervening > 0 {
					return errors.New("cleanup unavailable")
				}
				if err := atomicfile.WriteFile(p, b, o...); err != nil {
					return err
				}
				if writes == 1 && tc.published {
					return atomicfile.ErrNotDurable
				}
				return nil
			}
			_, err := claims.report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) {
				assert.False(t, tc.published, "a failed reservation must not enqueue")
				return "", errors.New("rejected")
			})
			require.Error(t, err)
			want := original
			if tc.intervening > 0 {
				var newer []string
				for range tc.intervening {
					clock.Advance(24 * time.Hour)
					status, err := NewDailyClaims(path).report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) { return StatusQueued, nil })
					require.NoError(t, err)
					assert.Equal(t, StatusQueued, status)
					newer = append(newer, clock.Now().Format(time.DateOnly))
				}
				status, err := claims.report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) {
					assert.Fail(t, "newer accepted date was erased")
					return StatusQueued, nil
				})
				require.NoError(t, err)
				assert.Equal(t, StatusSkipped, status)
				want = append([]string{}, original[1:]...)
				want = append(want, "2026-10-07")
				if tc.intervening == 35 {
					want = newer
				}
			}
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var state dailyState
			require.NoError(t, json.Unmarshal(data, &state))
			assert.Equal(t, want, acceptedDailyDates(t, state.Days[`["install","screen","queue"]`]))
			if tc.intervening == 35 {
				return
			}
			retainedDay := 1
			if tc.intervening > 0 {
				retainedDay = 2
			}
			clock.now = time.Date(2026, 9, retainedDay, 0, 0, 0, 0, time.UTC)
			status, err := claims.report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) {
				assert.Fail(t, "retained accepted date was lost")
				return StatusQueued, nil
			})
			require.NoError(t, err)
			assert.Equal(t, StatusSkipped, status)
		})
	}
}

func TestDailyClaimsAcceptedSettlementFailure(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(strconv.FormatBool(published), func(t *testing.T) {
			path, original := seedDailyCapacity(t)
			claims := NewDailyClaims(path)
			clock := &fakePostHogClock{now: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
			storageErr := errors.New("acceptance save failed")
			if published {
				storageErr = atomicfile.ErrNotDurable
			}
			writes := 0
			claims.write = func(p string, data []byte, options ...atomicfile.Option) error {
				writes++
				if writes == 2 && !published {
					return storageErr
				}
				if err := atomicfile.WriteFile(p, data, options...); err != nil {
					return err
				}
				if writes == 2 {
					return storageErr
				}
				return nil
			}
			sends := 0
			status, err := claims.report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
			require.ErrorIs(t, err, storageErr)
			assert.Empty(t, status, "a post-enqueue storage failure returns an error result")
			assert.Equal(t, 1, sends)
			clock.Advance(24 * time.Hour)
			status, err = NewDailyClaims(path).report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) { return StatusQueued, nil })
			require.NoError(t, err)
			assert.Equal(t, StatusQueued, status)
			clock.Advance(-24 * time.Hour)
			status, err = claims.report(t.Context(), nil, "install", "screen", "queue", clock.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
			require.NoError(t, err)
			assert.Equal(t, StatusSkipped, status)
			assert.Equal(t, 1, sends)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var state dailyState
			require.NoError(t, json.Unmarshal(data, &state))
			assert.Equal(t, append(original[2:], "2026-10-06", "2026-10-07"), acceptedDailyDates(t, state.Days[`["install","screen","queue"]`]))
		})
	}
}

func TestDailyClaimsMigratesVersionOne(t *testing.T) {
	path, _ := seedDailyCapacity(t)
	status, err := NewDailyClaims(path).report(t.Context(), nil, "install", "screen", "queue", func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }, func(time.Time) (Status, error) {
		assert.Fail(t, "migration repeated an accepted report")
		return StatusQueued, nil
	})
	require.NoError(t, err)
	assert.Equal(t, StatusSkipped, status)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(data, &state))
	assert.Equal(t, 2, state.Version)
}

func acceptedDailyDates(t *testing.T, claims []dailyClaim) []string {
	t.Helper()
	days := make([]string, 0, len(claims))
	for _, claim := range claims {
		require.False(t, claim.Pending, "accepted history contains an unsettled reservation")
		days = append(days, claim.Day)
	}
	return days
}
