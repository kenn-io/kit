package telemetry

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePostHogClient struct {
	message posthog.Message
	closed  bool
}

func (f *fakePostHogClient) Enqueue(message posthog.Message) error {
	f.message = message
	return nil
}

func (f *fakePostHogClient) Close() error {
	f.closed = true
	return nil
}

func TestPrefixedTelemetryEnabledEnv(t *testing.T) {
	assert.Equal(t, "KATA_TELEMETRY_ENABLED", PrefixedTelemetryEnabledEnv(" kata "))
	assert.Equal(t, "ROBOREV_TELEMETRY_ENABLED", PrefixedTelemetryEnabledEnv("ROBOREV"))
	assert.Empty(t, PrefixedTelemetryEnabledEnv(""))
}

func TestPostHogTelemetryEnabledFromEnvHonorsPrefixAndGenericDisable(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)

	t.Setenv("KATA_TELEMETRY_ENABLED", "0")
	assert.False(t, PostHogTelemetryEnabledFromEnv("kata"))

	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	assert.True(t, PostHogTelemetryEnabledFromEnv("kata"))

	t.Setenv(GenericTelemetryEnabledEnv, "0")
	assert.False(t, PostHogTelemetryEnabledFromEnv("kata"))
}

func TestPostHogTelemetryEnabledFromEnvHonorsProcessDisable(t *testing.T) {
	DisablePostHogTelemetry()
	t.Cleanup(enablePostHogTelemetryForTest)

	t.Setenv("KATA_TELEMETRY_ENABLED", "1")
	t.Setenv(GenericTelemetryEnabledEnv, "1")

	assert.False(t, PostHogTelemetryEnabledFromEnv("kata"))
}

func TestNewPostHogReporterDisabledByEnvSkipsRequiredFields(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericTelemetryEnabledEnv, "0")

	reporter, err := NewPostHogReporter(PostHogOptions{})

	require.NoError(t, err)
	assert.False(t, reporter.Enabled())
}

func TestNewPostHogReporterRequiresCallerOwnedConfigurationWhenEnabled(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericTelemetryEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	_, err := newPostHogReporter(PostHogOptions{
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
	}, func(string, posthog.Config) (postHogEnqueueCloser, error) {
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
	t.Setenv(GenericTelemetryEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	var gotAPIKey string
	var gotConfig posthog.Config
	reporter, err := newPostHogReporter(PostHogOptions{
		APIKey:      "caller-owned-key",
		Endpoint:    "https://posthog.example.test",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
	}, func(apiKey string, config posthog.Config) (postHogEnqueueCloser, error) {
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
	reporter := &PostHogReporter{
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
	capture, ok := client.message.(posthog.Capture)
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
	reporter := &PostHogReporter{
		client:        &fakePostHogClient{},
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	err := reporter.Capture("issue_created", map[string]any{"project_count": 1})

	require.ErrorIs(t, err, ErrUnsupportedTelemetryEvent)
}

func TestPostHogReporterCaptureDropsUnsafePropertyValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &PostHogReporter{
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
	capture, ok := client.message.(posthog.Capture)
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
	t.Setenv(GenericTelemetryEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	client := &fakePostHogClient{}
	reporter, err := newPostHogReporter(PostHogOptions{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		Version:     "v-test",
		Commit:      "abc123",
	}, func(string, posthog.Config) (postHogEnqueueCloser, error) {
		return client, nil
	}, WithAllowedEvent("event_without_properties"))
	require.NoError(err)

	err = reporter.Capture("event_without_properties", map[string]any{
		"private_path": "/Users/example/private",
	})
	require.NoError(err)

	capture, ok := client.message.(posthog.Capture)
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
	reporter := &PostHogReporter{
		client:        client,
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}
	require.True(reporter.Enabled())

	DisablePostHogTelemetry()
	t.Cleanup(enablePostHogTelemetryForTest)

	assert.False(reporter.Enabled())
	err := reporter.Capture("daemon_active", map[string]any{"project_count": 1})
	require.NoError(err)
	assert.Nil(client.message)

	require.NoError(reporter.Close())
	assert.True(client.closed)
}

func TestPostHogDisableTransportRejectsRequestsAfterProcessDisable(t *testing.T) {
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

	DisablePostHogTelemetry()

	resp, err := transport.RoundTrip(httptest.NewRequest(http.MethodPost, "https://posthog.example.test/batch", nil))

	require.ErrorIs(err, ErrPostHogTelemetryDisabled)
	assert.Nil(resp)
	assert.False(baseCalled)
}

func TestAllowTelemetryToken(t *testing.T) {
	value, ok := AllowTelemetryToken("pulls.list")
	require.True(t, ok)
	assert.Equal(t, "pulls.list", value)

	_, ok = AllowTelemetryToken("private/path")
	assert.False(t, ok)
}

func testAllowedTelemetryOptions() []PostHogOption {
	return []PostHogOption{
		WithAllowedEvent("daemon_active",
			AllowTelemetryProperty("project_count", AllowTelemetryNumber),
			AllowTelemetryProperty("sync_enabled", AllowTelemetryBool),
			AllowTelemetryProperty("view", AllowTelemetryToken),
		),
		WithAllowedEvent("daemon_started",
			AllowTelemetryProperty("project_count", AllowTelemetryNumber),
			AllowTelemetryProperty("sync_enabled", AllowTelemetryBool),
		),
	}
}

func testAllowedTelemetryEvents() map[string]map[string]TelemetryPropertyFilter {
	return map[string]map[string]TelemetryPropertyFilter{
		"daemon_active": {
			"project_count": AllowTelemetryNumber,
			"sync_enabled":  AllowTelemetryBool,
			"view":          AllowTelemetryToken,
		},
		"daemon_started": {
			"project_count": AllowTelemetryNumber,
			"sync_enabled":  AllowTelemetryBool,
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
