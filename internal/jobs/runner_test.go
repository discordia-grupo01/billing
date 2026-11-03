package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestStartRunsRightAwayAndRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	enough := make(chan struct{})
	wait := Start(ctx, quiet, Job{Name: "count", Interval: 5 * time.Millisecond, Run: func(context.Context) (int, error) {
		if runs.Add(1) == 3 {
			close(enough)
		}
		return 1, nil
	}})

	select {
	case <-enough:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not repeat")
	}
	cancel()
	wait()
	after := runs.Load()
	time.Sleep(20 * time.Millisecond)
	if runs.Load() != after {
		t.Error("job kept running after the context ended")
	}
}

func TestStartSurvivesErrorsAndPanics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	again := make(chan struct{})
	wait := Start(ctx, nil, Job{Name: "flaky", Interval: 5 * time.Millisecond, Run: func(context.Context) (int, error) {
		switch runs.Add(1) {
		case 1:
			return 0, errors.New("boom")
		case 2:
			panic("boom")
		case 3:
			close(again)
		}
		return 0, nil
	}})

	select {
	case <-again:
	case <-time.After(2 * time.Second):
		t.Fatal("job stopped after an error or a panic")
	}
	cancel()
	wait()
}

func TestRunGetsADeadline(t *testing.T) {
	var deadline time.Time
	runOnce(context.Background(), quiet, Job{Name: "deadline", Interval: time.Minute, Run: func(ctx context.Context) (int, error) {
		deadline, _ = ctx.Deadline()
		return 0, nil
	}})
	if deadline.IsZero() || time.Until(deadline) > time.Minute {
		t.Errorf("run should end within its interval, deadline %v", deadline)
	}
}

func TestRunUsesTimeoutWhenSet(t *testing.T) {
	var deadline time.Time
	runOnce(context.Background(), quiet, Job{Name: "slow", Interval: time.Second, Timeout: time.Hour, Run: func(ctx context.Context) (int, error) {
		deadline, _ = ctx.Deadline()
		return 0, nil
	}})
	if time.Until(deadline) < 59*time.Minute {
		t.Errorf("run should get the hour of Timeout, deadline %v", deadline)
	}
}
