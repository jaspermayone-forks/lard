package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestConsolidateStream exercises the NDJSON contract /consolidate speaks:
// progress events as they arrive, then the summary line.
func TestConsolidateStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/consolidate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer tok" {
			t.Errorf("bearer token not forwarded: %q", r.Header.Get("authorization"))
		}
		w.Header().Set("content-type", "application/x-ndjson")
		fmt.Fprintln(w, `{"phase":"extract","name":"sess-1","done":1,"total":2}`)
		fmt.Fprintln(w, `{"phase":"extract","name":"sess-2","done":2,"total":2}`)
		fmt.Fprintln(w, `{"phase":"synthesize","name":"areas/crush","done":1,"total":1}`)
		fmt.Fprintln(w, `{"finished":true,"extracted":2,"synthesized":1}`)
	}))
	defer srv.Close()

	up := NewUploader(srv.URL, "tok")
	var seen []string
	res, err := up.Consolidate(context.Background(), func(phase, name string, done, total int) {
		seen = append(seen, fmt.Sprintf("%s %s %d/%d", phase, name, done, total))
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Extracted != 2 || res.Synthesized != 1 {
		t.Fatalf("bad result: %+v", res)
	}
	want := []string{
		"extract sess-1 1/2",
		"extract sess-2 2/2",
		"synthesize areas/crush 1/1",
	}
	if len(seen) != len(want) {
		t.Fatalf("progress calls: %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("progress %d: got %q, want %q", i, seen[i], want[i])
		}
	}
}

// TestConsolidateError covers the failure line: the pass ended with an
// error, which surfaces instead of a result.
func TestConsolidateError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/x-ndjson")
		fmt.Fprintln(w, `{"finished":true,"error":"llm: rate limited"}`)
	}))
	defer srv.Close()

	up := NewUploader(srv.URL, "tok")
	_, err := up.Consolidate(context.Background(), nil)
	if err == nil || err.Error() != "llm: rate limited" {
		t.Fatalf("want the pass error, got %v", err)
	}
}

// TestConsolidateRefused covers a non-2xx body (e.g. 503 without an LLM):
// the server's message comes back verbatim.
func TestConsolidateRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, `{"error":"consolidation unavailable: no LLM client configured"}`)
	}))
	defer srv.Close()

	up := NewUploader(srv.URL, "tok")
	if _, err := up.Consolidate(context.Background(), nil); err == nil {
		t.Fatal("want an error for 503")
	}
}
