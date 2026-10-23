package posthog

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tokenRelay struct {
	server        *httptest.Server
	registrations atomic.Int32
	batches       atomic.Int32
	onRegister    func()
	events        chan phsdk.CaptureInApi
}

func newTokenRelay(t *testing.T, firstStatus int) *tokenRelay {
	t.Helper()
	relay := &tokenRelay{events: make(chan phsdk.CaptureInApi, 64)}
	relay.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, http.MethodPost, req.Method)
		switch req.URL.Path {
		case "/v1/register":
			attempt := relay.registrations.Add(1)
			if relay.onRegister != nil {
				relay.onRegister()
			}
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
			relay.batches.Add(1)
			var batch struct {
				Batch []phsdk.CaptureInApi `json:"batch"`
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
				assert.Equal(t, "install-a", event.DistinctId)
				assert.Equal(t, "test", event.Properties["application"])
				assert.Equal(t, "token-a", event.Properties["install_token"])
			}
			reporter := newSignedReporter(t, relay, dir, "install-b")
			require.NoError(t, reporter.Capture("command_run", nil))
			require.NoError(t, reporter.Close())
			assert.EqualValues(t, 2, relay.registrations.Load())
			require.Len(t, relay.events, 1)
			assert.Equal(t, "token-b", (<-relay.events).Properties["install_token"])
		})
	}
}

func TestSignedReporterRetriesRegistrationOnLaterUpload(t *testing.T) {
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, http.StatusTooManyRequests)
	path := filepath.Join(t.TempDir(), "telemetry-token.json")
	token := &installToken{path: path, distinctID: "install-a", application: "test", registerURL: relay.server.URL + "/v1/register", logger: slog.Default()}
	_, err := token.get(t.Context(), http.DefaultTransport)
	require.ErrorContains(t, err, "429")
	assert.EqualValues(t, 1, relay.registrations.Load())
	second := &installToken{path: path, distinctID: "install-a", application: "test", registerURL: relay.server.URL + "/v1/register", logger: slog.Default()}
	_, err = second.get(t.Context(), http.DefaultTransport)
	require.NoError(t, err)
	assert.EqualValues(t, 2, relay.registrations.Load())
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
			release = sync.OnceFunc(release)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := token.get(t.Context(), http.DefaultTransport)
				assert.NoError(t, err)
			}()
			defer func() {
				release()
				<-done
			}()
			// Observe get inside the held lock's wait before disabling telemetry.
			blockedIn := "sync.(*Mutex).Lock"
			if wait == "file lock" {
				blockedIn = "github.com/gofrs/flock.tryCtx"
			}
			require.Eventually(t, func() bool {
				stacks := make([]byte, 1<<20)
				n := runtime.Stack(stacks, true)
				for stack := range strings.SplitSeq(string(stacks[:n]), "\n\n") {
					if strings.Contains(stack, "(*installToken).get") && strings.Contains(stack, blockedIn) {
						return true
					}
				}
				return false
			}, 5*time.Second, time.Millisecond)
			DisableProcess()
			release()
			<-done
			assert.Zero(t, relay.registrations.Load())
		})
	}
}

func TestSignedReporterProcessDisabledDuringRegistrationSkipsBatch(t *testing.T) {
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, 0)
	relay.onRegister = DisableProcess
	reporter := newSignedReporter(t, relay, t.TempDir(), "install-a")
	require.NoError(t, reporter.Capture("command_run", nil))
	require.NoError(t, reporter.Close())
	assert.EqualValues(t, 1, relay.registrations.Load())
	assert.Zero(t, relay.batches.Load())
}

func TestSignedReportersShareTokenConcurrently(t *testing.T) {
	enableSignedTelemetry(t)
	relay := newTokenRelay(t, 0)
	path := filepath.Join(t.TempDir(), "telemetry-token.json")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		token := &installToken{path: path, distinctID: "install-a", application: "test", registerURL: relay.server.URL + "/v1/register", logger: slog.Default()}
		wg.Go(func() {
			<-start
			_, err := token.get(t.Context(), http.DefaultTransport)
			assert.NoError(t, err)
		})
	}
	close(start)
	wg.Wait()
	assert.EqualValues(t, 1, relay.registrations.Load())
}
