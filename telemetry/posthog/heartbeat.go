package posthog

import (
	"context"
	"log/slog"
	"time"
)

const (
	// EventDaemonActive is the event RunHeartbeat sends.
	EventDaemonActive = "daemon_active"
	// HeartbeatInterval is how often RunHeartbeat repeats EventDaemonActive.
	HeartbeatInterval = 24 * time.Hour
)

// RunHeartbeat sends EventDaemonActive through client at once and then every
// HeartbeatInterval until ctx ends. The client must allow the event. Capture
// errors are logged to logger, or slog.Default when logger is nil.
func RunHeartbeat(ctx context.Context, client Client, logger *slog.Logger) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	runHeartbeat(ctx, client, ticker.C, logger)
}

func runHeartbeat(ctx context.Context, client Client, ticks <-chan time.Time, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	for {
		if err := client.Capture(EventDaemonActive, nil); err != nil {
			logger.Warn("telemetry heartbeat failed", "event", EventDaemonActive, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}
