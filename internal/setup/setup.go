// Package setup is the collector's interactive first-run flow: ask where the
// server is, authenticate, and save the result.
//
// It exists so a new machine needs no flags and no copied secrets. Every
// prompt has a non-interactive equivalent, so scripts and headless boxes are
// never forced through a TUI.
package setup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"charm.land/huh/v2"
	"golang.org/x/term"

	"github.com/taciturnaxolotl/lard/internal/client"
)

// Interactive reports whether we can prompt: a TTY on both ends, and no CI
// marker. Anything else must fail with a message instead of hanging on input
// that will never arrive.
func Interactive() bool {
	if os.Getenv("CI") != "" || os.Getenv("LARD_NO_INTERACTIVE") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// Options carry anything already supplied on the command line, so the form
// only asks for what is genuinely missing.
type Options struct {
	URL          string
	Token        string
	Roots        []string
	CallbackPort int
	NoBrowser    bool
}

// Run resolves a working configuration, prompting where needed, and saves it.
func Run(ctx context.Context, opts Options) (*client.Config, error) {
	path := client.DefaultConfigPath()
	cfg, err := client.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if opts.URL != "" {
		cfg.URL = normalizeURL(opts.URL)
	}
	if len(opts.Roots) > 0 {
		cfg.Roots = opts.Roots
	}

	// LoadConfig defaults the URL to localhost, which is a fine fallback but a
	// bad thing to silently adopt on a machine whose server is elsewhere. Treat
	// an unedited default as "not yet configured" and ask.
	if opts.URL == "" && needsURL(path, cfg) {
		if !Interactive() {
			return nil, errors.New("no server configured: pass --url https://lard.example.com")
		}
		if err := askURL(cfg); err != nil {
			return nil, err
		}
	}
	if cfg.URL == "" {
		return nil, errors.New("no server URL")
	}

	if err := authenticate(ctx, cfg, opts); err != nil {
		return nil, err
	}
	if _, err := cfg.Verify(ctx); err != nil {
		return nil, err
	}
	if err := cfg.Save(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// needsURL reports whether the URL is still unset in any meaningful sense.
func needsURL(path string, cfg *client.Config) bool {
	if _, err := os.Stat(path); err == nil {
		return false // an existing file is the user's choice, default or not
	}
	return os.Getenv("LARD_URL") == ""
}

func askURL(cfg *client.Config) error {
	value := cfg.URL
	if value == "http://localhost:7477" {
		value = "" // don't pre-fill a guess the user probably doesn't want
	}
	// An explicit width is required, not cosmetic: bubbles' placeholder
	// rendering sizes a buffer from the input width and panics when that
	// width is unset.
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Where does lard live?").
				Description("The base URL of your central lard server.").
				Placeholder("https://lard.example.com").
				Value(&value).
				Validate(validateURL),
		),
	).WithWidth(formWidth())
	if err := form.Run(); err != nil {
		return err
	}
	cfg.URL = normalizeURL(value)
	return nil
}

// formWidth picks a readable form width that fits the terminal.
func formWidth() int {
	const fallback = 64
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return fallback
	}
	return min(w-4, fallback)
}

func validateURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("required")
	}
	u, err := url.Parse(normalizeURL(s))
	if err != nil {
		return errors.New("not a valid URL")
	}
	if u.Host == "" {
		return errors.New("needs a host, e.g. lard.example.com")
	}
	return nil
}

// normalizeURL fills in a scheme and trims the trailing slash, so a user can
// type a bare hostname and get something that works.
func normalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		// Assume TLS for a real host, plain HTTP for a local one.
		if strings.HasPrefix(s, "localhost") || strings.HasPrefix(s, "127.0.0.1") {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}
	return strings.TrimRight(s, "/")
}

// authenticate obtains credentials for cfg.URL, choosing the lightest path
// that works: an explicit token, then existing credentials that still verify,
// then the browser flow, then a typed token.
func authenticate(ctx context.Context, cfg *client.Config, opts Options) error {
	if opts.Token != "" {
		cfg.Token = opts.Token
		cfg.OAuth = nil
		return nil
	}
	// Already have something that works? Don't make the user re-authorize.
	if cfg.AuthMode() != "none" {
		if _, err := cfg.Verify(ctx); err == nil {
			return nil
		}
	}
	// Does the server need credentials at all?
	if _, err := cfg.Verify(ctx); err == nil {
		return nil
	}

	port := opts.CallbackPort
	if port <= 0 {
		port = client.CallbackPort
	}
	// Prefer the brokered flow when the server offers it: it needs no local
	// callback port, so it works identically over SSH, in a container, and on
	// a headless machine. Fall back to the local-listener flow otherwise.
	reg, regErr := client.FetchRegistration(ctx, cfg.URL)
	_, discErr := client.Discover(ctx, cfg.URL)
	if regErr == nil && reg.DeviceFlow {
		tok, err := client.LoginDevice(ctx, cfg.URL, !opts.NoBrowser)
		if err != nil {
			return err
		}
		cfg.OAuth = &client.OAuthToken{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			Expiry:       tok.Expiry,
			CallbackPort: 0, // brokered: no local callback involved
		}
		cfg.Token = ""
		return nil
	}
	if discErr == nil {
		tok, err := client.Login(ctx, cfg.URL, port, !opts.NoBrowser)
		if err != nil {
			return err
		}
		cfg.OAuth = &client.OAuthToken{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			Expiry:       tok.Expiry,
			CallbackPort: port,
		}
		cfg.Token = ""
		if tok.RefreshToken == "" {
			fmt.Fprintln(os.Stderr, "note: no refresh token issued; you will need to log in again when this expires")
		}
		return nil
	}

	// No OAuth on offer, so the server wants a shared secret.
	if !Interactive() {
		return fmt.Errorf("%s needs a token and does not offer OAuth; pass --token", cfg.URL)
	}
	var token string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Access token").
				Description(fmt.Sprintf("%s does not offer a browser login.\nPaste the server's LARD_TOKEN.", cfg.URL)).
				EchoMode(huh.EchoModePassword).
				Value(&token).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return errors.New("required")
					}
					return nil
				}),
		),
	).WithWidth(formWidth())
	if err := form.Run(); err != nil {
		return err
	}
	cfg.Token = strings.TrimSpace(token)
	cfg.OAuth = nil
	return nil
}
