package httpapi

import (
	"testing"
	"time"

	"github.com/taciturnaxolotl/lard/internal/pipeline"
)

// TestConsolidationJobProgress covers the pub-sub contract the /consolidate
// handler streams from: listeners get every event, buffered delivery means a
// slow listener never blocks the pass, and unsubscribe stops delivery.
func TestConsolidationJobProgress(t *testing.T) {
	job := &consolidationJob{done: make(chan struct{})}

	events := job.subscribe()
	job.publish(pipeline.ProgressEvent{Phase: "extract", Name: "sess-1", Done: 1, Total: 2})
	job.publish(pipeline.ProgressEvent{Phase: "extract", Name: "sess-2", Done: 2, Total: 2})

	for i, want := range []string{"sess-1", "sess-2"} {
		select {
		case ev := <-events:
			if ev.Name != want {
				t.Fatalf("event %d: got %q, want %q", i, ev.Name, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d: timed out", i)
		}
	}

	// Unsubscribing stops delivery.
	job.unsubscribe(events)
	job.publish(pipeline.ProgressEvent{Phase: "synthesize", Name: "areas/x", Done: 1, Total: 1})
	select {
	case ev := <-events:
		t.Fatalf("got event after unsubscribe: %+v", ev)
	default:
	}
}

// TestConsolidationJobDrain covers the finish drain: events published just
// before the pass ends are still readable from the buffered channel after
// done closes, so the summary line is always last.
func TestConsolidationJobDrain(t *testing.T) {
	job := &consolidationJob{done: make(chan struct{})}
	events := job.subscribe()

	job.publish(pipeline.ProgressEvent{Phase: "synthesize", Name: "areas/x", Done: 1, Total: 1})
	job.res = pipeline.Result{Extracted: 1, Synthesized: 1}
	close(job.done)

	select {
	case ev := <-events:
		if ev.Name != "areas/x" {
			t.Fatalf("drained the wrong event: %+v", ev)
		}
	default:
		t.Fatal("buffered event lost on finish")
	}
}
