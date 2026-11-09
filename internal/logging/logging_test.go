package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"github.com/discordia-grupo01/billing/internal/logging"
)

func entries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("la línea no es JSON: %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

func useLogger(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetFlags(log.LstdFlags)
	})
	buf := &bytes.Buffer{}
	slog.SetDefault(logging.New("svc", level, buf))
	return buf
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"INFO":    slog.LevelInfo,
		"debug":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		" Error ": slog.LevelError,
	}
	for name, want := range cases {
		got, err := logging.ParseLevel(name)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; quería %v", name, got, err, want)
		}
	}
	if _, err := logging.ParseLevel("verbose"); err == nil {
		t.Error("un nivel desconocido debería fallar")
	}
}

func TestSetupRejectsInvalidLevel(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv(logging.EnvLevel, "ruidoso")
	if err := logging.Setup("svc"); err == nil {
		t.Fatal("Setup debería fallar con un LOG_LEVEL inválido")
	}
}

func TestSetupAcceptsValidLevel(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetFlags(log.LstdFlags)
	})
	t.Setenv(logging.EnvLevel, "debug")
	if err := logging.Setup("svc"); err != nil {
		t.Fatalf("Setup falló: %v", err)
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("el nivel debug debería estar habilitado")
	}
}

func TestSetupAnnouncesStartWithoutRepeatingTheLevelKey(t *testing.T) {
	previous := slog.Default()
	previousStdout := os.Stdout
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetFlags(log.LstdFlags)
		os.Stdout = previousStdout
	})
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	t.Setenv(logging.EnvLevel, "debug")

	if err := logging.Setup("svc"); err != nil {
		t.Fatalf("Setup falló: %v", err)
	}
	writer.Close()
	raw, _ := io.ReadAll(reader)

	if strings.Count(string(raw), `"level"`) != 1 {
		t.Fatalf("la clave level debería aparecer una sola vez: %s", raw)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("no es JSON: %v: %s", err, raw)
	}
	if entry["message"] != "service_started" || entry["level"] != "info" || entry["log_level"] != "debug" || entry["service"] != "svc" {
		t.Errorf("entrada inesperada: %v", entry)
	}
}

func TestEntriesCarryTheTraceWhenThereIsOne(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	slog.InfoContext(ctx, "con traza", "k", "v")
	slog.Info("sin traza")

	got := entries(t, buf)
	if got[0]["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || got[0]["span_id"] != "00f067aa0ba902b7" || got[0]["k"] != "v" {
		t.Errorf("faltan los ids de la traza: %v", got[0])
	}
	if _, ok := got[1]["trace_id"]; ok {
		t.Errorf("sin traza no debería haber trace_id: %v", got[1])
	}
}

func TestEntryShape(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	slog.Warn("algo raro", "queue", "q1")

	got := entries(t, buf)
	if len(got) != 1 {
		t.Fatalf("esperaba 1 línea, hubo %d", len(got))
	}
	entry := got[0]
	if entry["level"] != "warning" || entry["service"] != "svc" || entry["message"] != "algo raro" || entry["queue"] != "q1" {
		t.Errorf("forma inesperada: %v", entry)
	}
	if _, ok := entry["time"]; !ok {
		t.Error("falta time")
	}
}

func TestLevelNames(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	slog.Debug("d")
	slog.Info("i")
	slog.Warn("w")
	slog.Error("e")

	var levels []string
	for _, entry := range entries(t, buf) {
		levels = append(levels, entry["level"].(string))
	}
	if strings.Join(levels, ",") != "debug,info,warning,error" {
		t.Errorf("niveles inesperados: %v", levels)
	}
}

func TestLevelFiltersLowerLevels(t *testing.T) {
	buf := useLogger(t, slog.LevelWarn)
	slog.Info("no debería salir")
	slog.Warn("sí")

	got := entries(t, buf)
	if len(got) != 1 || got[0]["message"] != "sí" {
		t.Errorf("filtrado incorrecto: %v", got)
	}
}

func TestSensitiveFieldsAreFiltered(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	slog.Info("login", "password", "hunter2", "Email", "a@b.com", "token", "abc", "authorization", "Bearer x", "user_id", "u-1")

	entry := entries(t, buf)[0]
	for _, key := range []string{"password", "Email", "token", "authorization"} {
		if entry[key] != "[FILTERED]" {
			t.Errorf("%s debería salir filtrado: %v", key, entry[key])
		}
	}
	if entry["user_id"] != "u-1" {
		t.Errorf("user_id no debería filtrarse: %v", entry["user_id"])
	}
}

func TestEmailsAndJWTsAreScrubbedFromText(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.firma-123"
	slog.Error("smtp rechazó a persona@example.com", "error", errors.New("550 <otra@example.org>: "+jwt), "detail", "dueño: x@y.io")

	line := buf.String()
	for _, leaked := range []string{"persona@example.com", "otra@example.org", "x@y.io", jwt, "firma-123"} {
		if strings.Contains(line, leaked) {
			t.Errorf("se coló %q en el log: %s", leaked, line)
		}
	}
	if !strings.Contains(line, "[EMAIL]") || !strings.Contains(line, "[JWT]") {
		t.Errorf("faltan los marcadores: %s", line)
	}
}

func TestPrintfHelpersUseTheirLevel(t *testing.T) {
	buf := useLogger(t, slog.LevelInfo)
	logging.Debugf("oculto %d", 1)
	logging.Infof("info %d", 2)
	logging.Warnf("warn %d", 3)
	logging.Errorf("error %d", 4)

	got := entries(t, buf)
	if len(got) != 3 {
		t.Fatalf("esperaba 3 líneas, hubo %d: %v", len(got), got)
	}
	want := []string{"info 2", "warn 3", "error 4"}
	for i, entry := range got {
		if entry["message"] != want[i] {
			t.Errorf("línea %d: %v", i, entry)
		}
	}
}

func TestStandardLogPackageIsRedirectedToJSON(t *testing.T) {
	buf := useLogger(t, slog.LevelInfo)
	log.Printf("desde el paquete log %d", 7)

	got := entries(t, buf)
	if len(got) != 1 || got[0]["message"] != "desde el paquete log 7" || got[0]["service"] != "svc" {
		t.Errorf("el paquete log no salió como JSON: %v", got)
	}
}

func newRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(logging.Middleware(), logging.Recovery())
	router.GET("/v1/items/:id", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	router.GET("/v1/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	router.GET("/v1/panic", func(c *gin.Context) { panic("se rompió con a@b.com") })
	router.GET("/livez", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	router.GET("/readyz", func(c *gin.Context) { c.Status(http.StatusServiceUnavailable) })
	return router
}

func serve(router *gin.Engine, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func TestMiddlewareLogsRoutePatternNotRealPathOrQuery(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	serve(newRouter(), http.MethodGet, "/v1/items/abc-secreto?token=zzz&email=a@b.com", nil)

	line := buf.String()
	for _, leaked := range []string{"abc-secreto", "token=zzz", "a@b.com"} {
		if strings.Contains(line, leaked) {
			t.Errorf("se coló %q: %s", leaked, line)
		}
	}
	entry := entries(t, buf)[0]
	if entry["message"] != "http_request" || entry["route"] != "/v1/items/:id" || entry["method"] != "GET" || entry["level"] != "info" {
		t.Errorf("entrada inesperada: %v", entry)
	}
	if entry["status"] != float64(200) {
		t.Errorf("status: %v", entry["status"])
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Error("falta duration_ms")
	}
}

func TestMiddlewareUnmatchedRouteHidesThePath(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	serve(newRouter(), http.MethodGet, "/no/existe/ni-con-id-123", nil)

	if strings.Contains(buf.String(), "ni-con-id-123") {
		t.Errorf("se coló el path: %s", buf.String())
	}
	if entry := entries(t, buf)[0]; entry["route"] != "unmatched" {
		t.Errorf("route: %v", entry["route"])
	}
}

func TestMiddlewareLevelsByStatus(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	router := newRouter()
	serve(router, http.MethodGet, "/v1/boom", nil)
	serve(router, http.MethodGet, "/livez", nil)
	serve(router, http.MethodGet, "/readyz", nil)

	got := entries(t, buf)
	want := []string{"error", "debug", "error"}
	if len(got) != len(want) {
		t.Fatalf("líneas: %v", got)
	}
	for i, level := range want {
		if got[i]["level"] != level {
			t.Errorf("línea %d (%v): nivel %v, quería %s", i, got[i]["route"], got[i]["level"], level)
		}
	}
}

func TestMiddlewareHidesHealthChecksAtInfo(t *testing.T) {
	buf := useLogger(t, slog.LevelInfo)
	serve(newRouter(), http.MethodGet, "/livez", nil)

	if buf.Len() != 0 {
		t.Errorf("/livez no debería verse en info: %s", buf.String())
	}
}

func TestRecoveryReturns500WithoutLeakingHeaders(t *testing.T) {
	buf := useLogger(t, slog.LevelDebug)
	recorder := serve(newRouter(), http.MethodGet, "/v1/panic", map[string]string{
		"Authorization": "Bearer super-secreto",
		"Cookie":        "sesion=galleta-secreta",
	})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", recorder.Code)
	}
	line := buf.String()
	for _, leaked := range []string{"super-secreto", "galleta-secreta", "a@b.com"} {
		if strings.Contains(line, leaked) {
			t.Errorf("se coló %q: %s", leaked, line)
		}
	}
	found := false
	for _, entry := range entries(t, buf) {
		if entry["message"] == "panic_recovered" && entry["level"] == "error" && entry["route"] == "/v1/panic" {
			found = true
		}
	}
	if !found {
		t.Errorf("falta panic_recovered: %s", line)
	}
}
