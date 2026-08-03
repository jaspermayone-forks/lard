// Package ui provides terminal styling for lard-client output.
package ui

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"
)

// Link renders a URL as an OSC 8 hyperlink with a subtle green underline,
// matching the charm/crush convention. The id parameter is optional; pass ""
// if you don't need a stable anchor.
func Link(url, id string) string {
	return lipgloss.NewStyle().
		Foreground(charmtone.Guac).
		Underline(true).
		Hyperlink(url, id).
		Render(url)
}

// Success formats text in the same green used by crush for positive status.
func Success(s string) string {
	return lipgloss.NewStyle().Foreground(charmtone.Julep).Render(s)
}

// Subtle formats text in a muted tone for secondary information. Faint
// follows the terminal's own theme rather than fixing a gray that clashes
// with both light and dark backgrounds.
func Subtle(s string) string {
	return lipgloss.NewStyle().Faint(true).Render(s)
}
