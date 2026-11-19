package circuitbreaker_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discordia-grupo01/billing/internal/circuitbreaker"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var errBoom = errors.New("boom")

func newBreaker(c *clock, threshold int, changes *[]string) *circuitbreaker.Breaker {
	return circuitbreaker.New(circuitbreaker.Config{
		Name:             "test",
		FailureThreshold: threshold,
		OpenTimeout:      30 * time.Second,
		Now:              c.Now,
		OnStateChange: func(name string, from, to circuitbreaker.State) {
			if changes != nil {
				*changes = append(*changes, from.String()+">"+to.String())
			}
		},
	})
}

func fail(b *circuitbreaker.Breaker) error {
	return b.Execute(func() error { return errBoom })
}

func succeed(b *circuitbreaker.Breaker) error {
	return b.Execute(func() error { return nil })
}

func TestOpensAfterConsecutiveFailures(t *testing.T) {
	var changes []string
	b := newBreaker(newClock(), 3, &changes)

	for i := 0; i < 2; i++ {
		if err := fail(b); !errors.Is(err, errBoom) {
			t.Fatalf("call %d: got %v", i, err)
		}
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state after 2 failures = %v", b.State())
	}
	_ = fail(b)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("state after 3 failures = %v", b.State())
	}
	if len(changes) != 1 || changes[0] != "closed>open" {
		t.Fatalf("changes = %v", changes)
	}
}

func TestASuccessResetsTheCount(t *testing.T) {
	b := newBreaker(newClock(), 3, nil)

	_ = fail(b)
	_ = fail(b)
	_ = succeed(b)
	_ = fail(b)
	_ = fail(b)

	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v, want closed", b.State())
	}
}

func TestOpenRejectsWithoutCallingThrough(t *testing.T) {
	b := newBreaker(newClock(), 1, nil)
	_ = fail(b)

	called := false
	err := b.Execute(func() error { called = true; return nil })

	if !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
	if called {
		t.Fatal("the protected call ran while the circuit was open")
	}
}

func TestHalfOpenProbeClosesOnSuccess(t *testing.T) {
	var changes []string
	c := newClock()
	b := newBreaker(c, 1, &changes)
	_ = fail(b)

	c.Advance(29 * time.Second)
	if err := succeed(b); !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Fatalf("before the timeout: %v", err)
	}

	c.Advance(2 * time.Second)
	if b.State() != circuitbreaker.HalfOpen {
		t.Fatalf("state = %v, want half_open", b.State())
	}
	if err := succeed(b); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v, want closed", b.State())
	}
	want := []string{"closed>open", "open>half_open", "half_open>closed"}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v", changes)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Fatalf("changes = %v, want %v", changes, want)
		}
	}
}

func TestFailedProbeReopensForAnotherFullTimeout(t *testing.T) {
	c := newClock()
	b := newBreaker(c, 1, nil)
	_ = fail(b)

	c.Advance(31 * time.Second)
	_ = fail(b)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("state = %v, want open", b.State())
	}

	c.Advance(29 * time.Second)
	if err := succeed(b); !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Fatalf("the timeout should restart after a failed probe: %v", err)
	}
}

func TestOnlyOneProbeAtATime(t *testing.T) {
	c := newClock()
	b := newBreaker(c, 1, nil)
	_ = fail(b)
	c.Advance(31 * time.Second)

	done, err := b.Allow()
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if _, err := b.Allow(); !errors.Is(err, circuitbreaker.ErrOpen) {
		t.Fatalf("second concurrent call: %v", err)
	}
	done(circuitbreaker.Success)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("after the probe closed the circuit: %v", err)
	}
}

func TestAnIgnoredProbeLetsTheNextOneThrough(t *testing.T) {
	c := newClock()
	b := newBreaker(c, 1, nil)
	_ = fail(b)
	c.Advance(31 * time.Second)

	done, _ := b.Allow()
	done(circuitbreaker.Ignore)

	if b.State() != circuitbreaker.HalfOpen {
		t.Fatalf("state = %v, want half_open", b.State())
	}
	if _, err := b.Allow(); err != nil {
		t.Fatalf("next probe: %v", err)
	}
}

func TestCancelledCallsDoNotCount(t *testing.T) {
	b := newBreaker(newClock(), 2, nil)

	for i := 0; i < 5; i++ {
		_ = b.Execute(func() error { return context.Canceled })
	}

	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v, want closed", b.State())
	}
}

func TestTheOutcomeIsRecordedOnlyOnce(t *testing.T) {
	b := newBreaker(newClock(), 2, nil)
	done, _ := b.Allow()
	done(circuitbreaker.Failure)
	done(circuitbreaker.Failure)
	done(circuitbreaker.Failure)

	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v, want closed", b.State())
	}
}

func TestDefaults(t *testing.T) {
	b := circuitbreaker.New(circuitbreaker.Config{Name: "defaults"})
	for i := 0; i < circuitbreaker.DefaultFailureThreshold-1; i++ {
		_ = fail(b)
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v", b.State())
	}
	_ = fail(b)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("state = %v", b.State())
	}
	if b.Name() != "defaults" {
		t.Fatalf("name = %q", b.Name())
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	b := circuitbreaker.New(circuitbreaker.Config{Name: "race", FailureThreshold: 3, OpenTimeout: time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = b.Execute(func() error {
					if (i+j)%3 == 0 {
						return errBoom
					}
					return nil
				})
				_ = b.State()
			}
		}(i)
	}
	wg.Wait()
}

func TestStateStrings(t *testing.T) {
	cases := map[circuitbreaker.State]string{
		circuitbreaker.Closed:   "closed",
		circuitbreaker.Open:     "open",
		circuitbreaker.HalfOpen: "half_open",
	}
	for state, want := range cases {
		if state.String() != want {
			t.Errorf("%d.String() = %q, want %q", state, state.String(), want)
		}
	}
}

func TestHTTPClientCountsServerFailuresButNotClientErrors(t *testing.T) {
	status := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	do := func(client *circuitbreaker.HTTPClient) (int, error) {
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}

	clientErrors := circuitbreaker.NewHTTPClient(newBreaker(newClock(), 2, nil), http.DefaultClient)
	status.Store(http.StatusNotFound)
	for i := 0; i < 5; i++ {
		if code, err := do(clientErrors); err != nil || code != http.StatusNotFound {
			t.Fatalf("call %d: %d %v", i, code, err)
		}
	}

	for _, failing := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		breaker := newBreaker(newClock(), 2, nil)
		client := circuitbreaker.NewHTTPClient(breaker, http.DefaultClient)
		status.Store(int32(failing))
		_, _ = do(client)
		_, _ = do(client)
		if breaker.State() != circuitbreaker.Open {
			t.Fatalf("status %d did not open the circuit", failing)
		}
		if _, err := do(client); !errors.Is(err, circuitbreaker.ErrOpen) {
			t.Fatalf("status %d: err = %v, want ErrOpen", failing, err)
		}
	}
}

func TestHTTPClientCountsNetworkErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	breaker := newBreaker(newClock(), 2, nil)
	client := circuitbreaker.NewHTTPClient(breaker, http.DefaultClient)
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if _, err := client.Do(req); err == nil {
			t.Fatal("expected a network error")
		}
	}

	if breaker.State() != circuitbreaker.Open {
		t.Fatalf("state = %v, want open", breaker.State())
	}
}

func TestHTTPClientDoesNotCountCancelledRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	breaker := newBreaker(newClock(), 1, nil)
	client := circuitbreaker.NewHTTPClient(breaker, http.DefaultClient)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("expected a cancellation error")
	}

	if breaker.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v, want closed", breaker.State())
	}
}
