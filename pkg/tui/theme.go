// Package tui provides shared theme constants and styles for all Mithril TUI commands.
package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// Mithril Server Theme — matches the teal accent used across all TUI commands.
// Primary color: ANSI 85 (teal), same as pkg/progress/progress.go bars.
var (
	MithrilTeal = lipgloss.Color("85") // Primary accent

	// Text hierarchy (dark-mode optimized: no pure white, disabled still readable)
	ColorTextPrimary   = lipgloss.Color("#e0e0e0") // body text — bright but not glowing
	ColorTextSecondary = lipgloss.Color("#b0b0b0") // secondary labels — ~70% brightness
	ColorTextMuted     = lipgloss.Color("#787878") // key labels, captions — clearly readable
	ColorTextDisabled  = lipgloss.Color("#606060") // hints, shortcuts — visible on dark bg

	// Semantic
	ColorSuccess = MithrilTeal             // teal doubles as success indicator
	ColorError   = lipgloss.Color("196")
	ColorWarn    = lipgloss.Color("214")

	// Borders
	ColorBorder       = lipgloss.Color("240") // unfocused
	ColorBorderActive = MithrilTeal           // focused
)

// Shared styles
var (
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(MithrilTeal)

	SuccessStyle = lipgloss.NewStyle().
			Foreground(ColorSuccess)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(ColorError)

	WarnStyle = lipgloss.NewStyle().
			Foreground(ColorWarn)

	DimStyle = lipgloss.NewStyle().
			Foreground(ColorTextMuted)
)
