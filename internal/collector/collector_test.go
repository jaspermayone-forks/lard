package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterNotFoundWhenUnconfigured(t *testing.T) {
	w := httptest.NewRecorder()
	New(Config{}).Register(w, httptest.NewRequest(http.MethodGet, "/auth/collector", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestRegisterPublishesIdentity(t *testing.T) {
	h := New(Config{ClientID: "ikc_abc"})
	w := httptest.NewRecorder()
	h.Register(w, httptest.NewRequest(http.MethodGet, "/auth/collector", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var reg Registration
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.ClientID != "ikc_abc" {
		t.Errorf("clientId = %q", reg.ClientID)
	}
	if len(reg.Scopes) == 0 {
		t.Error("no scopes published")
	}
}

func TestRegisterPublishesConfiguredScopes(t *testing.T) {
	h := New(Config{ClientID: "ikc_abc", Scopes: []string{"profile", "email"}})
	w := httptest.NewRecorder()
	h.Register(w, httptest.NewRequest(http.MethodGet, "/auth/collector", nil))
	var reg Registration
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	if strings.Join(reg.Scopes, " ") != "profile email" {
		t.Errorf("scopes = %v", reg.Scopes)
	}
}
