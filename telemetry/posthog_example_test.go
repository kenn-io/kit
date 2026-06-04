package telemetry_test

import (
	"log"
	"os"

	"go.kenn.io/kit/telemetry"
)

func ExamplePostHogReporter_Capture_daemonActive() {
	// Examples disable telemetry so `go test` never submits events.
	// Real callers should omit this when telemetry is allowed.
	if err := os.Setenv("KATA_TELEMETRY_ENABLED", "0"); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := os.Unsetenv("KATA_TELEMETRY_ENABLED"); err != nil {
			log.Fatal(err)
		}
	}()

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
		log.Fatal(err)
	}
	defer func() {
		if err := reporter.Close(); err != nil {
			log.Fatal(err)
		}
	}()

	if err := reporter.Capture("daemon_active", map[string]any{
		"project_count": 3,
	}); err != nil {
		log.Fatal(err)
	}
}
