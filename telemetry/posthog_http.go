package telemetry

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// maxPostHogCaptureBodyBytes bounds a capture request body. UI events carry a
// name and a few short properties, far below this.
const maxPostHogCaptureBodyBytes = 64 << 10

type postHogCaptureRequest struct {
	Event      string         `json:"event"`
	Properties map[string]any `json:"properties"`
}

type postHogCaptureResponse struct {
	Status string `json:"status"`
}

// NewPostHogCaptureHandler returns a handler that lets an application's own UI
// report events through reporter, so the browser holds no analytics key and
// loads no provider script. It accepts POST {"event": "...", "properties": {...}}
// as application/json, which makes browsers preflight cross-origin posts. The
// reporter's allowlist decides whether the event is accepted, its filters
// decide which properties are sent, and it stamps identity, version and install
// age. JSON numbers arrive as float64.
//
// Responses: 202 {"status":"queued"} when the reporter accepted the capture;
// 202 {"status":"disabled"} when the event is allowed but telemetry is opted
// out, disabled for the process or the reporter is closed, so nothing is sent;
// 400 for a malformed body, data after the JSON object, a blank event or an
// event the allowlist omits, in every reporter state; 405 for other methods;
// 413 for a body over 64 KiB; 415 for a content type other than
// application/json; 500 when Capture fails. The status is best effort: a
// reporter closed or disabled between the enabled check and Capture answers
// queued while Capture's own guard sends nothing.
//
// A nil reporter or DisabledPostHogReporter admits no event. Construct an
// opted-out reporter with the same WithAllowedEvent options as an enabled one.
// Callers mount the handler on their own router, inside the authentication
// that router already applies to UI routes.
func NewPostHogCaptureHandler(reporter *PostHogReporter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "telemetry request must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		req, err := decodePostHogCaptureRequest(w, r)
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			http.Error(w, "telemetry request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "invalid telemetry request", http.StatusBadRequest)
			return
		}
		if !reporter.EventAllowed(req.Event) {
			http.Error(w, ErrUnsupportedTelemetryEvent.Error(), http.StatusBadRequest)
			return
		}
		if !reporter.Enabled() {
			writePostHogCaptureStatus(w, "disabled")
			return
		}
		if err := reporter.Capture(req.Event, req.Properties); err != nil {
			http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
			return
		}
		writePostHogCaptureStatus(w, "queued")
	})
}

func decodePostHogCaptureRequest(
	w http.ResponseWriter, r *http.Request,
) (postHogCaptureRequest, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPostHogCaptureBodyBytes))
	var req postHogCaptureRequest
	if err := decoder.Decode(&req); err != nil {
		return req, err
	}
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return req, nil
	}
	if err == nil {
		err = errors.New("unexpected data after telemetry request")
	}
	return req, err
}

func writePostHogCaptureStatus(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(postHogCaptureResponse{Status: status}) //nolint:errchkjson // the 202 is already sent, so a failed body write has nowhere to go
}
