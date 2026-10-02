package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureHandlerTestOption() PostHogOption {
	return WithAllowedEvent("app_opened",
		AllowTelemetryProperty("view", AllowTelemetryStringValues("sessions", "search")),
		AllowTelemetryProperty("count", AllowTelemetryNumber),
	)
}

func postCapture(t *testing.T, h http.Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, "/telemetry/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var resp postHogCaptureResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp.Status
}

// newOptedOutCaptureTestReporter builds a reporter opted out by the prefixed
// environment variable and returns how often its client factory ran.
func newOptedOutCaptureTestReporter(t *testing.T) (*PostHogReporter, *int) {
	t.Helper()
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericTelemetryEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "0")
	factoryCalls := 0
	reporter, err := newPostHogReporter(PostHogOptions{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
	}, func(string, posthog.Config) (postHogEnqueueCloser, error) {
		factoryCalls++
		return &recordingPostHogClient{}, nil
	}, captureHandlerTestOption())
	require.NoError(t, err)
	return reporter, &factoryCalls
}

type captureHandlerTestState struct {
	name  string
	build func(t *testing.T) (reporter *PostHogReporter, sent func() int, factoryCalls func() int)
}

func optedOutCaptureState() captureHandlerTestState {
	return captureHandlerTestState{name: "env_opt_out", build: func(t *testing.T) (*PostHogReporter, func() int, func() int) {
		t.Helper()
		reporter, calls := newOptedOutCaptureTestReporter(t)
		return reporter, func() int { return 0 }, func() int { return *calls }
	}}
}

func processDisabledCaptureState() captureHandlerTestState {
	return captureHandlerTestState{name: "process_disabled", build: func(t *testing.T) (*PostHogReporter, func() int, func() int) {
		t.Helper()
		reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
		DisablePostHogTelemetry()
		t.Cleanup(enablePostHogTelemetryForTest)
		return reporter, func() int { return len(client.messages) }, nil
	}}
}

func closedCaptureState() captureHandlerTestState {
	return captureHandlerTestState{name: "closed", build: func(t *testing.T) (*PostHogReporter, func() int, func() int) {
		t.Helper()
		reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
		require.NoError(t, reporter.Close())
		return reporter, func() int { return len(client.messages) }, nil
	}}
}

func TestPostHogCaptureHandlerCapturesAllowedEventWithAllowedProperties(t *testing.T) {
	reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart.Add(-30*time.Hour), captureHandlerTestOption())

	rec := postCapture(t, NewPostHogCaptureHandler(reporter), http.MethodPost,
		`{"event":"app_opened","properties":{"view":"sessions","count":2,"path":"/Users/example/private","query":"secret"}}`)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "queued", decodeStatus(t, rec))
	require.Len(t, client.messages, 1)
	message := client.messages[0]
	assert.Equal(t, "app_opened", message.Event)
	assert.Equal(t, reporter.distinctID, message.DistinctId)
	assert.Equal(t, "sessions", message.Properties["view"])
	assert.IsType(t, float64(0), message.Properties["count"])
	assert.InDelta(t, 2, message.Properties["count"], 0)
	assert.NotContains(t, message.Properties, "path")
	assert.NotContains(t, message.Properties, "query")
	assert.Equal(t, int64(30), message.Properties[postHogInstallAgeProperty])
	assert.Equal(t, reporter.application, message.Properties["application"])
}

func TestPostHogCaptureHandlerRejectsUnknownEventInEveryState(t *testing.T) {
	states := []captureHandlerTestState{
		{name: "enabled", build: func(t *testing.T) (*PostHogReporter, func() int, func() int) {
			t.Helper()
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())
			return reporter, func() int { return len(client.messages) }, nil
		}},
		optedOutCaptureState(),
		processDisabledCaptureState(),
		closedCaptureState(),
		{name: "disabled_reporter", build: func(*testing.T) (*PostHogReporter, func() int, func() int) {
			return DisabledPostHogReporter(), func() int { return 0 }, nil
		}},
		{name: "nil_reporter", build: func(*testing.T) (*PostHogReporter, func() int, func() int) {
			return nil, func() int { return 0 }, nil
		}},
	}
	bodies := []string{
		`{"event":"app_closed"}`,
		`{"event":"   "}`,
		`{"event":""}`,
		`{"properties":{"view":"sessions"}}`,
		`null`,
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			reporter, sent, factoryCalls := state.build(t)
			handler := NewPostHogCaptureHandler(reporter)
			for _, body := range bodies {
				rec := postCapture(t, handler, http.MethodPost, body)
				assert.Equal(t, http.StatusBadRequest, rec.Code, body)
			}
			assert.Zero(t, sent())
			if factoryCalls != nil {
				assert.Zero(t, factoryCalls())
			}
		})
	}
}

func TestPostHogCaptureHandlerDisabledReporterAnswersDisabled(t *testing.T) {
	states := []captureHandlerTestState{
		optedOutCaptureState(),
		processDisabledCaptureState(),
		closedCaptureState(),
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			reporter, sent, factoryCalls := state.build(t)

			rec := postCapture(t, NewPostHogCaptureHandler(reporter), http.MethodPost,
				`{"event":"app_opened","properties":{"view":"sessions"}}`)

			require.Equal(t, http.StatusAccepted, rec.Code)
			assert.Equal(t, "disabled", decodeStatus(t, rec))
			assert.Zero(t, sent())
			if factoryCalls != nil {
				assert.Zero(t, factoryCalls())
			}
		})
	}
}

func TestPostHogCaptureHandlerKeepsReporterOwnedProperties(t *testing.T) {
	reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart.Add(-30*time.Hour),
		WithAllowedEvent("app_opened",
			AllowTelemetryProperty("application", allowAnyTelemetryValue),
			AllowTelemetryProperty("version", allowAnyTelemetryValue),
			AllowTelemetryProperty("source", allowAnyTelemetryValue),
			AllowTelemetryProperty(postHogInstallAgeProperty, allowAnyTelemetryValue),
			AllowTelemetryProperty("$process_person_profile", allowAnyTelemetryValue),
			AllowTelemetryProperty("$geoip_disable", allowAnyTelemetryValue),
			AllowTelemetryProperty("goos", allowAnyTelemetryValue),
		))

	rec := postCapture(t, NewPostHogCaptureHandler(reporter), http.MethodPost,
		`{"event":"app_opened","properties":{"application":"spoof","version":"spoof","source":"browser","install_age_hours":0,"$process_person_profile":true,"$geoip_disable":false,"goos":"plan9"}}`)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "queued", decodeStatus(t, rec))
	require.Len(t, client.messages, 1)
	message := client.messages[0]
	assert.Equal(t, reporter.distinctID, message.DistinctId)
	assert.Equal(t, reporter.application, message.Properties["application"])
	assert.Equal(t, reporter.version, message.Properties["version"])
	assert.Equal(t, reporter.source, message.Properties["source"])
	assert.Equal(t, int64(30), message.Properties[postHogInstallAgeProperty])
	assert.Equal(t, false, message.Properties["$process_person_profile"])
	assert.Equal(t, true, message.Properties["$geoip_disable"])
	assert.Equal(t, runtime.GOOS, message.Properties["goos"])
}

func TestPostHogCaptureHandlerDropsUnsafePropertyValues(t *testing.T) {
	tests := []struct {
		name string
		body string
		key  string
	}{
		{name: "path_view", body: `{"event":"app_opened","properties":{"view":"bad/path"}}`, key: "view"},
		{name: "array_view", body: `{"event":"app_opened","properties":{"view":["sessions"]}}`, key: "view"},
		{name: "string_count", body: `{"event":"app_opened","properties":{"count":"3"}}`, key: "count"},
		{name: "object_count", body: `{"event":"app_opened","properties":{"count":{"n":3}}}`, key: "count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCapture(t, NewPostHogCaptureHandler(reporter), http.MethodPost, tt.body)

			require.Equal(t, http.StatusAccepted, rec.Code)
			assert.Equal(t, "queued", decodeStatus(t, rec))
			require.Len(t, client.messages, 1)
			assert.NotContains(t, client.messages[0].Properties, tt.key)
		})
	}
}

func TestPostHogCaptureHandlerRejectsMalformedBody(t *testing.T) {
	bodies := []string{
		`{"event":`,
		`[]`,
		`{"event":"app_opened","properties":"view"}`,
		`{"event":7}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCapture(t, NewPostHogCaptureHandler(reporter), http.MethodPost, body)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Empty(t, client.messages)
		})
	}
}

func TestPostHogCaptureHandlerRejectsNonPost(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			reporter, client, _ := newInstallAgeTestReporter(t, postHogTestStart, captureHandlerTestOption())

			rec := postCapture(t, NewPostHogCaptureHandler(reporter), method, `{"event":"app_opened"}`)

			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
			assert.Empty(t, client.messages)
		})
	}
}

func TestPostHogCaptureHandlerReportsEnqueueFailure(t *testing.T) {
	reporter, _ := newInstallAgeTestReporterWithClient(t,
		failingPostHogClient{enqueueErr: posthog.ErrQueueFull}, postHogTestStart, captureHandlerTestOption())

	rec := postCapture(t, NewPostHogCaptureHandler(reporter), http.MethodPost, `{"event":"app_opened"}`)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "queued")
}
