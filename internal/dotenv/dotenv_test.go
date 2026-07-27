package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSetsUnsetVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# comment\n\nFOO=bar\nexport BAZ=\"qux\"\nQUOTED='single'\nSPACED = padded \nNOEQUALS\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"FOO", "BAZ", "QUOTED", "SPACED"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	Load(path)
	for k, want := range map[string]string{"FOO": "bar", "BAZ": "qux", "QUOTED": "single", "SPACED": "padded"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// A real environment variable must win, so an operator can override the file
// on the command line.
func TestLoadDoesNotOverrideEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("LARD_TEST_MODE=from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LARD_TEST_MODE", "from_env")
	Load(path)
	if got := os.Getenv("LARD_TEST_MODE"); got != "from_env" {
		t.Fatalf("got %q, want the environment to win", got)
	}
}

func TestLoadMissingFileIsNotFatal(t *testing.T) {
	Load(filepath.Join(t.TempDir(), "absent"))
}
