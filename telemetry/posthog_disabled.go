//go:build kit_posthog_disabled

package telemetry

func init() {
	DisablePostHogTelemetry()
}
