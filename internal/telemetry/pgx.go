package telemetry

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxStatementLength = 500

type PgxTracer struct{}

func (PgxTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	statement := data.SQL
	if len(statement) > maxStatementLength {
		statement = statement[:maxStatementLength]
	}
	operation := operationOf(data.SQL)
	ctx, _ = otel.Tracer(tracerName).Start(ctx, "db "+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.operation", operation),
			attribute.String("db.statement", statement),
		),
	)
	return ctx
}

func (PgxTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	defer span.End()
	if data.Err == nil {
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(data.Err, &pgErr) {
		span.SetAttributes(attribute.String("db.response.status_code", pgErr.Code))
	}
	span.SetStatus(codes.Error, "query failed")
}

func operationOf(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return "query"
	}
	return strings.ToUpper(fields[0])
}
