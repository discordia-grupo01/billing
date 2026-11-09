package logging_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/discordia-grupo01/billing/internal/logging"
)

const apiKey = "test-ingest-key"

type delivery struct {
	headers http.Header
	payload []struct {
		Common struct {
			Attributes map[string]string `json:"attributes"`
		} `json:"common"`
		Logs []struct {
			Timestamp  int64                      `json:"timestamp"`
			Message    string                     `json:"message"`
			Attributes map[string]json.RawMessage `json:"attributes"`
		} `json:"logs"`
	}
	raw string
}

func (d delivery) messages() []string {
	var out []string
	for _, group := range d.payload {
		for _, log := range group.Logs {
			out = append(out, log.Message)
		}
	}
	return out
}

func (d delivery) find(message string) (map[string]json.RawMessage, bool) {
	for _, group := range d.payload {
		for _, log := range group.Logs {
			if log.Message == message {
				return log.Attributes, true
			}
		}
	}
	return nil, false
}

type sink struct {
	server   *httptest.Server
	received chan delivery
	mu       sync.Mutex
	statuses []int
}

func newSink(t *testing.T, statuses ...int) *sink {
	t.Helper()
	s := &sink{received: make(chan delivery, 64), statuses: statuses}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("el cuerpo no viene en gzip: %v", err)
			return
		}
		body, _ := io.ReadAll(reader)
		d := delivery{headers: r.Header.Clone(), raw: string(body)}
		if err := json.Unmarshal(body, &d.payload); err != nil {
			t.Errorf("el cuerpo no es JSON: %v", err)
		}
		s.received <- d

		s.mu.Lock()
		status := http.StatusAccepted
		if len(s.statuses) > 0 {
			status, s.statuses = s.statuses[0], s.statuses[1:]
		}
		s.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *sink) next(t *testing.T) delivery {
	t.Helper()
	select {
	case d := <-s.received:
		return d
	case <-time.After(3 * time.Second):
		t.Fatal("no llegó ningún envío")
		return delivery{}
	}
}

func (s *sink) nextWith(t *testing.T, message string) delivery {
	t.Helper()
	for {
		d := s.next(t)
		for _, got := range d.messages() {
			if got == message {
				return d
			}
		}
	}
}

func (s *sink) quiet(wait time.Duration) []delivery {
	var extra []delivery
	deadline := time.After(wait)
	for {
		select {
		case d := <-s.received:
			extra = append(extra, d)
		case <-deadline:
			return extra
		}
	}
}

func config(url string) logging.ShipperConfig {
	return logging.ShipperConfig{
		URL:            url,
		APIKey:         apiKey,
		Service:        "billing-test",
		MaxBatch:       200,
		MaxQueue:       5000,
		FlushInterval:  20 * time.Millisecond,
		BaseBackoff:    20 * time.Millisecond,
		MaxBackoff:     40 * time.Millisecond,
		RequestTimeout: 2 * time.Second,
	}
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func shipping(t *testing.T, cfg logging.ShipperConfig) (*slog.Logger, *syncBuffer, *logging.Shipper) {
	t.Helper()
	out := &syncBuffer{}
	logger, s := logging.NewShipping("billing", slog.LevelInfo, out, cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return logger, out, s
}

func TestShipsTheEntriesGzippedWithTheAPIKey(t *testing.T) {
	s := newSink(t)
	logger, _, _ := shipping(t, config(s.server.URL))

	logger.Info("order_shipped", "user_id", "u-1")

	d := s.nextWith(t, "order_shipped")
	if d.headers.Get("Api-Key") != apiKey || d.headers.Get("Content-Encoding") != "gzip" || d.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("cabeceras inesperadas: %v", d.headers)
	}
	common := d.payload[0].Common.Attributes
	if common["service.name"] != "billing-test" || common["entity.name"] != "billing-test" {
		t.Fatalf("atributos comunes inesperados: %v", common)
	}
	attrs, _ := d.find("order_shipped")
	if string(attrs["level"]) != `"info"` || string(attrs["user_id"]) != `"u-1"` {
		t.Fatalf("atributos inesperados: %v", attrs)
	}
	if _, has := attrs["service"]; has {
		t.Fatalf("el servicio viaja en common, no en cada log: %v", attrs)
	}
	if d.payload[0].Logs[0].Timestamp < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatalf("timestamp inesperado: %d", d.payload[0].Logs[0].Timestamp)
	}
}

func TestRenamesTheTraceAndSpanIDsToWhatNewRelicUses(t *testing.T) {
	s := newSink(t)
	logger, _, _ := shipping(t, config(s.server.URL))
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	logger.InfoContext(ctx, "with_trace")
	logger.Info("without_trace")

	d := s.nextWith(t, "with_trace")
	attrs, _ := d.find("with_trace")
	if string(attrs["trace.id"]) != `"4bf92f3577b34da6a3ce929d0e0e4736"` || string(attrs["span.id"]) != `"00f067aa0ba902b7"` {
		t.Fatalf("faltan trace.id o span.id: %v", attrs)
	}
	if _, has := attrs["trace_id"]; has {
		t.Fatalf("no debería quedar trace_id: %v", attrs)
	}
	if other, ok := d.find("without_trace"); ok {
		if _, has := other["trace.id"]; has {
			t.Fatalf("un log sin traza no debería llevar trace.id: %v", other)
		}
	}
}

func TestShipsTheSameScrubbedContentAsTheConsole(t *testing.T) {
	s := newSink(t)
	logger, out, _ := shipping(t, config(s.server.URL))
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij"

	logger.Info("login de ana@example.com", "password", "hunter2", "ticket", "t-123", "note", "usa "+jwt, "content", "mensaje privado")

	d := s.nextWith(t, "login de [EMAIL]")
	for _, secret := range []string{"ana@example.com", "hunter2", "t-123", jwt, "mensaje privado"} {
		if strings.Contains(d.raw, secret) {
			t.Fatalf("el envío filtra %q: %s", secret, d.raw)
		}
		if strings.Contains(out.String(), secret) {
			t.Fatalf("la consola filtra %q", secret)
		}
	}
	attrs, _ := d.find("login de [EMAIL]")
	if string(attrs["password"]) != `"[FILTERED]"` || string(attrs["note"]) != `"usa [JWT]"` {
		t.Fatalf("saneado inesperado: %v", attrs)
	}
}

func TestTheConsoleKeepsWorkingAlongsideTheShipping(t *testing.T) {
	s := newSink(t)
	logger, out, _ := shipping(t, config(s.server.URL))

	logger.Info("visible_en_consola", "k", "v")

	s.nextWith(t, "visible_en_consola")
	if !strings.Contains(out.String(), `"message":"visible_en_consola"`) || !strings.Contains(out.String(), `"service":"billing"`) {
		t.Fatalf("la consola debería seguir escribiendo: %s", out.String())
	}
}

func TestGroupsSeveralEntriesInOneRequest(t *testing.T) {
	s := newSink(t)
	cfg := config(s.server.URL)
	cfg.FlushInterval = 150 * time.Millisecond
	logger, _, _ := shipping(t, cfg)

	for _, name := range []string{"entry_1", "entry_2", "entry_3", "entry_4", "entry_5"} {
		logger.Info(name)
	}

	d := s.nextWith(t, "entry_1")
	if got := strings.Join(d.messages(), ","); got != "entry_1,entry_2,entry_3,entry_4,entry_5" {
		t.Fatalf("esperaba los cinco juntos y en orden, hubo %s", got)
	}
}

func TestSendsRightAwayWhenTheBatchIsFull(t *testing.T) {
	s := newSink(t)
	cfg := config(s.server.URL)
	cfg.FlushInterval = time.Minute
	cfg.MaxBatch = 3
	logger, _, _ := shipping(t, cfg)

	for _, name := range []string{"full_1", "full_2", "full_3"} {
		logger.Info(name)
	}

	d := s.next(t)
	if len(d.messages()) != 3 {
		t.Fatalf("esperaba 3 entradas sin esperar al intervalo, hubo %v", d.messages())
	}
}

func TestKeepsTheEntriesAndRetriesWhenTheServerFails(t *testing.T) {
	s := newSink(t, 500, 429, 202)
	logger, _, _ := shipping(t, config(s.server.URL))

	logger.Info("retried_entry")

	for i := 0; i < 3; i++ {
		d := s.nextWith(t, "retried_entry")
		if len(d.messages()) != 1 {
			t.Fatalf("el reintento %d debería traer solo la entrada: %v", i+1, d.messages())
		}
	}
	for _, extra := range s.quiet(150 * time.Millisecond) {
		for _, message := range extra.messages() {
			if message == "retried_entry" {
				t.Fatal("la entrada se reenvió después de entregarse")
			}
		}
	}
}

func TestKeepsTheOriginalOrderWhenItRetries(t *testing.T) {
	s := newSink(t, 503, 202)
	cfg := config(s.server.URL)
	cfg.MaxBatch = 2
	logger, _, _ := shipping(t, cfg)

	for _, name := range []string{"ordered_1", "ordered_2", "ordered_3", "ordered_4"} {
		logger.Info(name)
	}

	var seen []string
	for len(seen) < 6 {
		d := s.next(t)
		seen = append(seen, d.messages()...)
		if strings.Contains(strings.Join(seen, ","), "ordered_4") {
			break
		}
	}
	var unique []string
	for _, message := range seen {
		if len(unique) == 0 || unique[len(unique)-1] != message {
			if !contains(unique, message) {
				unique = append(unique, message)
			}
		}
	}
	if strings.Join(unique, ",") != "ordered_1,ordered_2,ordered_3,ordered_4" {
		t.Fatalf("orden inesperado: %v", seen)
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func TestDiscardsWhatTheServerRejectsForGoodAndKeepsSending(t *testing.T) {
	s := newSink(t, 400, 202)
	logger, _, _ := shipping(t, config(s.server.URL))

	logger.Info("rejected_entry")
	s.nextWith(t, "rejected_entry")

	logger.Info("next_entry")
	d := s.nextWith(t, "next_entry")
	if contains(d.messages(), "rejected_entry") {
		t.Fatalf("el lote rechazado no debería reenviarse: %v", d.messages())
	}
}

func TestNeverShipsItsOwnWarnings(t *testing.T) {
	s := newSink(t, 500, 500, 202)
	logger, out, _ := shipping(t, config(s.server.URL))

	logger.Info("only_entry")
	for i := 0; i < 3; i++ {
		s.nextWith(t, "only_entry")
	}

	for _, extra := range s.quiet(150 * time.Millisecond) {
		for _, message := range extra.messages() {
			if strings.HasPrefix(message, "log_shipping_") {
				t.Fatalf("el envío no debe reenviar sus propios avisos: %s", message)
			}
		}
	}
	if !strings.Contains(out.String(), "log_shipping_failed") {
		t.Fatalf("el aviso debería salir por la consola: %s", out.String())
	}
}

func TestTheQueueIsBoundedAndDropsTheOldest(t *testing.T) {
	cfg := config("http://127.0.0.1:1/unreachable")
	cfg.MaxQueue = 3
	cfg.MaxBatch = 100
	cfg.FlushInterval = time.Hour
	logger, _, s := shipping(t, cfg)

	for _, name := range []string{"queued_1", "queued_2", "queued_3", "queued_4", "queued_5"} {
		logger.Info(name)
	}

	if s.QueueLen() != 3 || s.Dropped() != 2 {
		t.Fatalf("esperaba cola=3 y descartadas=2, hubo cola=%d descartadas=%d", s.QueueLen(), s.Dropped())
	}
}

func TestAnUnreachableServerDoesNotBreakTheCaller(t *testing.T) {
	logger, out, _ := shipping(t, config("http://127.0.0.1:1/unreachable"))

	logger.Info("sent_into_the_void")
	time.Sleep(150 * time.Millisecond)
	logger.Info("still_working")

	if !strings.Contains(out.String(), "still_working") {
		t.Fatalf("el logger debería seguir andando: %s", out.String())
	}
}

func TestShutdownSendsWhatWasPending(t *testing.T) {
	s := newSink(t)
	cfg := config(s.server.URL)
	cfg.FlushInterval = time.Hour
	logger, _, shipper := shipping(t, cfg)

	logger.Info("pending_at_shutdown")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shipper.Shutdown(ctx)

	s.nextWith(t, "pending_at_shutdown")
}

func TestTheAPIKeyNeverEndsUpInWhatIsSent(t *testing.T) {
	s := newSink(t)
	logger, _, _ := shipping(t, config(s.server.URL))

	logger.Info("check_key")

	if d := s.nextWith(t, "check_key"); strings.Contains(d.raw, apiKey) {
		t.Fatal("la clave no debe viajar en el cuerpo")
	}
}

func TestConfigComesFromTheEnvironment(t *testing.T) {
	t.Setenv(logging.EnvShipURL, "https://log-api.example/log/v1")
	t.Setenv("LOG_SHIP_API_KEY", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "foo=bar, Api-Key = from-otel ,x=y")
	t.Setenv("OTEL_SERVICE_NAME", "")

	cfg, ok := logging.ConfigFromEnv("billing")

	if !ok || cfg.URL != "https://log-api.example/log/v1" || cfg.APIKey != "from-otel" || cfg.Service != "billing" {
		t.Fatalf("config inesperada: %+v ok=%v", cfg, ok)
	}
}

func TestTheOwnKeyWinsOverTheOneFromTheOTLPHeaders(t *testing.T) {
	t.Setenv(logging.EnvShipURL, "https://x/log")
	t.Setenv("LOG_SHIP_API_KEY", "own-key")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "api-key=otel-key")
	t.Setenv("OTEL_SERVICE_NAME", "billing-staging")

	cfg, ok := logging.ConfigFromEnv("billing")

	if !ok || cfg.APIKey != "own-key" || cfg.Service != "billing-staging" {
		t.Fatalf("config inesperada: %+v ok=%v", cfg, ok)
	}
}

func TestShippingIsOffWithoutURLOrWithoutKey(t *testing.T) {
	t.Setenv("LOG_SHIP_API_KEY", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "api-key=otel-key")
	t.Setenv(logging.EnvShipURL, "")
	if _, ok := logging.ConfigFromEnv("billing"); ok {
		t.Fatal("sin URL no debería activarse")
	}

	t.Setenv(logging.EnvShipURL, "https://x/log")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	if _, ok := logging.ConfigFromEnv("billing"); ok {
		t.Fatal("sin clave no debería activarse")
	}
}

func TestAPIKeyIsFoundRegardlessOfCaseAndSpaces(t *testing.T) {
	cases := map[string]string{
		"api-key=abc":           "abc",
		"API-KEY=abc":           "abc",
		" a=b , api-key = abc ": "abc",
		"api-key=a=b":           "a=b",
		"other=1":               "",
		"":                      "",
	}
	for headers, want := range cases {
		if got := logging.APIKeyFromHeaders(headers); got != want {
			t.Fatalf("%q: esperaba %q, hubo %q", headers, want, got)
		}
	}
}
