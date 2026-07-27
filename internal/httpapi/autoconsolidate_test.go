package httpapi

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutoConsolidateWaitsForQuiet(t *testing.T) {
	var runs atomic.Int32
	a := newAutoConsolidator(60*time.Millisecond, time.Second, func(context.Context) error {
		runs.Add(1)
		return nil
	})
	defer a.Stop()

	// A burst of ingests should collapse into a single pass.
	for range 5 {
		a.Trigger()
		time.Sleep(15 * time.Millisecond)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("ran %d times during the burst; should have waited", got)
	}
	time.Sleep(150 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs = %d, want exactly 1", got)
	}
}

// A machine uploading continuously must not defer consolidation forever.
func TestAutoConsolidateHonorsMaxWait(t *testing.T) {
	var runs atomic.Int32
	a := newAutoConsolidator(50*time.Millisecond, 120*time.Millisecond, func(context.Context) error {
		runs.Add(1)
		return nil
	})
	defer a.Stop()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		a.Trigger()
		time.Sleep(20 * time.Millisecond)
	}
	if got := runs.Load(); got == 0 {
		t.Fatal("never ran despite continuous triggers; max wait was not honored")
	}
}

// Sessions arriving mid-pass must get their own pass afterwards, not be lost.
func TestAutoConsolidateReschedulesTriggerDuringRun(t *testing.T) {
	var runs atomic.Int32
	started := make(chan struct{}, 4)
	a := newAutoConsolidator(20*time.Millisecond, time.Second, func(context.Context) error {
		started <- struct{}{}
		runs.Add(1)
		time.Sleep(60 * time.Millisecond)
		return nil
	})
	defer a.Stop()

	a.Trigger()
	<-started // first pass is now running
	a.Trigger()

	time.Sleep(250 * time.Millisecond)
	if got := runs.Load(); got < 2 {
		t.Fatalf("runs = %d, want at least 2 (the mid-pass trigger was dropped)", got)
	}
}

func TestAutoConsolidateOnlyOneAtATime(t *testing.T) {
	var concurrent, maxSeen atomic.Int32
	a := newAutoConsolidator(10*time.Millisecond, time.Second, func(context.Context) error {
		n := concurrent.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		concurrent.Add(-1)
		return nil
	})
	defer a.Stop()

	for range 10 {
		a.Trigger()
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := maxSeen.Load(); got > 1 {
		t.Fatalf("saw %d concurrent passes, want 1", got)
	}
}

// A failing pass must not wedge the scheduler.
func TestAutoConsolidateRecoversFromError(t *testing.T) {
	var runs atomic.Int32
	a := newAutoConsolidator(20*time.Millisecond, time.Second, func(context.Context) error {
		runs.Add(1)
		return context.DeadlineExceeded
	})
	defer a.Stop()

	a.Trigger()
	time.Sleep(80 * time.Millisecond)
	a.Trigger()
	time.Sleep(80 * time.Millisecond)
	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
}

func TestAutoConsolidateStopCancelsPending(t *testing.T) {
	var runs atomic.Int32
	a := newAutoConsolidator(50*time.Millisecond, time.Second, func(context.Context) error {
		runs.Add(1)
		return nil
	})
	a.Trigger()
	a.Stop()
	time.Sleep(120 * time.Millisecond)
	if got := runs.Load(); got != 0 {
		t.Fatalf("runs = %d after Stop, want 0", got)
	}
}
