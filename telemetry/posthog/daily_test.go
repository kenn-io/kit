package posthog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
				_, err := claims.report(t.Context(), "id", "screen", "queue", clock, func(time.Time) (Status, error) {
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
				status, err := secondClaims.report(waitContext, "id", "screen", secondKey, clock, func(time.Time) (Status, error) {
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
				status, err := claims.report(t.Context(), "id", "screen", key, clock, func(time.Time) (Status, error) {
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
	// semaphore.Acquire calls ctx.Done once on an uncontended acquire; skip it to signal the file lock wait.
	waitContext := &dailyWaitContext{Context: ctx, doneCalled: make(chan struct{}), skipDoneCalls: 1}
	result := make(chan Status, 1)
	failures := make(chan error, 1)
	go func() {
		status, err := claims.report(waitContext, "id", "screen", "queue", clock, send)
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
	for _, body := range []string{`{`, `{"version":2,"days":{}}`, `{"version":1,"days":{"key":["tomorrow"]}}`, `null`, `{}`, `{"days":{}}`, `{"version":1}`, `{"version":1,"days":null}`} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		sends := 0
		_, err := NewDailyClaims(path).report(t.Context(), "id", "screen", "queue", time.Now, func(time.Time) (Status, error) { sends++; return StatusQueued, nil })
		require.Error(t, err, body)
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
		_, err := NewDailyClaims(path).report(waitContext, "id", "screen", "queue", time.Now, func(time.Time) (Status, error) { return StatusQueued, nil })
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
		for _, failure := range []string{"unpublished reservation", "published rollback", "published deferred cleanup", "published reservation"} {
			t.Run(failure+publishedErr.Error(), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "days.json")
				claims := NewDailyClaims(path)
				writes, accepted := 0, 0
				claims.write = func(p string, data []byte, options ...atomicfile.Option) error {
					writes++
					if failure == "unpublished reservation" && writes == 1 || failure == "published deferred cleanup" && writes == 2 {
						return errors.New("write failed before publication")
					}
					if err := atomicfile.WriteFile(p, data, options...); err != nil {
						return err
					}
					if failure == "published rollback" && writes == 2 || failure == "published deferred cleanup" && writes == 3 || failure == "published reservation" && writes == 1 {
						return fmt.Errorf("write failed after publication: %w", publishedErr)
					}
					return nil
				}
				send := func(time.Time) (Status, error) { accepted++; return StatusQueued, nil }
				report := func(c *DailyClaims, callback func(time.Time) (Status, error)) (Status, error) {
					return c.report(t.Context(), "id", "screen", "queue", time.Now, callback)
				}
				_, err := report(claims, func(time.Time) (Status, error) {
					if failure == "published reservation" || failure == "unpublished reservation" {
						assert.Fail(t, "capture after failed reservation")
					}
					return "", errors.New("rejected")
				})
				require.Error(t, err)
				assert.Zero(t, accepted)
				if failure == "published reservation" {
					require.NotNil(t, claims.pending)
					status, err := report(claims, send)
					require.NoError(t, err)
					assert.Equal(t, StatusQueued, status)
				} else {
					if failure == "published deferred cleanup" {
						_, err = report(claims, send)
						require.Error(t, err)
						assert.Zero(t, accepted)
					}
					assert.Nil(t, claims.pending)
					status, err := report(NewDailyClaims(path), send)
					require.NoError(t, err)
					assert.Equal(t, StatusQueued, status)
				}
				status, err := report(claims, send)
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
