package logging

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	EnvShipURL     = "LOG_SHIP_URL"
	EnvShipKey     = "LOG_SHIP_API_KEY"
	envOTLPHeaders = "OTEL_EXPORTER_OTLP_HEADERS"
	envServiceName = "OTEL_SERVICE_NAME"
)

type shipperConfig struct {
	URL            string
	APIKey         string
	Service        string
	MaxBatch       int
	MaxQueue       int
	FlushInterval  time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	RequestTimeout time.Duration
}

func shipperConfigFromEnv(service string) (shipperConfig, bool) {
	url := strings.TrimSpace(os.Getenv(EnvShipURL))
	key := strings.TrimSpace(os.Getenv(EnvShipKey))
	if key == "" {
		key = apiKeyFromHeaders(os.Getenv(envOTLPHeaders))
	}
	if url == "" || key == "" {
		return shipperConfig{}, false
	}
	if name := strings.TrimSpace(os.Getenv(envServiceName)); name != "" {
		service = name
	}
	return shipperConfig{
		URL:            url,
		APIKey:         key,
		Service:        service,
		MaxBatch:       200,
		MaxQueue:       5000,
		FlushInterval:  5 * time.Second,
		BaseBackoff:    time.Second,
		MaxBackoff:     time.Minute,
		RequestTimeout: 10 * time.Second,
	}, true
}

func apiKeyFromHeaders(headers string) string {
	for _, pair := range strings.Split(headers, ",") {
		name, value, found := strings.Cut(pair, "=")
		if found && strings.EqualFold(strings.TrimSpace(name), "api-key") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type outcome int

const (
	delivered outcome = iota
	retryable
	rejected
)

type shipper struct {
	config  shipperConfig
	client  *http.Client
	console *slog.Logger

	mu      sync.Mutex
	queue   [][]byte
	dropped int

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
}

func newShipper(config shipperConfig, console *slog.Logger) *shipper {
	s := &shipper{
		config:  config,
		client:  &http.Client{Timeout: config.RequestTimeout},
		console: console,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *shipper) Write(line []byte) (int, error) {
	entry := bytes.TrimRight(append([]byte(nil), line...), "\n")

	s.mu.Lock()
	if len(s.queue) >= s.config.MaxQueue {
		s.queue[0] = nil
		s.queue = s.queue[1:]
		s.dropped++
	}
	s.queue = append(s.queue, entry)
	full := len(s.queue) >= s.config.MaxBatch
	s.mu.Unlock()

	if full {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return len(line), nil
}

func (s *shipper) run() {
	defer close(s.stopped)

	failures := 0
	timer := time.NewTimer(s.config.FlushInterval)
	defer timer.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
			if failures > 0 {
				continue
			}
		case <-timer.C:
		}

		var delay time.Duration
		failures, delay = s.flush(failures)

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
}

func (s *shipper) flush(failures int) (int, time.Duration) {
	batch := s.take()
	if len(batch) == 0 {
		return failures, s.config.FlushInterval
	}

	result, reason := s.post(context.Background(), batch)
	switch result {
	case delivered:
		s.recover(failures)
		if s.pending() >= s.config.MaxBatch {
			return 0, 0
		}
		return 0, s.config.FlushInterval
	case retryable:
		s.requeue(batch)
		if failures == 0 {
			s.console.Warn("log_shipping_failed", "reason", reason)
		}
		return failures + 1, s.backoff(failures + 1)
	default:
		s.console.Warn("log_batch_discarded", "reason", reason, "count", len(batch))
		return 0, s.config.FlushInterval
	}
}

func (s *shipper) take() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := min(len(s.queue), s.config.MaxBatch)
	batch := make([][]byte, count)
	copy(batch, s.queue[:count])
	for i := 0; i < count; i++ {
		s.queue[i] = nil
	}
	s.queue = s.queue[count:]
	return batch
}

func (s *shipper) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *shipper) requeue(batch [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue := append(append(make([][]byte, 0, len(batch)+len(s.queue)), batch...), s.queue...)
	if overflow := len(queue) - s.config.MaxQueue; overflow > 0 {
		queue = queue[overflow:]
		s.dropped += overflow
	}
	s.queue = queue
}

func (s *shipper) recover(failures int) {
	s.mu.Lock()
	dropped := s.dropped
	s.dropped = 0
	s.mu.Unlock()

	switch {
	case failures > 0:
		s.console.Info("log_shipping_recovered", "dropped", dropped)
	case dropped > 0:
		s.console.Warn("log_entries_dropped", "count", dropped)
	}
}

func (s *shipper) backoff(failures int) time.Duration {
	delay := s.config.BaseBackoff
	for i := 1; i < failures && delay < s.config.MaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, s.config.MaxBackoff)
}

func (s *shipper) shutdown(ctx context.Context) {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.stopped

	for ctx.Err() == nil {
		batch := s.take()
		if len(batch) == 0 {
			return
		}
		if result, _ := s.post(ctx, batch); result != delivered {
			return
		}
	}
}

type shippedLog struct {
	Timestamp  int64                      `json:"timestamp"`
	Message    string                     `json:"message"`
	Attributes map[string]json.RawMessage `json:"attributes"`
}

type shippedCommon struct {
	Attributes map[string]string `json:"attributes"`
}

type shippedPayload struct {
	Common shippedCommon `json:"common"`
	Logs   []shippedLog  `json:"logs"`
}

func toLog(line []byte) (shippedLog, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return shippedLog{}, false
	}

	entry := shippedLog{Timestamp: time.Now().UnixMilli(), Attributes: make(map[string]json.RawMessage, len(fields))}
	for key, value := range fields {
		switch key {
		case "message":
			_ = json.Unmarshal(value, &entry.Message)
		case "time":
			var text string
			if json.Unmarshal(value, &text) == nil {
				if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
					entry.Timestamp = parsed.UnixMilli()
				}
			}
		case "service":
		case "trace_id":
			entry.Attributes["trace.id"] = value
		case "span_id":
			entry.Attributes["span.id"] = value
		default:
			entry.Attributes[key] = value
		}
	}
	return entry, true
}

func (s *shipper) body(batch [][]byte) ([]byte, error) {
	payload := shippedPayload{
		Common: shippedCommon{Attributes: map[string]string{
			"service.name": s.config.Service,
			"entity.name":  s.config.Service,
		}},
		Logs: make([]shippedLog, 0, len(batch)),
	}
	for _, line := range batch {
		if entry, ok := toLog(line); ok {
			payload.Logs = append(payload.Logs, entry)
		}
	}

	raw, err := json.Marshal([]shippedPayload{payload})
	if err != nil {
		return nil, err
	}

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func (s *shipper) post(ctx context.Context, batch [][]byte) (outcome, string) {
	body, err := s.body(batch)
	if err != nil {
		return rejected, "encode_failed"
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.URL, bytes.NewReader(body))
	if err != nil {
		return rejected, "bad_request"
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Api-Key", s.config.APIKey)

	response, err := s.client.Do(request)
	if err != nil {
		return retryable, "request_failed"
	}
	defer response.Body.Close()

	status := response.StatusCode
	switch {
	case status >= 200 && status < 300:
		return delivered, ""
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return retryable, fmt.Sprintf("status_%d", status)
	default:
		return rejected, fmt.Sprintf("status_%d", status)
	}
}
