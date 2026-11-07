package telemetry_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/discordia-grupo01/billing/internal/telemetry"
)

func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})
	return spans
}

func attributes(span sdktrace.ReadOnlySpan) map[string]any {
	out := map[string]any{}
	for _, attr := range span.Attributes() {
		out[string(attr.Key)] = attr.Value.AsInterface()
	}
	return out
}

func router(spanSeen *trace.SpanContext) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(telemetry.Middleware())
	r.GET("/v1/items/:id", func(c *gin.Context) {
		if spanSeen != nil {
			*spanSeen = trace.SpanContextFromContext(c.Request.Context())
		}
		c.String(http.StatusOK, "ok")
	})
	r.GET("/v1/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	return r
}

func serve(r *gin.Engine, target string, headers map[string]string) {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
}

func TestMiddlewareNamesTheSpanAfterTheRoutePattern(t *testing.T) {
	spans := recorder(t)
	var seen trace.SpanContext

	serve(router(&seen), "/v1/items/abc-secreto?token=zzz&email=a@b.com", nil)

	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("esperaba 1 span, hubo %d", len(ended))
	}
	span := ended[0]
	if span.Name() != "GET /v1/items/:id" || span.SpanKind() != trace.SpanKindServer {
		t.Errorf("span inesperado: %s (%v)", span.Name(), span.SpanKind())
	}
	got := attributes(span)
	if got["http.route"] != "/v1/items/:id" || got["http.request.method"] != "GET" || got["http.response.status_code"] != int64(200) {
		t.Errorf("atributos inesperados: %v", got)
	}
	if !seen.IsValid() || seen.SpanID() != span.SpanContext().SpanID() {
		t.Error("el handler debería recibir el contexto del span")
	}
}

func TestMiddlewareNeverRecordsThePathOrTheQuery(t *testing.T) {
	spans := recorder(t)

	serve(router(nil), "/v1/items/abc-secreto?token=zzz&email=a@b.com", map[string]string{"Authorization": "Bearer super-secreto"})

	for _, span := range spans.Ended() {
		dump := span.Name()
		for key, value := range attributes(span) {
			dump += " " + key + "=" + toString(value)
		}
		for _, leaked := range []string{"abc-secreto", "token=zzz", "a@b.com", "super-secreto"} {
			if strings.Contains(dump, leaked) {
				t.Errorf("se coló %q: %s", leaked, dump)
			}
		}
	}
}

func toString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func TestMiddlewareUnmatchedRoute(t *testing.T) {
	spans := recorder(t)

	serve(router(nil), "/no/existe/con-id-123", nil)

	span := spans.Ended()[0]
	if span.Name() != "GET unmatched" || strings.Contains(span.Name(), "con-id-123") {
		t.Errorf("nombre inesperado: %s", span.Name())
	}
}

func TestMiddlewareContinuesTheIncomingTrace(t *testing.T) {
	spans := recorder(t)
	incoming := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	serve(router(nil), "/v1/items/1", map[string]string{"traceparent": incoming})

	span := spans.Ended()[0]
	if span.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id: %s", span.SpanContext().TraceID())
	}
	if span.Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("parent: %s", span.Parent().SpanID())
	}
}

func TestMiddlewareMarksServerErrors(t *testing.T) {
	spans := recorder(t)

	serve(router(nil), "/v1/boom", nil)

	span := spans.Ended()[0]
	if span.Status().Code != codes.Error {
		t.Errorf("un 500 debería marcar el span como error: %v", span.Status())
	}
	if attributes(span)["http.response.status_code"] != int64(500) {
		t.Errorf("status: %v", attributes(span))
	}
}

func TestMiddlewareLeavesClientErrorsUnmarked(t *testing.T) {
	spans := recorder(t)

	serve(router(nil), "/no/existe", nil)

	if spans.Ended()[0].Status().Code == codes.Error {
		t.Error("un 404 no es un error del servidor")
	}
}

func TestTraceparentRoundTrip(t *testing.T) {
	spans := recorder(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "origin")

	traceparent := telemetry.Traceparent(ctx)
	span.End()
	_ = spans

	if !strings.HasPrefix(traceparent, "00-"+span.SpanContext().TraceID().String()+"-"+span.SpanContext().SpanID().String()) {
		t.Fatalf("traceparent inesperado: %q", traceparent)
	}
	restored := trace.SpanContextFromContext(telemetry.ContextWithTraceparent(context.Background(), traceparent))
	if !restored.IsValid() || !restored.IsRemote() || restored.TraceID() != span.SpanContext().TraceID() {
		t.Errorf("no se recuperó el contexto: %v", restored)
	}
}

func TestTraceparentIsEmptyWithoutASpan(t *testing.T) {
	recorder(t)

	if got := telemetry.Traceparent(context.Background()); got != "" {
		t.Errorf("sin span debería ser vacío, fue %q", got)
	}
	ctx := context.Background()
	if telemetry.ContextWithTraceparent(ctx, "") != ctx {
		t.Error("un traceparent vacío no debería cambiar el contexto")
	}
}

func TestAMQPHeadersCarryTheTrace(t *testing.T) {
	recorder(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "publish")
	defer span.End()

	headers := telemetry.InjectAMQP(ctx, nil)
	if _, ok := headers["traceparent"].(string); !ok {
		t.Fatalf("falta traceparent en los headers: %v", headers)
	}

	consumed := trace.SpanContextFromContext(telemetry.ExtractAMQP(context.Background(), headers))
	if consumed.TraceID() != span.SpanContext().TraceID() {
		t.Errorf("trace id distinto: %s", consumed.TraceID())
	}
}

func TestAMQPHeadersAreUntouchedWithoutASpan(t *testing.T) {
	recorder(t)
	existing := amqp.Table{"x-other": "kept"}

	if got := telemetry.InjectAMQP(context.Background(), nil); got != nil {
		t.Errorf("sin span no debería crear headers: %v", got)
	}
	if got := telemetry.InjectAMQP(context.Background(), existing); len(got) != 1 || got["x-other"] != "kept" {
		t.Errorf("headers modificados: %v", got)
	}
}

func TestAMQPInjectKeepsExistingHeaders(t *testing.T) {
	recorder(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "publish")
	defer span.End()

	headers := telemetry.InjectAMQP(ctx, amqp.Table{"x-other": "kept"})

	if headers["x-other"] != "kept" || headers["traceparent"] == nil {
		t.Errorf("headers inesperados: %v", headers)
	}
}

func TestSetupWithoutEndpointIsANoOp(t *testing.T) {
	t.Setenv(telemetry.EnvEndpoint, "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	shutdown, err := telemetry.Setup(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Setup falló: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown falló: %v", err)
	}
	if telemetry.Enabled() {
		t.Error("sin endpoint no debería estar habilitada")
	}
	if otel.GetTracerProvider() != previous {
		t.Error("sin endpoint no debería instalarse un proveedor")
	}
}

func TestSetupNeverExportsTheTextOfAnError(t *testing.T) {
	var mu sync.Mutex
	var body []byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = append(body, raw...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	t.Setenv(telemetry.EnvEndpoint, collector.URL)
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	shutdown, err := telemetry.Setup(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Setup falló: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "unit")
	span.RecordError(errors.New("duplicate key persona@example.com"))
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown falló: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(body) == 0 {
		t.Fatal("no se exportó nada")
	}
	if strings.Contains(string(body), "persona@example.com") || strings.Contains(string(body), "duplicate key") {
		t.Error("el texto del error salió en la exportación")
	}
	if !strings.Contains(string(body), "exception") {
		t.Error("el evento de la excepción debería conservarse, sin su mensaje")
	}
}

func TestSetupExportsSpansWithTheConfiguredHeaders(t *testing.T) {
	var mu sync.Mutex
	var paths, keys []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		keys = append(keys, r.Header.Get("api-key"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	t.Setenv(telemetry.EnvEndpoint, collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "api-key=clave-de-prueba")
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	shutdown, err := telemetry.Setup(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Setup falló: %v", err)
	}
	if !telemetry.Enabled() {
		t.Fatal("con endpoint debería estar habilitada")
	}
	_, span := otel.Tracer("test").Start(context.Background(), "unit")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown falló: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || paths[0] != "/v1/traces" {
		t.Fatalf("no se exportó a /v1/traces: %v", paths)
	}
	if keys[0] != "clave-de-prueba" {
		t.Errorf("falta la cabecera api-key: %q", keys[0])
	}
}

func TestStartConsumerContinuesTheTraceOfTheMessage(t *testing.T) {
	spans := recorder(t)
	delivery := amqp.Delivery{
		RoutingKey: "identity.profile_updated",
		Headers:    amqp.Table{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	}

	ctx, end := telemetry.StartConsumer(context.Background(), "servers.user_profiles", delivery)
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Error("el contexto devuelto debería llevar el span del consumidor")
	}
	end(false)

	span := spans.Ended()[0]
	if span.Name() != "identity.profile_updated process" || span.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("span inesperado: %s (%v)", span.Name(), span.SpanKind())
	}
	if span.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || span.Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("no continuó la traza del mensaje: %v", span.Parent())
	}
	got := attributes(span)
	if got["messaging.system"] != "rabbitmq" || got["messaging.destination.name"] != "servers.user_profiles" || got["messaging.rabbitmq.destination.routing_key"] != "identity.profile_updated" {
		t.Errorf("atributos inesperados: %v", got)
	}
	if span.Status().Code == codes.Error {
		t.Error("un procesamiento exitoso no es un error")
	}
}

func TestStartConsumerMarksFailuresAndStartsFreshWithoutHeaders(t *testing.T) {
	spans := recorder(t)

	_, end := telemetry.StartConsumer(context.Background(), "q", amqp.Delivery{RoutingKey: "x.y"})
	end(true)

	span := spans.Ended()[0]
	if span.Status().Code != codes.Error {
		t.Errorf("un reintento debería marcar el span como error: %v", span.Status())
	}
	if span.Parent().IsValid() {
		t.Errorf("sin cabecera debería empezar una traza nueva: %v", span.Parent())
	}
}
