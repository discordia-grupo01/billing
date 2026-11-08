package telemetry

import (
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	requestDurationName = "http.server.request.duration"
	exportInterval      = 60 * time.Second
)

var requestDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

func metricsEnabled() bool {
	return os.Getenv(EnvEndpoint) != "" || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

func deltaTemporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.DeltaTemporality
}

func newMeterProvider(res *resource.Resource, reader sdkmetric.Reader) *sdkmetric.MeterProvider {
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: requestDurationName},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: requestDurationBuckets}},
		)),
	)
}

func newRequestDuration() metric.Float64Histogram {
	histogram, err := otel.Meter(tracerName).Float64Histogram(requestDurationName,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests."),
	)
	if err != nil {
		otel.Handle(err)
		return noop.Float64Histogram{}
	}
	return histogram
}

func requestAttributes(method, route string, status int) []attribute.KeyValue {
	attributes := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("http.route", route),
		attribute.Int("http.response.status_code", status),
	}
	if status >= 500 {
		attributes = append(attributes, attribute.String("error.type", strconv.Itoa(status)))
	}
	return attributes
}
