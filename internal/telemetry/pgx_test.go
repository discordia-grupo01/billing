package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/discordia-grupo01/billing/internal/telemetry"
)

func TestPgxTracerIgnoresQueriesOutsideATrace(t *testing.T) {
	spans := recorder(t)
	tracer := telemetry.PgxTracer{}

	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	if len(spans.Ended()) != 0 {
		t.Errorf("no debería haber spans fuera de una traza: %d", len(spans.Ended()))
	}
}

func TestPgxTracerRecordsTheStatementButNeverTheArguments(t *testing.T) {
	spans := recorder(t)
	tracer := telemetry.PgxTracer{}
	parentCtx, parent := otel.Tracer("test").Start(context.Background(), "request")

	ctx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{
		SQL:  "  select id from users where email = $1",
		Args: []any{"persona@example.com"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	parent.End()

	var query sdktrace.ReadOnlySpan
	for _, span := range spans.Ended() {
		if span.Name() != "request" {
			query = span
		}
	}
	if query == nil {
		t.Fatal("falta el span de la consulta")
	}
	got := attributes(query)
	if query.Name() != "db SELECT" || got["db.operation"] != "SELECT" || got["db.system"] != "postgresql" {
		t.Errorf("span inesperado: %s %v", query.Name(), got)
	}
	if !strings.Contains(toString(got["db.statement"]), "email = $1") {
		t.Errorf("falta el statement: %v", got)
	}
	if strings.Contains(toString(got["db.statement"]), "persona@example.com") {
		t.Error("el argumento no debería registrarse")
	}
	if query.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("la consulta debería colgar del pedido")
	}
}

func TestPgxTracerMarksFailuresWithoutTheErrorText(t *testing.T) {
	spans := recorder(t)
	tracer := telemetry.PgxTracer{}
	parentCtx, parent := otel.Tracer("test").Start(context.Background(), "request")

	ctx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "INSERT INTO users VALUES ($1)"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		Err: &pgconn.PgError{Code: "23505", Message: "duplicate key value (persona@example.com)"},
	})
	parent.End()

	for _, span := range spans.Ended() {
		if span.Name() != "db INSERT" {
			continue
		}
		if span.Status().Code != codes.Error || span.Status().Description != "query failed" {
			t.Errorf("status inesperado: %v", span.Status())
		}
		if attributes(span)["db.response.status_code"] != "23505" {
			t.Errorf("falta el código SQLSTATE: %v", attributes(span))
		}
		for _, attr := range span.Attributes() {
			if strings.Contains(attr.Value.Emit(), "persona@example.com") {
				t.Errorf("se coló el mensaje de error: %v", attr)
			}
		}
		return
	}
	t.Fatal("falta el span de la consulta")
}

func TestPgxTracerNonDatabaseErrorStillFailsTheSpan(t *testing.T) {
	spans := recorder(t)
	tracer := telemetry.PgxTracer{}
	parentCtx, parent := otel.Tracer("test").Start(context.Background(), "request")

	ctx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: ""})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("connection reset")})
	parent.End()

	for _, span := range spans.Ended() {
		if span.Name() == "db query" && span.Status().Code == codes.Error {
			return
		}
	}
	t.Fatal("una consulta vacía con error debería llamarse db query y fallar")
}

func TestPgxTracerCutsLongStatements(t *testing.T) {
	spans := recorder(t)
	tracer := telemetry.PgxTracer{}
	parentCtx, parent := otel.Tracer("test").Start(context.Background(), "request")

	ctx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "SELECT " + strings.Repeat("x", 2000)})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	parent.End()

	for _, span := range spans.Ended() {
		if span.Name() == "db SELECT" && len(toString(attributes(span)["db.statement"])) == 500 {
			return
		}
	}
	t.Fatal("el statement debería cortarse a 500 caracteres")
}
