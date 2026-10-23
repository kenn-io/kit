package posthog

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tokenEvent struct {
	DistinctID string `json:"distinct_id"`
	Properties struct {
		Application string `json:"application"`
		Token       string `json:"install_token"`
	} `json:"properties"`
}

type tokenRelay struct {
	server        *httptest.Server
	registrations atomic.Int32
	events        chan tokenEvent
}

func newTokenRelay(t *testing.T, firstStatus int) *tokenRelay {
	t.Helper()
	relay := &tokenRelay{events: make(chan tokenEvent, 64)}
	relay.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, http.MethodPost, req.Method)
		switch req.URL.Path {
		case "/v1/register":
			attempt := relay.registrations.Add(1)
			assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
			var registration map[string]string
			if !assert.NoError(t, json.NewDecoder(req.Body).Decode(&registration)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Len(t, registration, 2)
			assert.Equal(t, "test", registration["application"])
			if firstStatus != 0 && attempt == 1 {
				w.WriteHeader(firstStatus)
				return
			}
			if registration["distinct_id"] == "install-b" {
				_, _ = w.Write([]byte(`{"token":"token-b"}`))
			} else {
				assert.Equal(t, "install-a", registration["distinct_id"])
				_, _ = w.Write([]byte(`{"token":"token-a"}`))
			}
		case "/batch/":
			var batch struct {
				Batch []tokenEvent `json:"batch"`
			}
			if assert.NoError(t, json.NewDecoder(req.Body).Decode(&batch)) {
				for _, event := range batch.Batch {
					relay.events <- event
				}
			}
		default:
			assert.Fail(t, "unexpected relay route", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(relay.server.Close)
	return relay
}

func enableSignedTelemetry(t *testing.T) {
	t.Helper()
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
}

func newSignedReporter(t *testing.T, relay *tokenRelay, dir, id string) *Reporter {
	t.Helper()
	reporter, err := NewReporter(Options{
		APIKey: "phc_test", Endpoint: relay.server.URL, TokenDir: dir,
		Application: "test", EnvPrefix: "TEST", DistinctID: id, Version: "v1.2.3",
	}, WithAllowedEvent("command_run"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reporter.Close() })
	return reporter
}

func TestSignedReporterRegistersOnceAcrossRestarts(t *testing.T) {
	for _, suffix := range []string{"", "/"} {
		t.Run("endpoint="+suffix, func(t *testing.T) {
			enableSignedTelemetry(t)
			relay := newTokenRelay(t, 0)
			relay.server.URL += suffix
			dir := t.TempDir()
			for range 2 {
				reporter := newSignedReporter(t, relay, dir, "install-a")
				require.NoError(t, reporter.Capture("command_run", nil))
				require.NoError(t, reporter.Capture("command_run", nil))
				require.NoError(t, reporter.Close())
			}
			assert.EqualValues(t, 1, relay.registrations.Load())
			require.Len(t, relay.events, 4)
			for range 4 {
				event := <-relay.events
				assert.Equal(t, "install-a", event.DistinctID)
				assert.Equal(t, "test", event.Properties.Application)
				assert.Equal(t, "token-a", event.Properties.Token)
			}
		})
	}
}

func TestSignedReporterRetriesRegistrationOnLaterUpload(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			enableSignedTelemetry(t)
			relay := newTokenRelay(t, status)
			dir := t.TempDir()
			token := &installToken{path: filepath.Join(dir, "telemetry-token.json"), distinctID: "install-a", application: "test", registerURL: relay.server.URL + "/v1/register", logger: slog.Default()}
			_, err := token.get(t.Context(), http.DefaultTransport)
			require.ErrorContains(t, err, strconv.Itoa(status))
			assert.EqualValues(t, 1, relay.registrations.Load())
			assert.Empty(t, relay.events)
			second := newSignedReporter(t, relay, dir, "install-a")
			require.NoError(t, second.Capture("command_run", nil))
			require.NoError(t, second.Close())
			assert.EqualValues(t, 2, relay.registrations.Load())
			require.Len(t, relay.events, 1)
			assert.Equal(t, "token-a", (<-relay.events).Properties.Token)
		})
	}
}

func TestInstallTokenOptOutAfterLockWait(t *testing.T) {
	for _, wait := range []string{"mutex", "file lock"} {
		t.Run(wait, func(t *testing.T) {
			enableSignedTelemetry(t)
			relay := newTokenRelay(t, 0)
			token := &installToken{path: filepath.Join(t.TempDir(), "telemetry-token.json"), distinctID: "install-a", application: "test", registerURL: relay.server.URL + "/v1/register", logger: slog.Default()}
			var release func()
			if wait == "mutex" {
				token.mu.Lock()
				release = token.mu.Unlock
			} else {
				lock := flock.New(token.path + ".lock")
				require.NoError(t, lock.Lock())
				release = func() { require.NoError(t, lock.Unlock()) }
			}
			started := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				close(started)
				_, err := token.get(t.Context(), http.DefaultTransport)
				assert.NoError(t, err)
			}()
			<-started
			DisableProcess()
			release()
			<-done
			assert.Zero(t, relay.registrations.Load())
			assert.Empty(t, relay.events)
		})
	}
}

func TestSignedReporterOptOutSkipsRelay(t *testing.T) {
	for _, optOut := range []string{"CI", "DO_NOT_TRACK", "TEST_TELEMETRY_ENABLED", "process"} {
		t.Run(optOut, func(t *testing.T) {
			enableSignedTelemetry(t)
			relay := newTokenRelay(t, 0)
			switch optOut {
			case "CI", "DO_NOT_TRACK":
				t.Setenv(optOut, "1")
			case "TEST_TELEMETRY_ENABLED":
				t.Setenv(optOut, "0")
			}
			reporter := newSignedReporter(t, relay, t.TempDir(), "install-a")
			if optOut == "process" {
				require.NoError(t, reporter.Capture("command_run", nil))
				DisableProcess()
			}
			require.NoError(t, reporter.Capture("command_run", nil))
			require.NoError(t, reporter.Close())
			assert.Zero(t, relay.registrations.Load())
			assert.Empty(t, relay.events)
		})
	}
}

func TestSignedReporterProcessDisabledDuringRegistrationSkipsBatch(t *testing.T) {
	enableSignedTelemetry(t)
	var registrations, batches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/register":
			registrations.Add(1)
			DisableProcess()
			_, _ = w.Write([]byte(`{"token":"token-a"}`))
		case "/batch/":
			batches.Add(1)
		default:
			assert.Fail(t, "unexpected relay route", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	reporter, err := NewReporter(Options{
		APIKey: "phc_test", Endpoint: server.URL, TokenDir: t.TempDir(),
		Application: "test", EnvPrefix: "TEST", DistinctID: "install-a", Version: "v1.2.3",
	}, WithAllowedEvent("command_run"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = reporter.Close() })
	require.NoError(t, reporter.Capture("command_run", nil))
	require.NoError(t, reporter.Close())
	assert.EqualValues(t, 1, registrations.Load())
	assert.Zero(t, batches.Load())
}

func TestSignedReporterReplacesTokenForAnotherIdentity(t *testing.T) {
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, 0)
	dir := t.TempDir()
	for _, id := range []string{"install-a", "install-b"} {
		reporter := newSignedReporter(t, relay, dir, id)
		require.NoError(t, reporter.Capture("command_run", nil))
		require.NoError(t, reporter.Close())
	}
	assert.EqualValues(t, 2, relay.registrations.Load())
	require.Len(t, relay.events, 2)
	assert.Equal(t, "token-a", (<-relay.events).Properties.Token)
	event := <-relay.events
	assert.Equal(t, "install-b", event.DistinctID)
	assert.Equal(t, "test", event.Properties.Application)
	assert.Equal(t, "token-b", event.Properties.Token)
}

func TestSignedReportersShareTokenConcurrently(t *testing.T) {
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, 0)
	dir := t.TempDir()
	reporters := make([]*Reporter, 16)
	for i := range reporters {
		reporters[i] = newSignedReporter(t, relay, dir, "install-a")
		require.NoError(t, reporters[i].Capture("command_run", nil))
	}
	var wg sync.WaitGroup
	for _, reporter := range reporters {
		wg.Go(func() { assert.NoError(t, reporter.Close()) })
	}
	wg.Wait()
	assert.EqualValues(t, 1, relay.registrations.Load())
	require.Len(t, relay.events, len(reporters))
	for range reporters {
		assert.Equal(t, "token-a", (<-relay.events).Properties.Token)
	}
}
