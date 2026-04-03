package setupcmd

import (
	"github.com/charmbracelet/lipgloss"
)

// Mithril Server Theme — matches oc-wallet's MithrilServerTheme exactly
// Colors from: pkg/progress/progress.go (ANSI 85 teal), cmd/mithril-tui (ANSI 240 borders)
var (
	mithrilTeal = lipgloss.Color("85") // Primary accent — THE Mithril color

	// Text hierarchy
	colorTextPrimary   = lipgloss.Color("#e4e4e4")
	colorTextSecondary = lipgloss.Color("#a8a8a8")
	colorTextMuted     = lipgloss.Color("#6c6c6c")
	colorTextDisabled  = lipgloss.Color("#4e4e4e")

	// Semantic
	colorSuccess = lipgloss.Color("85")
	colorError   = lipgloss.Color("196")
	colorWarn    = lipgloss.Color("214")

	// Borders
	colorBorder       = lipgloss.Color("240") // unfocused
	colorBorderActive = lipgloss.Color("85")  // focused
)

// Shared styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(mithrilTeal)

	successStyle = lipgloss.NewStyle().
			Foreground(colorSuccess)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorError)

	warnStyle = lipgloss.NewStyle().
			Foreground(colorWarn)

	dimStyle = lipgloss.NewStyle().
			Foreground(colorTextMuted)
)
