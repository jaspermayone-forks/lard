package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeLard serves the discovery chain a real lard publishes.
func fakeLard(t *testing.T) (lardURL string) {
	t.Helper()
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"issuer":"` + asBase(r) + `","authorization_endpoint":"` + asBase(r) + `/auth/authorize","token_endpoint":"` + asBase(r) + `/auth/token","code_challenge_methods_supported":["S256"]}`))
	}))
	t.Cleanup(as.Close)

	lard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"resource":"x","authorization_servers":["` + as.URL + `"]}`))
	}))
	t.Cleanup(lard.Close)
	return lard.URL
}

func asBase(r *http.Request) string { return "http://" + r.Host }

func TestDiscoverWalksResourceToAuthServer(t *testing.T) {
	eps, err := Discover(context.Background(), fakeLard(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(eps.Authorization, "/auth/authorize") {
		t.Errorf("authorization endpoint = %q", eps.Authorization)
	}
	if !strings.HasSuffix(eps.Token, "/auth/token") {
		t.Errorf("token endpoint = %q", eps.Token)
	}
	if eps.Issuer == "" {
		t.Error("issuer empty")
	}
}

// A server with auth off publishes nothing, and the client must say so plainly
// rather than opening a browser at a nonexistent endpoint.
func TestDiscoverFailsWhenServerHasNoOAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()
	if _, err := Discover(context.Background(), srv.URL); err == nil {
		t.Fatal("want an error when discovery is unavailable")
	}
}

// The client id must be derived from the callback port, since indiko rejects a
// client id whose host differs from the redirect host.
func TestClientIDMatchesCallbackHost(t *testing.T) {
	if got := ClientID(40714); got != "http://localhost:40714/" {
		t.Fatalf("got %q", got)
	}
}

// Over SSH a local browser is the wrong browser, so the flow must know the
// difference and lean on the printed URL instead.
func TestIsLocalDetectsSSH(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_CLIENT", "")
	t.Setenv("SSH_TTY", "")
	os.Unsetenv("SSH_CONNECTION")
	os.Unsetenv("SSH_CLIENT")
	os.Unsetenv("SSH_TTY")
	if !isLocal() {
		t.Error("no SSH vars should read as local")
	}
	for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, "set")
			if isLocal() {
				t.Errorf("%s set should read as remote", k)
			}
		})
	}
}

func TestListenPortReportsBoundPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := listenPort(ln); got <= 0 {
		t.Fatalf("listenPort = %d, want a real port", got)
	}
}

// listenAny must fall through to the next port rather than failing, so a
// leftover process on one port does not block login.
func TestListenAnyFallsThroughBusyPorts(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	ln, port, err := listenAny([]int{busyPort, 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if port == busyPort {
		t.Fatalf("bound the busy port %d", busyPort)
	}
}

func TestListenAnyFailsWhenAllBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	p := busy.Addr().(*net.TCPAddr).Port
	if _, _, err := listenAny([]int{p}); err == nil {
		t.Fatal("want an error when every port is taken")
	}
}

// The registration's redirect URIs are the source of truth for which ports the
// server will accept, so parsing them must not silently drop any.
func TestRegistrationPorts(t *testing.T) {
	reg := &Registration{RedirectURIs: []string{
		"http://localhost:40714/callback",
		"http://localhost:40715/callback",
		"not a url at all::",
	}}
	got := reg.Ports()
	if len(got) != 2 || got[0] != 40714 || got[1] != 40715 {
		t.Fatalf("Ports() = %v", got)
	}
}
