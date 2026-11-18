package posthog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	phsdk "github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsentReadWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err := ReadConsent()
	require.NoError(t, err)
	assert.Equal(t, ConsentUnanswered, got)
	for _, tc := range []struct {
		data string
		want Consent
	}{{"yes\n", ConsentGranted}, {"no\n", ConsentDenied}, {"yes\r\n", ConsentGranted}, {"garbage", ConsentUnanswered}} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".kenn"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".kenn", "telemetry-consent"), []byte(tc.data), 0o600))
		got, err = ReadConsent()
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	for _, tc := range []struct {
		granted bool
		want    Consent
		data    string
	}{{true, ConsentGranted, "yes\n"}, {false, ConsentDenied, "no\n"}} {
		require.NoError(t, RecordConsent(tc.granted))
		got, err = ReadConsent()
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
		data, err := os.ReadFile(filepath.Join(home, ".kenn", "telemetry-consent"))
		require.NoError(t, err)
		assert.Equal(t, tc.data, string(data))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(home, ".kenn", "telemetry-consent"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	require.NoError(t, os.Remove(filepath.Join(home, ".kenn", "telemetry-consent")))
	require.NoError(t, os.Mkdir(filepath.Join(home, ".kenn", "telemetry-consent"), 0o700))
	_, err = ReadConsent()
	require.Error(t, err)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".kenn"), []byte("file"), 0o600))
	require.Error(t, RecordConsent(true))
}

func TestConsentReporter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	client := &fakePostHogClient{}
	newReporter := func() *Reporter {
		r, err := newPostHogReporter(Options{APIKey: "test", Application: "test", EnvPrefix: "KATA", DistinctID: "id", Version: "1.2.3"}, func(string, phsdk.Config) (postHogEnqueueCloser, error) { return client, nil }, WithAllowedEvent("event"))
		require.NoError(t, err)
		return r
	}
	r := newReporter()
	require.NoError(t, r.Capture("event", nil))
	assert.Nil(t, client.message)
	require.NoError(t, r.Close())

	require.NoError(t, RecordConsent(true))
	r = newReporter()
	require.NoError(t, r.Capture("event", nil))
	capture, ok := client.message.(phsdk.Capture)
	require.True(t, ok)
	assert.Equal(t, true, capture.Properties["telemetry_consent"])
	require.NoError(t, r.Close())
	require.NoError(t, RecordConsent(false))
	assert.False(t, Eligible("KATA", "1.2.3"))
}

func TestConsentAcrossProcesses(t *testing.T) {
	if os.Getenv("CONSENT_CHILD") == "1" {
		client := &fakePostHogClient{}
		r, err := newPostHogReporter(Options{APIKey: "test", Application: "other", EnvPrefix: "OTHER", DistinctID: "id", Version: "1.2.3"}, func(string, phsdk.Config) (postHogEnqueueCloser, error) { return client, nil }, WithAllowedEvent("event"))
		require.NoError(t, err)
		require.NoError(t, r.Capture("event", nil))
		if os.Getenv("CONSENT_EXPECT") == "yes" {
			require.NotNil(t, client.message)
		} else {
			assert.Nil(t, client.message)
		}
		require.NoError(t, r.Close())
		return
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct {
		yes      bool
		expected string
	}{{true, "yes"}, {false, "no"}} {
		require.NoError(t, RecordConsent(tc.yes))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConsentAcrossProcesses$")
		cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "TELEMETRY_ENABLED=", "OTHER_TELEMETRY_ENABLED=", "CONSENT_CHILD=1", "CONSENT_EXPECT="+tc.expected)
		out, err := cmd.CombinedOutput()
		cancel()
		require.NoError(t, err, string(out))
	}
}
