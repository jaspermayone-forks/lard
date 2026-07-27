// Package service installs the collector as a background agent so it keeps
// running across logins and reboots without the user writing plist XML.
//
// macOS only for now. Linux (systemd user units) is the obvious next step, and
// the Install/Uninstall/Status shape is deliberately platform-neutral so
// adding it means one more file rather than reshaping callers.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Label is the launchd job label, also used for the plist filename.
const Label = "sh.dunkirk.lard-client"

// Options describe the agent to install.
type Options struct {
	// Binary is the absolute path to lard-client.
	Binary string
	// Interval between sync passes.
	Interval time.Duration
	// Roots to scan for .crush directories. Empty means the client's own
	// default (~/code).
	Roots []string
	// LogDir holds stdout/stderr. Defaults to ~/Library/Logs.
	LogDir string
}

// Supported reports whether service management works on this platform.
func Supported() bool { return runtime.GOOS == "darwin" }

// PlistPath is where the LaunchAgent definition lives.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// Install writes the LaunchAgent and loads it. Re-running is safe: the old job
// is unloaded first, so this doubles as "upgrade to the current binary".
func Install(opts Options) (plistPath string, err error) {
	if !Supported() {
		return "", fmt.Errorf("service install is only implemented for macOS; run 'lard-client daemon' under your init system instead")
	}
	if opts.Binary == "" {
		return "", fmt.Errorf("binary path required")
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.LogDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		opts.LogDir = filepath.Join(home, "Library", "Logs")
	}
	if err := os.MkdirAll(opts.LogDir, 0o755); err != nil {
		return "", err
	}
	path, err := PlistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Unload any previous version first; ignore the error since a missing job
	// is the normal first-install case.
	_ = unload(path)
	if err := os.WriteFile(path, []byte(renderPlist(opts)), 0o644); err != nil {
		return "", err
	}
	if err := load(path); err != nil {
		return path, err
	}
	return path, nil
}

// Uninstall stops the agent and removes its definition.
func Uninstall() error {
	if !Supported() {
		return fmt.Errorf("service management is only implemented for macOS")
	}
	path, err := PlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("not installed (no %s)", path)
	}
	_ = unload(path)
	return os.Remove(path)
}

// Status reports whether the agent is installed and currently loaded.
func Status() (installed, loaded bool, detail string, err error) {
	if !Supported() {
		return false, false, "", fmt.Errorf("service management is only implemented for macOS")
	}
	path, err := PlistPath()
	if err != nil {
		return false, false, "", err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return false, false, "", nil
	}
	out, listErr := exec.Command("launchctl", "list", Label).CombinedOutput()
	if listErr != nil {
		return true, false, "", nil
	}
	return true, true, parseLaunchctlList(string(out)), nil
}

// parseLaunchctlList pulls the PID and last exit status out of launchctl's
// plist-ish output, which is more useful to a human than the raw dump.
func parseLaunchctlList(out string) string {
	var pid, status string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, `"PID" = `):
			pid = strings.Trim(strings.TrimPrefix(line, `"PID" = `), ";")
		case strings.HasPrefix(line, `"LastExitStatus" = `):
			status = strings.Trim(strings.TrimPrefix(line, `"LastExitStatus" = `), ";")
		}
	}
	switch {
	case pid != "":
		return "running, pid " + pid
	case status != "" && status != "0":
		return "not running, last exit status " + status
	default:
		return "loaded, waiting for next interval"
	}
}

func load(path string) error {
	if out, err := exec.Command("launchctl", "load", "-w", path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func unload(path string) error {
	return exec.Command("launchctl", "unload", "-w", path).Run()
}

// renderPlist builds the LaunchAgent definition. StartInterval handles the
// schedule, so the daemon runs one pass and exits rather than sleeping in a
// loop: launchd is a better timekeeper than a long-lived process that a laptop
// suspend can stall.
func renderPlist(opts Options) string {
	args := []string{opts.Binary, "sync"}
	for _, r := range opts.Roots {
		args = append(args, "--root", r)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", Label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", escapeXML(a))
	}
	b.WriteString("  </array>\n")
	fmt.Fprintf(&b, "  <key>StartInterval</key>\n  <integer>%d</integer>\n", int(opts.Interval.Seconds()))
	// Also run once at load, so install gives immediate feedback.
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	fmt.Fprintf(&b, "  <key>StandardOutPath</key>\n  <string>%s</string>\n",
		escapeXML(filepath.Join(opts.LogDir, "lard-client.log")))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key>\n  <string>%s</string>\n",
		escapeXML(filepath.Join(opts.LogDir, "lard-client.log")))
	// Keep the job quiet in Activity Monitor and off the critical path.
	b.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	b.WriteString("  <key>LowPriorityIO</key>\n  <true/>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
