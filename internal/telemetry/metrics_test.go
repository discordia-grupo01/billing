package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/discordia-grupo01/billing/internal/telemetry"
)

func useMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(func(sdkmetric.InstrumentKind) metricdata.Temporality {
		return metricdata.DeltaTemporality
	}))
	provider := telemetry.NewMeterProvider(resource.Empty(), reader)
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return reader
}

func metricsRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(telemetry.Middleware())
	r.GET("/v1/items/:id", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/v1/missing", func(c *gin.Context) { c.Status(http.StatusNotFound) })
	r.GET("/v1/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	return r
}

func request(r *gin.Engine, target string) {
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatalf("no pude leer las métricas: %v", err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("la métrica no es un histograma: %T", m.Data)
			}
			if histogram.Temporality != metricdata.DeltaTemporality {
				t.Fatalf("esperaba temporalidad delta, hubo %v", histogram.Temporality)
			}
			if m.Unit != "s" {
				t.Fatalf("esperaba unidad s, hubo %q", m.Unit)
			}
			return histogram.DataPoints
		}
	}
	return nil
}

func attributesOf(point metricdata.HistogramDataPoint[float64]) map[string]any {
	out := map[string]any{}
	for _, attr := range point.Attributes.ToSlice() {
		out[string(attr.Key)] = attr.Value.AsInterface()
	}
	return out
}

func TestMiddlewareRecordsTheRequestDurationInSeconds(t *testing.T) {
	reader := useMetrics(t)

	request(metricsRouter(), "/v1/items/abc?token=zzz")

	points := collect(t, reader)
	if len(points) != 1 {
		t.Fatalf("esperaba 1 punto, hubo %d", len(points))
	}
	attrs := attributesOf(points[0])
	if attrs["http.request.method"] != "GET" || attrs["http.route"] != "/v1/items/:id" || attrs["http.response.status_code"] != int64(200) {
		t.Fatalf("atributos inesperados: %v", attrs)
	}
	if points[0].Count != 1 || points[0].Sum < 0 || points[0].Sum > 1 {
		t.Fatalf("la duración debería estar en segundos y ser chica: count=%d sum=%v", points[0].Count, points[0].Sum)
	}
}

func TestMetricsUseTheRoutePatternAndNeverTheRealPathOrQuery(t *testing.T) {
	reader := useMetrics(t)

	request(metricsRouter(), "/v1/items/abc-secreto?token=zzz&email=a@b.com")
	request(metricsRouter(), "/no/existe/otro-secreto")

	var dump strings.Builder
	for _, point := range collect(t, reader) {
		for key, value := range attributesOf(point) {
			dump.WriteString(key)
			dump.WriteString("=")
			dump.WriteString(strings.TrimSpace(stringValue(value)))
			dump.WriteString(" ")
		}
	}
	for _, secret := range []string{"secreto", "token", "zzz", "a@b.com", "email"} {
		if strings.Contains(dump.String(), secret) {
			t.Fatalf("las métricas filtran %q: %s", secret, dump.String())
		}
	}
	if !strings.Contains(dump.String(), "unmatched") {
		t.Fatalf("las rutas inexistentes deberían agruparse como unmatched: %s", dump.String())
	}
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func TestOnlyServerErrorsCarryErrorType(t *testing.T) {
	reader := useMetrics(t)

	request(metricsRouter(), "/v1/boom")
	request(metricsRouter(), "/v1/missing")
	request(metricsRouter(), "/v1/items/1")

	byStatus := map[int64]map[string]any{}
	for _, point := range collect(t, reader) {
		attrs := attributesOf(point)
		byStatus[attrs["http.response.status_code"].(int64)] = attrs
	}
	if byStatus[500]["error.type"] != "500" {
		t.Fatalf("un 500 debería llevar error.type=500: %v", byStatus[500])
	}
	for _, status := range []int64{404, 200} {
		if _, has := byStatus[status]["error.type"]; has {
			t.Fatalf("un %d no debería llevar error.type: %v", status, byStatus[status])
		}
	}
}

func TestRequestsWithTheSameAttributesShareOneDataPoint(t *testing.T) {
	reader := useMetrics(t)

	for i := 0; i < 3; i++ {
		request(metricsRouter(), "/v1/items/abc")
	}

	points := collect(t, reader)
	if len(points) != 1 || points[0].Count != 3 {
		t.Fatalf("esperaba un punto con 3 pedidos, hubo %+v", points)
	}
}

func TestDeltaTemporalityDoesNotRepeatWhatWasAlreadyCollected(t *testing.T) {
	reader := useMetrics(t)

	request(metricsRouter(), "/v1/items/abc")
	if len(collect(t, reader)) != 1 {
		t.Fatal("la primera lectura debería traer el pedido")
	}

	if points := collect(t, reader); len(points) != 0 {
		t.Fatalf("la segunda lectura no debería repetirlo: %+v", points)
	}
}

func TestDurationBucketsAreInSeconds(t *testing.T) {
	reader := useMetrics(t)

	request(metricsRouter(), "/v1/items/abc")

	bounds := collect(t, reader)[0].Bounds
	if len(bounds) != 14 || bounds[0] != 0.005 || bounds[len(bounds)-1] != 10 {
		t.Fatalf("buckets inesperados: %v", bounds)
	}
}
