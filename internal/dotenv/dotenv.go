// Package dotenv loads a .env file into the process environment.
//
// It lives on its own rather than inside a consumer because several unrelated
// settings (auth, LLM credentials, paths) come from the same file, and loading
// it as a side effect of one of them makes the others depend on call order.
// Load it once, first, before reading any configuration.
package dotenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Load reads KEY=VALUE pairs from the given file into the environment without
// overriding variables already set. Best effort: a missing or malformed file
// never blocks boot.
func Load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" || os.Getenv(k) != "" {
			continue
		}
		_ = os.Setenv(k, v)
	}
}

// LoadDefault loads .env from the working directory, then from the user's
// lard config directory. Working directory wins, so running inside a checkout
// keeps using that checkout's file.
func LoadDefault() {
	Load(".env")
	if d, err := os.UserConfigDir(); err == nil {
		Load(filepath.Join(d, "lard", ".env"))
	}
}
