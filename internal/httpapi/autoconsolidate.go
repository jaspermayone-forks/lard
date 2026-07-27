package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Consolidation cadence. Sessions arrive in bursts (a laptop wakes up and
// uploads an afternoon's work), so consolidating on every ingest would burn
// API calls on a queue that is still filling. Instead each ingest resets a
// short quiet timer, and the pass runs once the uploads stop.
const (
	// DefaultConsolidateAfter is how long the queue must be quiet.
	DefaultConsolidateAfter = 5 * time.Minute
	// DefaultConsolidateMaxWait caps the total delay, so a machine uploading
	// continuously still gets consolidated instead of deferring forever.
	DefaultConsolidateMaxWait = 30 * time.Minute
)

// autoConsolidator runs a consolidation pass once ingests go quiet.
//
// It guarantees only one pass runs at a time. A trigger arriving mid-pass is
// remembered and starts a fresh wait afterwards, so sessions uploaded during a
// long pass are never dropped on the floor.
type autoConsolidator struct {
	after   time.Duration
	maxWait time.Duration
	run     func(context.Context) error

	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time // hard cap for the current burst
	running  bool
	pending  bool // a trigger arrived while a pass was running
}

func newAutoConsolidator(after, maxWait time.Duration, run func(context.Context) error) *autoConsolidator {
	if after <= 0 {
		after = DefaultConsolidateAfter
	}
	if maxWait < after {
		maxWait = after
	}
	return &autoConsolidator{after: after, maxWait: maxWait, run: run}
}

// Trigger notes that new sessions landed and (re)starts the quiet timer.
func (a *autoConsolidator) Trigger() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		a.pending = true
		return
	}
	now := time.Now()
	if a.deadline.IsZero() {
		a.deadline = now.Add(a.maxWait)
	}
	// Wait for quiet, but never past the burst deadline.
	wait := a.after
	if left := time.Until(a.deadline); left < wait {
		wait = max(left, 0)
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timer = time.AfterFunc(wait, a.fire)
	slog.Debug("consolidate: scheduled", "in", wait)
}

// Stop cancels any pending pass. Safe to call more than once.
func (a *autoConsolidator) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

func (a *autoConsolidator) fire() {
	a.mu.Lock()
	if a.running {
		a.pending = true
		a.mu.Unlock()
		return
	}
	a.running = true
	a.timer = nil
	a.deadline = time.Time{}
	a.mu.Unlock()

	slog.Info("consolidate: starting scheduled pass")
	if err := a.run(context.Background()); err != nil {
		slog.Error("consolidate: scheduled pass failed", "error", err)
	}

	a.mu.Lock()
	a.running = false
	again := a.pending
	a.pending = false
	a.mu.Unlock()

	// Sessions arrived mid-pass; wait out another quiet period for them.
	if again {
		a.Trigger()
	}
}
