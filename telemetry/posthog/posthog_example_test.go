package posthog_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"time"
	"uuid"

	"go.kenn.io/kit/telemetry/posthog"
)

func ExampleSendGroup() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintln(w, `{"status":"queued"}`)
	}))
	defer server.Close()

	var sends posthog.SendGroup
	sends.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := posthog.PostEvent(ctx, server.Client(), server.URL, "screen_viewed", nil)
		if err != nil {
			slog.Debug("telemetry report failed", "error", err)
		}
	})

	// Run command work here, then bound the remaining wait at exit.
	sends.Wait(50 * time.Millisecond)
	// Output:
}

//nolint:testableexamples // Compile-only usage with a caller-configured reporter and persisted identity.
func ExampleReporter_CaptureConfirmed() {
	var reporter *posthog.Reporter                                 // Construct with a stable DistinctID and event allowlist.
	delivery := posthog.Delivery{ID: uuid.New(), Time: time.Now()} // Persist before sending; reload on retry.
	status, err := reporter.CaptureConfirmed(context.Background(), "milestone", nil, delivery)
	if err == nil && status == posthog.StatusDelivered {
		// Persist completion; other results leave the tuple pending for retry.
		fmt.Println("delivered")
	}
}

func ExampleReporter_Capture_daemonActive() {
	if err := captureDaemonActive(); err != nil {
		fmt.Println("error:", err)
	}
	// Output:
}

// captureDaemonActive shows the reporter lifecycle: construct with an event
// allowlist, capture, and close. It returns errors so the example can stay a
// plain function without process exits after deferred cleanup.
func captureDaemonActive() error {
	// Examples disable telemetry so `go test` never submits events.
	// Real callers should omit this when telemetry is allowed.
	if err := os.Setenv("KATA_TELEMETRY_ENABLED", "0"); err != nil {
		return err
	}
	defer func() { _ = os.Unsetenv("KATA_TELEMETRY_ENABLED") }()

	// Real callers load this from the state file that stores DistinctID.
	installedAt := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.UTC)

	reporter, err := posthog.NewReporter(posthog.Options{
		APIKey:      "caller-owned-posthog-project-api-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		// Persist this beside DistinctID when the ID is first created; each
		// event then carries install_age_hours so reports can filter young
		// installs.
		InstalledAt: installedAt,
		Version:     "v1.2.3",
		Commit:      "abc1234",
	}, posthog.WithAllowedEvent("daemon_active",
		posthog.AllowProperty("project_count", posthog.AllowNumber),
	))
	if err != nil {
		return err
	}
	if err := reporter.Capture("daemon_active", map[string]any{
		"project_count": 3,
	}); err != nil {
		return errors.Join(err, reporter.Close())
	}
	return reporter.Close()
}
