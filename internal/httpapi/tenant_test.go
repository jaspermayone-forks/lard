package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/taciturnaxolotl/lard/internal/auth"
	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/tenant"
	"github.com/taciturnaxolotl/lard/internal/types"
)

func multiUserServer(t *testing.T) *Server {
	t.Helper()
	s := NewMultiUser(MultiUserConfig{Layout: tenant.Layout{Root: t.TempDir()}}, nil)
	t.Cleanup(func() { s.Close() })
	return s
}

// as builds a request carrying an authenticated identity, the way the auth
// middleware hands it to the handlers.
func as(subject, method, target string, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if subject != "" {
		r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{Subject: subject}))
	}
	return r
}

func do(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestTenantsDoNotSeeEachOther(t *testing.T) {
	s := multiUserServer(t)

	w := do(s, as("https://alice.example", "POST", "/memory/topics/coffee", `{"line":"alice drinks decaf"}`))
	if w.Code != 200 {
		t.Fatalf("alice write: %d %s", w.Code, w.Body)
	}

	// Bob asks for the same path. Alice's line must not be there.
	w = do(s, as("https://bob.example", "GET", "/memory/topics/coffee", ""))
	if w.Code != 404 {
		t.Fatalf("bob read: want 404, got %d %s", w.Code, w.Body)
	}

	// And Bob's listing must be empty rather than showing alice's subject.
	w = do(s, as("https://bob.example", "GET", "/memory", ""))
	var listing []types.SubjectListing
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing: %v (%s)", err, w.Body)
	}
	if len(listing) != 0 {
		t.Fatalf("bob sees alice's memory: %+v", listing)
	}

	// Alice still has hers.
	w = do(s, as("https://alice.example", "GET", "/memory/topics/coffee", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "decaf") {
		t.Fatalf("alice read back: %d %s", w.Code, w.Body)
	}
}

func TestTenantPersistsAcrossRequests(t *testing.T) {
	s := multiUserServer(t)
	first, err := s.TenantFor(as("https://alice.example", "GET", "/", "").Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.TenantFor(as("https://alice.example", "GET", "/", "").Context())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same identity opened two stores")
	}
}

func TestUnidentifiedRequestIsRefused(t *testing.T) {
	s := multiUserServer(t)
	w := do(s, as("", "GET", "/memory", ""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for an unidentified caller, got %d %s", w.Code, w.Body)
	}
}

func TestPrimaryUserOwnsUnidentifiedRequests(t *testing.T) {
	s := NewMultiUser(MultiUserConfig{
		Layout:      tenant.Layout{Root: t.TempDir()},
		PrimaryUser: "https://krn.example",
	}, nil)
	t.Cleanup(func() { s.Close() })

	if w := do(s, as("", "POST", "/memory/topics/x", `{"line":"from a token client"}`)); w.Code != 200 {
		t.Fatalf("unidentified write: %d %s", w.Code, w.Body)
	}
	w := do(s, as("https://krn.example", "GET", "/memory/topics/x", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "token client") {
		t.Fatalf("primary user should own that write: %d %s", w.Code, w.Body)
	}
}

func TestSingleUserServerIgnoresIdentity(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "lard.db"), filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, nil)
	t.Cleanup(func() { s.Close() })

	if w := do(s, as("https://alice.example", "POST", "/memory/topics/x", `{"line":"one memory"}`)); w.Code != 200 {
		t.Fatalf("write: %d %s", w.Code, w.Body)
	}
	// No tenants in single-user mode: everyone lands on the same store.
	for _, subject := range []string{"https://bob.example", ""} {
		w := do(s, as(subject, "GET", "/memory/topics/x", ""))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "one memory") {
			t.Fatalf("subject %q: %d %s", subject, w.Code, w.Body)
		}
	}
}
