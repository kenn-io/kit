package posthog

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePostHogClient struct {
	message phsdk.Message
	closed  bool
}

func (f *fakePostHogClient) Enqueue(message phsdk.Message) error {
	f.message = message
	return nil
}

func (f *fakePostHogClient) Close() error {
	f.closed = true
	return nil
}

type blockingPostHogClient struct {
	message        phsdk.Message
	enqueueStarted chan struct{}
	unblockEnqueue chan struct{}
	closeCalled    atomic.Bool
}

func (b *blockingPostHogClient) Enqueue(message phsdk.Message) error {
	b.message = message
	close(b.enqueueStarted)
	<-b.unblockEnqueue
	return nil
}

func (b *blockingPostHogClient) Close() error {
	b.closeCalled.Store(true)
	return nil
}

func TestPrefixedTelemetryEnabledEnv(t *testing.T) {
	assert.Equal(t, "KATA_TELEMETRY_ENABLED", PrefixedEnabledEnv(" kata "))
	assert.Equal(t, "ROBOREV_TELEMETRY_ENABLED", PrefixedEnabledEnv("ROBOREV"))
	assert.Empty(t, PrefixedEnabledEnv(""))
}

func TestPostHogTelemetryEnabledFromEnvHonorsPrefixAndGenericDisable(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)

	t.Setenv("KATA_TELEMETRY_ENABLED", "0")
	assert.False(t, EnabledFromEnv("kata"))

	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	assert.True(t, EnabledFromEnv("kata"))

	t.Setenv(GenericEnabledEnv, "0")
	assert.False(t, EnabledFromEnv("kata"))
}

func TestEnabledFromEnvOptOutSpellings(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	for _, value := range []string{"0", "false", "no", "off", " FALSE ", "Off"} {
		for _, env := range []string{GenericEnabledEnv, "KATA_TELEMETRY_ENABLED"} {
			t.Run(env+"="+value, func(t *testing.T) {
				t.Setenv(GenericEnabledEnv, "1")
				t.Setenv("KATA_TELEMETRY_ENABLED", "1")
				t.Setenv(env, value)

				reporter, err := NewReporter(Options{EnvPrefix: "KATA"}, WithAllowedEvent("daemon_active"))
				require.NoError(t, err)
				assert.False(t, reporter.Enabled())
				assert.True(t, reporter.EventAllowed("daemon_active"))
			})
		}
	}
	for _, value := range []string{"", "1", "true", "yes", "on"} {
		t.Setenv(GenericEnabledEnv, value)
		t.Setenv("KATA_TELEMETRY_ENABLED", value)
		assert.True(t, EnabledFromEnv("kata"), "value %q", value)
	}
}

func TestReporterCloseGivesUpAfterShutdownTimeout(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	// A server that never answers stands in for a network that drops packets.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	reporter, err := NewReporter(Options{
		APIKey: "phc_test", Endpoint: server.URL, Application: "kata",
		EnvPrefix: "KATA", DistinctID: "anonymous-instance-id",
	}, WithAllowedEvent("daemon_active"))
	require.NoError(t, err)
	require.NoError(t, reporter.Capture("daemon_active", nil))

	start := time.Now()
	err = reporter.Close()

	require.Error(t, err)
	assert.Less(t, time.Since(start), ShutdownTimeout+time.Second)
	assert.False(t, reporter.Enabled())
}

func TestPostHogTelemetryEnabledFromEnvHonorsProcessDisable(t *testing.T) {
	DisableProcess()
	t.Cleanup(enablePostHogTelemetryForTest)

	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	t.Setenv(GenericEnabledEnv, "1")

	assert.False(t, EnabledFromEnv("kata"))
}

func TestNewPostHogReporterDisabledByEnvSkipsRequiredFields(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "0")

	reporter, err := NewReporter(Options{})

	require.NoError(t, err)
	assert.False(t, reporter.Enabled())
}

func TestNewPostHogReporterDisabledRetainsAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		disable func(t *testing.T)
	}{
		{name: "generic_env", disable: func(t *testing.T) {
			t.Helper()
			t.Setenv(GenericEnabledEnv, " 0 ")
			t.Setenv("KATA_TELEMETRY_ENABLED", "1")
		}},
		{name: "prefixed_env", disable: func(t *testing.T) {
			t.Helper()
			t.Setenv(GenericEnabledEnv, "1")
			t.Setenv("KATA_TELEMETRY_ENABLED", " 0")
		}},
		{name: "process_disable", disable: func(t *testing.T) {
			t.Helper()
			t.Setenv(GenericEnabledEnv, "1")
			t.Setenv("KATA_TELEMETRY_ENABLED", "1")
			DisableProcess()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enablePostHogTelemetryForTest()
			t.Cleanup(enablePostHogTelemetryForTest)
			tt.disable(t)
			options := []Option{nil, WithAllowedEvent("app_opened")}

			nilFactoryReporter, err := newPostHogReporter(Options{EnvPrefix: "KATA"}, nil, options...)
			require.NoError(t, err)
			assert.True(t, nilFactoryReporter.EventAllowed("app_opened"))

			factoryCalls := 0
			reporter, err := newPostHogReporter(Options{EnvPrefix: "KATA"}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
				factoryCalls++
				return &recordingPostHogClient{}, nil
			}, options...)
			require.NoError(t, err)

			assert.False(t, reporter.Enabled())
			assert.True(t, reporter.EventAllowed("app_opened"))
			assert.False(t, reporter.EventAllowed("app_closed"))
			require.NoError(t, reporter.Capture("app_closed", nil))
			assert.Zero(t, factoryCalls)
		})
	}
}

func TestNewPostHogReporterRequiresCallerOwnedConfigurationWhenEnabled(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	_, err := newPostHogReporter(Options{
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
	}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
		return &fakePostHogClient{}, nil
	}, testAllowedTelemetryOptions()...)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "api key")
}

func TestNewPostHogReporterPassesMandatoryAPIKeyAndEndpointToPostHog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	var gotAPIKey string
	var gotConfig phsdk.Config
	reporter, err := newPostHogReporter(Options{
		APIKey:      "caller-owned-key",
		Endpoint:    "https://posthog.example.test",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
	}, func(apiKey string, config phsdk.Config) (postHogEnqueueCloser, error) {
		gotAPIKey = apiKey
		gotConfig = config
		return &fakePostHogClient{}, nil
	}, testAllowedTelemetryOptions()...)

	require.NoError(err)
	require.True(reporter.Enabled())
	assert.Equal("caller-owned-key", gotAPIKey)
	assert.Equal("https://posthog.example.test", gotConfig.Endpoint)
	require.NotNil(gotConfig.DisableGeoIP)
	assert.True(*gotConfig.DisableGeoIP)
}

func TestPostHogReporterCaptureUsesAnonymousDistinctIDAndPrivacyDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		version:       "v-test",
		commit:        "abc123",
		source:        "daemon",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	err := reporter.Capture("daemon_started", map[string]any{
		"$geoip_disable":          false,
		"$process_person_profile": true,
		"application":             "caller-app",
		"distinct_id":             "user-provided",
		"path":                    "/Users/example/private",
		"project_count":           3,
		"sync_enabled":            true,
		"view":                    "dashboard",
	})

	require.NoError(err)
	capture, ok := client.message.(phsdk.Capture)
	require.True(ok)
	assert.Equal("anonymous-instance-id", capture.DistinctId)
	assert.Equal("daemon_started", capture.Event)
	assert.Equal(3, capture.Properties["project_count"])
	assert.Equal(true, capture.Properties["sync_enabled"])
	assert.NotContains(capture.Properties, "distinct_id")
	assert.NotContains(capture.Properties, "path")
	assert.NotContains(capture.Properties, "view")
	assert.False(capture.Properties["$process_person_profile"].(bool))
	assert.True(capture.Properties["$geoip_disable"].(bool))
	assert.Equal("kata", capture.Properties["application"])
	assert.Equal("v-test", capture.Properties["version"])
	assert.Equal("abc123", capture.Properties["commit"])
	assert.Equal(runtime.GOOS, capture.Properties["goos"])
	assert.Equal(runtime.GOARCH, capture.Properties["goarch"])
	assert.Equal("daemon", capture.Properties["source"])
}

func TestPostHogReporterCaptureRejectsUnsupportedEvents(t *testing.T) {
	reporter := &Reporter{
		client:        &fakePostHogClient{},
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	err := reporter.Capture("issue_created", map[string]any{"project_count": 1})

	require.ErrorIs(t, err, ErrUnsupportedEvent)
}

func TestPostHogReporterCaptureDropsUnsafePropertyValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		source:        "daemon",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	err := reporter.Capture("daemon_active", map[string]any{
		"project_count": "private-project-name",
		"sync_enabled":  "yes",
		"view":          "bad/path",
	})

	require.NoError(err)
	capture, ok := client.message.(phsdk.Capture)
	require.True(ok)
	assert.NotContains(capture.Properties, "project_count")
	assert.NotContains(capture.Properties, "sync_enabled")
	assert.NotContains(capture.Properties, "view")
}

func TestPostHogReporterAllowsDefaultPropertiesOnlyEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	client := &fakePostHogClient{}
	reporter, err := newPostHogReporter(Options{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		Version:     "v-test",
		Commit:      "abc123",
	}, func(string, phsdk.Config) (postHogEnqueueCloser, error) {
		return client, nil
	}, WithAllowedEvent("event_without_properties"))
	require.NoError(err)

	err = reporter.Capture("event_without_properties", map[string]any{
		"private_path": "/Users/example/private",
	})
	require.NoError(err)

	capture, ok := client.message.(phsdk.Capture)
	require.True(ok)
	assert.Equal("event_without_properties", capture.Event)
	assert.NotContains(capture.Properties, "private_path")
	assert.Equal("kata", capture.Properties["application"])
	assert.Equal("daemon", capture.Properties["source"])
	assert.Equal("v-test", capture.Properties["version"])
	assert.Equal("abc123", capture.Properties["commit"])
	assert.False(capture.Properties["$process_person_profile"].(bool))
	assert.True(capture.Properties["$geoip_disable"].(bool))
}

func TestPostHogReporterCaptureHonorsProcessDisableAfterCreation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}
	require.True(reporter.Enabled())

	DisableProcess()
	t.Cleanup(enablePostHogTelemetryForTest)

	assert.False(reporter.Enabled())
	err := reporter.Capture("daemon_active", map[string]any{"project_count": 1})
	require.NoError(err)
	assert.Nil(client.message)

	require.NoError(reporter.Close())
	assert.True(client.closed)
	assert.False(reporter.Enabled())
}

func TestPostHogDisableTransportNoOpsRequestsAfterProcessDisable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)

	baseCalled := false
	transport := postHogDisableTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			baseCalled = true
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}

	DisableProcess()

	resp, err := transport.RoundTrip(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://posthog.example.test/batch", nil))

	require.NoError(err)
	require.NotNil(resp)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusNoContent, resp.StatusCode)
	assert.False(baseCalled)
}

func TestPostHogReporterCloseAfterProcessDisableDoesNotFlushQueuedEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	require.NoError(reporter.Capture("daemon_active", map[string]any{"project_count": 1}))
	require.NotNil(client.message)

	DisableProcess()
	t.Cleanup(enablePostHogTelemetryForTest)

	require.NoError(reporter.Close())
	assert.True(client.closed)
	assert.False(reporter.Enabled())
}

func TestPostHogReporterCloseWaitsForInFlightCapture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &blockingPostHogClient{
		enqueueStarted: make(chan struct{}),
		unblockEnqueue: make(chan struct{}),
	}
	reporter := &Reporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	captureErr := make(chan error, 1)
	go func() {
		captureErr <- reporter.Capture("daemon_active", map[string]any{"project_count": 1})
	}()
	<-client.enqueueStarted

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- reporter.Close()
	}()

	assert.Never(client.closeCalled.Load, 50*time.Millisecond, 5*time.Millisecond)

	close(client.unblockEnqueue)
	require.NoError(<-captureErr)
	require.NoError(<-closeErr)
	assert.True(client.closeCalled.Load())
	assert.False(reporter.Enabled())
}

func TestAllowTelemetryStringValues(t *testing.T) {
	filter := AllowStringValues("pulls.list")

	value, ok := filter(" pulls.list ")
	require.True(t, ok)
	assert.Equal(t, "pulls.list", value)

	_, ok = filter("private-project-name")
	assert.False(t, ok)
}

func testAllowedTelemetryOptions() []Option {
	return []Option{
		WithAllowedEvent("daemon_active",
			AllowProperty("project_count", AllowNumber),
			AllowProperty("sync_enabled", AllowBool),
			AllowProperty("view", AllowStringValues("dashboard", "summary")),
		),
		WithAllowedEvent("daemon_started",
			AllowProperty("project_count", AllowNumber),
			AllowProperty("sync_enabled", AllowBool),
		),
	}
}

func testAllowedTelemetryEvents() map[string]map[string]PropertyFilter {
	return map[string]map[string]PropertyFilter{
		"daemon_active": {
			"project_count": AllowNumber,
			"sync_enabled":  AllowBool,
			"view":          AllowStringValues("dashboard", "summary"),
		},
		"daemon_started": {
			"project_count": AllowNumber,
			"sync_enabled":  AllowBool,
		},
	}
}

func enablePostHogTelemetryForTest() {
	postHogTelemetryDisabled.Store(false)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReporterRoutesSDKLogsToSlog(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic ingest rejection", http.StatusUnauthorized)
	}))
	defer server.Close()
	reporter, err := NewReporter(Options{
		APIKey: "phc_test", Endpoint: server.URL, Application: "test",
		EnvPrefix: "TEST", DistinctID: "synthetic-install",
	}, WithAllowedEvent(EventDaemonActive))
	require.NoError(t, err)
	require.NoError(t, reporter.Capture(EventDaemonActive, nil))
	_ = reporter.Close()
	assert.Contains(t, logs.String(), `"level":"INFO"`)
	assert.Contains(t, logs.String(), `"component":"posthog"`)
	assert.Contains(t, logs.String(), "synthetic ingest rejection")
}

func TestReporterPreservesSDKLogLevelsWithCustomLogger(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericEnabledEnv, "1")
	t.Setenv("TEST_TELEMETRY_ENABLED", "1")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reporter, err := newPostHogReporter(Options{
		APIKey: "phc_test", Application: "test", EnvPrefix: "TEST", DistinctID: "synthetic-install", Logger: logger,
	}, func(_ string, config phsdk.Config) (postHogEnqueueCloser, error) {
		require.NotNil(t, config.Logger)
		config.Logger.Debugf("debug %d", 1)
		config.Logger.Logf("info %d", 2)
		config.Logger.Warnf("warning %d", 3)
		config.Logger.Errorf("error %d", 4)
		return &fakePostHogClient{}, nil
	}, WithAllowedEvent(EventDaemonActive))
	require.NoError(t, err)
	require.NoError(t, reporter.Close())
	for _, pair := range [][2]string{{"DEBUG", "debug 1"}, {"INFO", "info 2"}, {"WARN", "warning 3"}, {"ERROR", "error 4"}} {
		assert.Contains(t, logs.String(), `"level":"`+pair[0]+`","msg":"`+pair[1]+`","component":"posthog"`)
	}
}
