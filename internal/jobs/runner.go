// Package jobs runs billing's periodic work inside the API process.
package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Job is one periodic task. Run gets a context that ends after Interval, so
// a slow run never overlaps the next one. It reports how many items it
// handled, for the logs.
type Job struct {
	Name     string
	Interval time.Duration
	Run      func(ctx context.Context) (int, error)
}

// Start runs each job right away and then every Interval, until ctx ends.
// The returned function waits for the runs in progress to finish.
//
// Several instances can run the same jobs: every use case behind them is
// idempotent, and ExpireDue splits the rows with SKIP LOCKED.
func Start(ctx context.Context, logger *slog.Logger, jobs ...Job) (wait func()) {
	if logger == nil {
		logger = slog.Default()
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx, logger.With("job", job.Name), job)
		}()
	}
	return wg.Wait
}

func loop(ctx context.Context, log *slog.Logger, job Job) {
	ticker := time.NewTicker(job.Interval)
	defer ticker.Stop()
	for {
		runOnce(ctx, log, job)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runOnce(ctx context.Context, log *slog.Logger, job Job) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("job panicked", "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, job.Interval)
	defer cancel()

	start := time.Now()
	n, err := job.Run(ctx)
	if err != nil {
		log.Error("job failed", "error", err, "duration", time.Since(start))
		return
	}
	if n > 0 {
		log.Info("job done", "items", n, "duration", time.Since(start))
	}
}
