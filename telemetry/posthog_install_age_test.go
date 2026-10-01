package telemetry

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingPostHogClient struct {
	mu       sync.Mutex
	messages []posthog.Capture
	closed   bool
	closeErr error
}

func (c *recordingPostHogClient) Enqueue(message posthog.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, message.(posthog.Capture))
	return nil
}

func (c *recordingPostHogClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	c.closed = true
	return nil
}

func (c *recordingPostHogClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

type fakePostHogClock struct {
	now time.Time
}

func (c *fakePostHogClock) Now() time.Time { return c.now }

func (c *fakePostHogClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type fakePostHogTimer struct {
	delay   time.Duration
	fire    func()
	stopped bool
}

type fakePostHogTimers struct {
	scheduled []*fakePostHogTimer
}

func (f *fakePostHogTimers) afterFunc(d time.Duration, fn func()) func() bool {
	timer := &fakePostHogTimer{delay: d, fire: fn}
	f.scheduled = append(f.scheduled, timer)
	return func() bool {
		timer.stopped = true
		return true
	}
}

func (f *fakePostHogTimers) last(t *testing.T) *fakePostHogTimer {
	t.Helper()
	require.NotEmpty(t, f.scheduled, "no release timer scheduled")
	return f.scheduled[len(f.scheduled)-1]
}

var postHogTestStart = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newInstallAgeTestReporter(t *testing.T, installedAt time.Time) (*PostHogReporter, *recordingPostHogClient, *fakePostHogClock, *fakePostHogTimers) {
	t.Helper()
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericTelemetryEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	client := &recordingPostHogClient{}
	clock := &fakePostHogClock{now: postHogTestStart}
	timers := &fakePostHogTimers{}
	options := append(testAllowedTelemetryOptions(), postHogOptionFunc(func(config *postHogReporterConfig) {
		config.now = clock.Now
		config.afterFunc = timers.afterFunc
	}))
	reporter, err := newPostHogReporter(PostHogOptions{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		InstalledAt: installedAt,
	}, func(string, posthog.Config) (postHogEnqueueCloser, error) {
		return client, nil
	}, options...)
	require.NoError(t, err)
	return reporter, client, clock, timers
}

func capturedEvents(messages []posthog.Capture) []string {
	events := make([]string, 0, len(messages))
	for _, message := range messages {
		events = append(events, message.Event)
	}
	return events
}

func TestPostHogReporterHoldsEventsFromYoungInstall(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, _, timers := newInstallAgeTestReporter(t, postHogTestStart.Add(-time.Hour))

	require.NoError(reporter.Capture("daemon_started", map[string]any{"project_count": 1}))
	require.NoError(reporter.Capture("daemon_active", map[string]any{"project_count": 2}))

	assert.Empty(client.messages)
	assert.True(reporter.Enabled())
	assert.Equal(23*time.Hour, timers.last(t).delay)
}

func TestPostHogReporterTimerSendsHeldEventsWithOriginalTimestamps(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock, timers := newInstallAgeTestReporter(t, postHogTestStart)

	require.NoError(reporter.Capture("daemon_started", map[string]any{
		"project_count": 1,
		"path":          "/Users/example/private",
	}))
	clock.Advance(3 * time.Hour)
	require.NoError(reporter.Capture("daemon_active", map[string]any{"project_count": 2}))
	require.Empty(client.messages)

	clock.Advance(21 * time.Hour)
	timers.last(t).fire()

	require.Len(client.messages, 2)
	assert.Equal([]string{"daemon_started", "daemon_active"}, capturedEvents(client.messages))
	assert.Equal(postHogTestStart, client.messages[0].Timestamp)
	assert.Equal(postHogTestStart.Add(3*time.Hour), client.messages[1].Timestamp)
	assert.Equal(1, client.messages[0].Properties["project_count"])
	assert.NotContains(client.messages[0].Properties, "path")
	assert.Equal("kata", client.messages[0].Properties["application"])

	clock.Advance(time.Minute)
	require.NoError(reporter.Capture("daemon_active", map[string]any{"project_count": 3}))
	require.Len(client.messages, 3)
	assert.Equal(clock.Now(), client.messages[2].Timestamp)
}

func TestPostHogReporterCaptureAfter24HoursSendsHeldEventsFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock, _ := newInstallAgeTestReporter(t, postHogTestStart)

	require.NoError(reporter.Capture("daemon_started", nil))
	clock.Advance(postHogInstallHoldPeriod)
	require.NoError(reporter.Capture("daemon_active", nil))

	assert.Equal([]string{"daemon_started", "daemon_active"}, capturedEvents(client.messages))
	assert.Equal(postHogTestStart, client.messages[0].Timestamp)
	assert.Equal(postHogTestStart.Add(postHogInstallHoldPeriod), client.messages[1].Timestamp)
}

func TestPostHogReporterMatureInstallSendsImmediately(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, _, timers := newInstallAgeTestReporter(t, postHogTestStart.Add(-25*time.Hour))

	require.NoError(reporter.Capture("daemon_active", map[string]any{"project_count": 1}))

	require.Len(client.messages, 1)
	assert.Equal(postHogTestStart, client.messages[0].Timestamp)
	assert.Empty(timers.scheduled)
}

func TestPostHogReporterZeroInstalledAtSendsImmediately(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, _, timers := newInstallAgeTestReporter(t, time.Time{})

	require.NoError(reporter.Capture("daemon_started", nil))

	require.Len(client.messages, 1)
	assert.Equal(postHogTestStart, client.messages[0].Timestamp)
	assert.Empty(timers.scheduled)
}

func TestPostHogReporterCloseBefore24HoursDropsHeldEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock, timers := newInstallAgeTestReporter(t, postHogTestStart)
	timer := timers.last(t)

	require.NoError(reporter.Capture("daemon_started", nil))
	clock.Advance(time.Second)
	require.NoError(reporter.Close())

	assert.Empty(client.messages)
	assert.True(client.closed)
	assert.True(timer.stopped)
	assert.False(reporter.Enabled())

	clock.Advance(postHogInstallHoldPeriod)
	timer.fire()
	assert.Empty(client.messages)
}

func TestPostHogReporterFailedCloseKeepsReleaseTimer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock, timers := newInstallAgeTestReporter(t, postHogTestStart)
	timer := timers.last(t)
	closeErr := errors.New("close failed")
	client.closeErr = closeErr

	require.NoError(reporter.Capture("daemon_started", nil))
	require.ErrorIs(reporter.Close(), closeErr)
	assert.False(timer.stopped)
	assert.True(reporter.Enabled())

	require.NoError(reporter.Capture("daemon_active", nil))
	clock.Advance(postHogInstallHoldPeriod)
	timer.fire()
	assert.Equal([]string{"daemon_active"}, capturedEvents(client.messages))
}

func TestPostHogReporterCloseAfter24HoursSendsHeldEvents(t *testing.T) {
	require := require.New(t)

	reporter, client, clock, _ := newInstallAgeTestReporter(t, postHogTestStart)

	require.NoError(reporter.Capture("daemon_started", nil))
	clock.Advance(postHogInstallHoldPeriod)
	require.NoError(reporter.Close())

	assert.Equal(t, []string{"daemon_started"}, capturedEvents(client.messages))
	assert.True(t, client.closed)
}

func TestPostHogReporterCapsHeldEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock, timers := newInstallAgeTestReporter(t, postHogTestStart)

	require.NoError(reporter.Capture("daemon_started", nil))
	for range postHogMaxHeldEvents + 10 {
		clock.Advance(time.Second)
		require.NoError(reporter.Capture("daemon_active", nil))
	}
	clock.now = postHogTestStart.Add(postHogInstallHoldPeriod)
	timers.last(t).fire()

	require.Len(client.messages, postHogMaxHeldEvents)
	assert.Equal("daemon_started", client.messages[0].Event)
	assert.Equal(postHogTestStart.Add(time.Duration(postHogMaxHeldEvents-1)*time.Second), client.messages[postHogMaxHeldEvents-1].Timestamp)
}

func TestPostHogReporterTimerWaitsOutWallClockDifference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter, client, clock, timers := newInstallAgeTestReporter(t, postHogTestStart)
	require.NoError(reporter.Capture("daemon_started", nil))

	clock.Advance(postHogInstallHoldPeriod - time.Hour)
	timers.last(t).fire()

	assert.Empty(client.messages)
	require.Len(timers.scheduled, 2)
	assert.Equal(time.Hour, timers.last(t).delay)

	clock.Advance(time.Hour)
	timers.last(t).fire()
	assert.Equal([]string{"daemon_started"}, capturedEvents(client.messages))
}

func TestPostHogReporterFutureInstalledAtCountsAsNewInstall(t *testing.T) {
	reporter, client, _, timers := newInstallAgeTestReporter(t, postHogTestStart.Add(72*time.Hour))

	require.NoError(t, reporter.Capture("daemon_started", nil))

	assert.Empty(t, client.messages)
	assert.Equal(t, postHogInstallHoldPeriod, timers.last(t).delay)
}

func TestPostHogReporterTimerDropsHeldEventsAfterProcessDisable(t *testing.T) {
	require := require.New(t)

	reporter, client, clock, timers := newInstallAgeTestReporter(t, postHogTestStart)
	require.NoError(reporter.Capture("daemon_started", nil))

	DisablePostHogTelemetry()
	clock.Advance(postHogInstallHoldPeriod)
	timers.last(t).fire()
	require.NoError(reporter.Close())

	assert.Empty(t, client.messages)
	assert.True(t, client.closed)
}

func TestPostHogReporterRealTimerReleasesHeldEvents(t *testing.T) {
	enablePostHogTelemetryForTest()
	t.Cleanup(enablePostHogTelemetryForTest)
	t.Setenv(GenericTelemetryEnabledEnv, "1")
	t.Setenv("KATA_TELEMETRY_ENABLED", "1")

	client := &recordingPostHogClient{}
	reporter, err := newPostHogReporter(PostHogOptions{
		APIKey:      "caller-owned-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		InstalledAt: time.Now().Add(-postHogInstallHoldPeriod + 50*time.Millisecond),
	}, func(string, posthog.Config) (postHogEnqueueCloser, error) {
		return client, nil
	}, testAllowedTelemetryOptions()...)
	require.NoError(t, err)

	require.NoError(t, reporter.Capture("daemon_started", nil))
	assert.Eventually(t, func() bool { return client.count() == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, reporter.Capture("daemon_active", nil))
	require.NoError(t, reporter.Close())
	assert.Equal(t, 2, client.count())
}

type failingPostHogClient struct {
	enqueueErr error
	closeErr   error
}

func (c failingPostHogClient) Enqueue(posthog.Message) error { return c.enqueueErr }

func (c failingPostHogClient) Close() error { return c.closeErr }

func TestPostHogReporterReturnsClientErrorsUnwrappedWithoutHeldEvents(t *testing.T) {
	enqueueErr := errors.New("enqueue failed")
	closeErr := errors.New("close failed")
	reporter := &PostHogReporter{
		client:        failingPostHogClient{enqueueErr: enqueueErr, closeErr: closeErr},
		distinctID:    "anonymous-instance-id",
		application:   "kata",
		allowedEvents: testAllowedTelemetryEvents(),
		enabled:       true,
	}

	assert.Equal(t, enqueueErr, reporter.Capture("daemon_active", nil))
	assert.Equal(t, closeErr, reporter.Close())
}
