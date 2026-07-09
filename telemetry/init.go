package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Option customizes OpenTelemetry initialization.
type Option interface {
	apply(*initConfig)
}

type optionFunc func(*initConfig)

func (f optionFunc) apply(config *initConfig) {
	f(config)
}

type initConfig struct {
	resourceOptions       []resource.Option
	tracerProviderOptions []trace.TracerProviderOption
	meterProviderOptions  []metric.Option
	propagators           []propagation.TextMapPropagator
}

type initDependencies struct {
	newSpanExporter      func(context.Context) (trace.SpanExporter, error)
	newMetricReader      func(context.Context) (metric.Reader, error)
	setTracerProvider    func(oteltrace.TracerProvider)
	setMeterProvider     func(otelmetric.MeterProvider)
	setTextMapPropagator func(propagation.TextMapPropagator)
}

var defaultInitDependencies = initDependencies{
	newSpanExporter:      newSpanExporterFromEnv,
	newMetricReader:      newMetricReaderFromEnv,
	setTracerProvider:    otel.SetTracerProvider,
	setMeterProvider:     otel.SetMeterProvider,
	setTextMapPropagator: otel.SetTextMapPropagator,
}

// WithResourceOptions adds resource options to the resource shared by the
// tracer and meter providers.
func WithResourceOptions(options ...resource.Option) Option {
	return optionFunc(func(config *initConfig) {
		config.resourceOptions = append(config.resourceOptions, options...)
	})
}

// WithTracerProviderOptions adds options to the tracer provider.
func WithTracerProviderOptions(options ...trace.TracerProviderOption) Option {
	return optionFunc(func(config *initConfig) {
		config.tracerProviderOptions = append(config.tracerProviderOptions, options...)
	})
}

// WithMeterProviderOptions adds options to the meter provider.
func WithMeterProviderOptions(options ...metric.Option) Option {
	return optionFunc(func(config *initConfig) {
		config.meterProviderOptions = append(config.meterProviderOptions, options...)
	})
}

// WithPropagators sets the propagators used when OTEL_PROPAGATORS is unset.
func WithPropagators(propagators ...propagation.TextMapPropagator) Option {
	return optionFunc(func(config *initConfig) {
		config.propagators = append(config.propagators, propagators...)
	})
}

// Init configures and globally registers OpenTelemetry tracing, metrics, and
// context propagation. Trace and metric export default to disabled; set
// OTEL_TRACES_EXPORTER or OTEL_METRICS_EXPORTER to enable an exporter.
func Init(ctx context.Context, options ...Option) (func(context.Context) error, error) {
	return initWithDependencies(ctx, defaultInitDependencies, options...)
}

func initWithDependencies(
	ctx context.Context,
	dependencies initDependencies,
	options ...Option,
) (func(context.Context) error, error) {
	config := initConfig{}
	for _, option := range options {
		if option != nil {
			option.apply(&config)
		}
	}

	configuredResource, err := newResource(ctx, config.resourceOptions...)
	if err != nil {
		return nil, fmt.Errorf("initialize OpenTelemetry resource: %w", err)
	}

	spanExporter, err := dependencies.newSpanExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize OpenTelemetry trace exporter: %w", err)
	}
	metricReader, err := dependencies.newMetricReader(ctx)
	if err != nil {
		initErr := fmt.Errorf("initialize OpenTelemetry metric reader: %w", err)
		if spanExporter == nil {
			return nil, initErr
		}
		cleanupErr := spanExporter.Shutdown(ctx)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("shutdown OpenTelemetry trace exporter: %w", cleanupErr)
		}
		return nil, errors.Join(initErr, cleanupErr)
	}

	tracerOptions := []trace.TracerProviderOption{trace.WithResource(configuredResource)}
	if spanExporter != nil {
		tracerOptions = append(tracerOptions, trace.WithBatcher(spanExporter))
	}
	tracerOptions = append(tracerOptions, config.tracerProviderOptions...)
	tracerProvider := trace.NewTracerProvider(tracerOptions...)

	meterOptions := []metric.Option{metric.WithResource(configuredResource)}
	if metricReader != nil {
		meterOptions = append(meterOptions, metric.WithReader(metricReader))
	}
	meterOptions = append(meterOptions, config.meterProviderOptions...)
	meterProvider := metric.NewMeterProvider(meterOptions...)

	dependencies.setTracerProvider(tracerProvider)
	dependencies.setMeterProvider(meterProvider)
	dependencies.setTextMapPropagator(autoprop.NewTextMapPropagator(config.propagators...))

	return func(ctx context.Context) error {
		return shutdownAll(ctx, meterProvider.Shutdown, tracerProvider.Shutdown)
	}, nil
}

func newResource(ctx context.Context, options ...resource.Option) (*resource.Resource, error) {
	options = append([]resource.Option{resource.WithFromEnv()}, options...)
	configuredResource, err := resource.New(ctx, options...)
	if err != nil {
		return nil, err
	}
	return resource.Merge(resource.Default(), configuredResource)
}

func shutdownAll(ctx context.Context, shutdowns ...func(context.Context) error) error {
	errs := make([]error, 0, len(shutdowns))
	for _, shutdown := range shutdowns {
		errs = append(errs, shutdown(ctx))
	}
	return errors.Join(errs...)
}

func newSpanExporterFromEnv(ctx context.Context) (trace.SpanExporter, error) {
	exporter, err := autoexport.NewSpanExporter(ctx, autoexport.WithFallbackSpanExporter(
		func(context.Context) (trace.SpanExporter, error) {
			return nil, nil
		},
	))
	if err != nil || autoexport.IsNoneSpanExporter(exporter) {
		return nil, err
	}
	return exporter, nil
}

func newMetricReaderFromEnv(ctx context.Context) (metric.Reader, error) {
	reader, err := autoexport.NewMetricReader(ctx, autoexport.WithFallbackMetricReader(
		func(context.Context) (metric.Reader, error) {
			return nil, nil
		},
	))
	if err != nil || autoexport.IsNoneMetricReader(reader) {
		return nil, err
	}
	return reader, nil
}
