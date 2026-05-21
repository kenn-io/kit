package telemetry_test

import (
	"context"
	"testing"
	"time"

	"github.com/kenn-io/kit/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestDeferredInt64CounterEmitsZeroWhenNoAddsWereQueued(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	counter, err := provider.Meter("test").Int64Counter("requests")
	if err != nil {
		t.Fatal(err)
	}

	instr := telemetry.NewDeferredInt64Counter(counter, attribute.String("route", "/ready"))
	instr.Emit(context.Background(), attribute.String("result", "ok"))

	points := collectInt64Counter(t, reader, "requests")
	if len(points) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(points))
	}
	if points[0].Value != 0 {
		t.Fatalf("expected zero value, got %d", points[0].Value)
	}
	assertAttr(t, points[0].Attributes, "route", "/ready")
	assertAttr(t, points[0].Attributes, "result", "ok")
}

func TestDeferredInt64CounterEmitsQueuedAddsWithAttributes(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	counter, err := provider.Meter("test").Int64Counter("errors")
	if err != nil {
		t.Fatal(err)
	}

	instr := telemetry.NewDeferredInt64Counter(counter, attribute.String("component", "worker"))
	instr.Add(1, attribute.String("error.type", "timeout"))
	instr.Emit(context.Background(), attribute.String("operation", "sync"))

	points := collectInt64Counter(t, reader, "errors")
	if len(points) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(points))
	}
	if points[0].Value != 1 {
		t.Fatalf("expected value 1, got %d", points[0].Value)
	}
	assertAttr(t, points[0].Attributes, "component", "worker")
	assertAttr(t, points[0].Attributes, "error.type", "timeout")
	assertAttr(t, points[0].Attributes, "operation", "sync")
}

func TestDeferredInt64CounterEmitIsIdempotent(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	counter, err := provider.Meter("test").Int64Counter("once")
	if err != nil {
		t.Fatal(err)
	}

	instr := telemetry.NewDeferredInt64Counter(counter)
	instr.Add(1)
	instr.Emit(context.Background())
	instr.Emit(context.Background())

	points := collectInt64Counter(t, reader, "once")
	if len(points) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(points))
	}
	if points[0].Value != 1 {
		t.Fatalf("expected value 1, got %d", points[0].Value)
	}
}

func TestDeferredInstrumentTypesEmitZeroWhenUnset(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	meter := provider.Meter("test")

	floatCounter, err := meter.Float64Counter("float_counter")
	if err != nil {
		t.Fatal(err)
	}
	intUpDownCounter, err := meter.Int64UpDownCounter("int_up_down_counter")
	if err != nil {
		t.Fatal(err)
	}
	floatUpDownCounter, err := meter.Float64UpDownCounter("float_up_down_counter")
	if err != nil {
		t.Fatal(err)
	}
	intHistogram, err := meter.Int64Histogram("int_histogram")
	if err != nil {
		t.Fatal(err)
	}
	floatHistogram, err := meter.Float64Histogram("float_histogram")
	if err != nil {
		t.Fatal(err)
	}
	intGauge, err := meter.Int64Gauge("int_gauge")
	if err != nil {
		t.Fatal(err)
	}
	floatGauge, err := meter.Float64Gauge("float_gauge")
	if err != nil {
		t.Fatal(err)
	}

	telemetry.NewDeferredFloat64Counter(floatCounter).Emit(context.Background())
	telemetry.NewDeferredInt64UpDownCounter(intUpDownCounter).Emit(context.Background())
	telemetry.NewDeferredFloat64UpDownCounter(floatUpDownCounter).Emit(context.Background())
	telemetry.NewDeferredInt64Histogram(intHistogram).Emit(context.Background())
	telemetry.NewDeferredFloat64Histogram(floatHistogram).Emit(context.Background())
	telemetry.NewDeferredInt64Gauge(intGauge).Emit(context.Background())
	telemetry.NewDeferredFloat64Gauge(floatGauge).Emit(context.Background())

	rm := collect(t, reader)
	assertSumPoint(t, findMetric(t, rm, "float_counter"), float64(0))
	assertSumPoint(t, findMetric(t, rm, "int_up_down_counter"), int64(0))
	assertSumPoint(t, findMetric(t, rm, "float_up_down_counter"), float64(0))
	assertHistogramPoint(t, findMetric(t, rm, "int_histogram"), int64(0))
	assertHistogramPoint(t, findMetric(t, rm, "float_histogram"), float64(0))
	assertGaugePoint(t, findMetric(t, rm, "int_gauge"), int64(0))
	assertGaugePoint(t, findMetric(t, rm, "float_gauge"), float64(0))
}

func TestDeferredGaugesEmitLatestSetValue(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	gauge, err := provider.Meter("test").Int64Gauge("depth")
	if err != nil {
		t.Fatal(err)
	}

	instr := telemetry.NewDeferredInt64Gauge(gauge, attribute.String("queue", "jobs"))
	instr.Set(3, attribute.String("ignored", "true"))
	instr.Set(7, attribute.String("ignored", "false"))
	instr.Emit(context.Background(), attribute.String("host", "local"))

	rm := collect(t, reader)
	data := findMetric(t, rm, "depth").Data
	gaugeData, ok := data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric data type = %T, want metricdata.Gauge[int64]", data)
	}
	if len(gaugeData.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(gaugeData.DataPoints))
	}
	if gaugeData.DataPoints[0].Value != 7 {
		t.Fatalf("expected value 7, got %d", gaugeData.DataPoints[0].Value)
	}
	assertAttr(t, gaugeData.DataPoints[0].Attributes, "queue", "jobs")
	assertAttr(t, gaugeData.DataPoints[0].Attributes, "ignored", "false")
	assertAttr(t, gaugeData.DataPoints[0].Attributes, "host", "local")
}

func TestDeferredTimerRecordsElapsedSeconds(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	histogram, err := provider.Meter("test").Float64Histogram("duration")
	if err != nil {
		t.Fatal(err)
	}

	timer := telemetry.NewDeferredTimer(histogram, attribute.String("operation", "sync"))
	time.Sleep(time.Millisecond)
	timer.Emit(context.Background())
	timer.Emit(context.Background())

	rm := collect(t, reader)
	data := findMetric(t, rm, "duration").Data
	histogramData, ok := data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric data type = %T, want metricdata.Histogram[float64]", data)
	}
	if len(histogramData.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(histogramData.DataPoints))
	}
	point := histogramData.DataPoints[0]
	if point.Count != 1 {
		t.Fatalf("expected count 1, got %d", point.Count)
	}
	if point.Sum <= 0 {
		t.Fatalf("expected positive elapsed seconds, got %f", point.Sum)
	}
	assertAttr(t, point.Attributes, "operation", "sync")
}

func collectInt64Counter(t *testing.T, reader *metric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()

	rm := collect(t, reader)
	metric := findMetric(t, rm, name)
	sum, ok := metric.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q data type = %T, want metricdata.Sum[int64]", name, metric.Data)
	}
	return sum.DataPoints
}

func collect(t *testing.T, reader *metric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			return m
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Metrics{}
}

func assertSumPoint[N int64 | float64](t *testing.T, metric metricdata.Metrics, want N) {
	t.Helper()

	sum, ok := metric.Data.(metricdata.Sum[N])
	if !ok {
		t.Fatalf("metric %q data type = %T, want metricdata.Sum", metric.Name, metric.Data)
	}
	if len(sum.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(sum.DataPoints))
	}
	if sum.DataPoints[0].Value != want {
		t.Fatalf("expected value %v, got %v", want, sum.DataPoints[0].Value)
	}
}

func assertHistogramPoint[N int64 | float64](t *testing.T, metric metricdata.Metrics, want N) {
	t.Helper()

	histogram, ok := metric.Data.(metricdata.Histogram[N])
	if !ok {
		t.Fatalf("metric %q data type = %T, want metricdata.Histogram", metric.Name, metric.Data)
	}
	if len(histogram.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(histogram.DataPoints))
	}
	if histogram.DataPoints[0].Count != 1 {
		t.Fatalf("expected count 1, got %d", histogram.DataPoints[0].Count)
	}
	if histogram.DataPoints[0].Sum != want {
		t.Fatalf("expected sum %v, got %v", want, histogram.DataPoints[0].Sum)
	}
}

func assertGaugePoint[N int64 | float64](t *testing.T, metric metricdata.Metrics, want N) {
	t.Helper()

	gauge, ok := metric.Data.(metricdata.Gauge[N])
	if !ok {
		t.Fatalf("metric %q data type = %T, want metricdata.Gauge", metric.Name, metric.Data)
	}
	if len(gauge.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(gauge.DataPoints))
	}
	if gauge.DataPoints[0].Value != want {
		t.Fatalf("expected value %v, got %v", want, gauge.DataPoints[0].Value)
	}
}

func assertAttr(t *testing.T, set attribute.Set, key attribute.Key, want string) {
	t.Helper()

	value, ok := set.Value(key)
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	if got := value.AsString(); got != want {
		t.Fatalf("attribute %q = %q, want %q", key, got, want)
	}
}
