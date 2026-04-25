package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const logoWidth = 65 // visible width of the ASCII art

var logoLines = []string{
	`     _______ __________________          _______ _________ _`,
	`    (       )\__   __/\__   __/|\     /|(  ____ )\__   __/( \`,
	`    | () () |   ) (      ) (   | )   ( || (    )|   ) (   | (`,
	`    | || || |   | |      | |   | (___) || (____)|   | |   | |`,
	`    | |(_)| |   | |      | |   |  ___  ||     __)   | |   | |`,
	`    | |   | |   | |      | |   | (   ) || (\ (      | |   | |`,
	`    | )   ( |___) (___   | |   | )   ( || ) \ \_____) (___| (____/\`,
	`    |/     \|\_______/   )_(   |/     \||/   \__/\_______/(_______/`,
}

// RenderLogo returns the full Mithril ASCII art logo, left-aligned with divider.
func RenderLogo() string {
	return RenderLogoWidth(0)
}

// RenderLogoWidth returns the logo centered within the given width.
// Falls back to compact banner if terminal is too narrow for the ASCII art.
func RenderLogoWidth(width int) string {
	// If terminal is narrower than the logo, use compact banner
	if width > 0 && width < logoWidth+4 {
		return RenderBanner()
	}

	style := lipgloss.NewStyle().Foreground(MithrilTeal)

	// Pad all lines to the same width so centering aligns correctly
	var padded []string
	for _, line := range logoLines {
		pad := logoWidth - len(line)
		if pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		padded = append(padded, line)
	}

	// Center the block within the terminal width
	if width > logoWidth {
		leftPad := (width - logoWidth) / 2
		prefix := strings.Repeat(" ", leftPad)
		var lines []string
		for _, line := range padded {
			lines = append(lines, style.Render(prefix+line))
		}
		return strings.Join(lines, "\n")
	}

	// No centering needed
	var lines []string
	for _, line := range padded {
		lines = append(lines, style.Render(line))
	}
	return strings.Join(lines, "\n")
}

// RenderBanner returns a compact one-line banner.
func RenderBanner() string {
	name := lipgloss.NewStyle().Foreground(MithrilTeal).Bold(true).Render("\u25ce MITHRIL")
	divider := lipgloss.NewStyle().
		Foreground(ColorTextDisabled).
		Render("  " + strings.Repeat("\u2500", 50))
	return "\n  " + name + "\n" + divider
}
