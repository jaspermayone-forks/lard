// Package collector serves the OAuth client identity that edge collectors
// adopt when logging in.
//
// A collector cannot invent its own client id: the authorization server
// decides which clients exist, and lard decides which clients it trusts, so a
// client id guessed by the collector is one the server will reject. Instead
// the server publishes the identity to use, and the collector adopts it, then
// runs the OAuth device authorization grant (RFC 8628) against the
// authorization server directly. Login, exchange, and refresh all happen
// provider-side; this package is just the registration document.
package collector

import (
	"encoding/json"
	"net/http"
)

// DefaultScopes is what a collector asks for: enough to identify the user.
var DefaultScopes = []string{"profile"}

// Config describes the collector OAuth registration this server hands out.
type Config struct {
	// ClientID is the OAuth client collectors authenticate as. Empty means no
	// registration is published.
	ClientID string
	// Scopes the collector should request.
	Scopes []string
}

// Configured reports whether a registration is published.
func (c Config) Configured() bool { return c.ClientID != "" }

func (c Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return DefaultScopes
}

// Registration is the document a collector fetches before logging in.
type Registration struct {
	ClientID string   `json:"clientId"`
	Scopes   []string `json:"scopes"`
}

// Handler serves the collector registration.
type Handler struct {
	cfg Config
}

// New builds the handler.
func New(cfg Config) *Handler {
	return &Handler{cfg: cfg}
}

// Register serves GET: the registration a collector should adopt.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Configured() {
		http.Error(w, `{"error":"no collector registration configured"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(Registration{
		ClientID: h.cfg.ClientID,
		Scopes:   h.cfg.scopes(),
	})
}
