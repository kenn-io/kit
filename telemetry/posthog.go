package telemetry

import (
	"errors"
	"math"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/posthog/posthog-go"
)

const (
	// DefaultPostHogEndpoint is PostHog's US ingest endpoint.
	DefaultPostHogEndpoint = "https://us.i.posthog.com"
	// GenericTelemetryEnabledEnv disables telemetry for callers that honor the
	// conventional unprefixed variable.
	GenericTelemetryEnabledEnv = "TELEMETRY_ENABLED"
)

// ErrUnsupportedTelemetryEvent is returned when an event is not in a reporter's
// event allowlist.
var ErrUnsupportedTelemetryEvent = errors.New("unsupported telemetry event")

var postHogTelemetryDisabled atomic.Bool

// TelemetryPropertyFilter validates and returns a safe event property value.
type TelemetryPropertyFilter func(any) (any, bool)

// AllowedTelemetryProperty configures one safe property for an allowed event.
type AllowedTelemetryProperty struct {
	name   string
	filter TelemetryPropertyFilter
}

// PostHogOption customizes a PostHog telemetry reporter.
type PostHogOption interface {
	applyPostHogOption(*postHogReporterConfig)
}

// PostHogClient is the daemon-facing telemetry reporter contract.
type PostHogClient interface {
	Capture(event string, properties map[string]any) error
	Close() error
	Enabled() bool
}

// PostHogOptions configures a PostHog telemetry reporter.
type PostHogOptions struct {
	// APIKey is the PostHog project API key. It is a public ingest identifier,
	// but callers must still pass it explicitly so kit never embeds app keys.
	APIKey string
	// Endpoint defaults to DefaultPostHogEndpoint when empty.
	Endpoint string
	// Application is included on every event and cannot be overridden by Capture.
	Application string
	// EnvPrefix enables PREFIX_TELEMETRY_ENABLED=0 opt-out handling. The generic
	// TELEMETRY_ENABLED=0 variable is also honored.
	EnvPrefix string
	// DistinctID must be an anonymous stable installation or instance ID.
	DistinctID string
	Version    string
	Commit     string
	// Source defaults to "daemon" when empty.
	Source string
}

// PostHogReporter sanitizes and submits anonymous telemetry events to PostHog.
type PostHogReporter struct {
	mu            sync.Mutex
	client        postHogEnqueueCloser
	distinctID    string
	version       string
	commit        string
	application   string
	source        string
	allowedEvents map[string]map[string]TelemetryPropertyFilter
	enabled       bool
}

type postHogEnqueueCloser interface {
	Enqueue(posthog.Message) error
	Close() error
}

type postHogClientFactory func(apiKey string, config posthog.Config) (postHogEnqueueCloser, error)

type postHogReporterConfig struct {
	allowedEvents map[string]map[string]TelemetryPropertyFilter
}

type postHogOptionFunc func(*postHogReporterConfig)

func (f postHogOptionFunc) applyPostHogOption(config *postHogReporterConfig) {
	f(config)
}

// AllowTelemetryProperty creates an allowlisted property entry for WithAllowedEvent.
func AllowTelemetryProperty(name string, filter TelemetryPropertyFilter) AllowedTelemetryProperty {
	return AllowedTelemetryProperty{
		name:   strings.TrimSpace(name),
		filter: filter,
	}
}

// WithAllowedEvent allows event and the listed sanitized properties.
func WithAllowedEvent(event string, properties ...AllowedTelemetryProperty) PostHogOption {
	return postHogOptionFunc(func(config *postHogReporterConfig) {
		if config == nil {
			return
		}
		event = strings.TrimSpace(event)
		if event == "" {
			return
		}
		if config.allowedEvents == nil {
			config.allowedEvents = map[string]map[string]TelemetryPropertyFilter{}
		}
		allowedProperties := config.allowedEvents[event]
		if allowedProperties == nil {
			allowedProperties = map[string]TelemetryPropertyFilter{}
			config.allowedEvents[event] = allowedProperties
		}
		for _, property := range properties {
			if property.name == "" || property.filter == nil {
				continue
			}
			allowedProperties[property.name] = property.filter
		}
	})
}

// PostHogTelemetryEnabledFromEnv reports whether telemetry is enabled for envPrefix.
func PostHogTelemetryEnabledFromEnv(envPrefix string) bool {
	if PostHogTelemetryDisabled() {
		return false
	}
	if strings.TrimSpace(os.Getenv(GenericTelemetryEnabledEnv)) == "0" {
		return false
	}
	if env := PrefixedTelemetryEnabledEnv(envPrefix); env != "" {
		return strings.TrimSpace(os.Getenv(env)) != "0"
	}
	return true
}

// PrefixedTelemetryEnabledEnv returns the telemetry opt-out environment variable
// for prefix, such as KATA_TELEMETRY_ENABLED.
func PrefixedTelemetryEnabledEnv(prefix string) string {
	prefix = strings.Trim(strings.ToUpper(strings.TrimSpace(prefix)), "_")
	if prefix == "" {
		return ""
	}
	return prefix + "_TELEMETRY_ENABLED"
}

// DisablePostHogTelemetry disables PostHog telemetry for this process.
//
// Callers can use this from their own build-tagged file, for example:
//
//	//go:build kata_test
//	package telemetry
//
//	import kittelemetry "go.kenn.io/kit/telemetry"
//
//	func init() {
//		kittelemetry.DisablePostHogTelemetry()
//	}
func DisablePostHogTelemetry() {
	postHogTelemetryDisabled.Store(true)
}

// PostHogTelemetryDisabled reports whether telemetry was disabled for this process.
func PostHogTelemetryDisabled() bool {
	return postHogTelemetryDisabled.Load()
}

// NewPostHogReporter builds an enabled reporter or returns a disabled reporter
// when telemetry is opted out by build tag or environment variable.
func NewPostHogReporter(opts PostHogOptions, options ...PostHogOption) (*PostHogReporter, error) {
	return newPostHogReporter(opts, func(apiKey string, config posthog.Config) (postHogEnqueueCloser, error) {
		return posthog.NewWithConfig(apiKey, config)
	}, options...)
}

func newPostHogReporter(opts PostHogOptions, newClient postHogClientFactory, options ...PostHogOption) (*PostHogReporter, error) {
	if !PostHogTelemetryEnabledFromEnv(opts.EnvPrefix) {
		return DisabledPostHogReporter(), nil
	}
	if newClient == nil {
		return nil, errors.New("posthog client factory is required")
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("posthog api key is required")
	}
	if strings.TrimSpace(opts.Application) == "" {
		return nil, errors.New("telemetry application is required")
	}
	if strings.TrimSpace(opts.EnvPrefix) == "" {
		return nil, errors.New("telemetry env prefix is required")
	}
	if strings.TrimSpace(opts.DistinctID) == "" {
		return nil, errors.New("telemetry distinct id is required")
	}

	config := postHogReporterConfig{}
	for _, option := range options {
		if option != nil {
			option.applyPostHogOption(&config)
		}
	}
	allowedEvents := cloneAllowedTelemetryEvents(config.allowedEvents)
	if len(allowedEvents) == 0 {
		return nil, errors.New("telemetry allowed events are required")
	}

	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		endpoint = DefaultPostHogEndpoint
	}
	disableGeoIP := true
	client, err := newClient(strings.TrimSpace(opts.APIKey), posthog.Config{
		Endpoint:     endpoint,
		DisableGeoIP: &disableGeoIP,
		Transport:    postHogDisableTransport{},
	})
	if err != nil {
		return nil, err
	}

	return &PostHogReporter{
		client:        client,
		distinctID:    strings.TrimSpace(opts.DistinctID),
		version:       opts.Version,
		commit:        opts.Commit,
		application:   strings.TrimSpace(opts.Application),
		source:        defaultString(strings.TrimSpace(opts.Source), "daemon"),
		allowedEvents: allowedEvents,
		enabled:       true,
	}, nil
}

// DisabledPostHogReporter returns a reporter that drops events without network calls.
func DisabledPostHogReporter() *PostHogReporter {
	return &PostHogReporter{}
}

// Enabled reports whether the reporter can submit telemetry events.
func (r *PostHogReporter) Enabled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLocked() && !PostHogTelemetryDisabled()
}

// EventAllowed reports whether event is included in the reporter's allowlist.
func (r *PostHogReporter) EventAllowed(event string) bool {
	if r == nil {
		return false
	}
	_, ok := r.allowedEvents[strings.TrimSpace(event)]
	return ok
}

// SanitizeProperties returns only allowlisted properties for event.
func (r *PostHogReporter) SanitizeProperties(event string, properties map[string]any) (map[string]any, error) {
	if r == nil {
		return nil, ErrUnsupportedTelemetryEvent
	}
	allowedProperties, ok := r.allowedEvents[strings.TrimSpace(event)]
	if !ok {
		return nil, ErrUnsupportedTelemetryEvent
	}

	safeProperties := map[string]any{}
	for key, value := range properties {
		key = strings.TrimSpace(key)
		filter, ok := allowedProperties[key]
		if !ok {
			continue
		}
		if safeValue, ok := filter(value); ok {
			safeProperties[key] = safeValue
		}
	}
	r.addDefaultProperties(safeProperties)
	return safeProperties, nil
}

// Capture sanitizes and queues an anonymous telemetry event.
func (r *PostHogReporter) Capture(event string, properties map[string]any) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.activeLocked() || PostHogTelemetryDisabled() {
		return nil
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return errors.New("telemetry event is required")
	}

	props, err := r.SanitizeProperties(event, properties)
	if err != nil {
		return err
	}

	return r.client.Enqueue(posthog.Capture{
		DistinctId: r.distinctID,
		Event:      event,
		Timestamp:  time.Now().UTC(),
		Properties: posthog.Properties(props),
	})
}

// Close stops the underlying telemetry client when the reporter is enabled.
// Reporter-created PostHog clients use a process-disable-aware transport, so
// Close can drain the SDK locally without network sends after telemetry is
// disabled for the process.
func (r *PostHogReporter) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.activeLocked() {
		return nil
	}
	if err := r.client.Close(); err != nil {
		return err
	}
	r.deactivateLocked()
	return nil
}

func (r *PostHogReporter) activeLocked() bool {
	return r.enabled && r.client != nil
}

func (r *PostHogReporter) deactivateLocked() {
	r.enabled = false
	r.client = nil
}

type postHogDisableTransport struct {
	base http.RoundTripper
}

func (t postHogDisableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if PostHogTelemetryDisabled() {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 " + http.StatusText(http.StatusNoContent),
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func (r *PostHogReporter) addDefaultProperties(props map[string]any) {
	props["$process_person_profile"] = false
	props["$geoip_disable"] = true
	props["application"] = r.application
	props["source"] = r.source
	props["version"] = r.version
	props["commit"] = r.commit
	props["goos"] = runtime.GOOS
	props["goarch"] = runtime.GOARCH
}

// AllowTelemetryNumber accepts finite numeric telemetry values.
func AllowTelemetryNumber(value any) (any, bool) {
	switch v := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, true
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		return v, true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return v, true
	default:
		return nil, false
	}
}

// AllowTelemetryBool accepts boolean telemetry values.
func AllowTelemetryBool(value any) (any, bool) {
	v, ok := value.(bool)
	return v, ok
}

// AllowTelemetryStringValues accepts only the listed string values.
func AllowTelemetryStringValues(values ...string) TelemetryPropertyFilter {
	allowed := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			allowed[value] = struct{}{}
		}
	}
	return func(value any) (any, bool) {
		v, ok := value.(string)
		if !ok {
			return nil, false
		}
		v = strings.TrimSpace(v)
		if _, ok := allowed[v]; !ok {
			return nil, false
		}
		return v, true
	}
}

func cloneAllowedTelemetryEvents(events map[string]map[string]TelemetryPropertyFilter) map[string]map[string]TelemetryPropertyFilter {
	cloned := make(map[string]map[string]TelemetryPropertyFilter, len(events))
	for event, properties := range events {
		event = strings.TrimSpace(event)
		if event == "" {
			continue
		}
		clonedProperties := map[string]TelemetryPropertyFilter{}
		for property, filter := range properties {
			property = strings.TrimSpace(property)
			if property == "" || filter == nil {
				continue
			}
			clonedProperties[property] = filter
		}
		cloned[event] = clonedProperties
	}
	return cloned
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
