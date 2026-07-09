package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestInitAppliesNativeOptionsAndRegistersGlobals(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_METRICS_EXPORTER", "")
	t.Setenv("OTEL_PROPAGATORS", "")

	spanRecorder := tracetest.NewSpanRecorder()
	metricReader := metric.NewManualReader()
	shutdown, err := Init(context.Background(),
		WithResourceOptions(resource.WithAttributes(attribute.String("test.resource", "configured"))),
		WithTracerProviderOptions(trace.WithSpanProcessor(spanRecorder)),
		WithMeterProviderOptions(metric.WithReader(metricReader)),
		WithPropagators(testPropagator{}),
	)
	require.NoError(t, err)
	require.NotNil(t, shutdown)
	t.Cleanup(func() {
		assert.NoError(t, shutdown(context.Background()))
	})

	_, span := otel.Tracer("go.kenn.io/kit/telemetry/init_test").Start(context.Background(), "test")
	span.End()
	spans := spanRecorder.Ended()
	require.Len(t, spans, 1)
	value, ok := spans[0].Resource().Set().Value("test.resource")
	require.True(t, ok)
	assert.Equal(t, "configured", value.AsString())

	counter, err := otel.Meter("go.kenn.io/kit/telemetry/init_test").Int64Counter("test.counter")
	require.NoError(t, err)
	counter.Add(context.Background(), 1)

	var metrics metricdata.ResourceMetrics
	require.NoError(t, metricReader.Collect(context.Background(), &metrics))
	require.NotEmpty(t, metrics.ScopeMetrics)
	assert.Equal(t, []string{"test-propagator"}, otel.GetTextMapPropagator().Fields())
}

func TestDefaultExporterFactoriesDisableEmptySelectors(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_METRICS_EXPORTER", "")

	spanExporter, err := newSpanExporterFromEnv(context.Background())
	require.NoError(t, err)
	assert.Nil(t, spanExporter)

	metricReader, err := newMetricReaderFromEnv(context.Background())
	require.NoError(t, err)
	assert.Nil(t, metricReader)
}

func TestDefaultExporterFactoriesDisableExplicitNone(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")

	spanExporter, err := newSpanExporterFromEnv(context.Background())
	require.NoError(t, err)
	assert.Nil(t, spanExporter)

	metricReader, err := newMetricReaderFromEnv(context.Background())
	require.NoError(t, err)
	assert.Nil(t, metricReader)
}

func TestInitCleansUpSpanExporterWhenMetricInitializationFails(t *testing.T) {
	initErr := errors.New("metric initialization failed")
	cleanupErr := errors.New("span exporter cleanup failed")
	exporter := &testSpanExporter{shutdownErr: cleanupErr}
	registrations := 0

	shutdown, err := initWithDependencies(context.Background(), initDependencies{
		newSpanExporter: func(context.Context) (trace.SpanExporter, error) {
			return exporter, nil
		},
		newMetricReader: func(context.Context) (metric.Reader, error) {
			return nil, initErr
		},
		setTracerProvider: func(oteltrace.TracerProvider) {
			registrations++
		},
		setMeterProvider: func(otelmetric.MeterProvider) {
			registrations++
		},
		setTextMapPropagator: func(propagation.TextMapPropagator) {
			registrations++
		},
	})

	assert.Nil(t, shutdown)
	require.ErrorIs(t, err, initErr)
	require.ErrorIs(t, err, cleanupErr)
	assert.Equal(t, 1, exporter.shutdownCalls)
	assert.Zero(t, registrations)
}

func TestShutdownAllAttemptsEveryFunctionAndJoinsErrors(t *testing.T) {
	meterErr := errors.New("meter shutdown failed")
	tracerErr := errors.New("tracer shutdown failed")
	calls := []string{}

	err := shutdownAll(context.Background(),
		func(context.Context) error {
			calls = append(calls, "meter")
			return meterErr
		},
		func(context.Context) error {
			calls = append(calls, "tracer")
			return tracerErr
		},
	)

	assert.Equal(t, []string{"meter", "tracer"}, calls)
	require.ErrorIs(t, err, meterErr)
	require.ErrorIs(t, err, tracerErr)
}

type testPropagator struct {
	propagation.TraceContext
}

func (testPropagator) Fields() []string {
	return []string{"test-propagator"}
}

type testSpanExporter struct {
	shutdownCalls int
	shutdownErr   error
}

func (*testSpanExporter) ExportSpans(context.Context, []trace.ReadOnlySpan) error {
	return nil
}

func (e *testSpanExporter) Shutdown(context.Context) error {
	e.shutdownCalls++
	return e.shutdownErr
}
