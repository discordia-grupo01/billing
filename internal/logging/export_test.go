package logging

import (
	"context"
	"io"
	"log/slog"
)

type ShipperConfig = shipperConfig

type Shipper = shipper

var (
	ConfigFromEnv     = shipperConfigFromEnv
	APIKeyFromHeaders = apiKeyFromHeaders
)

func NewShipping(service string, level slog.Level, out io.Writer, config ShipperConfig) (*slog.Logger, *Shipper) {
	console := New(service, level, out)
	s := newShipper(config, console)
	return build(service, level, out, s), s
}

func (s *shipper) Shutdown(ctx context.Context) { s.shutdown(ctx) }

func (s *shipper) QueueLen() int { return s.pending() }

func (s *shipper) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}
