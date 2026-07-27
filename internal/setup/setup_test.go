package setup

import "testing"

// A user types a bare hostname; the flow must produce something that works.
func TestNormalizeURLInfersScheme(t *testing.T) {
	cases := map[string]string{
		"lard.example.com":          "https://lard.example.com",
		"https://lard.example.com/": "https://lard.example.com",
		"http://lard.example.com":   "http://lard.example.com",
		"  lard.example.com  ":      "https://lard.example.com",
		"lard.example.com/":         "https://lard.example.com",
		// Local addresses almost never have TLS, so assume plain HTTP.
		"localhost:7477": "http://localhost:7477",
		"127.0.0.1:7477": "http://127.0.0.1:7477",
		"":               "",
	}
	for in, want := range cases {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	good := []string{"lard.example.com", "https://lard.example.com", "localhost:7477"}
	for _, s := range good {
		if err := validateURL(s); err != nil {
			t.Errorf("validateURL(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"", "   "}
	for _, s := range bad {
		if err := validateURL(s); err == nil {
			t.Errorf("validateURL(%q) = nil, want an error", s)
		}
	}
}

// The form must never be sized zero: bubbles' placeholder rendering allocates
// from the width and panics on a negative one.
func TestFormWidthIsPositive(t *testing.T) {
	if w := formWidth(); w <= 0 {
		t.Fatalf("formWidth() = %d, want > 0", w)
	}
}
