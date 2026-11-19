package circuitbreaker

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var ErrOpen = errors.New("circuit breaker is open")

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

type Outcome int

const (
	Success Outcome = iota
	Failure
	Ignore
)

const (
	DefaultFailureThreshold = 5
	DefaultOpenTimeout      = 30 * time.Second
)

type Config struct {
	Name             string
	FailureThreshold int
	OpenTimeout      time.Duration
	Now              func() time.Time
	OnStateChange    func(name string, from, to State)
}

type Breaker struct {
	name          string
	threshold     int
	openTimeout   time.Duration
	now           func() time.Time
	onStateChange func(name string, from, to State)

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

func New(cfg Config) *Breaker {
	b := &Breaker{
		name:          cfg.Name,
		threshold:     cfg.FailureThreshold,
		openTimeout:   cfg.OpenTimeout,
		now:           cfg.Now,
		onStateChange: cfg.OnStateChange,
	}
	if b.threshold <= 0 {
		b.threshold = DefaultFailureThreshold
	}
	if b.openTimeout <= 0 {
		b.openTimeout = DefaultOpenTimeout
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

func (b *Breaker) Name() string {
	return b.name
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.now().Sub(b.openedAt) >= b.openTimeout {
		return HalfOpen
	}
	return b.state
}

func (b *Breaker) Allow() (func(Outcome), error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Open:
		if b.now().Sub(b.openedAt) < b.openTimeout {
			return nil, ErrOpen
		}
		b.transition(HalfOpen)
		b.probing = true
		return b.finisher(true), nil
	case HalfOpen:
		if b.probing {
			return nil, ErrOpen
		}
		b.probing = true
		return b.finisher(true), nil
	default:
		return b.finisher(false), nil
	}
}

func (b *Breaker) finisher(probe bool) func(Outcome) {
	var once sync.Once
	return func(outcome Outcome) {
		once.Do(func() { b.record(probe, outcome) })
	}
}

func (b *Breaker) record(probe bool, outcome Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if probe {
		b.probing = false
		switch outcome {
		case Success:
			b.failures = 0
			b.transition(Closed)
		case Failure:
			b.open()
		}
		return
	}

	if b.state != Closed {
		return
	}
	switch outcome {
	case Success:
		b.failures = 0
	case Failure:
		b.failures++
		if b.failures >= b.threshold {
			b.open()
		}
	}
}

func (b *Breaker) open() {
	b.openedAt = b.now()
	b.failures = 0
	b.transition(Open)
}

func (b *Breaker) transition(to State) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	if b.onStateChange != nil {
		b.onStateChange(b.name, from, to)
	}
}

func (b *Breaker) Execute(fn func() error) error {
	done, err := b.Allow()
	if err != nil {
		return err
	}
	if err := fn(); err != nil {
		if errors.Is(err, context.Canceled) {
			done(Ignore)
		} else {
			done(Failure)
		}
		return err
	}
	done(Success)
	return nil
}

type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

type HTTPClient struct {
	breaker *Breaker
	inner   Doer
}

func NewHTTPClient(breaker *Breaker, inner Doer) *HTTPClient {
	return &HTTPClient{breaker: breaker, inner: inner}
}

func (c *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	done, err := c.breaker.Allow()
	if err != nil {
		return nil, err
	}
	resp, err := c.inner.Do(req)
	switch {
	case err != nil && errors.Is(err, context.Canceled):
		done(Ignore)
	case err != nil:
		done(Failure)
	case IsServerFailure(resp.StatusCode):
		done(Failure)
	default:
		done(Success)
	}
	return resp, err
}

func IsServerFailure(status int) bool {
	return status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
}
