package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeProvider is a lard server and its authorization server in one mux: the
// discovery documents, dynamic registration, and the device grant.
func fakeProvider(t *testing.T, seen *map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              srv.URL,
			"authorization_servers": []string{srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                        srv.URL,
			"authorization_endpoint":        srv.URL + "/auth/authorize",
			"token_endpoint":                srv.URL + "/auth/token",
			"device_authorization_endpoint": srv.URL + "/auth/device",
			"registration_endpoint":         srv.URL + "/oauth/register",
		})
	})
	mux.HandleFunc("/oauth/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if name, _ := body["client_name"].(string); name != "" {
			(*seen)["client_name"] = name
		}
		if _, ok := body["redirect_uris"]; ok {
			(*seen)["redirect_uris"] = "sent"
		}
		if m, _ := body["token_endpoint_auth_method"].(string); m != "" {
			(*seen)["auth_method"] = m
		}
		w.WriteHeader(http.StatusCreated)
		// A public registration: no client_secret comes back (RFC 7591 §3.2.1).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  "ikc_fresh",
			"grant_types":                body["grant_types"],
			"token_endpoint_auth_method": "none",
		})
	})
	mux.HandleFunc("/auth/device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		(*seen)["resource"] = r.PostForm.Get("resource")
		(*seen)["device_client_id"] = r.PostForm.Get("client_id")
		(*seen)["device_secret"] = r.PostForm.Get("client_secret")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev", "user_code": "WDJB-MJHT",
			"verification_uri": srv.URL + "/device", "expires_in": 600, "interval": 1,
		})
	})
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		(*seen)["poll_secret"] = r.PostForm.Get("client_secret")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "refresh_token": "rt", "token_type": "Bearer", "expires_in": 3600,
		})
	})
	return srv
}

// A machine with no credentials registers its own, and the token it asks for
// is bound to this lard by the resource indicator.
func TestLoginDeviceRegistersAndBindsAudience(t *testing.T) {
	seen := map[string]string{}
	srv := fakeProvider(t, &seen)

	tok, creds, err := LoginDevice(context.Background(), srv.URL, Credentials{}, DefaultScopes, false)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tok.AccessToken != "at" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
	if creds.ClientID != "ikc_fresh" || creds.ClientSecret != "" {
		t.Errorf("credentials not returned for saving: %+v", creds)
	}
	if seen["device_client_id"] != "ikc_fresh" {
		t.Errorf("device request used client_id %q", seen["device_client_id"])
	}
	if seen["resource"] != srv.URL {
		t.Errorf("resource indicator = %q, want %q", seen["resource"], srv.URL)
	}
	if seen["client_name"] == "" {
		t.Error("registration published no client_name, so the consent screen has nothing to show")
	}
	// A device client has nowhere to redirect, so it claims no redirect_uris
	// rather than inventing a placeholder it will never use.
	if seen["redirect_uris"] != "" {
		t.Error("registration sent redirect_uris for a device-only client")
	}
	// Registering public is what keeps a login to one command: no secret to
	// hold, and none to carry between machines.
	if seen["auth_method"] != "none" {
		t.Errorf("registered with token_endpoint_auth_method %q, want none", seen["auth_method"])
	}
	if seen["device_secret"] != "" || seen["poll_secret"] != "" {
		t.Errorf("a public client sent a secret: start=%q poll=%q",
			seen["device_secret"], seen["poll_secret"])
	}
}

// An authorization server may hand back a secret regardless of what was asked
// for, and one that does expects to see it again on every grant.
func TestLoginDeviceHonorsAnUnrequestedSecret(t *testing.T) {
	seen := map[string]string{}
	srv := fakeProvider(t, &seen)

	have := Credentials{ClientID: "ikc_known", ClientSecret: "iks_known"}
	_, _, err := LoginDevice(context.Background(), srv.URL, have, DefaultScopes, false)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if seen["device_secret"] != "iks_known" || seen["poll_secret"] != "iks_known" {
		t.Errorf("held a secret but did not present it: start=%q poll=%q",
			seen["device_secret"], seen["poll_secret"])
	}
}

// An existing registration is reused: a second login is about the grant, not
// about becoming a new client at the authorization server.
func TestLoginDeviceKeepsExistingCredentials(t *testing.T) {
	seen := map[string]string{}
	srv := fakeProvider(t, &seen)

	have := Credentials{ClientID: "ikc_known", ClientSecret: "iks_known"}
	_, creds, err := LoginDevice(context.Background(), srv.URL, have, DefaultScopes, false)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if creds != have {
		t.Errorf("credentials = %+v, want the ones passed in", creds)
	}
	if seen["client_name"] != "" {
		t.Error("re-registered a machine that already had a client identity")
	}
}

// RFC 7591 §3.2.1 lets the server register something narrower than the
// request, so a missing device grant has to surface at registration rather
// than as a puzzling refusal at the first poll.
func TestRegisterRejectsMissingDeviceGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":   "ikc_narrow",
			"grant_types": []string{"authorization_code"},
		})
	}))
	defer srv.Close()

	_, err := Register(context.Background(), srv.URL, "lard-client")
	if err == nil {
		t.Fatal("want an error when the device grant was not registered")
	}
	if !strings.Contains(err.Error(), "device grant") {
		t.Errorf("error should name the missing grant, got %v", err)
	}
}

// An authorization server explains its refusals; passing only the status code
// through leaves the user guessing at a question the server already answered.
func TestStartDeviceReportsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_client",
			"error_description": "client_secret is required for pre-registered clients",
		})
	}))
	defer srv.Close()

	_, err := startDevice(context.Background(), srv.URL, Credentials{ClientID: "ikc_x"}, "", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "client_secret is required") {
		t.Errorf("error should carry the server's description, got %v", err)
	}
}
