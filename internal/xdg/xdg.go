// Package xdg resolves lard's config directory.
//
// Go's os.UserConfigDir returns ~/Library/Application Support on macOS, which
// is right for GUI apps and wrong for a CLI: every other tool in a terminal
// keeps its dotfiles in ~/.config. This package follows the XDG basedir spec
// on every platform so the path is predictable and easy to type.
package xdg

import (
	"os"
	"path/filepath"
)

// ConfigDir returns lard's config directory, honoring XDG_CONFIG_HOME.
func ConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "lard")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "lard")
	}
	// Last resort: the working directory, so nothing silently vanishes.
	return "lard"
}

// ConfigPath joins name onto the config directory.
func ConfigPath(name string) string {
	return filepath.Join(ConfigDir(), name)
}

// DataDir returns lard's data directory, honoring XDG_DATA_HOME. The database
// and memory files live here rather than next to config, since they are state
// a user backs up rather than settings a user edits.
func DataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "lard")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "lard")
	}
	return "lard"
}

// DataPath joins name onto the data directory.
func DataPath(name string) string {
	return filepath.Join(DataDir(), name)
}
