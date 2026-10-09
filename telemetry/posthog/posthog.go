package posthog

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	phsdk "github.com/posthog/posthog-go"
)

const (
	// DefaultEndpoint is PostHog's US ingest endpoint.
	DefaultEndpoint = "https://us.i.posthog.com"
	// GenericEnabledEnv disables telemetry for callers that honor the
	// conventional unprefixed variable.
	GenericEnabledEnv = "TELEMETRY_ENABLED"

	// ShutdownTimeout bounds how long Reporter.Close waits for queued events,
	// so a daemon without network exits instead of waiting through retries.
	ShutdownTimeout = 2 * time.Second

	// postHogInstallAgeProperty holds whole hours since Options.InstalledAt.
	postHogInstallAgeProperty = "install_age_hours"
)

// ErrUnsupportedEvent is returned when an event is not in a reporter's
// event allowlist.
var ErrUnsupportedEvent = errors.New("unsupported telemetry event")

// ErrInvalidProperty marks a missing or rejected required event property.
var ErrInvalidProperty = errors.New("invalid telemetry property")

var postHogTelemetryDisabled atomic.Bool

// PropertyFilter validates and returns a safe event property value.
type PropertyFilter func(any) (any, bool)

// AllowedProperty configures one safe property for an allowed event.
type AllowedProperty struct {
	name     string
	filter   PropertyFilter
	required bool
}

// Option customizes a PostHog telemetry reporter.
type Option interface {
	applyPostHogOption(*postHogReporterConfig)
}

// Client is the daemon-facing telemetry reporter contract.
type Client interface {
	Capture(event string, properties map[string]any) error
	Close() error
	Enabled() bool
}

// Options configures a PostHog telemetry reporter.
type Options struct {
	// Logger receives SDK logs. Nil uses slog.Default().
	Logger *slog.Logger
	// APIKey is the PostHog project API key. It is a public ingest identifier,
	// but callers must still pass it explicitly so kit never embeds app keys.
	APIKey string
	// Endpoint defaults to DefaultEndpoint when empty.
	Endpoint string
	// Application is included on every event and cannot be overridden by Capture.
	Application string
	// EnvPrefix enables PREFIX_TELEMETRY_ENABLED opt-out handling. The generic
	// TELEMETRY_ENABLED variable is also honored. See EnabledFromEnv.
	EnvPrefix string
	// DistinctID must be an anonymous stable installation or instance ID.
	DistinctID string
	Version    string
	Commit     string
	// Source defaults to "daemon" when empty.
	Source string
	// InstalledAt is when DistinctID was first created, usually persisted
	// beside it. When set, every event carries install_age_hours, the whole
	// hours from InstalledAt to the capture (0 when InstalledAt is in the
	// future), so reports can filter young installs such as sandboxes and test
	// harnesses. Zero omits the property.
	InstalledAt time.Time
}

// Reporter sanitizes and submits anonymous telemetry events to PostHog.
type Reporter struct {
	mu             sync.Mutex
	client         postHogEnqueueCloser
	distinctID     string
	version        string
	commit         string
	application    string
	source         string
	allowedEvents  map[string]map[string]PropertyFilter
	requiredEvents map[string]map[string]bool
	enabled        bool
	now            func() time.Time
	installedAt    time.Time
}

type postHogEnqueueCloser interface {
	Enqueue(phsdk.Message) error
	Close() error
}

type postHogClientFactory func(apiKey string, config phsdk.Config) (postHogEnqueueCloser, error)

type postHogReporterConfig struct {
	allowedEvents  map[string]map[string]PropertyFilter
	requiredEvents map[string]map[string]bool
	now            func() time.Time
}

type postHogOptionFunc func(*postHogReporterConfig)

func (f postHogOptionFunc) applyPostHogOption(config *postHogReporterConfig) {
	f(config)
}

// AllowProperty creates an allowlisted property entry for WithAllowedEvent.
func AllowProperty(name string, filter PropertyFilter) AllowedProperty {
	return AllowedProperty{
		name:   strings.TrimSpace(name),
		filter: filter,
	}
}

// RequireProperty rejects an event when name is missing or its filter rejects it.
// Its declaration requires a nonblank name and nonnil filter.
func RequireProperty(name string, filter PropertyFilter) AllowedProperty {
	property := AllowProperty(name, filter)
	property.required = true
	return property
}

// WithAllowedEvent allows event and the listed sanitized properties.
// Requiredness survives later declarations; the latest valid filter applies.
func WithAllowedEvent(event string, properties ...AllowedProperty) Option {
	return postHogOptionFunc(func(config *postHogReporterConfig) {
		if config == nil {
			return
		}
		event = strings.TrimSpace(event)
		if event == "" {
			return
		}
		if config.allowedEvents == nil {
			config.allowedEvents = map[string]map[string]PropertyFilter{}
		}
		allowedProperties := config.allowedEvents[event]
		if allowedProperties == nil {
			allowedProperties = map[string]PropertyFilter{}
			config.allowedEvents[event] = allowedProperties
		}
		for _, property := range properties {
			if property.required {
				if config.requiredEvents == nil {
					config.requiredEvents = make(map[string]map[string]bool)
				}
				if config.requiredEvents[event] == nil {
					config.requiredEvents[event] = make(map[string]bool)
				}
				valid, declared := config.requiredEvents[event][property.name]
				config.requiredEvents[event][property.name] = property.name != "" && property.filter != nil && (!declared || valid)
			}
			if property.name == "" || property.filter == nil {
				continue
			}
			allowedProperties[property.name] = property.filter
		}
	})
}

// EnabledFromEnv reports whether telemetry is enabled for envPrefix. Setting
// TELEMETRY_ENABLED or PREFIX_TELEMETRY_ENABLED to 0, false, no or off, in any
// case, turns it off.
func EnabledFromEnv(envPrefix string) bool {
	if ProcessDisabled() || optedOut(GenericEnabledEnv) {
		return false
	}
	if env := PrefixedEnabledEnv(envPrefix); env != "" {
		return !optedOut(env)
	}
	return true
}

func optedOut(env string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(env))) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

// PrefixedEnabledEnv returns the telemetry opt-out environment variable
// for prefix, such as KATA_TELEMETRY_ENABLED.
func PrefixedEnabledEnv(prefix string) string {
	prefix = strings.Trim(strings.ToUpper(strings.TrimSpace(prefix)), "_")
	if prefix == "" {
		return ""
	}
	return prefix + "_TELEMETRY_ENABLED"
}

// DisableProcess disables PostHog telemetry for this process.
//
// Callers can use this from their own build-tagged file, for example:
//
//	//go:build myapp_test
//	package myapp
//
//	import "go.kenn.io/kit/telemetry/posthog"
//
//	func init() {
//		posthog.DisableProcess()
//	}
func DisableProcess() {
	postHogTelemetryDisabled.Store(true)
}

// ProcessDisabled reports whether telemetry was disabled for this process.
func ProcessDisabled() bool {
	return postHogTelemetryDisabled.Load()
}

// NewReporter builds an enabled reporter or returns a disabled reporter
// when telemetry is opted out by build tag or environment variable. The
// disabled reporter keeps its configured events, so a capture handler can still
// reject unknown ones.
func NewReporter(opts Options, options ...Option) (*Reporter, error) {
	return newPostHogReporter(opts, func(apiKey string, config phsdk.Config) (postHogEnqueueCloser, error) {
		return phsdk.NewWithConfig(apiKey, config)
	}, options...)
}

func newPostHogReporter(opts Options, newClient postHogClientFactory, options ...Option) (*Reporter, error) {
	config := postHogReporterConfig{}
	for _, option := range options {
		if option != nil {
			option.applyPostHogOption(&config)
		}
	}
	for event, properties := range config.requiredEvents {
		for property, valid := range properties {
			if !valid {
				return nil, fmt.Errorf("required telemetry property %q for %q needs a nonblank name and nonnil filter", property, event)
			}
		}
	}
	allowedEvents := cloneAllowedTelemetryEvents(config.allowedEvents)
	if !EnabledFromEnv(opts.EnvPrefix) {
		return &Reporter{allowedEvents: allowedEvents, requiredEvents: config.requiredEvents}, nil
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

	if len(allowedEvents) == 0 {
		return nil, errors.New("telemetry allowed events are required")
	}

	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	disableGeoIP := true
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client, err := newClient(strings.TrimSpace(opts.APIKey), phsdk.Config{
		Logger:       sdkLogger{logger.With("component", "posthog")},
		Endpoint:     endpoint,
		DisableGeoIP: &disableGeoIP,
		// Reporters run in CLIs and daemons on user machines, not servers.
		IsServer:        new(false),
		Transport:       postHogDisableTransport{},
		ShutdownTimeout: ShutdownTimeout,
	})
	if err != nil {
		return nil, err
	}

	return &Reporter{
		client:         client,
		distinctID:     strings.TrimSpace(opts.DistinctID),
		version:        opts.Version,
		commit:         opts.Commit,
		application:    strings.TrimSpace(opts.Application),
		source:         defaultString(strings.TrimSpace(opts.Source), "daemon"),
		allowedEvents:  allowedEvents,
		requiredEvents: config.requiredEvents,
		enabled:        true,
		now:            config.now,
		installedAt:    opts.InstalledAt,
	}, nil
}

// DisabledReporter returns a reporter that drops events without network calls.
func DisabledReporter() *Reporter {
	return &Reporter{}
}

// Enabled reports whether the reporter can submit telemetry events.
func (r *Reporter) Enabled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLocked() && !ProcessDisabled()
}

// EventAllowed reports whether event is included in the reporter's allowlist.
func (r *Reporter) EventAllowed(event string) bool {
	if r == nil {
		return false
	}
	_, ok := r.allowedEvents[strings.TrimSpace(event)]
	return ok
}

// SanitizeProperties returns only allowlisted properties for event.
func (r *Reporter) SanitizeProperties(event string, properties map[string]any) (map[string]any, error) {
	if r == nil {
		return nil, ErrUnsupportedEvent
	}
	allowedProperties, ok := r.allowedEvents[strings.TrimSpace(event)]
	if !ok {
		return nil, ErrUnsupportedEvent
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
	for key := range r.requiredEvents[strings.TrimSpace(event)] {
		if _, present := safeProperties[key]; !present {
			return nil, fmt.Errorf("%w: %s", ErrInvalidProperty, key)
		}
	}
	r.addDefaultProperties(safeProperties)
	return safeProperties, nil
}

// Capture sanitizes and queues an anonymous telemetry event. When
// Options.InstalledAt is set, the event carries install_age_hours.
func (r *Reporter) Capture(event string, properties map[string]any) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.activeLocked() || ProcessDisabled() {
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

	now := r.clock()
	if !r.installedAt.IsZero() {
		// A future InstalledAt means the clock moved back; count the install as new.
		props[postHogInstallAgeProperty] = int64(max(now.Sub(r.installedAt), 0) / time.Hour)
	}
	return r.client.Enqueue(phsdk.Capture{
		DistinctId: r.distinctID,
		Event:      event,
		Timestamp:  now.UTC(),
		Properties: phsdk.Properties(props),
	})
}

// Close stops the underlying telemetry client when the reporter is enabled.
// It waits at most ShutdownTimeout for queued events and returns an error
// when it gives up on them. Reporter-created PostHog clients use a process-disable-aware transport, so
// Close can drain the SDK locally without network sends after telemetry is
// disabled for the process.
func (r *Reporter) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.activeLocked() {
		return nil
	}
	// The SDK refuses a second Close, so a timed-out reporter is done too.
	err := r.client.Close()
	r.deactivateLocked()
	return err
}

func (r *Reporter) activeLocked() bool {
	return r.enabled && r.client != nil
}

func (r *Reporter) deactivateLocked() {
	r.enabled = false
	r.client = nil
}

func (r *Reporter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

type postHogDisableTransport struct {
	base http.RoundTripper
}

func (t postHogDisableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if ProcessDisabled() {
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

func (r *Reporter) addDefaultProperties(props map[string]any) {
	props["$process_person_profile"] = false
	props["$geoip_disable"] = true
	props["application"] = r.application
	props["source"] = r.source
	props["version"] = r.version
	props["commit"] = r.commit
	props["goos"] = runtime.GOOS
	props["goarch"] = runtime.GOARCH
	// The install age is reporter-owned; Capture sets it from InstalledAt.
	delete(props, postHogInstallAgeProperty)
}

// AllowNumber accepts finite numeric telemetry values.
func AllowNumber(value any) (any, bool) {
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

// AllowBool accepts boolean telemetry values.
func AllowBool(value any) (any, bool) {
	v, ok := value.(bool)
	return v, ok
}

// AllowStringValues accepts only the listed string values.
func AllowStringValues(values ...string) PropertyFilter {
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

func cloneAllowedTelemetryEvents(events map[string]map[string]PropertyFilter) map[string]map[string]PropertyFilter {
	cloned := make(map[string]map[string]PropertyFilter, len(events))
	for event, properties := range events {
		event = strings.TrimSpace(event)
		if event == "" {
			continue
		}
		clonedProperties := map[string]PropertyFilter{}
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
