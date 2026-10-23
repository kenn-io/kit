package posthog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

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

func newTokenRelay(t *testing.T, failFirst bool) *tokenRelay {
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
			if failFirst && attempt == 1 {
				w.WriteHeader(http.StatusBadRequest)
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
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, false)
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
}

func TestSignedReporterRetriesRegistrationOnLaterUpload(t *testing.T) {
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, true)
	dir := t.TempDir()
	first := newSignedReporter(t, relay, dir, "install-a")
	require.NoError(t, first.Capture("command_run", nil))
	require.NoError(t, first.Close())
	assert.EqualValues(t, 1, relay.registrations.Load())
	assert.Empty(t, relay.events)
	second := newSignedReporter(t, relay, dir, "install-a")
	require.NoError(t, second.Capture("command_run", nil))
	require.NoError(t, second.Close())
	assert.EqualValues(t, 2, relay.registrations.Load())
	require.Len(t, relay.events, 1)
	assert.Equal(t, "token-a", (<-relay.events).Properties.Token)
}

func TestSignedReporterOptOutSkipsRelay(t *testing.T) {
	for _, optOut := range []string{"CI", "DO_NOT_TRACK", "TEST_TELEMETRY_ENABLED", "process"} {
		t.Run(optOut, func(t *testing.T) {
			enableSignedTelemetry(t)
			relay := newTokenRelay(t, false)
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

func TestSignedReporterReplacesTokenForAnotherIdentity(t *testing.T) {
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, false)
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
	relay := newTokenRelay(t, false)
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
