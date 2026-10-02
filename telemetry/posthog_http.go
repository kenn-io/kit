package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type postHogCaptureRequest struct {
	Event      string         `json:"event"`
	Properties map[string]any `json:"properties"`
}

type postHogCaptureResponse struct {
	Status string `json:"status"`
}

// NewPostHogCaptureHandler returns a handler that lets an application's own UI
// report events through reporter, so the browser holds no analytics key and
// loads no provider script. It accepts POST {"event": "...", "properties": {...}}.
// The reporter's allowlist decides whether the event is accepted, its filters
// decide which properties are sent, and it stamps identity, version and install
// age. JSON numbers arrive as float64.
//
// Responses: 202 {"status":"queued"} when the reporter accepted the capture;
// 202 {"status":"disabled"} when the event is allowed but telemetry is opted
// out, disabled for the process or the reporter is closed, so nothing is sent;
// 400 for a malformed body, a blank event or an event the allowlist omits, in
// every reporter state; 405 for other methods; 500 when Capture fails. The
// status is best effort: a reporter closed or disabled between the enabled
// check and Capture answers queued while Capture's own guard sends nothing.
//
// A nil reporter or DisabledPostHogReporter admits no event. Construct an
// opted-out reporter with the same WithAllowedEvent options as an enabled one.
// Callers mount the handler on their own router, inside the authentication and
// request-size limits that router already applies to UI routes.
func NewPostHogCaptureHandler(reporter *PostHogReporter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req postHogCaptureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid telemetry request", http.StatusBadRequest)
			return
		}
		event := strings.TrimSpace(req.Event)
		if event == "" || !reporter.EventAllowed(event) {
			http.Error(w, ErrUnsupportedTelemetryEvent.Error(), http.StatusBadRequest)
			return
		}
		if !reporter.Enabled() {
			writePostHogCaptureStatus(w, "disabled")
			return
		}
		if err := reporter.Capture(event, req.Properties); err != nil {
			if errors.Is(err, ErrUnsupportedTelemetryEvent) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
			return
		}
		writePostHogCaptureStatus(w, "queued")
	})
}

func writePostHogCaptureStatus(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(postHogCaptureResponse{Status: status}) //nolint:errchkjson // the 202 is already sent, so a failed body write has nowhere to go
}
