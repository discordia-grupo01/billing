package telemetry

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}

		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		ctx, span := otel.Tracer(tracerName).Start(ctx, c.Request.Method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Request.Method),
				attribute.String("http.route", route),
			),
		)
		defer span.End()

		c.Request = c.Request.WithContext(ctx)
		c.Next()

		status := c.Writer.Status()
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
}
