package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
)

const EnvLevel = "LOG_LEVEL"

const (
	filtered      = "[FILTERED]"
	maxTextLength = 4000
)

var sensitiveKeys = map[string]struct{}{
	"password":      {},
	"token":         {},
	"access_token":  {},
	"refresh_token": {},
	"ticket":        {},
	"jwt":           {},
	"authorization": {},
	"cookie":        {},
	"secret":        {},
	"api_key":       {},
	"apikey":        {},
	"email":         {},
	"content":       {},
}

var (
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	jwtPattern   = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]*`)
)

func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("%s inválido %q: usar debug, info, warning o error", EnvLevel, name)
}

func New(service string, level slog.Level, out io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level, ReplaceAttr: replace})
	return slog.New(traceHandler{handler}).With("service", service)
}

type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, record slog.Record) error {
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, record)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

func Setup(service string) error {
	level, err := ParseLevel(os.Getenv(EnvLevel))
	if err != nil {
		return err
	}
	logger := New(service, level, os.Stdout)
	slog.SetDefault(logger)
	logger.Info("service_started", "log_level", levelName(level))
	return nil
}

func MustSetup(service string) {
	if err := Setup(service); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func Debugf(format string, args ...any) { logf(slog.LevelDebug, format, args) }

func Infof(format string, args ...any) { logf(slog.LevelInfo, format, args) }

func Warnf(format string, args ...any) { logf(slog.LevelWarn, format, args) }

func Errorf(format string, args ...any) { logf(slog.LevelError, format, args) }

func Fatalf(format string, args ...any) {
	logf(slog.LevelError, format, args)
	os.Exit(1)
}

func logf(level slog.Level, format string, args []any) {
	logger := slog.Default()
	ctx := context.Background()
	if !logger.Enabled(ctx, level) {
		return
	}
	logger.Log(ctx, level, fmt.Sprintf(format, args...))
}

func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := routeOf(c)
		status := c.Writer.Status()
		level := slog.LevelInfo
		switch {
		case status >= http.StatusInternalServerError:
			level = slog.LevelError
		case route == "/livez" || route == "/readyz":
			level = slog.LevelDebug
		}
		slog.Default().Log(c.Request.Context(), level, "http_request",
			"method", c.Request.Method,
			"route", route,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}

func Recovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		slog.Error("panic_recovered",
			"method", c.Request.Method,
			"route", routeOf(c),
			"error", fmt.Sprint(recovered),
			"stack", string(debug.Stack()),
		)
		c.AbortWithStatus(http.StatusInternalServerError)
	})
}

func routeOf(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	return "unmatched"
}

func replace(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch attr.Key {
		case slog.TimeKey:
			attr.Value = slog.StringValue(attr.Value.Time().UTC().Format(time.RFC3339Nano))
			return attr
		case slog.LevelKey:
			if level, ok := attr.Value.Any().(slog.Level); ok {
				attr.Value = slog.StringValue(levelName(level))
			}
			return attr
		case slog.MessageKey:
			attr.Key = "message"
			attr.Value = slog.StringValue(scrub(attr.Value.String()))
			return attr
		}
	}
	if _, sensitive := sensitiveKeys[strings.ToLower(attr.Key)]; sensitive {
		attr.Value = slog.StringValue(filtered)
		return attr
	}
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(scrub(attr.Value.String()))
	case slog.KindAny:
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(scrub(err.Error()))
		}
	}
	return attr
}

func scrub(text string) string {
	if len(text) > maxTextLength {
		text = text[:maxTextLength]
	}
	text = jwtPattern.ReplaceAllString(text, "[JWT]")
	return emailPattern.ReplaceAllString(text, "[EMAIL]")
}

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "error"
	case level >= slog.LevelWarn:
		return "warning"
	case level >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
