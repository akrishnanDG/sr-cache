package breaker

import (
	"testing"
	"time"
)

func TestBreaker_TripsAfterThreshold(t *testing.T) {
	b := New(3, 50*time.Millisecond, nil)
	if !b.Allow() {
		t.Fatal("expected closed breaker to allow")
	}
	b.OnFailure()
	b.OnFailure()
	if b.State() != Closed {
		t.Fatalf("expected closed after 2 failures, got %s", b.State())
	}
	b.OnFailure()
	if b.State() != Open {
		t.Fatalf("expected open after 3 failures, got %s", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker should not allow")
	}
}

func TestBreaker_HalfOpenProbe(t *testing.T) {
	b := New(1, 30*time.Millisecond, nil)
	b.OnFailure()
	if b.State() != Open {
		t.Fatalf("expected open, got %s", b.State())
	}
	time.Sleep(40 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected half-open probe to be allowed after cooldown")
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected half-open after probe, got %s", b.State())
	}
	// successful probe → closed
	b.OnSuccess()
	if b.State() != Closed {
		t.Fatalf("expected closed after success, got %s", b.State())
	}
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	b := New(1, 30*time.Millisecond, nil)
	b.OnFailure()
	time.Sleep(40 * time.Millisecond)
	_ = b.Allow() // transition to half-open
	b.OnFailure()
	if b.State() != Open {
		t.Fatalf("expected re-open after half-open failure, got %s", b.State())
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	b := New(3, time.Second, nil)
	b.OnFailure()
	b.OnFailure()
	b.OnSuccess()
	b.OnFailure()
	b.OnFailure() // total 2 fails since last success — should not trip
	if b.State() != Closed {
		t.Fatalf("expected closed, got %s", b.State())
	}
}
