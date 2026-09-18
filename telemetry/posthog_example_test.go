package telemetry_test

import (
	"errors"
	"fmt"
	"os"

	"go.kenn.io/kit/telemetry"
)

func ExamplePostHogReporter_Capture_daemonActive() {
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

	reporter, err := telemetry.NewPostHogReporter(telemetry.PostHogOptions{
		APIKey:      "caller-owned-posthog-project-api-key",
		Application: "kata",
		EnvPrefix:   "KATA",
		DistinctID:  "anonymous-instance-id",
		Version:     "v1.2.3",
		Commit:      "abc1234",
	}, telemetry.WithAllowedEvent("daemon_active",
		telemetry.AllowTelemetryProperty("project_count", telemetry.AllowTelemetryNumber),
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
