package collector

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// Device-flow timings, following RFC 8628's guidance.
const (
	// DeviceCodeTTL is how long the user has to finish authorizing.
	DeviceCodeTTL = 10 * time.Minute
	// DevicePollInterval is the minimum gap between polls the client is told
	// to respect.
	DevicePollInterval = 2 * time.Second
	// devicePollFloor is enforced server-side; a client polling faster than
	// this gets slow_down rather than an answer.
	devicePollFloor = 1 * time.Second
)

// Device-flow poll errors, using RFC 8628's names so a client can branch on
// them without parsing prose.
const (
	ErrAuthorizationPending = "authorization_pending"
	ErrSlowDown             = "slow_down"
	ErrExpiredToken         = "expired_token"
	ErrAccessDenied         = "access_denied"
	ErrInvalidGrant         = "invalid_grant"
)

// deviceSession is one in-flight authorization.
//
// It is deliberately memory-only: a pending login is worth less than the ten
// minutes it lives for, and persisting half-finished credentials to disk buys
// nothing but a place for them to leak.
type deviceSession struct {
	DeviceCode string
	UserCode   string
	// State is the OAuth state parameter, which is how the callback finds its
	// way back to this session.
	State    string
	Verifier string
	Expires  time.Time

	// Filled in once the user finishes.
	Token *oauth2.Token
	Err   string

	lastPoll time.Time
}

// deviceStore holds pending device authorizations.
type deviceStore struct {
	mu       sync.Mutex
	byDevice map[string]*deviceSession
	byState  map[string]*deviceSession
	byUser   map[string]*deviceSession
}

func newDeviceStore() *deviceStore {
	return &deviceStore{
		byDevice: map[string]*deviceSession{},
		byState:  map[string]*deviceSession{},
		byUser:   map[string]*deviceSession{},
	}
}

// create mints a new session with fresh codes.
func (d *deviceStore) create(verifier string) (*deviceSession, error) {
	deviceCode, err := randomCode(32)
	if err != nil {
		return nil, err
	}
	state, err := randomCode(16)
	if err != nil {
		return nil, err
	}
	userCode, err := randomUserCode()
	if err != nil {
		return nil, err
	}
	s := &deviceSession{
		DeviceCode: deviceCode,
		UserCode:   userCode,
		State:      state,
		Verifier:   verifier,
		Expires:    time.Now().Add(DeviceCodeTTL),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked()
	d.byDevice[deviceCode] = s
	d.byState[state] = s
	d.byUser[userCode] = s
	return s, nil
}

func (d *deviceStore) byStateCode(state string) (*deviceSession, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.byState[state]
	if !ok || time.Now().After(s.Expires) {
		return nil, false
	}
	return s, true
}

func (d *deviceStore) byUserCode(code string) (*deviceSession, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.byUser[NormalizeUserCode(code)]
	if !ok || time.Now().After(s.Expires) {
		return nil, false
	}
	return s, true
}

// complete records the outcome of an authorization.
func (d *deviceStore) complete(s *deviceSession, tok *oauth2.Token, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s.Token = tok
	s.Err = errMsg
}

// poll returns the session's outcome, enforcing the poll interval and
// consuming the session once a token is handed over. Single use: a device code
// that keeps returning a token is a device code worth stealing.
func (d *deviceStore) poll(deviceCode string) (*oauth2.Token, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.byDevice[deviceCode]
	if !ok {
		return nil, ErrInvalidGrant
	}
	if time.Now().After(s.Expires) {
		d.forgetLocked(s)
		return nil, ErrExpiredToken
	}
	if !s.lastPoll.IsZero() && time.Since(s.lastPoll) < devicePollFloor {
		return nil, ErrSlowDown
	}
	s.lastPoll = time.Now()
	switch {
	case s.Token != nil:
		tok := s.Token
		d.forgetLocked(s)
		return tok, ""
	case s.Err != "":
		errMsg := s.Err
		d.forgetLocked(s)
		return nil, errMsg
	default:
		return nil, ErrAuthorizationPending
	}
}

func (d *deviceStore) forgetLocked(s *deviceSession) {
	delete(d.byDevice, s.DeviceCode)
	delete(d.byState, s.State)
	delete(d.byUser, s.UserCode)
}

// sweepLocked drops expired sessions so an abandoned login cannot accumulate.
func (d *deviceStore) sweepLocked() {
	now := time.Now()
	for _, s := range d.byDevice {
		if now.After(s.Expires) {
			d.forgetLocked(s)
		}
	}
}

// randomCode returns a URL-safe random string of n bytes of entropy.
func randomCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

// userCodeAlphabet omits characters that are easy to misread aloud or by eye
// (0/O, 1/I/L, 2/Z, 5/S, 8/B), since this code gets typed by a human.
const userCodeAlphabet = "ACDEFGHJKMNPQRTUVWXY34679"

// randomUserCode returns a short human-typable code, formatted XXXX-XXXX.
func randomUserCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, v := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeAlphabet[int(v)%len(userCodeAlphabet)])
	}
	return string(out), nil
}

// NormalizeUserCode makes a typed code comparable: upper case, no spaces, and
// dashes optional, because nobody types the dash reliably.
func NormalizeUserCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	if len(s) == 8 {
		return s[:4] + "-" + s[4:]
	}
	return s
}
