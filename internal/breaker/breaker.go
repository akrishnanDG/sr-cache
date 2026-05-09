package breaker

import (
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	}
	return "unknown"
}

// Breaker is a small circuit breaker tuned for upstream 429 handling.
// Trip on N consecutive failures; cool down for OpenDuration; allow one probe.
type Breaker struct {
	mu             sync.Mutex
	state          State
	failures       int
	openedAt       time.Time
	failThreshold  int
	openDuration   time.Duration
	onStateChange  func(State)
}

func New(failThreshold int, openDuration time.Duration, onStateChange func(State)) *Breaker {
	return &Breaker{
		state:         Closed,
		failThreshold: failThreshold,
		openDuration:  openDuration,
		onStateChange: onStateChange,
	}
}

// Allow reports whether a new upstream call may proceed.
// In Open state, transitions to HalfOpen after openDuration and allows one probe.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if time.Since(b.openedAt) >= b.openDuration {
			b.transition(HalfOpen)
			return true
		}
		return false
	case HalfOpen:
		// only the goroutine that flipped to half-open should proceed; others wait
		return false
	}
	return false
}

func (b *Breaker) OnSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	if b.state != Closed {
		b.transition(Closed)
	}
}

// OnFailure should be called only for failures that should trip the breaker
// (e.g. upstream 429). Normal 4xx like 404 should NOT call this.
func (b *Breaker) OnFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	switch b.state {
	case Closed:
		if b.failures >= b.failThreshold {
			b.openedAt = time.Now()
			b.transition(Open)
		}
	case HalfOpen:
		b.openedAt = time.Now()
		b.transition(Open)
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) transition(s State) {
	if b.state == s {
		return
	}
	b.state = s
	if s == Closed {
		b.failures = 0
	}
	if b.onStateChange != nil {
		// call without holding the lock to avoid recursion
		go b.onStateChange(s)
	}
}
